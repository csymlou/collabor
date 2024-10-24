package collabor

import (
	"sync"
	"time"

	"github.com/cloudwego/kitex/pkg/klog"
)

type WaitGroup struct {
	*sync.WaitGroup
}

func NewWaitGroup() *WaitGroup {
	return &WaitGroup{
		WaitGroup: &sync.WaitGroup{},
	}
}

func (wg *WaitGroup) Wait(timeout time.Duration) bool {
	if timeout <= 0 {
		wg.WaitGroup.Wait()
		return true
	}
	done := make(chan struct{}, 1)
	go func() {
		wg.WaitGroup.Wait()
		done <- struct{}{}
	}()
	select {
	case <-done:
		klog.Info("WaitGroup all finished")
		return true
	case <-time.After(timeout):
		klog.Warn("WaitGroup timeout")
		return false
	}
}
