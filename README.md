# collabor

A simple and useful loading framework written in Go.

## Introduction
**collabor** means collaborate, which can manage jobs run in order and efficiently.

This library bases on Directed Acyclic Graph (DAG). It provides an efficient and reliable way to manage concurrent operations and ensure the correct execution order.


## Features

- **Efficiently**: Jobs run immediately after their dependencies are completed, which is the most efficient way to complete operations. For example,
```flow
        A(10ms)
      /        \
    B(100ms)    C(10ms)
    |           |
    D(5ms)      E(50ms)
     \         /
       F(10ms)
```
A(10ms) means that job A requires 10 milliseconds to be completed.

If run by level, the steps are:
1. level 1, run A:    cost 10ms
2. level 2, run B, C: cost 100ms
3. level 3, run D, E: cost 50ms
4. level 4, run F:    cost 10ms

The total time is 10ms + 100ms + 50ms + 10ms = 170ms.

If use collabor, the time line is:
1. 0: start
2. 10ms: A is completed, B and C start
3. 20ms: C is completed, E start
4. 70ms: E is completed
5. 110ms: B is completed, D start
6. 105ms: D is completed, F start
7. 115ms: F is completed

The total time is 115ms.

- **Easy-to-use**: Collabor provides a simple and easy-to-use API for managing concurrent operations.

You don't need to use goroutine, channel, sync or any other concurrency mechanism, collabor will manage all the concurrency for you.

## How to use

### Basic usage
see [example.go](https://github.com/csymlou/collabor/blob/main/example.go)
```go
// 1. define a struct to contain data
type Convey struct {
    // define the data you need
}

// 2. new a collabor instance
co := NewCo()

// 3. add jobs
var A = co.AddJob("A", func(ctx context.Context, i interface{}) error {
    convey := i.(*Convey)
    // do something
    return nil
}) // A depends nothing
var B = co.AddJob("B", func(ctx context.Context, i interface{}) error {
    convey := i.(*Convey)
    // do something
    return nil
}, A) // B depends on A

// 4. run jobs
convey := &Convey{}
err := co.Do(context.Background(), convey)
if err != nil {
    // handle error
}
```

### Error
Case 1: If an error occurs in one job, but does not affect others, then the job doesn't need `return err`, use `return nil` instead, and other jobs will run as normal. 

Case 2: If an error occurs in one job, and the dependent jobs can not run (missing necessary data), then the job needs `return err`, and other jobs will be canceled, an error will be returned by the `Do` method.

### Timeout

You can set a timeout for all jobs. When the timeout arrives but the jobs are not completely finished, the remaining jobs which have not started will be cancelled. 

Note: The started jobs will not be canceled.

```go
// set timeout
var co = NewCo().WithTimeout(time.Second)

```

### Panic

If a job throws panic, the job and other jobs will be canceled, and an error containing stack information will be returned. 
