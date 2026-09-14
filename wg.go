package collabor

import (
	"sync"
	"time"
)

// WaitGroup is a sync.WaitGroup with an optional timeout wait.
type WaitGroup struct{ *sync.WaitGroup }

func NewWaitGroup() *WaitGroup { return &WaitGroup{WaitGroup: &sync.WaitGroup{}} }

// Wait returns false if timeout expires before all goroutines finish.
func (wg *WaitGroup) Wait(timeout time.Duration) bool {
	if timeout <= 0 {
		wg.WaitGroup.Wait()
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	done := make(chan struct{})
	go func() {
		wg.WaitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
