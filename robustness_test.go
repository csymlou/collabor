package collabor

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCancellationAfterStartPreventsDescendants(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	co := NewCo()
	root := co.AddJob("root", func(ctx context.Context, _ interface{}) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	var descendantRan atomicBool
	co.AddJob("descendant", func(context.Context, interface{}) error {
		descendantRan.Store(true)
		return nil
	}, root)

	done := make(chan error, 1)
	go func() { done <- co.Do(ctx, nil) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if descendantRan.Load() {
		t.Fatal("descendant ran after cancellation")
	}
}

func TestExecuteJobSkipsUserFunctionWhenAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var entered atomicBool
	cfg := jobConfig{name: "cancelled", fn: func(context.Context, interface{}) error {
		entered.Store(true)
		return nil
	}}
	execution := &activeJob{done: make(chan struct{})}
	results := make(chan *activeJob, 1)
	executeJob(ctx, cfg, nil, execution, results)
	<-results
	if entered.Load() {
		t.Fatal("entered user function with an already-cancelled context")
	}
	if !errors.Is(execution.result.err, context.Canceled) {
		t.Fatalf("unexpected result: %v", execution.result.err)
	}
}

func TestPanicNilDoesNotHang(t *testing.T) {
	co := NewCo()
	co.AddJob("panic-nil", func(context.Context, interface{}) error { panic(nil) })
	done := make(chan error, 1)
	go func() { done <- co.Do(context.Background(), nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("panic(nil) was treated as success")
		}
	case <-time.After(time.Second):
		t.Fatal("panic(nil) blocked Do")
	}
}

func TestConcurrentConfigurationAndSnapshotsAreRaceFree(t *testing.T) {
	co := NewCo()
	co.AddJob("base", func(context.Context, interface{}) error { return nil })

	const iterations = 100
	var wg sync.WaitGroup
	errs := make(chan error, iterations)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			co.AddJob(fmt.Sprintf("job-%d", i), func(context.Context, interface{}) error { return nil })
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			co.WithTimeout(0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			errs <- co.Do(context.Background(), nil)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentJobTimeoutUpdatesAreRaceFree(t *testing.T) {
	co := NewCo()
	job := co.AddJob("job", func(context.Context, interface{}) error { return nil })
	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			job.WithTimeout(0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := co.Do(context.Background(), nil); err != nil {
				t.Errorf("execution %d: %v", i, err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestPerJobDeadlineInterruptsReadyDispatch(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	const roots = 30000
	var entered atomicInt32
	releaseTimed := make(chan struct{})

	co := NewCo()
	co.AddJob("timed", func(ctx context.Context, _ interface{}) error {
		<-releaseTimed // intentionally ignore cancellation until the test releases it
		return ctx.Err()
	}).WithTimeout(time.Nanosecond)
	for i := 0; i < roots; i++ {
		co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error {
			entered.Add(1)
			return nil
		})
	}

	done := make(chan error, 1)
	go func() { done <- co.Do(context.Background(), nil) }()
	select {
	case err := <-done:
		close(releaseTimed)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want context deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		close(releaseTimed)
		t.Fatal("per-job deadline did not interrupt ready dispatch")
	}
	if got := entered.Load(); got != 0 {
		t.Fatalf("%d ready jobs entered after an already-expired per-job deadline", got)
	}
}

func TestManyConcurrentFailuresReturnPromptly(t *testing.T) {
	co := NewCo()
	const jobs = 500
	start := make(chan struct{})
	for i := 0; i < jobs; i++ {
		co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error {
			<-start
			return errors.New("failure")
		})
	}
	done := make(chan error, 1)
	go func() { done <- co.Do(context.Background(), nil) }()
	close(start)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent failures blocked Do")
	}
}

func FuzzCollaborDAG(f *testing.F) {
	f.Add([]byte{0, 0, 1, 2, 3})
	f.Add([]byte{3, 1, 4, 1, 5, 9, 2, 6})
	f.Add([]byte{255, 254, 253, 252})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 100 {
			data = data[:100]
		}
		co := NewCo()
		nodes := make([]*Job, 0, len(data)+1)
		done := make([]atomicBool, len(data)+1)
		nodes = append(nodes, co.AddJob("0", func(context.Context, interface{}) error {
			done[0].Store(true)
			return nil
		}))
		for i, value := range data {
			index := i + 1
			dependencyIndex := int(value) % index
			dependency := nodes[dependencyIndex]
			nodes = append(nodes, co.AddJob(fmt.Sprint(index), func(context.Context, interface{}) error {
				if !done[dependencyIndex].Load() {
					return fmt.Errorf("dependency %d incomplete", dependencyIndex)
				}
				done[index].Store(true)
				return nil
			}, dependency))
		}
		if err := co.Do(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		for i := range done {
			if !done[i].Load() {
				t.Fatalf("job %d did not run", i)
			}
		}
	})
}

func BenchmarkCollaborWide1000(b *testing.B) {
	co := NewCo()
	for i := 0; i < 1000; i++ {
		co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error { return nil })
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := co.Do(context.Background(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCollaborDeep1000(b *testing.B) {
	co := NewCo()
	var previous *Job
	for i := 0; i < 1000; i++ {
		var dependencies []*Job
		if previous != nil {
			dependencies = []*Job{previous}
		}
		previous = co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error { return nil }, dependencies...)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := co.Do(context.Background(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func TestNoUnexpectedGoroutineGrowthAfterSuccessfulRuns(t *testing.T) {
	co := NewCo()
	for i := 0; i < 100; i++ {
		co.AddJob(fmt.Sprint(i), func(context.Context, interface{}) error { return nil })
	}
	baseline := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		if err := co.Do(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	// Let workers that have already published buffered results return.
	runtime.Gosched()
	if added := runtime.NumGoroutine() - baseline; added > 5 {
		t.Fatalf("successful runs retained %d goroutines", added)
	}
}
