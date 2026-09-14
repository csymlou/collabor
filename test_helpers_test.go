package collabor

import "sync/atomic"

// These helpers keep the test suite compatible with the Go 1.18 version
// declared by go.mod. The typed atomic values were added in Go 1.19.
type atomicBool uint32

func (b *atomicBool) Store(value bool) {
	var encoded uint32
	if value {
		encoded = 1
	}
	atomic.StoreUint32((*uint32)(b), encoded)
}

func (b *atomicBool) Load() bool {
	return atomic.LoadUint32((*uint32)(b)) != 0
}

type atomicInt32 int32

func (i *atomicInt32) Add(delta int32) int32 {
	return atomic.AddInt32((*int32)(i), delta)
}

func (i *atomicInt32) Load() int32 {
	return atomic.LoadInt32((*int32)(i))
}
