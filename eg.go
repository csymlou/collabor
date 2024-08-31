package collabor

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/kitex/pkg/klog"
	"golang.org/x/sync/errgroup"
)

var (
	ErrTimeout = errors.New("collabor timeout")
)

type ErrGroup struct {
	*errgroup.Group
}

func NewErrGroup() *ErrGroup {
	return &ErrGroup{
		Group: &errgroup.Group{},
	}
}

func (eg *ErrGroup) WithContext(ctx context.Context) (*ErrGroup, context.Context) {
	eg.Group, ctx = errgroup.WithContext(ctx)
	return eg, ctx
}

func (eg *ErrGroup) Wait(timeout time.Duration) error {
	if timeout <= 0 {
		return eg.Group.Wait()
	}
	done := make(chan struct{})
	var err error
	go func() {
		err = eg.Group.Wait()
		done <- struct{}{}
	}()
	select {
	case <-done:
		klog.Info("ErrGroup all finished")
		return err
	case <-time.After(timeout):
		klog.Warn("ErrGroup timeout")
		return ErrTimeout
	}
}
