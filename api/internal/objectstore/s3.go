// Package objectstore implements Kamui Store: project-scoped object storage
// over a replaceable backend (local filesystem implemented; S3-compatible
// adapter is the same interface), with checksums, quotas, atomic uploads and
// pagination.
package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3BackendConfig holds configuration for an S3-compatible storage backend.
type S3BackendConfig struct {
	Region          string
	Endpoint        string // Custom endpoint for S3-compatible services (MinIO, etc.)
	Bucket          string // Target bucket name
	AccessKeyID     string
	SecretAccessKey string
	UsePathStyle    bool // Use path-style addressing (required for MinIO)
	ForcePathStyle  bool // Alias for UsePathStyle
}

// S3Backend implements the Backend interface using AWS SDK v2 for S3-compatible storage.
type S3Backend struct {
	client     *s3.Client
	bucket     string
	rootPrefix string // Prefix for all keys (e.g., "kamui/objects/")
}

var _ Backend = (*S3Backend)(nil)

// NewS3Backend creates a new S3-compatible storage backend.
// If cfg.Endpoint is empty, it uses the default AWS S3 endpoint.
// The backend will create the bucket if it doesn't exist.
func NewS3Backend(ctx context.Context, cfg S3BackendConfig) (*S3Backend, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("bucket name is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("access key ID and secret access key are required")
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		)),
	}

	if cfg.Endpoint != "" {
		opts = append(opts, func(o *config.LoadOptions) error {
			o.BaseEndpoint = cfg.Endpoint
			return nil
		})
	}

	sdkConfig, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(sdkConfig, func(o *s3.Options) {
		if cfg.UsePathStyle || cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
	})

	// Ensure bucket exists
	_, err = s3Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &cfg.Bucket})
	if err != nil {
		// Try to create the bucket
		_, err = s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &cfg.Bucket})
		if err != nil {
			// If bucket already exists (race), that's fine
			if !strings.Contains(err.Error(), "BucketAlreadyExists") &&
				!strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
				return nil, fmt.Errorf("failed to ensure bucket exists: %w", err)
			}
		}
	}

	rootPrefix := "kamui/objects/"
	return &S3Backend{
		client:     s3Client,
		bucket:     cfg.Bucket,
		rootPrefix: rootPrefix,
	}, nil
}

// resolveKey returns the full S3 key for a given relative path.
func (b *S3Backend) resolveKey(key string) string {
	// Ensure the key is relative and doesn't have leading slashes
	key = strings.TrimPrefix(key, "/")
	return b.rootPrefix + key
}

// Put uploads a blob to S3, computing SHA-256 checksum on the fly.
func (b *S3Backend) Put(ctx context.Context, key string, body io.Reader) (int64, string, error) {
	fullKey := b.resolveKey(key)

	// Use a pipe to compute checksum while uploading
	pr, pw := io.Pipe()
	hasher := sha256.New()

	// Start a goroutine to copy data to both the pipe and the hasher
	go func() {
		defer pw.Close()
		tee := io.TeeReader(body, hasher)
		if _, err := io.Copy(pw, tee); err != nil {
			pw.CloseWithError(err)
		}
	}()

	// Upload via PutObject
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &b.bucket,
		Key:    &fullKey,
		Body:   pr,
	})
	if err != nil {
		return 0, "", fmt.Errorf("s3 upload failed: %w", err)
	}

	// Get the size from a head request
	headOut, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &b.bucket,
		Key:    &fullKey,
	})
	if err != nil {
		return 0, "", fmt.Errorf("failed to get object size after upload: %w", err)
	}
	size := int64(0)
	if headOut.ContentLength != nil {
		size = *headOut.ContentLength
	}
	checksum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	return size, checksum, nil
}

// Open returns a ReadSeekCloser for the object at the given key.
func (b *S3Backend) Open(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	fullKey := b.resolveKey(key)

	// First, get object metadata to verify existence and get size
	headOut, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &b.bucket,
		Key:    &fullKey,
	})
	if err != nil {
		var apiErr *types.NotFound
		if err != nil && (err == io.EOF || (apiErr != nil && errors.As(err, &apiErr))) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 head object failed: %w", err)
	}

	size := headOut.ContentLength
	if size == nil {
		size = aws.Int64(0)
	}

	// Create a custom reader that supports seeking via range requests
	return &s3ReadSeekCloser{
		client: b.client,
		bucket: b.bucket,
		key:    fullKey,
		size:   *size,
	}, nil
}

// Delete removes an object from S3.
func (b *S3Backend) Delete(ctx context.Context, key string) error {
	fullKey := b.resolveKey(key)
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &b.bucket,
		Key:    &fullKey,
	})
	if err != nil {
		var apiErr *types.NotFound
		if errors.As(err, &apiErr) {
			return ErrNotFound
		}
		return fmt.Errorf("s3 delete failed: %w", err)
	}
	return nil
}

// s3ReadSeekCloser implements io.ReadSeekCloser for S3 objects using range requests.
type s3ReadSeekCloser struct {
	client     *s3.Client
	bucket     string
	key        string
	size       int64
	offset     int64
	buffer     []byte
	bufferPos  int
	bufferSize int
	closed     bool
}

func (r *s3ReadSeekCloser) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}

	// If we have buffered data, consume it first
	if r.bufferPos < r.bufferSize {
		n := copy(p, r.buffer[r.bufferPos:r.bufferSize])
		r.bufferPos += n
		r.offset += int64(n)
		return n, nil
	}

	// Determine how much to read
	readSize := int64(len(p))
	if r.offset+readSize > r.size {
		readSize = r.size - r.offset
	}

	// Download the range using S3 GetObject with Range header
	end := r.offset + readSize - 1
	rangeHeader := fmt.Sprintf("bytes=%d-%d", r.offset, end)

	out, err := r.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: &r.bucket,
		Key:    &r.key,
		Range:  &rangeHeader,
	})
	if err != nil {
		return 0, fmt.Errorf("s3 download failed: %w", err)
	}
	defer out.Body.Close()

	r.buffer = make([]byte, readSize)
	n, err := io.ReadFull(out.Body, r.buffer)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, fmt.Errorf("s3 read failed: %w", err)
	}
	r.bufferPos = 0
	r.bufferSize = n

	n = copy(p, r.buffer[:r.bufferSize])
	r.bufferPos += n
	r.offset += int64(n)
	return n, nil
}

func (r *s3ReadSeekCloser) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = r.offset + offset
	case io.SeekEnd:
		newOffset = r.size + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}
	if newOffset < 0 {
		newOffset = 0
	}
	if newOffset > r.size {
		newOffset = r.size
	}
	r.offset = newOffset
	// Invalidate buffer
	r.bufferPos = r.bufferSize
	return r.offset, nil
}

func (r *s3ReadSeekCloser) Close() error {
	r.closed = true
	return nil
}
