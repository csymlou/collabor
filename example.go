package collabor

import (
	"context"

	"github.com/cloudwego/kitex/pkg/klog"
)

func Example() {
	/**
	* a task contains A, B, C, D 4 stages,
	* their dependencies are as follows:
	*     A
	*   /	\
	*  B	 C
	*   \  	/
	*	  D
	*
	* B, C can run concurrently, but depend on A,
	* and D depend on B and C all finished.
	*
	**/

	// 1. define a struct to contain data
	type Convey struct {
		input     any // eg. input args
		temporary any // eg. temporary data, use to pass between jobs
		output    any // eg. output data
	}

	// 2. new a collabor instance
	co := NewCo()

	// 3. add jobs
	var A = co.AddJob("A", func(ctx context.Context, i any) error {
		convey := i.(*Convey)
		// do something
		convey.output = 1
		convey.temporary = 2
		return nil
	}) // A depends nothing
	var B = co.AddJob("B", func(ctx context.Context, i any) error {
		convey := i.(*Convey)
		// do something
		convey.output = 2
		return nil
	}, A) // B depends on A
	var C = co.AddJob("C", func(ctx context.Context, i any) error {
		convey := i.(*Convey)
		// do something
		convey.output = 3
		return nil
	}, A) // C depends on A
	var _ = co.AddJob("D", func(ctx context.Context, i any) error {
		convey := i.(*Convey)
		// do something
		convey.output = 4
		return nil
	}, B, C) // D depends on B and C

	// 4. run jobs
	convey := &Convey{
		input: 1,
	}
	err := co.Do(context.Background(), convey)
	if err != nil {
		klog.Errorf("collabor error: %v", err)
	}
}
