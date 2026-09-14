[中文](README.zh_CN.md)

# collabor

A lightweight DAG task runner for Go. A job starts as soon as all of its dependencies succeed. Errors, panics, and cancellation stop jobs that have not started.

```shell
go get github.com/csymlou/collabor
```

## Features

- Directed acyclic graph (DAG) dependencies
- Concurrent execution of independent jobs
- Fail-fast error propagation and cancellation
- Parent context, graph-level timeout, and per-job timeout support
- Panic recovery with the panic value and stack trace
- Validation for cycles, foreign jobs, nil dependencies, and duplicates
- Concurrent reuse of a completed graph definition

## Basic usage

```go
type Convey struct {
    Input  int
    FromB int
    FromC int
    Output int
}

co := collabor.NewCo()

a := co.AddJob("A", func(ctx context.Context, input interface{}) error {
    convey := input.(*Convey)
    convey.Output = convey.Input
    return nil
})

b := co.AddJob("B", func(ctx context.Context, input interface{}) error {
    input.(*Convey).FromB = 2
    return nil
}, a)

c := co.AddJob("C", func(ctx context.Context, input interface{}) error {
    input.(*Convey).FromC = 3
    return nil
}, a)

co.AddJob("D", func(ctx context.Context, input interface{}) error {
    convey := input.(*Convey)
    convey.Output += convey.FromB + convey.FromC
    return nil
}, b, c)

err := co.Do(context.Background(), &Convey{Input: 1})
```

## Errors and cancellation

When a job returns a non-nil error, the execution is cancelled, dependent jobs do not start, and `Do` returns an error wrapping the original error. Use `errors.Is` to inspect it.

Jobs should observe `ctx.Done()` and stop promptly:

```go
co.AddJob("request", func(ctx context.Context, input interface{}) error {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return err
    }
    _, err = http.DefaultClient.Do(req)
    return err
})
```

Cancellation is cooperative: Go cannot forcibly stop a function that ignores its context. Such a function may continue after `Do` returns.

## Timeouts

Set a timeout for the entire execution:

```go
co.WithTimeout(time.Second)
```

Set a timeout for one job:

```go
job := co.AddJob("slow", fn).WithTimeout(100 * time.Millisecond)
```

A graph timeout matches `collabor.ErrTimeout` with `errors.Is`. A per-job timeout matches `context.DeadlineExceeded`.

## Concurrency safety

A fully configured `Collabor` can be reused by concurrent calls to `Do`; each execution has independent runtime state.

The caller owns the value passed to `Do`. Concurrent jobs must not access the same mutable fields without synchronization. Give jobs separate output fields, or use mutexes, atomics, or channels. Graphs should normally be configured before execution starts.

## Graph validation

`Do` rejects:

- nil job functions or dependencies
- duplicate dependencies
- dependencies from another `Collabor`
- dependency cycles

Use `errors.Is` with `collabor.ErrInvalidGraph` or `collabor.ErrCycle` to identify these errors.
