package collabor

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCollaborOrderAndConcurrency(t *testing.T) {
	co := NewCo()
	var mu sync.Mutex
	finished := make([]string, 0, 4)
	record := func(name string) {
		mu.Lock()
		finished = append(finished, name)
		mu.Unlock()
	}

	bothStarted := make(chan struct{})
	releaseB := make(chan struct{})
	releaseC := make(chan struct{})
	var branchStarts atomicInt32
	a := co.AddJob("A", func(context.Context, interface{}) error {
		record("A")
		return nil
	})
	b := co.AddJob("B", func(context.Context, interface{}) error {
		if branchStarts.Add(1) == 2 {
			close(bothStarted)
		}
		<-releaseB
		record("B")
		return nil
	}, a)
	c := co.AddJob("C", func(context.Context, interface{}) error {
		if branchStarts.Add(1) == 2 {
			close(bothStarted)
		}
		<-releaseC
		record("C")
		return nil
	}, a)
	co.AddJob("D", func(context.Context, interface{}) error {
		record("D")
		return nil
	}, b, c)

	done := make(chan error, 1)
	go func() { done <- co.Do(context.Background(), nil) }()
	select {
	case <-bothStarted:
	case <-time.After(time.Second):
		t.Fatal("independent branches did not start concurrently")
	}
	close(releaseC)
	for {
		mu.Lock()
		cFinished := len(finished) == 2
		mu.Unlock()
		if cFinished {
			break
		}
		runtime.Gosched()
	}
	close(releaseB)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := fmt.Sprint(finished)
	mu.Unlock()
	if got != "[A C B D]" {
		t.Fatalf("unexpected completion order: %s", got)
	}
}

func TestCollaborErrorStopsDependentJobs(t *testing.T) {
	want := errors.New("boom")
	co := NewCo()
	a := co.AddJob("A", func(context.Context, interface{}) error { return want })
	var dependentRan atomicBool
	co.AddJob("B", func(context.Context, interface{}) error {
		dependentRan.Store(true)
		return nil
	}, a)

	err := co.Do(context.Background(), nil)
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want wrapped %v", err, want)
	}
	if dependentRan.Load() {
		t.Fatal("dependent job ran after dependency failure")
	}
}

func TestCollaborPanic(t *testing.T) {
	co := NewCo()
	co.AddJob("explode", func(context.Context, interface{}) error { panic("boom") })
	err := co.Do(context.Background(), nil)
	if err == nil || !contains(err.Error(), "job explode panic: boom") {
		t.Fatalf("unexpected panic error: %v", err)
	}
}

func TestCollaborTimeoutCancelsContext(t *testing.T) {
	co := NewCo().WithTimeout(30 * time.Millisecond)
	cancelled := make(chan struct{})
	co.AddJob("wait", func(ctx context.Context, _ interface{}) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})

	err := co.Do(context.Background(), nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("job did not observe timeout cancellation")
	}
}

func TestJobTimeout(t *testing.T) {
	co := NewCo()
	co.AddJob("slow", func(ctx context.Context, _ interface{}) error {
		<-ctx.Done()
		return ctx.Err()
	}).WithTimeout(20 * time.Millisecond)

	err := co.Do(context.Background(), nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeout) {
		t.Fatalf("unexpected job timeout error: %v", err)
	}
}

func TestJobResultBeforeDeadlineWins(t *testing.T) {
	co := NewCo()
	co.AddJob("quick", func(context.Context, interface{}) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	}).WithTimeout(100 * time.Millisecond)
	if err := co.Do(context.Background(), nil); err != nil {
		t.Fatalf("result before deadline was rejected: %v", err)
	}
}

func TestJobResultAfterDeadlineIsTimeout(t *testing.T) {
	businessErr := errors.New("late business error")
	co := NewCo()
	co.AddJob("late", func(context.Context, interface{}) error {
		time.Sleep(30 * time.Millisecond) // intentionally ignores cancellation
		return businessErr
	}).WithTimeout(10 * time.Millisecond)
	if err := co.Do(context.Background(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
}

func TestDeadlineHeapUsesStableJobOrder(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	h := deadlineHeap{
		&deadlineEntry{deadline: deadline, order: 2},
		&deadlineEntry{deadline: deadline, order: 0},
		&deadlineEntry{deadline: deadline, order: 1},
	}
	heap.Init(&h)
	for want := 0; want < 3; want++ {
		if got := heap.Pop(&h).(*deadlineEntry).order; got != want {
			t.Fatalf("got order %d, want %d", got, want)
		}
	}
}

func TestExecuteJobRecordsCompletion(t *testing.T) {
	cfg := jobConfig{name: "quick", fn: func(context.Context, interface{}) error { return nil }}
	execution := &activeJob{done: make(chan struct{})}
	results := make(chan *activeJob, 1)
	started := time.Now()
	executeJob(context.Background(), cfg, nil, execution, results)
	observedAt := time.Now()
	if got := <-results; got != execution {
		t.Fatal("unexpected execution result")
	}
	if execution.result.err != nil {
		t.Fatal(execution.result.err)
	}
	finishedAt := execution.result.finishedAt
	if finishedAt.Before(started) || finishedAt.After(observedAt) {
		t.Fatalf("completion timestamp %v is outside [%v, %v]", finishedAt, started, observedAt)
	}
}

func TestCancelledGraphDoesNotEnterReadyJobs(t *testing.T) {
	var entered atomicInt32
	co := NewCo().WithTimeout(time.Nanosecond)
	for i := 0; i < 100; i++ {
		co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error {
			entered.Add(1)
			return nil
		})
	}
	if err := co.Do(context.Background(), nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
	if got := entered.Load(); got != 0 {
		t.Fatalf("entered %d jobs after graph timeout", got)
	}
}

func TestRuntimeGoexitReportsCompletion(t *testing.T) {
	co := NewCo()
	co.AddJob("goexit", func(context.Context, interface{}) error {
		runtime.Goexit()
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- co.Do(context.Background(), nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrJobTerminated) {
			t.Fatalf("got %v, want ErrJobTerminated", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Do blocked after runtime.Goexit")
	}
}

type blockingError struct {
	release <-chan struct{}
}

func (e blockingError) Error() string {
	<-e.release
	return "business error"
}

func TestErrorFormattingDoesNotLoseCompletedResult(t *testing.T) {
	release := make(chan struct{})
	businessErr := blockingError{release: release}
	co := NewCo()
	co.AddJob("quick", func(context.Context, interface{}) error {
		return businessErr
	}).WithTimeout(100 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- co.Do(context.Background(), nil) }()
	select {
	case err := <-done:
		// Do must return the wrapped error without invoking Error().
		if !errors.Is(err, businessErr) {
			close(release)
			t.Fatal("business error was not preserved")
		}
	case <-time.After(50 * time.Millisecond):
		close(release)
		t.Fatal("error formatting blocked result publication")
	}
	close(release)
}

func TestExternalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	co := NewCo()
	co.AddJob("wait", func(ctx context.Context, _ interface{}) error {
		<-ctx.Done()
		return ctx.Err()
	})
	cancel()
	if err := co.Do(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestGraphValidation(t *testing.T) {
	t.Run("nil function", func(t *testing.T) {
		co := NewCo()
		co.AddJob("bad", nil)
		if err := co.Do(context.Background(), nil); !errors.Is(err, ErrInvalidGraph) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("foreign dependency", func(t *testing.T) {
		foreign := NewCo().AddJob("foreign", func(context.Context, interface{}) error { return nil })
		co := NewCo()
		co.AddJob("bad", func(context.Context, interface{}) error { return nil }, foreign)
		if err := co.Do(context.Background(), nil); !errors.Is(err, ErrInvalidGraph) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("duplicate dependency", func(t *testing.T) {
		co := NewCo()
		a := co.AddJob("A", func(context.Context, interface{}) error { return nil })
		co.AddJob("B", func(context.Context, interface{}) error { return nil }, a, a)
		if err := co.Do(context.Background(), nil); !errors.Is(err, ErrInvalidGraph) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		co := NewCo()
		a := co.AddJob("A", func(context.Context, interface{}) error { return nil })
		b := co.AddJob("B", func(context.Context, interface{}) error { return nil }, a)
		a.deps = []*Job{b} // construct malformed graph to verify validation
		if err := co.Do(context.Background(), nil); !errors.Is(err, ErrCycle) {
			t.Fatalf("got %v, want ErrCycle", err)
		}
	})
}

func TestOnlyReadyJobsStartGoroutines(t *testing.T) {
	co := NewCo()
	started := make(chan struct{})
	release := make(chan struct{})
	previous := co.AddJob("0", func(context.Context, interface{}) error {
		close(started)
		<-release
		return nil
	})
	for i := 1; i < 1000; i++ {
		previous = co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error { return nil }, previous)
	}

	baseline := runtime.NumGoroutine()
	done := make(chan error, 1)
	go func() { done <- co.Do(context.Background(), nil) }()
	<-started
	// Allow the scheduler to settle. A scheduler that starts one waiting
	// goroutine per node would add roughly 1000 goroutines here.
	time.Sleep(20 * time.Millisecond)
	if added := runtime.NumGoroutine() - baseline; added > 20 {
		close(release)
		<-done
		t.Fatalf("blocked graph created too many goroutines: %d", added)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCollaborCanBeReusedConcurrently(t *testing.T) {
	co := NewCo()
	a := co.AddJob("A", func(context.Context, interface{}) error { return nil })
	co.AddJob("B", func(context.Context, interface{}) error { return nil }, a)

	const executions = 20
	var wg sync.WaitGroup
	errs := make(chan error, executions)
	for i := 0; i < executions; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- co.Do(context.Background(), nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmptyCollabor(t *testing.T) {
	if err := NewCo().Do(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
