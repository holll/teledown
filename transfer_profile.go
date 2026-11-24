package main

import (
	"context"
	"time"
)

const premiumDownloadThreads = 8

// downloadThreadLimit keeps explicit user settings and the ordinary profile for
// bots. An unavailable Premium check does not prevent downloads.
func downloadThreadLimit(ctx context.Context, ordinary int, explicit, bot bool, lookup func(context.Context) (bool, error)) (int, error) {
	if explicit || bot {
		return ordinary, ctx.Err()
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	premium, err := lookup(checkCtx)
	if err != nil || !premium {
		return ordinary, err
	}
	return premiumDownloadThreads, nil
}
