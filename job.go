package collabor

import (
	"context"
	"time"

	"golang.org/x/sync/semaphore"
)

type Func func(ctx context.Context, i any) error

type Job struct {
	name    string
	fn      Func
	timeout time.Duration

	deps []*Job
	ntfs []*Job
	sem  *semaphore.Weighted
}

func (j *Job) WithTimeout(timeout time.Duration) *Job {
	j.timeout = timeout
	return j
}
