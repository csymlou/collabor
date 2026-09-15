package collabor

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// benchmarkTask is intentionally small: the benchmark compares scheduling
// strategies, so all three runners share the same generated durations and DAG.
type benchmarkTask struct {
	duration time.Duration
	deps     []int
}

type benchmarkDAG struct {
	tasks        []benchmarkTask
	layers       [][]int
	edges        int
	serialTime   time.Duration
	criticalPath time.Duration
}

// generateBenchmarkDAG creates a reproducible random DAG. Node IDs are already
// in topological order: every dependency of node i is selected from [0, i).
// Apart from the root, every node has between one and three distinct parents.
func generateBenchmarkDAG(nodes int, seed int64) benchmarkDAG {
	if nodes <= 0 {
		return benchmarkDAG{}
	}

	rng := rand.New(rand.NewSource(seed))
	tasks := make([]benchmarkTask, nodes)
	levels := make([]int, nodes)
	criticalTo := make([]time.Duration, nodes)
	maxLevel := 0
	edges := 0
	serialTime := time.Duration(0)
	criticalPath := time.Duration(0)

	for i := range tasks {
		duration := time.Duration(10+rng.Intn(91)) * time.Millisecond
		tasks[i].duration = duration
		serialTime += duration

		if i > 0 {
			maxParents := 3
			if i < maxParents {
				maxParents = i
			}
			parentCount := 1 + rng.Intn(maxParents)
			seen := make(map[int]struct{}, parentCount)
			for len(tasks[i].deps) < parentCount {
				parent := rng.Intn(i)
				if _, exists := seen[parent]; exists {
					continue
				}
				seen[parent] = struct{}{}
				tasks[i].deps = append(tasks[i].deps, parent)
			}
			edges += len(tasks[i].deps)
		}

		maxParentLevel := -1
		maxParentPath := time.Duration(0)
		for _, parent := range tasks[i].deps {
			if levels[parent] > maxParentLevel {
				maxParentLevel = levels[parent]
			}
			if criticalTo[parent] > maxParentPath {
				maxParentPath = criticalTo[parent]
			}
		}
		levels[i] = maxParentLevel + 1
		criticalTo[i] = maxParentPath + duration
		if levels[i] > maxLevel {
			maxLevel = levels[i]
		}
		if criticalTo[i] > criticalPath {
			criticalPath = criticalTo[i]
		}
	}

	layers := make([][]int, maxLevel+1)
	for node, level := range levels {
		layers[level] = append(layers[level], node)
	}
	return benchmarkDAG{
		tasks: tasks, layers: layers, edges: edges,
		serialTime: serialTime, criticalPath: criticalPath,
	}
}

func (d benchmarkDAG) runSerial(counts []int32) {
	// IDs are topologically sorted, so dependencies have already completed.
	for i, task := range d.tasks {
		time.Sleep(task.duration)
		atomic.AddInt32(&counts[i], 1)
	}
}

func (d benchmarkDAG) runLayered(counts []int32) {
	for _, layer := range d.layers {
		var wg sync.WaitGroup
		wg.Add(len(layer))
		for _, node := range layer {
			node := node
			go func() {
				defer wg.Done()
				time.Sleep(d.tasks[node].duration)
				atomic.AddInt32(&counts[node], 1)
			}()
		}
		wg.Wait()
	}
}

func (d benchmarkDAG) collabor() *Collabor {
	co := NewCo()
	jobs := make([]*Job, len(d.tasks))
	for i, task := range d.tasks {
		i, task := i, task
		deps := make([]*Job, len(task.deps))
		for j, parent := range task.deps {
			deps[j] = jobs[parent]
		}
		jobs[i] = co.AddJob(fmt.Sprint(i), func(_ context.Context, input interface{}) error {
			time.Sleep(task.duration)
			atomic.AddInt32(&input.([]int32)[i], 1)
			return nil
		}, deps...)
	}
	return co
}

func resetBenchmarkCounts(counts []int32) {
	for i := range counts {
		atomic.StoreInt32(&counts[i], 0)
	}
}

func verifyBenchmarkCounts(b *testing.B, counts []int32) {
	b.Helper()
	for node := range counts {
		if got := atomic.LoadInt32(&counts[node]); got != 1 {
			b.Fatalf("node %d executed %d times, want exactly once", node, got)
		}
	}
}

func reportBenchmarkDAG(b *testing.B, dag benchmarkDAG) {
	b.Helper()
	b.ReportMetric(float64(len(dag.tasks)), "nodes")
	b.ReportMetric(float64(dag.edges), "edges")
	b.ReportMetric(float64(len(dag.layers)), "layers")
	b.ReportMetric(float64(dag.serialTime.Milliseconds()), "serial-ms")
	b.ReportMetric(float64(dag.criticalPath.Milliseconds()), "critical-ms")
	b.ReportAllocs()
}

// BenchmarkSchedulingStrategies compares end-to-end wall time on exactly the
// same deterministic workload. Run one or more fixed iterations explicitly:
//
//	go test -run '^$' -bench '^BenchmarkSchedulingStrategies$' \
//	  -benchtime=1x -count=3 -benchmem
//
// The 1000-node serial case averages roughly 55 seconds per iteration because
// task durations are intentionally real 10-100ms sleeps. Keep -benchtime fixed
// rather than allowing the benchmark harness to auto-calibrate long workloads.
func BenchmarkSchedulingStrategies(b *testing.B) {
	for _, nodes := range []int{5, 10, 100, 1000} {
		nodes := nodes
		// Derive a stable but different graph for each size.
		dag := generateBenchmarkDAG(nodes, 20260915+int64(nodes))
		co := dag.collabor()

		b.Run(fmt.Sprintf("nodes=%04d/serial", nodes), func(b *testing.B) {
			counts := make([]int32, nodes)
			reportBenchmarkDAG(b, dag)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetBenchmarkCounts(counts)
				b.StartTimer()
				dag.runSerial(counts)
				b.StopTimer()
				verifyBenchmarkCounts(b, counts)
			}
		})

		b.Run(fmt.Sprintf("nodes=%04d/layered", nodes), func(b *testing.B) {
			counts := make([]int32, nodes)
			reportBenchmarkDAG(b, dag)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetBenchmarkCounts(counts)
				b.StartTimer()
				dag.runLayered(counts)
				b.StopTimer()
				verifyBenchmarkCounts(b, counts)
			}
		})

		b.Run(fmt.Sprintf("nodes=%04d/collabor", nodes), func(b *testing.B) {
			counts := make([]int32, nodes)
			reportBenchmarkDAG(b, dag)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetBenchmarkCounts(counts)
				b.StartTimer()
				if err := co.Do(context.Background(), counts); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				verifyBenchmarkCounts(b, counts)
			}
		})
	}
}

func TestGeneratedBenchmarkDAG(t *testing.T) {
	for _, nodes := range []int{5, 10, 100, 1000} {
		dag := generateBenchmarkDAG(nodes, 20260915+int64(nodes))
		if len(dag.tasks) != nodes || len(dag.layers) == 0 {
			t.Fatalf("nodes=%d generated invalid graph", nodes)
		}
		for node, task := range dag.tasks {
			if task.duration < 10*time.Millisecond || task.duration > 100*time.Millisecond {
				t.Fatalf("node %d duration %v outside [10ms,100ms]", node, task.duration)
			}
			for _, parent := range task.deps {
				if parent < 0 || parent >= node {
					t.Fatalf("node %d has non-topological dependency %d", node, parent)
				}
			}
		}
	}
}
