package collabor

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/cloudwego/kitex/pkg/klog"
	"golang.org/x/sync/semaphore"
)

type Collabor struct {
	jobs    []*Job
	timeout time.Duration
}

func NewCo() *Collabor {
	return &Collabor{}
}

func (c *Collabor) WithTimeout(timeout time.Duration) *Collabor {
	c.timeout = timeout
	return c
}

func (c *Collabor) AddJob(name string, jobFn Func, depends ...*Job) *Job {
	job := &Job{name: name, fn: jobFn, deps: depends}
	for _, dep := range depends {
		dep.ntfs = append(dep.ntfs, job)
	}
	c.jobs = append(c.jobs, job)
	return job
}

func (c *Collabor) Do(ctx context.Context, i interface{}) error {
	for _, job := range c.jobs {
		job.sem = semaphore.NewWeighted(int64(len(job.ntfs)))
		job.sem.Acquire(ctx, int64(len(job.ntfs)))
	}
	eg, ctx := NewErrGroup().WithContext(ctx)
	for _, job := range c.jobs {
		job := job
		eg.Go(func() error {
			// waiting for all dependencies
			for _, dep := range job.deps {
				dep.sem.Acquire(ctx, 1)
			}
			// check error
			select {
			case <-ctx.Done():
				klog.Warnf("job %s canceled because of dependent job error", job.name)
				return fmt.Errorf("job %s canceled", job.name)
			default:
			}
			// start job
			err := func() (err error) {
				defer func() {
					if e := recover(); e != nil {
						err = fmt.Errorf("job %s panic: %v", job.name, string(debug.Stack()))
					}
				}()
				err = job.fn(ctx, i)
				return err
			}()
			if err != nil {
				klog.Errorf("job %s error: %v", job.name, err)
			} else {
				klog.Infof("job %s done", job.name)
			}
			// notify
			job.sem.Release(int64(len(job.ntfs)))
			return err
		})
	}
	return eg.Wait(c.timeout)
}
