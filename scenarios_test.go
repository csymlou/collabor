package collabor

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicAPIAndInputPropagation(t *testing.T) {
	type contextKey struct{}
	type payload struct{ value int }

	co := NewCo().WithTimeout(0)
	job := co.AddJob("named", func(ctx context.Context, input interface{}) error {
		if got := ctx.Value(contextKey{}); got != "context-value" {
			return fmt.Errorf("context value = %v", got)
		}
		input.(*payload).value = 42
		return nil
	}).WithTimeout(0)
	if got := job.Name(); got != "named" {
		t.Fatalf("Name() = %q, want named", got)
	}

	input := &payload{}
	ctx := context.WithValue(context.Background(), contextKey{}, "context-value")
	if err := co.Do(ctx, input); err != nil {
		t.Fatal(err)
	}
	if input.value != 42 {
		t.Fatalf("input value = %d, want 42", input.value)
	}
}

func TestSequentialReuse(t *testing.T) {
	co := NewCo()
	var calls atomicInt32
	a := co.AddJob("A", func(context.Context, interface{}) error {
		calls.Add(1)
		return nil
	})
	co.AddJob("B", func(context.Context, interface{}) error {
		calls.Add(1)
		return nil
	}, a)

	for i := 0; i < 100; i++ {
		if err := co.Do(context.Background(), nil); err != nil {
			t.Fatalf("execution %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 200 {
		t.Fatalf("calls = %d, want 200", got)
	}
}

func TestRandomDAGDependenciesAlwaysCompleteFirst(t *testing.T) {
	const jobs = 200
	rng := rand.New(rand.NewSource(1))
	co := NewCo()
	nodes := make([]*Job, 0, jobs)
	done := make([]atomicBool, jobs)

	for i := 0; i < jobs; i++ {
		dependencyIndexes := make([]int, 0, 3)
		if i > 0 {
			maxDependencies := 3
			if i < maxDependencies {
				maxDependencies = i
			}
			count := rng.Intn(maxDependencies + 1)
			seen := make(map[int]struct{}, count)
			for len(dependencyIndexes) < count {
				index := rng.Intn(i)
				if _, exists := seen[index]; exists {
					continue
				}
				seen[index] = struct{}{}
				dependencyIndexes = append(dependencyIndexes, index)
			}
		}
		dependencies := make([]*Job, len(dependencyIndexes))
		for j, index := range dependencyIndexes {
			dependencies[j] = nodes[index]
		}
		index := i
		indexes := append([]int(nil), dependencyIndexes...)
		nodes = append(nodes, co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error {
			for _, dependency := range indexes {
				if !done[dependency].Load() {
					return fmt.Errorf("job %d ran before dependency %d", index, dependency)
				}
			}
			done[index].Store(true)
			return nil
		}, dependencies...))
	}

	if err := co.Do(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for i := range done {
		if !done[i].Load() {
			t.Fatalf("job %d did not run", i)
		}
	}
}

func TestBusinessErrorIncludesJobAndPreservesCause(t *testing.T) {
	cause := errors.New("database unavailable")
	co := NewCo()
	co.AddJob("load-user", func(context.Context, interface{}) error { return cause })
	err := co.Do(context.Background(), nil)
	if !errors.Is(err, cause) {
		t.Fatalf("error %v does not wrap cause", err)
	}
	if got, want := err.Error(), "job load-user: database unavailable"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestBusinessErrorCancelsRunningSibling(t *testing.T) {
	cause := errors.New("failed")
	bothStarted := make(chan struct{})
	cancelObserved := make(chan struct{})
	var starts atomicInt32
	markStarted := func() {
		if starts.Add(1) == 2 {
			close(bothStarted)
		}
	}

	co := NewCo()
	co.AddJob("failure", func(context.Context, interface{}) error {
		markStarted()
		<-bothStarted
		return cause
	})
	co.AddJob("sibling", func(ctx context.Context, _ interface{}) error {
		markStarted()
		<-bothStarted
		<-ctx.Done()
		close(cancelObserved)
		return ctx.Err()
	})

	if err := co.Do(context.Background(), nil); !errors.Is(err, cause) {
		t.Fatalf("got %v, want cause", err)
	}
	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("running sibling did not observe cancellation")
	}
}

func TestFailedJobPreventsAllDescendants(t *testing.T) {
	co := NewCo()
	root := co.AddJob("root", func(context.Context, interface{}) error { return errors.New("failed") })
	var descendants atomicInt32
	child := co.AddJob("child", func(context.Context, interface{}) error {
		descendants.Add(1)
		return nil
	}, root)
	co.AddJob("grandchild", func(context.Context, interface{}) error {
		descendants.Add(1)
		return nil
	}, child)

	if err := co.Do(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
	if got := descendants.Load(); got != 0 {
		t.Fatalf("ran %d descendants", got)
	}
}

func TestTimeoutPrecedence(t *testing.T) {
	wait := func(ctx context.Context, _ interface{}) error {
		<-ctx.Done()
		return ctx.Err()
	}

	t.Run("parent before graph", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		co := NewCo().WithTimeout(time.Second)
		co.AddJob("wait", wait)
		err := co.Do(ctx, nil)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("graph before job", func(t *testing.T) {
		co := NewCo().WithTimeout(10 * time.Millisecond)
		co.AddJob("wait", wait).WithTimeout(time.Second)
		err := co.Do(context.Background(), nil)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("job before graph", func(t *testing.T) {
		co := NewCo().WithTimeout(time.Second)
		co.AddJob("wait", wait).WithTimeout(10 * time.Millisecond)
		err := co.Do(context.Background(), nil)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestJobTimeoutPreventsDependent(t *testing.T) {
	co := NewCo()
	root := co.AddJob("root", func(ctx context.Context, _ interface{}) error {
		<-ctx.Done()
		return ctx.Err()
	}).WithTimeout(10 * time.Millisecond)
	var ran atomicBool
	co.AddJob("dependent", func(context.Context, interface{}) error {
		ran.Store(true)
		return nil
	}, root)

	if err := co.Do(context.Background(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
	if ran.Load() {
		t.Fatal("dependent ran after job timeout")
	}
}

func TestNonCooperativeJobCanFinishAfterDoReturns(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	co := NewCo().WithTimeout(10 * time.Millisecond)
	co.AddJob("blocking", func(context.Context, interface{}) error {
		<-release
		close(finished)
		return nil
	})

	if err := co.Do(context.Background(), nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("unexpected error: %v", err)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("non-cooperative job could not finish after Do returned")
	}
}

func TestPanicDetailsAndDescendantCancellation(t *testing.T) {
	co := NewCo()
	root := co.AddJob("panic-job", func(context.Context, interface{}) error { panic("secret") })
	var childRan atomicBool
	co.AddJob("child", func(context.Context, interface{}) error {
		childRan.Store(true)
		return nil
	}, root)

	err := co.Do(context.Background(), nil)
	if err == nil {
		t.Fatal("expected panic error")
	}
	message := err.Error()
	if !strings.Contains(message, "job panic-job panic: secret") || !strings.Contains(message, "goroutine") {
		t.Fatalf("panic details missing: %s", message)
	}
	if childRan.Load() {
		t.Fatal("child ran after panic")
	}
}

func TestGoexitPreventsDependent(t *testing.T) {
	co := NewCo()
	root := co.AddJob("goexit", func(context.Context, interface{}) error {
		runtime.Goexit()
		return nil
	})
	var childRan atomicBool
	co.AddJob("child", func(context.Context, interface{}) error {
		childRan.Store(true)
		return nil
	}, root)

	if err := co.Do(context.Background(), nil); !errors.Is(err, ErrJobTerminated) {
		t.Fatalf("unexpected error: %v", err)
	}
	if childRan.Load() {
		t.Fatal("child ran after Goexit")
	}
}

func TestInvalidInputsAndGraphs(t *testing.T) {
	nop := func(context.Context, interface{}) error { return nil }

	t.Run("nil context", func(t *testing.T) {
		if err := NewCo().Do(nil, nil); !errors.Is(err, ErrInvalidGraph) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty graph honors cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := NewCo().Do(ctx, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("nil dependency", func(t *testing.T) {
		co := NewCo()
		co.AddJob("bad", nop, nil)
		if err := co.Do(context.Background(), nil); !errors.Is(err, ErrInvalidGraph) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("self cycle", func(t *testing.T) {
		co := NewCo()
		job := co.AddJob("self", nop)
		job.deps = []*Job{job}
		if err := co.Do(context.Background(), nil); !errors.Is(err, ErrCycle) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate names are allowed", func(t *testing.T) {
		co := NewCo()
		co.AddJob("same", nop)
		co.AddJob("same", nop)
		if err := co.Do(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLargeDeepAndWideGraphs(t *testing.T) {
	t.Run("deep", func(t *testing.T) {
		const count = 3000
		co := NewCo()
		var previous *Job
		var ran atomicInt32
		for i := 0; i < count; i++ {
			previous = co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error {
				ran.Add(1)
				return nil
			}, nonNilJobs(previous)...)
		}
		if err := co.Do(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if got := ran.Load(); got != count {
			t.Fatalf("ran %d jobs, want %d", got, count)
		}
	})

	t.Run("wide", func(t *testing.T) {
		const count = 1000
		co := NewCo()
		var ran atomicInt32
		for i := 0; i < count; i++ {
			co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error {
				ran.Add(1)
				return nil
			})
		}
		if err := co.Do(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if got := ran.Load(); got != count {
			t.Fatalf("ran %d jobs, want %d", got, count)
		}
	})
}

func TestDeadlineHeapRemovalMaintainsOrder(t *testing.T) {
	now := time.Now()
	a := &deadlineEntry{deadline: now.Add(3 * time.Second), order: 0}
	b := &deadlineEntry{deadline: now.Add(time.Second), order: 1}
	c := &deadlineEntry{deadline: now.Add(2 * time.Second), order: 2}
	h := make(deadlineHeap, 0, 3)
	heap.Init(&h)
	heap.Push(&h, a)
	heap.Push(&h, b)
	heap.Push(&h, c)
	heap.Remove(&h, c.index)
	if got := heap.Pop(&h).(*deadlineEntry); got != b {
		t.Fatal("earliest deadline was not preserved")
	}
	if got := heap.Pop(&h).(*deadlineEntry); got != a {
		t.Fatal("remaining deadline order was not preserved")
	}
}

func TestConcurrentExecutionsHaveIndependentInputs(t *testing.T) {
	co := NewCo()
	co.AddJob("increment", func(_ context.Context, input interface{}) error {
		input.(*atomicInt32).Add(1)
		return nil
	})

	const executions = 100
	inputs := make([]atomicInt32, executions)
	var wg sync.WaitGroup
	errs := make(chan error, executions)
	for i := range inputs {
		wg.Add(1)
		go func(input *atomicInt32) {
			defer wg.Done()
			errs <- co.Do(context.Background(), input)
		}(&inputs[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range inputs {
		if got := inputs[i].Load(); got != 1 {
			t.Fatalf("input %d = %d, want 1", i, got)
		}
	}
}

func nonNilJobs(job *Job) []*Job {
	if job == nil {
		return nil
	}
	return []*Job{job}
}
