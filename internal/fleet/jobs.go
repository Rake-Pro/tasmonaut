package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// Job is one bulk operation with a line per device. Jobs live in memory and
// the newest maxJobs are kept.
type Job struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"` // power, command, upgrade, backup
	Summary    string     `json:"summary"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt time.Time  `json:"finishedAt,omitempty"`
	Items      []*JobItem `json:"items"`
}

// JobItem is one device's progress inside a job.
type JobItem struct {
	DeviceID string    `json:"deviceId"`
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	State    string    `json:"state"` // pending, running, ok, failed
	Message  string    `json:"message,omitempty"`
	Updated  time.Time `json:"updated"`
}

// Done reports whether every item has settled.
func (j *Job) Done() bool { return !j.FinishedAt.IsZero() }

// Counts returns how many items are ok and how many failed.
func (j *Job) Counts() (ok, failed int) {
	for _, it := range j.Items {
		switch it.State {
		case "ok":
			ok++
		case "failed":
			failed++
		}
	}
	return
}

const maxJobs = 50

type jobStore struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

func newJobStore() *jobStore { return &jobStore{jobs: make(map[string]*Job)} }

func newJobID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *jobStore) create(kind, summary string, devices []*Device) *Job {
	j := &Job{ID: newJobID(), Kind: kind, Summary: summary, StartedAt: time.Now()}
	for _, d := range devices {
		j.Items = append(j.Items, &JobItem{DeviceID: d.ID, Name: d.DisplayName(), Host: d.Host, State: "pending", Updated: j.StartedAt})
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	if len(s.jobs) > maxJobs {
		all := s.listLocked()
		for _, old := range all[maxJobs:] {
			delete(s.jobs, old.ID)
		}
	}
	s.mu.Unlock()
	return j
}

// set updates one item. Jobs are read concurrently by the API, so every
// mutation goes through the store lock.
func (s *jobStore) set(j *Job, it *JobItem, state, msg string) {
	s.mu.Lock()
	it.State, it.Message, it.Updated = state, msg, time.Now()
	s.mu.Unlock()
}

func (s *jobStore) finish(j *Job) {
	s.mu.Lock()
	j.FinishedAt = time.Now()
	s.mu.Unlock()
}

func (s *jobStore) get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return j.snapshot(), true
}

func (s *jobStore) list() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.listLocked() {
		out = append(out, j.snapshot())
	}
	return out
}

// listLocked returns jobs newest first; the caller holds the lock.
func (s *jobStore) listLocked() []*Job {
	all := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		all = append(all, j)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].StartedAt.After(all[b].StartedAt) })
	return all
}

// snapshot copies a job so readers never see a half-written item.
func (j *Job) snapshot() Job {
	c := *j
	c.Items = make([]*JobItem, len(j.Items))
	for i, it := range j.Items {
		cp := *it
		c.Items[i] = &cp
	}
	return c
}
