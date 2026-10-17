package scheduler

import (
	"errors"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var (
	ErrNotFound = errors.New("job not found")
	ErrConflict = errors.New("conflict")
)

type Job struct {
	ID         string   `json:"id"`
	Image      string   `json:"image"`
	Command    []string `json:"command,omitempty"`
	Status     string   `json:"status"`
	NodeID     string   `json:"node_id,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Logs       string   `json:"logs,omitempty"`
	CreatedAt  string   `json:"created_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
}
