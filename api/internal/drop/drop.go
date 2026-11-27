// Package drop implements Kamui Drop: authorized file transfers between
// principals with streaming upload/download, checksums, recipient approval,
// expiration and bounded storage. Backend and CLI only; received files are
// never executed or opened automatically.
package drop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	MaxTransferBytes = int64(512) << 20
	MaxFilenameLen   = 255
)

var (
	ErrNotFound  = errors.New("transfer not found")
	ErrBadInput  = errors.New("invalid transfer request")
	ErrForbidden = errors.New("transfer not authorized")
	ErrTooLarge  = errors.New("file exceeds the transfer size limit")
	ErrConflict  = errors.New("conflict")

	filenameRe = regexp.MustCompile(`^[^/\\]{1,255}$`)
)

type Store struct {
	db   *sql.DB
	root string
}

func NewStore(db *sql.DB, root string) *Store {
	return &Store{db: db, root: root}
}

type Transfer struct {
	ID          string     `json:"id"`
	SenderID    string     `json:"sender_id"`
	RecipientID string     `json:"recipient_id"`
	Filename    string     `json:"filename"`
	SizeBytes   int64      `json:"size_bytes"`
	Checksum    string     `json:"checksum,omitempty"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

const transferColumns = "id,sender_id,recipient_id,filename,size_bytes,checksum,status,reason,expires_at,created_at,completed_at"

func scanTransfer(row interface{ Scan(...any) error }) (Transfer, error) {
	var t Transfer
	var expires, createdAt int64
	var completed sql.NullInt64
	if err := row.Scan(&t.ID, &t.SenderID, &t.RecipientID, &t.Filename, &t.SizeBytes, &t.Checksum,
		&t.Status, &t.Reason, &expires, &createdAt, &completed); err != nil {
		return t, err
	}
	t.ExpiresAt = time.Unix(0, expires).UTC()
	t.CreatedAt = time.Unix(0, createdAt).UTC()
	if completed.Valid {
		c := time.Unix(0, completed.Int64).UTC()
		t.CompletedAt = &c
	}
	return t, nil
}

func (s *Store) transferPath(id string) (string, error) {
	if len(id) < 10 || strings.ContainsAny(id, "/\\.") {
		return "", ErrBadInput
	}
	return filepath.Join(s.root, "drop", id), nil
}

// SanitizeFilename rejects traversal and control characters; the stored name
// never carries directory components.
func SanitizeFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxFilenameLen || !filenameRe.MatchString(name) {
		return "", fmt.Errorf("%w: filename is missing, unsafe or too long", ErrBadInput)
	}
	if strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("%w: filename must not start with a dot or contain control characters", ErrBadInput)
	}
	base := filepath.Base(name)
	if base != name || base == "." || base == ".." {
		return "", fmt.Errorf("%w: filename must not contain directory components", ErrBadInput)
	}
	return base, nil
}

// CreateTransfer opens a transfer and streams the upload. Status is pending
// until the recipient approves; approval gates download, never intake.
func (s *Store) CreateTransfer(ctx context.Context, senderID, recipientID, filename string, ttl time.Duration, body io.Reader) (Transfer, error) {
	safe, err := SanitizeFilename(filename)
	if err != nil {
		return Transfer{}, err
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = 24 * time.Hour
	}
	if recipientID == "" || recipientID == senderID {
		return Transfer{}, fmt.Errorf("%w: a distinct recipient is required", ErrBadInput)
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return Transfer{}, err
	}
	id := "xfr-" + hex.EncodeToString(idBytes)
	path, err := s.transferPath(id)
	if err != nil {
		return Transfer{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return Transfer{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return Transfer{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(body, MaxTransferBytes+1))
	closeErr := tmp.Close()
	if err != nil {
		return Transfer{}, err
	}
	if closeErr != nil {
		return Transfer{}, closeErr
	}
	if size > MaxTransferBytes {
		return Transfer{}, ErrTooLarge
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO transfers(id,sender_id,recipient_id,filename,size_bytes,checksum,status,expires_at,created_at) VALUES(?,?,?,?,?,?,'pending',?,?)",
		id, senderID, recipientID, safe, size, "sha256:"+hex.EncodeToString(hasher.Sum(nil)), now.Add(ttl).UnixNano(), now.UnixNano())
	if err != nil {
		return Transfer{}, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM transfers WHERE id=?", id)
		return Transfer{}, err
	}
	return s.GetTransferAs(ctx, id, senderID)
}

func (s *Store) GetTransferAs(ctx context.Context, id, viewer string) (Transfer, error) {
	transfer, err := s.GetTransfer(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	if transfer.SenderID != viewer && transfer.RecipientID != viewer && viewer != identityAdmin {
		return Transfer{}, ErrForbidden
	}
	return transfer, nil
}

const identityAdmin = "local-admin"

func (s *Store) GetTransfer(ctx context.Context, id string) (Transfer, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+transferColumns+" FROM transfers WHERE id=?", id)
	transfer, err := scanTransfer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Transfer{}, ErrNotFound
	}
	return transfer, err
}

// ListTransfers shows the transfers a principal sent or received.
func (s *Store) ListTransfers(ctx context.Context, viewer string) ([]Transfer, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+transferColumns+" FROM transfers WHERE sender_id=? OR recipient_id=? ORDER BY created_at DESC LIMIT 100",
		viewer, viewer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Transfer, 0)
	for rows.Next() {
		transfer, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, transfer)
	}
	return items, rows.Err()
}

// Approve authorizes the download. Only the named recipient can approve.
func (s *Store) Approve(ctx context.Context, id, recipientID string) (Transfer, error) {
	transfer, err := s.GetTransfer(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	if transfer.RecipientID != recipientID {
		return Transfer{}, ErrForbidden
	}
	if transfer.Status != "pending" {
		return Transfer{}, fmt.Errorf("%w: transfer is %s", ErrConflict, transfer.Status)
	}
	if time.Now().UTC().After(transfer.ExpiresAt) {
		_, _ = s.db.ExecContext(ctx, "UPDATE transfers SET status='expired' WHERE id=?", id)
		return Transfer{}, fmt.Errorf("%w: transfer has expired", ErrConflict)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE transfers SET status='approved' WHERE id=?", id); err != nil {
		return Transfer{}, err
	}
	return s.GetTransfer(ctx, id)
}

// Reject declines the transfer; the blob is removed immediately.
func (s *Store) Reject(ctx context.Context, id, recipientID string) (Transfer, error) {
	transfer, err := s.GetTransfer(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	if transfer.RecipientID != recipientID {
		return Transfer{}, ErrForbidden
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE transfers SET status='rejected', reason='declined by recipient' WHERE id=?", id); err != nil {
		return Transfer{}, err
	}
	if path, err := s.transferPath(id); err == nil {
		_ = os.Remove(path)
	}
	return s.GetTransfer(ctx, id)
}

// OpenForDownload streams the blob after authorization, checksum verification
// happens on the client against the recorded digest.
func (s *Store) OpenForDownload(ctx context.Context, id, viewer string) (*os.File, Transfer, error) {
	transfer, err := s.GetTransferAs(ctx, id, viewer)
	if err != nil {
		return nil, Transfer{}, err
	}
	if time.Now().UTC().After(transfer.ExpiresAt) {
		_, _ = s.db.ExecContext(ctx, "UPDATE transfers SET status='expired' WHERE id=?", id)
		return nil, Transfer{}, fmt.Errorf("%w: transfer has expired", ErrConflict)
	}
	switch transfer.Status {
	case "approved":
	case "pending":
		return nil, Transfer{}, fmt.Errorf("%w: the recipient has not approved this transfer", ErrForbidden)
	default:
		return nil, Transfer{}, fmt.Errorf("%w: transfer is %s", ErrConflict, transfer.Status)
	}
	path, err := s.transferPath(id)
	if err != nil {
		return nil, Transfer{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Transfer{}, ErrNotFound
	}
	if err != nil {
		return nil, Transfer{}, err
	}
	return file, transfer, nil
}

// MarkCompleted records the download for history.
func (s *Store) MarkCompleted(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, "UPDATE transfers SET status='completed', completed_at=? WHERE id=? AND status='approved'",
		time.Now().UTC().UnixNano(), id)
}

// Cancel removes a pending or approved transfer by its sender.
func (s *Store) Cancel(ctx context.Context, id, senderID string) error {
	transfer, err := s.GetTransferAs(ctx, id, senderID)
	if err != nil {
		return err
	}
	if transfer.SenderID != senderID {
		return ErrForbidden
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE transfers SET status='cancelled' WHERE id=?", id); err != nil {
		return err
	}
	if path, err := s.transferPath(id); err == nil {
		_ = os.Remove(path)
	}
	return nil
}

// PruneExpired removes expired or rejected transfer blobs and stamps status.
func (s *Store) PruneExpired(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM transfers WHERE expires_at<? AND status IN ('pending','approved')", time.Now().UTC().UnixNano())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_, _ = s.db.ExecContext(ctx, "UPDATE transfers SET status='expired' WHERE id=?", id)
		if path, err := s.transferPath(id); err == nil {
			_ = os.Remove(path)
		}
	}
}
