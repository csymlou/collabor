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
	// ErrJobTerminated reports a job that exited through runtime.Goexit or an
	// equivalent abnormal path without returning or panicking with a value.
	ErrJobTerminated = errors.New("collabor job terminated without returning")
)

type jobError struct {
	name string
	err  error
}

func (e *jobError) Error() string { return "job " + e.name + ": " + e.err.Error() }
func (e *jobError) Unwrap() error { return e.err }

type jobPanicError struct {
	name  string
	value interface{}
	stack []byte
}

func (e *jobPanicError) Error() string {
	return fmt.Sprintf("job %s panic: %v\n%s", e.name, e.value, e.stack)
}

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
	err        error
	finishedAt time.Time
}

type activeJob struct {
	job      *Job
	name     string
	deadline time.Time
	cancel   context.CancelFunc
	entry    *deadlineEntry
	done     chan struct{}
	result   jobResult
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

	results := make(chan *activeJob, len(configs))
	active := make(map[*Job]*activeJob)
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
			effectiveDeadline, _ := jobCtx.Deadline()
			parentDeadline, hasParentDeadline := runCtx.Deadline()
			// Track only a deadline introduced by this job. If the parent
			// deadline is earlier (or equal), cancellation must be reported as
			// caller/global cancellation rather than a per-job timeout.
			if !hasParentDeadline || effectiveDeadline.Before(parentDeadline) {
				deadline = effectiveDeadline
				entry = &deadlineEntry{job: job, deadline: deadline, order: cfg.order}
				heap.Push(&deadlines, entry)
			}
		}
		execution := &activeJob{
			job: job, name: cfg.name, deadline: deadline, cancel: jobCancel,
			entry: entry, done: make(chan struct{}),
		}
		active[job] = execution
		go executeJob(jobCtx, cfg, input, execution, results)
	}

	completed := 0
	handleResult := func(execution *activeJob) error {
		current, ok := active[execution.job]
		if !ok {
			// The deadline path may process a published result before the worker's
			// notification is selected. Ignore that later duplicate notification.
			return nil
		}
		if current != execution {
			return fmt.Errorf("%w: result from unexpected job execution", ErrInvalidGraph)
		}
		execution.cancel()
		if execution.entry != nil && execution.entry.index >= 0 {
			heap.Remove(&deadlines, execution.entry.index)
		}
		delete(active, execution.job)
		completed++

		// Caller cancellation wins over the graph timeout, which wins over a
		// per-job timeout. A job result is accepted only if it was published
		// before its own deadline.
		if err := executionError(ctx, runCtx, timeout); err != nil {
			return err
		}
		if !execution.deadline.IsZero() && !execution.result.finishedAt.Before(execution.deadline) {
			return &jobError{name: execution.name, err: context.DeadlineExceeded}
		}
		if execution.result.err != nil {
			return wrapJobError(execution.name, execution.result.err)
		}
		for _, child := range children[execution.job] {
			remaining[child]--
			if remaining[child] == 0 {
				ready = append(ready, child)
			}
		}
		return nil
	}

	// handleExpiredDeadline is shared by ready dispatch and the timer path so
	// an expired per-job deadline is observed even while a large ready queue is
	// being drained. handled is true when an expired deadline was processed.
	handleExpiredDeadline := func(now time.Time) (handled bool, err error) {
		if len(deadlines) == 0 || deadlines[0].deadline.After(now) {
			return false, nil
		}
		execution := active[deadlines[0].job]
		if execution == nil {
			return true, fmt.Errorf("%w: deadline for inactive job", ErrInvalidGraph)
		}

		// The function may have completed before the deadline while its
		// notification is waiting to be selected. done publishes the raw result
		// and establishes the happens-before edge needed to read it safely.
		select {
		case <-execution.done:
			return true, handleResult(execution)
		default:
		}

		execution.cancel()
		return true, &jobError{name: execution.name, err: context.DeadlineExceeded}
	}

	deadlineTimer := time.NewTimer(time.Hour)
	if !deadlineTimer.Stop() {
		<-deadlineTimer.C
	}
	defer deadlineTimer.Stop()

	for completed < len(configs) {
		for len(ready) > 0 {
			// Do not blindly drain a large ready queue: cancellation or a job
			// error may already be observable and must win before more work starts.
			if err := executionError(ctx, runCtx, timeout); err != nil {
				return err
			}
			select {
			case execution := <-results:
				if err := handleResult(execution); err != nil {
					return err
				}
				continue
			default:
			}
			if handled, err := handleExpiredDeadline(time.Now()); err != nil {
				return err
			} else if handled {
				continue
			}
			if err := runCtx.Err(); err != nil {
				return executionError(ctx, runCtx, timeout)
			}
			job := ready[0]
			ready = ready[1:]
			start(job)
		}
		if len(active) == 0 {
			return fmt.Errorf("%w: unfinished jobs but no runnable jobs", ErrInvalidGraph)
		}

		timerC := resetDeadlineTimer(deadlineTimer, deadlines)
		select {
		case execution := <-results:
			stopTimer(deadlineTimer)
			if err := handleResult(execution); err != nil {
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
				case execution := <-results:
					if err := handleResult(execution); err != nil {
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
			if handled, err := handleExpiredDeadline(time.Now()); err != nil {
				return err
			} else if handled {
				continue
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

func executeJob(ctx context.Context, cfg jobConfig, input interface{}, execution *activeJob, results chan<- *activeJob) {
	published := false
	defer func() {
		if !published {
			// A non-nil recovered value is a panic. A nil value means either
			// runtime.Goexit or panic(nil) on Go versions where they cannot be
			// distinguished; both are reported as abnormal termination.
			if recovered := recover(); recovered != nil {
				execution.result.err = &jobPanicError{
					name: cfg.name, value: recovered, stack: debug.Stack(),
				}
			} else {
				execution.result.err = ErrJobTerminated
			}
			execution.result.finishedAt = time.Now()
			close(execution.done)
		}

		// Keep the scheduler notification in this outermost defer so
		// runtime.Goexit also produces exactly one completion event.
		results <- execution
	}()

	// Cancellation may happen after dispatch but before this goroutine runs.
	if err := ctx.Err(); err != nil {
		execution.result = jobResult{err: err, finishedAt: time.Now()}
		close(execution.done)
		published = true
		return
	}

	err := cfg.fn(ctx, input)
	// Publish the raw error immediately, without formatting it. A user-defined
	// Error method must not delay publication or alter the deadline decision.
	execution.result = jobResult{err: err, finishedAt: time.Now()}
	close(execution.done)
	published = true
}

func wrapJobError(name string, err error) error {
	if _, ok := err.(*jobPanicError); ok {
		return err
	}
	return &jobError{name: name, err: err}
}
