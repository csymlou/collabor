package collabor

import (
	"context"
	"sync"
	"time"
)

// Func is a unit of work. Implementations should stop promptly when ctx is
// cancelled. Concurrent jobs must synchronize access to shared mutable input.
type Func func(ctx context.Context, input interface{}) error

// Job describes a node in a Collabor graph.
type Job struct {
	mu      sync.RWMutex
	name    string
	fn      Func
	timeout time.Duration
	deps    []*Job
	owner   *Collabor
}

// WithTimeout sets this job's timeout. A non-positive value disables it.
// Timeout cancellation is cooperative; the function must observe ctx.
func (j *Job) WithTimeout(timeout time.Duration) *Job {
	j.mu.Lock()
	j.timeout = timeout
	j.mu.Unlock()
	return j
}

// Name returns the job's name.
func (j *Job) Name() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.name
}
