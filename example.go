package collabor

import "context"

func Example() {
	type Convey struct {
		input  int
		fromB  int // parallel jobs write separate fields
		fromC  int
		output int
	}

	co := NewCo()
	a := co.AddJob("A", func(ctx context.Context, input interface{}) error {
		convey := input.(*Convey)
		convey.output = convey.input
		return nil
	})
	b := co.AddJob("B", func(ctx context.Context, input interface{}) error {
		input.(*Convey).fromB = 2
		return nil
	}, a)
	c := co.AddJob("C", func(ctx context.Context, input interface{}) error {
		input.(*Convey).fromC = 3
		return nil
	}, a)
	co.AddJob("D", func(ctx context.Context, input interface{}) error {
		convey := input.(*Convey)
		convey.output += convey.fromB + convey.fromC
		return nil
	}, b, c)

	_ = co.Do(context.Background(), &Convey{input: 1})
}
