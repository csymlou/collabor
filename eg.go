package collabor

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"
)

// ErrGroup is kept for source compatibility. New code should normally use
// Collabor directly.
type ErrGroup struct {
	*errgroup.Group
}

func NewErrGroup() *ErrGroup { return &ErrGroup{Group: &errgroup.Group{}} }

func (eg *ErrGroup) WithContext(ctx context.Context) (*ErrGroup, context.Context) {
	eg.Group, ctx = errgroup.WithContext(ctx)
	return eg, ctx
}

// Wait waits for completion or returns ErrTimeout. It cannot cancel functions
// added directly to ErrGroup; prefer context.WithTimeout with WithContext.
func (eg *ErrGroup) Wait(timeout time.Duration) error {
	if timeout <= 0 {
		return eg.Group.Wait()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	done := make(chan error, 1)
	go func() { done <- eg.Group.Wait() }()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return ErrTimeout
	}
}
