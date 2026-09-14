package collabor

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollaborOrderAndConcurrency(t *testing.T) {
	co := NewCo()
	var mu sync.Mutex
	finished := make([]string, 0, 4)
	record := func(name string, delay time.Duration) Func {
		return func(ctx context.Context, _ interface{}) error {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			mu.Lock()
			finished = append(finished, name)
			mu.Unlock()
			return nil
		}
	}

	a := co.AddJob("A", record("A", 10*time.Millisecond))
	b := co.AddJob("B", record("B", 30*time.Millisecond), a)
	c := co.AddJob("C", record("C", 10*time.Millisecond), a)
	co.AddJob("D", record("D", 0), b, c)

	start := time.Now()
	if err := co.Do(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed >= 80*time.Millisecond {
		t.Fatalf("jobs did not run concurrently: %v", elapsed)
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
	var dependentRan atomic.Bool
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
