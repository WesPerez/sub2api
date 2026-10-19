package service

import (
	"context"

	"golang.org/x/sync/singleflight"
)

// Cache refreshes may outlive an individual waiter. Never let a request's
// deadline wait for another request's database or Redis operation to return.
func doContextSingleflight(ctx context.Context, group *singleflight.Group, key string, fn func() (any, error)) (any, error, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err, false
	}
	result := group.DoChan(key, fn)
	select {
	case <-ctx.Done():
		return nil, ctx.Err(), false
	case value := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err, value.Shared
		}
		return value.Val, value.Err, value.Shared
	}
}
