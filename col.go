package collabor

import (
	"container/heap"
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
	order   int
}

type jobResult struct {
	job        *Job
	err        error
	finishedAt time.Time
}

type activeJob struct {
	name     string
	deadline time.Time
	cancel   context.CancelFunc
	entry    *deadlineEntry
}

type deadlineEntry struct {
	job      *Job
	deadline time.Time
	order    int
	index    int
}

type deadlineHeap []*deadlineEntry

func (h deadlineHeap) Len() int { return len(h) }
func (h deadlineHeap) Less(i, j int) bool {
	if h[i].deadline.Equal(h[j].deadline) {
		return h[i].order < h[j].order
	}
	return h[i].deadline.Before(h[j].deadline)
}
func (h deadlineHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *deadlineHeap) Push(value interface{}) {
	entry := value.(*deadlineEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}
func (h *deadlineHeap) Pop() interface{} {
	old := *h
	last := len(old) - 1
	entry := old[last]
	old[last] = nil
	entry.index = -1
	*h = old[:last]
	return entry
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
	deadlines := make(deadlineHeap, 0, len(configs))
	heap.Init(&deadlines)
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
		var entry *deadlineEntry
		if cfg.timeout > 0 {
			jobCtx, jobCancel = context.WithTimeout(runCtx, cfg.timeout)
			deadline, _ = jobCtx.Deadline()
			entry = &deadlineEntry{job: job, deadline: deadline, order: cfg.order}
			heap.Push(&deadlines, entry)
		}
		active[job] = activeJob{name: cfg.name, deadline: deadline, cancel: jobCancel, entry: entry}
		go func() {
			err, finishedAt := runJob(jobCtx, cfg, input)
			results <- jobResult{job: job, err: err, finishedAt: finishedAt}
		}()
	}

	completed := 0
	handleResult := func(result jobResult) error {
		execution, ok := active[result.job]
		if !ok {
			return fmt.Errorf("%w: result from inactive job", ErrInvalidGraph)
		}
		execution.cancel()
		if execution.entry != nil && execution.entry.index >= 0 {
			heap.Remove(&deadlines, execution.entry.index)
		}
		delete(active, result.job)
		completed++

		// Caller cancellation wins over the graph timeout, which wins over a
		// per-job timeout. A job result is accepted only if the function
		// returned before its own deadline.
		if err := executionError(ctx, runCtx, timeout); err != nil {
			return err
		}
		if !execution.deadline.IsZero() && !result.finishedAt.Before(execution.deadline) {
			return fmt.Errorf("job %s: %w", execution.name, context.DeadlineExceeded)
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
		return nil
	}

	deadlineTimer := time.NewTimer(time.Hour)
	if !deadlineTimer.Stop() {
		<-deadlineTimer.C
	}
	defer deadlineTimer.Stop()

	for completed < len(configs) {
		for len(ready) > 0 {
			job := ready[0]
			ready = ready[1:]
			start(job)
		}
		if len(active) == 0 {
			return fmt.Errorf("%w: unfinished jobs but no runnable jobs", ErrInvalidGraph)
		}

		timerC := resetDeadlineTimer(deadlineTimer, deadlines)
		select {
		case result := <-results:
			stopTimer(deadlineTimer)
			if err := handleResult(result); err != nil {
				return err
			}
		case <-runCtx.Done():
			stopTimer(deadlineTimer)
			return executionError(ctx, runCtx, timeout)
		case <-timerC:
			// Prefer results that were already published when the timer fired.
			// Their completion timestamp decides whether they beat the deadline.
			draining := true
			for draining {
				select {
				case result := <-results:
					if err := handleResult(result); err != nil {
						return err
					}
				default:
					draining = false
				}
			}
			if completed == len(configs) {
				return nil
			}
			if err := executionError(ctx, runCtx, timeout); err != nil {
				return err
			}
			if len(deadlines) > 0 && !deadlines[0].deadline.After(time.Now()) {
				execution := active[deadlines[0].job]
				execution.cancel()
				return fmt.Errorf("job %s: %w", execution.name, context.DeadlineExceeded)
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

func resetDeadlineTimer(timer *time.Timer, deadlines deadlineHeap) <-chan time.Time {
	stopTimer(timer)
	if len(deadlines) == 0 {
		return nil
	}
	delay := time.Until(deadlines[0].deadline)
	if delay < 0 {
		delay = 0
	}
	timer.Reset(delay)
	return timer.C
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (c *Collabor) snapshot() ([]jobConfig, time.Duration, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	configs := make([]jobConfig, 0, len(c.jobs))
	for order, job := range c.jobs {
		if job == nil {
			return nil, 0, fmt.Errorf("%w: nil job", ErrInvalidGraph)
		}
		job.mu.RLock()
		configs = append(configs, jobConfig{
			job: job, name: job.name, fn: job.fn,
			deps: append([]*Job(nil), job.deps...), timeout: job.timeout,
			order: order,
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

func runJob(ctx context.Context, cfg jobConfig, input interface{}) (err error, finishedAt time.Time) {
	completed := false
	defer func() {
		if !completed {
			finishedAt = time.Now()
			recovered := recover()
			err = fmt.Errorf("job %s panic: %v\n%s", cfg.name, recovered, debug.Stack())
		} else if err != nil {
			err = fmt.Errorf("job %s: %w", cfg.name, err)
		}
	}()
	err = cfg.fn(ctx, input)
	// Record completion before framework-side error wrapping and result delivery.
	finishedAt = time.Now()
	completed = true
	return err, finishedAt
}
