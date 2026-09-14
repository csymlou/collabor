package collabor

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

var (
	ErrTimeout      = errors.New("collabor timeout")
	ErrInvalidGraph = errors.New("collabor invalid graph")
	ErrCycle        = errors.New("collabor dependency cycle")
)

// Collabor defines a reusable directed acyclic graph of jobs.
// A Collabor may be executed concurrently after its jobs have been added.
type Collabor struct {
	mu      sync.RWMutex
	jobs    []*Job
	timeout time.Duration
}

// NewCo creates an empty job graph.
func NewCo() *Collabor { return &Collabor{} }

// WithTimeout sets the maximum duration of an execution. A non-positive value
// disables the Collabor-level timeout.
func (c *Collabor) WithTimeout(timeout time.Duration) *Collabor {
	c.mu.Lock()
	c.timeout = timeout
	c.mu.Unlock()
	return c
}

// AddJob adds a job and its dependencies to the graph.
// Invalid functions and dependencies are reported by Do.
func (c *Collabor) AddJob(name string, jobFn Func, depends ...*Job) *Job {
	c.mu.Lock()
	defer c.mu.Unlock()

	job := &Job{name: name, fn: jobFn, deps: append([]*Job(nil), depends...), owner: c}
	c.jobs = append(c.jobs, job)
	return job
}

type jobConfig struct {
	job     *Job
	name    string
	fn      Func
	deps    []*Job
	timeout time.Duration
}

type jobResult struct {
	job *Job
	err error
}

type activeJob struct {
	name     string
	deadline time.Time
	cancel   context.CancelFunc
}

// Do executes all jobs whose dependencies complete successfully. It returns
// the first job error, a context error, or ErrTimeout for a Collabor timeout.
// Cancellation is cooperative: job functions should observe ctx and return.
// Only ready jobs get a goroutine, and each started job uses one goroutine.
func (c *Collabor) Do(ctx context.Context, input interface{}) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidGraph)
	}

	configs, timeout, err := c.snapshot()
	if err != nil {
		return err
	}
	if err := validateGraph(configs); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(configs) == 0 {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeout > 0 {
		var timeoutCancel context.CancelFunc
		runCtx, timeoutCancel = context.WithTimeout(runCtx, timeout)
		defer timeoutCancel()
	}

	byJob := make(map[*Job]jobConfig, len(configs))
	remaining := make(map[*Job]int, len(configs))
	children := make(map[*Job][]*Job, len(configs))
	ready := make([]*Job, 0, len(configs))
	for _, cfg := range configs {
		byJob[cfg.job] = cfg
		remaining[cfg.job] = len(cfg.deps)
		if len(cfg.deps) == 0 {
			ready = append(ready, cfg.job)
		}
		for _, dep := range cfg.deps {
			children[dep] = append(children[dep], cfg.job)
		}
	}

	results := make(chan jobResult, len(configs))
	active := make(map[*Job]activeJob)
	defer func() {
		for _, execution := range active {
			execution.cancel()
		}
	}()

	start := func(job *Job) {
		cfg := byJob[job]
		jobCtx := runCtx
		jobCancel := func() {}
		var deadline time.Time
		if cfg.timeout > 0 {
			jobCtx, jobCancel = context.WithTimeout(runCtx, cfg.timeout)
			deadline, _ = jobCtx.Deadline()
		}
		active[job] = activeJob{name: cfg.name, deadline: deadline, cancel: jobCancel}
		go func() {
			results <- jobResult{job: job, err: runJob(jobCtx, cfg, input)}
		}()
	}

	completed := 0
	for completed < len(configs) {
		for len(ready) > 0 {
			job := ready[0]
			ready = ready[1:]
			start(job)
		}

		timer, timerC := nextJobTimer(active)
		select {
		case result := <-results:
			if timer != nil {
				timer.Stop()
			}
			execution := active[result.job]
			execution.cancel()
			delete(active, result.job)
			completed++

			// Prefer a parent/global cancellation if it raced with completion.
			if err := executionError(ctx, runCtx, timeout); err != nil {
				return err
			}
			if result.err != nil {
				return result.err
			}
			for _, child := range children[result.job] {
				remaining[child]--
				if remaining[child] == 0 {
					ready = append(ready, child)
				}
			}
		case <-runCtx.Done():
			if timer != nil {
				timer.Stop()
			}
			return executionError(ctx, runCtx, timeout)
		case now := <-timerC:
			for _, execution := range active {
				if !execution.deadline.IsZero() && !execution.deadline.After(now) {
					execution.cancel()
					return fmt.Errorf("job %s: %w", execution.name, context.DeadlineExceeded)
				}
			}
		}
	}
	return nil
}

func executionError(parent, runCtx context.Context, timeout time.Duration) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrTimeout, runCtx.Err())
	}
	return runCtx.Err()
}

func nextJobTimer(active map[*Job]activeJob) (*time.Timer, <-chan time.Time) {
	var nearest time.Time
	for _, execution := range active {
		if execution.deadline.IsZero() {
			continue
		}
		if nearest.IsZero() || execution.deadline.Before(nearest) {
			nearest = execution.deadline
		}
	}
	if nearest.IsZero() {
		return nil, nil
	}
	delay := time.Until(nearest)
	if delay < 0 {
		delay = 0
	}
	timer := time.NewTimer(delay)
	return timer, timer.C
}

func (c *Collabor) snapshot() ([]jobConfig, time.Duration, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	configs := make([]jobConfig, 0, len(c.jobs))
	for _, job := range c.jobs {
		if job == nil {
			return nil, 0, fmt.Errorf("%w: nil job", ErrInvalidGraph)
		}
		job.mu.RLock()
		configs = append(configs, jobConfig{
			job: job, name: job.name, fn: job.fn,
			deps: append([]*Job(nil), job.deps...), timeout: job.timeout,
		})
		job.mu.RUnlock()
	}
	return configs, c.timeout, nil
}

func validateGraph(configs []jobConfig) error {
	known := make(map[*Job]jobConfig, len(configs))
	for _, cfg := range configs {
		if cfg.fn == nil {
			return fmt.Errorf("%w: job %q has a nil function", ErrInvalidGraph, cfg.name)
		}
		known[cfg.job] = cfg
	}

	indegree := make(map[*Job]int, len(configs))
	children := make(map[*Job][]*Job, len(configs))
	for _, cfg := range configs {
		seen := make(map[*Job]struct{}, len(cfg.deps))
		for _, dep := range cfg.deps {
			if dep == nil {
				return fmt.Errorf("%w: job %q has a nil dependency", ErrInvalidGraph, cfg.name)
			}
			if _, ok := known[dep]; !ok || dep.owner != cfg.job.owner {
				return fmt.Errorf("%w: dependency %q of job %q belongs to another graph", ErrInvalidGraph, dep.name, cfg.name)
			}
			if _, duplicate := seen[dep]; duplicate {
				return fmt.Errorf("%w: job %q contains duplicate dependency %q", ErrInvalidGraph, cfg.name, dep.name)
			}
			seen[dep] = struct{}{}
			indegree[cfg.job]++
			children[dep] = append(children[dep], cfg.job)
		}
	}

	queue := make([]*Job, 0, len(configs))
	for _, cfg := range configs {
		if indegree[cfg.job] == 0 {
			queue = append(queue, cfg.job)
		}
	}
	visited := 0
	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		visited++
		for _, child := range children[job] {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if visited != len(configs) {
		return ErrCycle
	}
	return nil
}

func runJob(ctx context.Context, cfg jobConfig, input interface{}) (err error) {
	completed := false
	defer func() {
		if !completed {
			recovered := recover()
			err = fmt.Errorf("job %s panic: %v\n%s", cfg.name, recovered, debug.Stack())
		} else if err != nil {
			err = fmt.Errorf("job %s: %w", cfg.name, err)
		}
	}()
	err = cfg.fn(ctx, input)
	completed = true
	return err
}
