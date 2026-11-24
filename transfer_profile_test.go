package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestDownloadThreadLimit(t *testing.T) {
	unavailable := errors.New("unavailable")
	for _, tc := range []struct {
		name                   string
		explicit, bot, premium bool
		lookupErr              error
		ordinary, want, calls  int
	}{
		{"ordinary", false, false, false, nil, 4, 4, 1},
		{"premium", false, false, true, nil, 4, 8, 1},
		{"explicit", true, false, true, nil, 4, 4, 0},
		{"explicit override", true, false, true, nil, 1, 1, 0},
		{"bot", false, true, true, nil, 4, 4, 0},
		{"cannot confirm", false, false, false, unavailable, 4, 4, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, err := downloadThreadLimit(t.Context(), tc.ordinary, tc.explicit, tc.bot, func(context.Context) (bool, error) {
				calls++
				return tc.premium, tc.lookupErr
			})
			if got != tc.want || err != tc.lookupErr || calls != tc.calls {
				t.Fatalf("got %d, %v, %d lookups; want %d, %v, %d lookups", got, err, calls, tc.want, tc.lookupErr, tc.calls)
			}
		})
	}
}

func TestDownloadThreadLookupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		got, err := downloadThreadLimit(t.Context(), 4, false, false, func(ctx context.Context) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		})
		if got != 4 || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 3*time.Second {
			t.Fatalf("got %d, %v, after %v", got, err, time.Since(start))
		}
	})
}
