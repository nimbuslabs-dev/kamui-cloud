package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/client"
)

// Builder executes image builds on the coordinator's trusted daemon through
// the Docker SDK. Build contexts are materialized into a bounded temporary
// directory from authorized sources only; nothing of the host is mounted.
type Builder struct {
	client  *client.Client
	queue   chan buildInput
	workDir string
}

func NewBuilder(cli *client.Client, workDir string) *Builder {
	return &Builder{client: cli, queue: make(chan buildInput, 8), workDir: workDir}
}

type buildResult struct {
	Digest string
	Err    error
}

const (
	MaxGitArchiveBytes = int64(64) << 20
	MaxExtractedBytes  = int64(96) << 20
)

// BuildFromGit fetches an authorized GitHub repository snapshot, materializes
// it into a bounded build context and builds it.
func (b *Builder) BuildFromGit(ctx context.Context, repoURL, commit, token, baseTag string) buildResult {
	archive, sha, err := fetchGitArchive(ctx, repoURL, commit, token)
	if err != nil {
		return buildResult{Err: err}
	}
	if sha != "" {
		_ = sha // recorded by the store from the build input; archive content is authoritative
	}
	contextDir, err := b.materialize(archive)
	if err != nil {
		return buildResult{Err: err}
	}
	defer os.RemoveAll(contextDir)
	return b.buildContext(ctx, contextDir, baseTag)
}

// BuildFromDockerfile builds from a client-supplied Dockerfile with a context
// of just that file — no arbitrary content, no host paths.
func (b *Builder) BuildFromDockerfile(ctx context.Context, dockerfile []byte, baseTag string) buildResult {
	if len(dockerfile) == 0 || len(dockerfile) > 128<<10 {
		return buildResult{Err: fmt.Errorf("%w: a Dockerfile of 1 byte to 128 KiB is required", ErrBadInput)}
	}
	if !safeDockerfile(dockerfile) {
		return buildResult{Err: fmt.Errorf("%w: the Dockerfile mounts or copies from unauthorized host paths", ErrBadInput)}
	}
	contextDir, err := os.MkdirTemp(b.workDir, "ctx-*")
	if err != nil {
		return buildResult{Err: err}
	}
	defer os.RemoveAll(contextDir)
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), dockerfile, 0600); err != nil {
		return buildResult{Err: err}
	}
	return b.buildContext(ctx, contextDir, baseTag)
}

// ResolveImage validates an operator-provisioned image that exists locally.
func (b *Builder) ResolveImage(ctx context.Context, image string) buildResult {
	inspect, _, err := b.client.ImageInspectWithRaw(ctx, image)
	if err != nil {
		return buildResult{Err: fmt.Errorf("%w: image %s is not available on the coordinator daemon", ErrBadInput, image)}
	}
	if inspect.Os != "linux" {
		return buildResult{Err: fmt.Errorf("%w: deploy images must be Linux images", ErrBadInput)}
	}
	return buildResult{Digest: inspect.ID}
}

// safeDockerfile rejects COPY/ADD sources that escape the context and
// mounts that would touch the host.
func safeDockerfile(dockerfile []byte) bool {
	for _, line := range strings.Split(string(dockerfile), "\n") {
		line = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "COPY ") || strings.HasPrefix(upper, "ADD ") {
			rest := strings.TrimPrefix(upper, "COPY ")
			rest = strings.TrimPrefix(rest, "ADD ")
			if strings.Contains(rest, "..") || strings.Contains(rest, "--from=/") {
				return false
			}
		}
		if strings.Contains(upper, "RUN docker") || strings.Contains(upper, "MOUNT") && strings.Contains(upper, "TYPE=BIND") {
			return false
		}
	}
	return true
}

func fetchGitArchive(ctx context.Context, repoURL, commit, token string) ([]byte, string, error) {
	parts := strings.TrimPrefix(repoURL, "https://github.com/")
	parts = strings.TrimSuffix(parts, ".git")
	parts = strings.Trim(parts, "/")
	if strings.Count(parts, "/") != 1 {
		return nil, "", fmt.Errorf("%w: repository URL must be https://github.com/{owner}/{repo}", ErrBadInput)
	}
	sha := commit
	if sha == "" {
		sha = "HEAD"
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/tarball/%s", parts, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch repository snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
		return nil, "", fmt.Errorf("%w: repository is unavailable or the credentials are not authorized for it", ErrForbidden)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("repository snapshot request returned HTTP %d", resp.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, MaxGitArchiveBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(archive)) > MaxGitArchiveBytes {
		return nil, "", fmt.Errorf("%w: repository snapshot exceeds the build size limit", ErrBadInput)
	}
	return archive, "", nil
}

// materialize extracts a GitHub tarball (which nests a single top-level
// directory) into a temporary build context, guarding against path traversal,
// symlinks and decompression bombs.
func (b *Builder) materialize(archive []byte) (string, error) {
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", fmt.Errorf("%w: repository snapshot is not a valid tarball", ErrBadInput)
	}
	contextDir, err := os.MkdirTemp(b.workDir, "ctx-*")
	if err != nil {
		return "", err
	}
	tarReader := tar.NewReader(reader)
	total := int64(0)
	entries := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.RemoveAll(contextDir)
			return "", fmt.Errorf("%w: repository snapshot is not a valid tarball", ErrBadInput)
		}
		entries++
		if entries > 20000 {
			os.RemoveAll(contextDir)
			return "", fmt.Errorf("%w: repository snapshot has too many entries", ErrBadInput)
		}
		name := header.Name
		if idx := strings.Index(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		if name == "" {
			continue
		}
		clean := filepath.Clean(name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			os.RemoveAll(contextDir)
			return "", fmt.Errorf("%w: repository snapshot contains an unsafe path", ErrBadInput)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Join(contextDir, clean), 0700); err != nil {
				os.RemoveAll(contextDir)
				return "", err
			}
		case tar.TypeReg:
			total += header.Size
			if total > MaxExtractedBytes {
				os.RemoveAll(contextDir)
				return "", fmt.Errorf("%w: repository snapshot exceeds the extracted size limit", ErrBadInput)
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(contextDir, clean)), 0700); err != nil {
				os.RemoveAll(contextDir)
				return "", err
			}
			file, err := os.OpenFile(filepath.Join(contextDir, clean), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
			if err != nil {
				os.RemoveAll(contextDir)
				return "", err
			}
			if _, err := io.Copy(file, io.LimitReader(tarReader, header.Size+1)); err != nil {
				file.Close()
				os.RemoveAll(contextDir)
				return "", err
			}
			if err := file.Close(); err != nil {
				os.RemoveAll(contextDir)
				return "", err
			}
		default:
			// Symlinks, devices and other special entries are ignored: the
			// build context must be plain files only.
			continue
		}
	}
	if _, err := os.Stat(filepath.Join(contextDir, "Dockerfile")); err != nil {
		os.RemoveAll(contextDir)
		return "", fmt.Errorf("%w: the repository has no Dockerfile at its root", ErrBadInput)
	}
	return contextDir, nil
}

func (b *Builder) buildContext(ctx context.Context, contextDir, baseTag string) buildResult {
	tarContext, err := tarDirectory(contextDir)
	if err != nil {
		return buildResult{Err: err}
	}
	if int64(len(tarContext)) > MaxBuildContextBytes {
		return buildResult{Err: fmt.Errorf("%w: build context exceeds %d bytes", ErrBadInput, MaxBuildContextBytes)}
	}
	tag := baseTag
	if tag == "" {
		tag = "kamui-build:pending"
	}
	result, err := b.client.ImageBuild(ctx, bytes.NewReader(tarContext), build.ImageBuildOptions{
		Tags:           []string{tag},
		Dockerfile:     "Dockerfile",
		Platform:       "linux/" + hostArch(),
		SuppressOutput: false,
		NoCache:        false,
	})
	if err != nil {
		return buildResult{Err: fmt.Errorf("image build failed: %w", err)}
	}
	defer result.Body.Close()
	logStream, _ := io.ReadAll(io.LimitReader(result.Body, MaxBuildLogBytes))
	var failure struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(logStream, &failure)
	if failure.Error != "" {
		return buildResult{Err: fmt.Errorf("image build failed: %s", failure.Error)}
	}
	inspect, _, err := b.client.ImageInspectWithRaw(ctx, tag)
	if err != nil {
		return buildResult{Err: fmt.Errorf("built image is not inspectable: %w", err)}
	}
	if inspect.Os != "linux" {
		return buildResult{Err: fmt.Errorf("built image must be a Linux image")}
	}
	return buildResult{Digest: inspect.ID}
}

func tarDirectory(dir string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return nil
		}
		if info.IsDir() {
			return writer.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeDir, Mode: 0700, ModTime: info.ModTime()})
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func hostArch() string {
	arch := os.Getenv("KAMUI_BUILD_ARCH")
	if arch == "" {
		return "arm64"
	}
	return arch
}

func newID(prefix string) string {
	var bytes [8]byte
	_, _ = rand.Read(bytes[:])
	return prefix + "-" + hex.EncodeToString(bytes[:])
}
