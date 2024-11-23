[English](README.md)
# collabor

一个简单实用的并发加载框架。

## 介绍
**collabor** 字面意思是协作，它可以管理并发任务按顺序完成，效率极高。

它基于有向无环图 (DAG) 实现，提供高效的并发和依赖管理，确保任务按顺序完成。


## 特点

- **高效**: 每个任务都只依赖自己的前置任务，无其他依赖和等待，这是最高效的方式。例如：
```flow
        A(10ms)
      /        \
    B(100ms)    C(10ms)
    |           |
    D(5ms)      E(50ms)
     \         /
       F(10ms)
```
A(10ms) 表示任务A需要10ms完成。

如何按层级执行，步骤如下：
1. 第1层，运行A: 耗时10ms
2. 第2层，运行B, C: 耗时100ms
3. 第3层，运行D, E: 耗时50ms
4. 第4层，运行F: 耗时10ms
总耗时为10ms + 100ms + 50ms + 10ms = 170ms。

如果按 collabor 方式执行，时间线如下：
1. 0: 开始
2. 第10ms: A完成，B, C开始
3. 第20ms: C完成，E开始
4. 第70ms: E完成
5. 第110ms: B完成，D开始
6. 第115ms: D完成，F开始
7. 第125ms: F完成
总耗时为125ms。

- **易用**: Collabor 提供了简单且易用的 API 控制并发，你不需要使用 goroutine, channel, sync 或任何其他并发机制，collabor 会管理所有的并发。

## 如何使用

### 基础用法
参考 [example.go](example.go)
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

### 处理错误

情况1：如果一个任务发生错误，但是不影响其他任务（弱依赖），则该任务不需要 `return err`, 使用 `return nil` 即可，其他任务会正常运行。

情况2：如果一个任务发生错误，且依赖该任务的任务都无法运行（如缺少必要数据/强依赖），则该任务需要 `return err`, 其他未开始的任务会被取消，`Do` 方法会返回错误。

### 超时

你可以设置一个整体超时时间，当超时时间到达但任务未完全完成时，剩余未开始的任务将被取消，`Do` 方法会返回错误。

注意：已开始的任务不会被取消。

```go
// set timeout
var co = NewCo().WithTimeout(time.Second)

```

### 处理 Panic

如果一个任务抛出 panic，该任务和其他未开始的任务会被取消，`Do` 方法会返回包含堆栈的错误。
