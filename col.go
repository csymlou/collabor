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

type jobRun struct {
	done chan struct{}
	err  error
}

// Do executes all jobs whose dependencies complete successfully. It returns
// the first job error, a context error, or ErrTimeout for a Collabor timeout.
// Cancellation is cooperative: job functions should observe ctx and return.
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
	if len(configs) == 0 {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timeoutCancel context.CancelFunc
	if timeout > 0 {
		runCtx, timeoutCancel = context.WithTimeout(runCtx, timeout)
		defer timeoutCancel()
	}

	runs := make(map[*Job]*jobRun, len(configs))
	for _, cfg := range configs {
		runs[cfg.job] = &jobRun{done: make(chan struct{})}
	}

	var wg sync.WaitGroup
	var firstMu sync.Mutex
	var firstErr error
	setFirstError := func(err error) {
		firstMu.Lock()
		if firstErr == nil {
			firstErr = err
			// Record the error before cancellation so a newly-ready job cannot
			// mistake dependency failure for successful completion.
			cancel()
		}
		firstMu.Unlock()
	}
	getFirstError := func() error {
		firstMu.Lock()
		defer firstMu.Unlock()
		return firstErr
	}

	for _, cfg := range configs {
		cfg := cfg
		state := runs[cfg.job]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(state.done)

			for _, dep := range cfg.deps {
				depState := runs[dep]
				select {
				case <-depState.done:
					if depState.err != nil {
						state.err = fmt.Errorf("job %s skipped: dependency %s failed", cfg.name, dep.name)
						return
					}
				case <-runCtx.Done():
					state.err = runCtx.Err()
					return
				}
			}

			// Synchronize with error publication immediately before starting.
			firstMu.Lock()
			if firstErr != nil || runCtx.Err() != nil {
				state.err = runCtx.Err()
				firstMu.Unlock()
				return
			}
			firstMu.Unlock()

			state.err = executeJob(runCtx, cfg, input)
			if state.err != nil && runCtx.Err() == nil {
				setFirstError(state.err)
			}
		}()
	}

	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
		if err := getFirstError(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %v", ErrTimeout, runCtx.Err())
		}
		return nil
	case <-runCtx.Done():
		if err := getFirstError(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %v", ErrTimeout, runCtx.Err())
		}
		return runCtx.Err()
	}
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

func executeJob(ctx context.Context, cfg jobConfig, input interface{}) error {
	jobCtx := ctx
	cancel := func() {}
	if cfg.timeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, cfg.timeout)
	}
	defer cancel()

	result := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("job %s panic: %v\n%s", cfg.name, recovered, debug.Stack())
			}
			result <- err
		}()
		err = cfg.fn(jobCtx, input)
	}()

	select {
	case err := <-result:
		if err != nil {
			return fmt.Errorf("job %s: %w", cfg.name, err)
		}
		return nil
	case <-jobCtx.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("job %s: %w", cfg.name, jobCtx.Err())
	}
}
