package collabor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCollabor(t *testing.T) {
	in := func(s string, ss []string) bool {
		for _, s1 := range ss {
			if s == s1 {
				return true
			}
		}
		return false
	}
	approximately := func(a, b time.Duration) bool {
		delta := time.Duration(float64(b) * 0.05)
		return a >= b-delta && a <= b+delta
	}
	timeit := func(f func()) time.Duration {
		begin := time.Now()
		f()
		return time.Since(begin)
	}

	type Convey struct {
		result string
	}
	fake := func(name string, milli int64, args ...string) func(ctx context.Context, i interface{}) error {
		return func(ctx context.Context, i interface{}) error {
			item := i.(*Convey)
			time.Sleep(time.Millisecond * time.Duration(milli))
			item.result += name
			if in("err", args) {
				return fmt.Errorf("job %s error", name)
			}
			if in("panic", args) {
				panic(fmt.Sprintf("job %s panic", name))
			}
			return nil
		}
	}

	t.Run("single", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		co.AddJob("A", fake("A", 100))
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		assert.Equal(t, "A", convey.result)
		assert.True(t, approximately(it, time.Millisecond*100))
	})

	t.Run("parallel", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		co.AddJob("A", fake("A", 100))
		co.AddJob("B", fake("B", 100))
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		assert.Contains(t, []string{"AB", "BA"}, convey.result)
		assert.True(t, approximately(it, time.Millisecond*100))
	})

	t.Run("serial", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 100))
		_ = co.AddJob("B", fake("B", 100), A)
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		assert.Equal(t, "AB", convey.result)
		assert.True(t, approximately(it, time.Millisecond*200))
	})

	t.Run("triangle", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 100))
		var _ = co.AddJob("B", fake("B", 50), A)
		var _ = co.AddJob("C", fake("C", 80), A)
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		t.Log(convey.result, it)
		assert.Equal(t, "ABC", convey.result)
		assert.True(t, approximately(it, time.Millisecond*180))
	})

	t.Run("diamond", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 100))
		var B = co.AddJob("B", fake("B", 50), A)
		var C = co.AddJob("C", fake("C", 100), A)
		var _ = co.AddJob("D", fake("D", 20), B, C)
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		t.Log(convey.result, it)
		assert.Equal(t, "ABCD", convey.result)
		assert.True(t, approximately(it, time.Millisecond*220))
	})

	t.Run("tree", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 100))
		var B = co.AddJob("B", fake("B", 50), A)
		var C = co.AddJob("C", fake("C", 120))
		var _ = co.AddJob("D", fake("D", 20), B, C)
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		t.Log(convey.result, it)
		assert.Equal(t, "ACBD", convey.result)
		assert.True(t, approximately(it, time.Millisecond*170))
	})

	t.Run("graph#1", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 5))
		var B = co.AddJob("B", fake("B", 2))
		var C = co.AddJob("C", fake("C", 5), A, B)
		var D = co.AddJob("D", fake("D", 50), C)
		var E = co.AddJob("E", fake("E", 5), C)
		var _ = co.AddJob("F", fake("F", 1), D)
		var _ = co.AddJob("G", fake("G", 40), E)
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		t.Log(convey.result, it)
		assert.Equal(t, "BACEGDF", convey.result)
		assert.True(t, approximately(it, time.Millisecond*61))
	})

	t.Run("graph#2", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 5))
		var B = co.AddJob("B", fake("B", 2), A)
		var C = co.AddJob("C", fake("C", 4), A)
		var D = co.AddJob("D", fake("D", 50))
		var E = co.AddJob("E", fake("E", 5), D)
		var _ = co.AddJob("F", fake("F", 1), C, E)
		var _ = co.AddJob("G", fake("G", 40), A, B, C, E)
		it := timeit(func() {
			co.Do(context.Background(), convey)
		})
		t.Log(convey.result, it)
		assert.Equal(t, "ABCDEFG", convey.result)
		assert.True(t, approximately(it, time.Millisecond*96))
	})

	t.Run("error#1", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var _ = co.AddJob("A", fake("A", 5, "err"))
		timeit(func() {
			err := co.Do(context.Background(), convey)
			assert.EqualError(t, err, "job A error")
		})
	})

	t.Run("error#2", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 5))
		var B = co.AddJob("B", fake("B", 5, "err"), A)
		var _ = co.AddJob("C", fake("C", 5), B)
		timeit(func() {
			err := co.Do(context.Background(), convey)
			assert.EqualError(t, err, "job B error")
		})
	})

	t.Run("timeout", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo().WithTimeout(time.Second)
		var _ = co.AddJob("A", fake("A", 2000))
		it := timeit(func() {
			err := co.Do(context.Background(), convey)
			assert.ErrorIs(t, err, ErrTimeout)
		})
		assert.True(t, approximately(it, time.Second))
	})

	t.Run("cancel", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 100))
		var B = co.AddJob("B", fake("B", 100, "err"), A)
		var C = co.AddJob("C", fake("C", 100), B)
		var _ = co.AddJob("D", fake("D", 100), C)
		it := timeit(func() {
			err := co.Do(context.Background(), convey)
			assert.ErrorContains(t, err, "job B error")
		})
		assert.True(t, approximately(it, time.Millisecond*200))
	})

	t.Run("panic#1", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var _ = co.AddJob("A", fake("A", 5, "panic"))
		timeit(func() {
			err := co.Do(context.Background(), convey)
			assert.ErrorContains(t, err, "job A panic")
		})
	})

	t.Run("panic#2", func(t *testing.T) {
		convey := &Convey{}
		var co = NewCo()
		var A = co.AddJob("A", fake("A", 5))
		var B = co.AddJob("B", fake("B", 5, "panic"), A)
		var _ = co.AddJob("C", fake("C", 5), B)
		timeit(func() {
			err := co.Do(context.Background(), convey)
			assert.ErrorContains(t, err, "job B panic")
		})
	})

}
