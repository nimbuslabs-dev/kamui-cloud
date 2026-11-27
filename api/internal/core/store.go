package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/001.sql
var schema001 string

//go:embed migrations/002.sql
var schema002 string

const LatestSchemaVersion = 2

type Repository interface {
	EventReader
	Submit(context.Context, Submission, string) (Workload, bool, error)
	Get(context.Context, string) (Workload, error)
	List(context.Context, bool) ([]Workload, error)
	Save(context.Context, Workload, bool) error
	SaveWorker(context.Context, Worker) error
	Worker(context.Context) (Worker, error)
	Cleanup(context.Context) ([]ExecutionRef, error)
	Cleaned(context.Context, ExecutionRef) error
}

type Store struct {
	db   *sql.DB
	lock *sql.DB
}

func openDB(path, pragmas string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// SQLite's WAL and shared-memory files inherit the database file mode.
	file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	slashPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	uri := url.URL{Scheme: "file", Path: slashPath, RawQuery: pragmas}
	db, err := sql.Open("sqlite", uri.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func Open(ctx context.Context, path string) (*Store, error) {
	// Hold a SQLite write lock on a separate file for the process lifetime.
	// The OS releases it on exit, without making the workload database read-only.
	lock, err := openDB(path+".lock", "_pragma=busy_timeout(0)")
	if err != nil {
		return nil, err
	}
	if _, err := lock.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		lock.Close()
		return nil, fmt.Errorf("acquire coordinator database lock: %w", err)
	}
	db, err := openDB(path, "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{db: db, lock: lock}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return errors.Join(s.db.Close(), s.lock.Close()) }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	migrations := []struct {
		version int
		sql     string
	}{
		{1, schema001},
		{2, schema002},
	}
	for _, migration := range migrations {
		if version < migration.version {
			if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
				return fmt.Errorf("migration %d: %w", migration.version, err)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", migration.version)); err != nil {
				return err
			}
		}
	}
	if version > LatestSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than this coordinator", version)
	}
	return tx.Commit()
}

const columns = "snapshot, revision, log_tail"
const active = "state NOT IN ('SUCCEEDED','FAILED','CANCELLED','STOPPED')"
const defaultProject = "default"

type scanner interface{ Scan(...any) error }

func scanWorkload(row scanner) (Workload, error) {
	var w Workload
	var data []byte
	if err := row.Scan(&data, &w.Revision, &w.LogTail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return w, ErrNotFound
		}
		return w, err
	}
	return w, json.Unmarshal(data, &w)
}

func (s *Store) Submit(ctx context.Context, spec Submission, key string) (Workload, bool, error) {
	return s.SubmitForProject(ctx, defaultProject, spec, key)
}

func (s *Store) SubmitForProject(ctx context.Context, projectID string, spec Submission, key string) (Workload, bool, error) {
	var zero Workload
	if projectID == "" {
		projectID = defaultProject
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return zero, false, err
	}
	sum := sha256.Sum256(data)
	fingerprint := hex.EncodeToString(sum[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, false, err
	}
	defer tx.Rollback()
	var priorID, priorHash string
	err = tx.QueryRowContext(ctx, "SELECT id, fingerprint FROM workloads WHERE project_id=? AND idempotency_key=?", projectID, key).Scan(&priorID, &priorHash)
	if err == nil {
		if priorHash != fingerprint {
			return zero, false, ErrConflict
		}
		prior, err := scanWorkload(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM workloads WHERE id=?", priorID))
		return prior, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, false, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM workloads WHERE "+active).Scan(&count); err != nil {
		return zero, false, err
	}
	if count >= 128 {
		return zero, false, ErrLimit
	}
	now := time.Now().UTC()
	w := Workload{ID: newID(), ProjectID: projectID, Submission: spec, State: Pending, CreatedAt: now, UpdatedAt: now, Revision: 1}
	snapshot, err := json.Marshal(w)
	if err != nil {
		return zero, false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO workloads(id,project_id,idempotency_key,fingerprint,state,created_at,revision,snapshot) VALUES(?,?,?,?,?,?,?,?)",
		w.ID, w.ProjectID, key, fingerprint, w.State, now.UnixNano(), w.Revision, snapshot)
	if err != nil {
		return zero, false, err
	}
	if err := recordEvent(ctx, tx, w); err != nil {
		return zero, false, err
	}
	return w, true, tx.Commit()
}

func (s *Store) Get(ctx context.Context, id string) (Workload, error) {
	return scanWorkload(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM workloads WHERE id=?", id))
}

func (s *Store) List(ctx context.Context, activeOnly bool) ([]Workload, error) {
	query := "SELECT " + columns + " FROM workloads ORDER BY created_at DESC, id DESC LIMIT 100"
	if activeOnly {
		query = "SELECT " + columns + " FROM workloads WHERE " + active + " ORDER BY created_at, id"
	}
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Workload, 0)
	for rows.Next() {
		w, err := scanWorkload(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, w)
	}
	return items, rows.Err()
}

func (s *Store) ListForProjects(ctx context.Context, projectIDs []string) ([]Workload, error) {
	if len(projectIDs) == 0 {
		return []Workload{}, nil
	}
	placeholders := strings.Repeat("?,", len(projectIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(projectIDs))
	for i, id := range projectIDs {
		args[i] = id
	}
	query := "SELECT " + columns + " FROM workloads WHERE project_id IN (" + placeholders + ") ORDER BY created_at DESC, id DESC LIMIT 100"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Workload, 0)
	for rows.Next() {
		w, err := scanWorkload(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, w)
	}
	return items, rows.Err()
}

func (s *Store) Save(ctx context.Context, w Workload, cleanup bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old State
	err = tx.QueryRowContext(ctx, "SELECT state FROM workloads WHERE id=? AND revision=?", w.ID, w.Revision).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	w.UpdatedAt = time.Now().UTC()
	snapshot, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE workloads SET snapshot=?, state=?, log_tail=?, revision=revision+1 WHERE id=?", snapshot, w.State, w.LogTail, w.ID)
	if err != nil {
		return err
	}
	if old != w.State {
		if err := recordEvent(ctx, tx, w); err != nil {
			return err
		}
	}
	if cleanup {
		if _, err := tx.ExecContext(ctx, "INSERT INTO execution_cleanup VALUES(?,?)", w.ID, w.Attempt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func recordEvent(ctx context.Context, tx *sql.Tx, w Workload) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO events(workload_id,state,attempt,at) VALUES(?,?,?,?)",
		w.ID, w.State, w.Attempt, w.UpdatedAt.UnixNano())
	return err
}

func (s *Store) Events(ctx context.Context, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,workload_id,state,attempt,at FROM events WHERE id>? ORDER BY id LIMIT 100", after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &e.WorkloadID, &e.State, &e.Attempt, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(0, at).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) EventsForProjects(ctx context.Context, projectIDs []string, after int64) ([]Event, error) {
	if len(projectIDs) == 0 {
		return []Event{}, nil
	}
	placeholders := strings.Repeat("?,", len(projectIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, 1+len(projectIDs))
	args = append(args, after)
	for _, id := range projectIDs {
		args = append(args, id)
	}
	query := "SELECT e.id,e.workload_id,e.state,e.attempt,e.at FROM events e JOIN workloads w ON w.id=e.workload_id WHERE e.id>? AND w.project_id IN (" + placeholders + ") ORDER BY e.id LIMIT 100"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &e.WorkloadID, &e.State, &e.Attempt, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(0, at).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) SaveWorker(ctx context.Context, worker Worker) error {
	data, err := json.Marshal(worker)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO workers VALUES(?,?) ON CONFLICT(id) DO UPDATE SET snapshot=excluded.snapshot", worker.ID, data)
	return err
}

func (s *Store) Worker(ctx context.Context) (Worker, error) {
	var raw []byte
	var worker Worker
	err := s.db.QueryRowContext(ctx, "SELECT snapshot FROM workers WHERE id=?", LocalWorker).Scan(&raw)
	if err != nil {
		return worker, err
	}
	return worker, json.Unmarshal(raw, &worker)
}

func (s *Store) Cleanup(ctx context.Context) ([]ExecutionRef, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT workload_id, attempt FROM execution_cleanup ORDER BY rowid LIMIT 16")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []ExecutionRef
	for rows.Next() {
		var ref ExecutionRef
		if err := rows.Scan(&ref.WorkloadID, &ref.Attempt); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (s *Store) Cleaned(ctx context.Context, ref ExecutionRef) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM execution_cleanup WHERE workload_id=? AND attempt=?", ref.WorkloadID, ref.Attempt)
	return err
}
