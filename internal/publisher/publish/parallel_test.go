package publish

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestParallel(t *testing.T) {
	t.Run("all indexes", func(t *testing.T) {
		var seen [100]atomic.Int32

		if err := parallel(t.Context(), len(seen), 4, func(_ context.Context, i int) error {
			seen[i].Add(1)

			return nil
		}); err != nil {
			t.Fatal(err)
		}

		for i := range seen {
			if n := seen[i].Load(); n != 1 {
				t.Errorf("index %d ran %d times", i, n)
			}
		}
	})

	t.Run("first error cancels the rest", func(t *testing.T) {
		boom := errors.New("boom")

		var started atomic.Int32

		err := parallel(t.Context(), 1000, 1, func(ctx context.Context, i int) error {
			started.Add(1)

			if i == 3 {
				return boom
			}

			return ctx.Err()
		})
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want boom", err)
		}

		if n := started.Load(); n != 4 {
			t.Errorf("%d indexes started with one job, want 4", n)
		}
	})

	t.Run("canceled parent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		var started atomic.Int32

		err := parallel(ctx, 10, 2, func(context.Context, int) error {
			started.Add(1)

			return nil
		})
		if fault.KindOf(err) != fault.Canceled || !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want canceled", err)
		}

		if n := started.Load(); n != 0 {
			t.Errorf("%d indexes started after cancellation", n)
		}
	})

	t.Run("canceled while running", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var started atomic.Int32

		err := parallel(ctx, 1000, 1, func(context.Context, int) error {
			if started.Add(1) == 2 {
				cancel()
			}

			return nil
		})
		if fault.KindOf(err) != fault.Canceled {
			t.Fatalf("error = %v, want canceled", err)
		}

		if n := started.Load(); n != 2 {
			t.Errorf("%d indexes started, want 2", n)
		}
	})
}
