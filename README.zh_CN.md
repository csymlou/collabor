[English](README.md)

# collabor

一个轻量的 Go DAG 并发任务执行器。任务会在其全部依赖成功后立即执行；错误、panic 或取消会阻止尚未开始的任务继续运行。

```shell
go get github.com/csymlou/collabor
```

## 特性

- 按有向无环图（DAG）描述任务依赖
- 无依赖任务并行执行，依赖满足后立即调度
- 任务错误快速失败，并取消本次执行
- 支持外部 context、全局超时和单任务超时
- 捕获任务 panic，并返回 panic 值及堆栈
- 运行前检查环、跨图依赖、nil 和重复依赖
- 同一个构建完成的 DAG 可并发执行多次

## 基础用法

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

## 错误与取消

任务返回非 nil 错误后，本次执行会被取消，依赖它的任务不会启动，`Do` 返回包含任务名的包装错误。可以使用 `errors.Is` 检查原始错误。

任务应该监听 `ctx.Done()` 并尽快退出：

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

取消是协作式的：Go 无法强制终止一个忽略 context 的函数。此类函数可能在 `Do` 返回后继续运行，并继续访问传入的 `input` 及其引用的资源。调用方必须确保这些数据在任务实际退出前仍然有效，且不能在无同步的情况下复用或修改。

## 超时

设置本次 DAG 执行的整体超时：

```go
co.WithTimeout(time.Second)
```

设置单个任务的超时：

```go
job := co.AddJob("slow", fn).WithTimeout(100 * time.Millisecond)
```

全局超时可通过 `errors.Is(err, collabor.ErrTimeout)` 判断；单任务超时可通过 `errors.Is(err, context.DeadlineExceeded)` 判断。多个终止条件同时发生时，调用方 context 优先于全局超时，全局超时优先于单任务超时。单任务函数在 deadline 前返回时采用其结果，在 deadline 时或之后返回时采用超时结果。

## 并发安全

构建完成后的 `Collabor` 可以被多个 goroutine 并发调用 `Do`，每次执行拥有独立状态。

传给 `Do` 的数据由调用方管理。并行任务不能无同步地读写同一字段；请让它们写不同字段，或使用 mutex、atomic、channel 等同步机制。任务图通常应在执行前构建完成。

## 图校验

`Do` 会拒绝以下配置：

- nil 任务函数或 nil 依赖
- 重复依赖
- 引用另一个 `Collabor` 中的任务
- 循环依赖

可以分别使用 `errors.Is(err, collabor.ErrInvalidGraph)` 和 `errors.Is(err, collabor.ErrCycle)` 判断。
