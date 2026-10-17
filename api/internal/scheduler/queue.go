package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

type jobEntry struct {
	job Job
	seq uint64
}

type Queue struct {
	mu   sync.Mutex
	jobs map[string]*jobEntry
	seq  uint64
	now  func() time.Time
}

func NewQueue(now func() time.Time) *Queue {
	if now == nil {
		now = time.Now
	}
	return &Queue{
		jobs: make(map[string]*jobEntry),
		now:  now,
	}
}

func (q *Queue) generateID() string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id := "j-" + hex.EncodeToString(b[:])
		if _, exists := q.jobs[id]; !exists {
			return id
		}
	}
}

func (q *Queue) Create(image string, command []string) Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	id := q.generateID()
	q.seq++
	entry := &jobEntry{
		job: Job{
			ID:        id,
			Image:     image,
			Command:   command,
			Status:    StatusQueued,
			CreatedAt: q.now().UTC().Format(time.RFC3339),
		},
		seq: q.seq,
	}
	q.jobs[id] = entry
	return entry.job
}

func (q *Queue) List() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	entries := make([]*jobEntry, 0, len(q.jobs))
	for _, entry := range q.jobs {
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].seq > entries[j].seq
	})

	jobs := make([]Job, 0, len(entries))
	for _, entry := range entries {
		jobs = append(jobs, entry.job)
	}
	return jobs
}

func (q *Queue) Get(id string) (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry, exists := q.jobs[id]
	if !exists {
		return Job{}, ErrNotFound
	}
	return entry.job, nil
}

func (q *Queue) Claim(nodeID string) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	var oldest *jobEntry
	for _, entry := range q.jobs {
		if entry.job.Status == StatusQueued {
			if oldest == nil || entry.seq < oldest.seq {
				oldest = entry
			}
		}
	}

	if oldest == nil {
		return Job{}, false
	}

	oldest.job.Status = StatusRunning
	oldest.job.NodeID = nodeID
	return oldest.job, true
}

func (q *Queue) Finish(id string, exitCode int, logs string) (Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry, exists := q.jobs[id]
	if !exists {
		return Job{}, ErrNotFound
	}

	if entry.job.Status != StatusRunning {
		return Job{}, ErrConflict
	}

	if exitCode == 0 {
		entry.job.Status = StatusSucceeded
	} else {
		entry.job.Status = StatusFailed
	}

	entry.job.ExitCode = &exitCode
	entry.job.Logs = logs
	entry.job.FinishedAt = q.now().UTC().Format(time.RFC3339)
	return entry.job, nil
}
