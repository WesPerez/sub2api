package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/singleflight"
)

func TestContextSingleflightWaiterDeadlineDoesNotWaitForLeader(t *testing.T) {
	var group singleflight.Group
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		_, _, _ = doContextSingleflight(context.Background(), &group, "cache", func() (any, error) {
			close(started)
			<-release // deliberately ignores the waiter's cancellation
			return "ready", nil
		})
	}()
	<-started
	defer func() { close(release); <-finished }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var calls atomic.Int32
	begin := time.Now()
	_, err, _ := doContextSingleflight(ctx, &group, "cache", func() (any, error) {
		calls.Add(1)
		return nil, nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(begin), 300*time.Millisecond)
	require.Zero(t, calls.Load(), "canceled follower must neither duplicate nor forget the shared refresh")
}

func TestContextSingleflightLeaderCancellationPreservesSharedRefresh(t *testing.T) {
	var group singleflight.Group
	leader, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	started, release := make(chan struct{}), make(chan struct{})
	leaderResult := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		_, err, _ := doContextSingleflight(leader, &group, "cache", func() (any, error) {
			calls.Add(1)
			refresh, cancel := context.WithTimeout(context.WithoutCancel(leader), 2*time.Second)
			defer cancel()
			close(started)
			select {
			case <-release:
				return "ready", nil
			case <-refresh.Done():
				return nil, refresh.Err()
			}
		})
		leaderResult <- err
	}()
	<-started
	cancelLeader()
	require.ErrorIs(t, <-leaderResult, context.Canceled)
	followerCtx, cancelFollower := context.WithTimeout(context.Background(), time.Second)
	defer cancelFollower()
	joined := make(chan struct{})
	follower := &singleflightJoinedContext{Context: followerCtx, joined: joined}
	go func() { <-joined; close(release) }()
	value, err, shared := doContextSingleflight(follower, &group, "cache", func() (any, error) {
		calls.Add(1)
		return "duplicate", nil
	})
	require.NoError(t, err)
	require.Equal(t, "ready", value)
	require.True(t, shared)
	require.Equal(t, int32(1), calls.Load())
}

// Done is consulted by the wait select after DoChan joins the shared call.
type singleflightJoinedContext struct {
	context.Context
	joined chan struct{}
}

func (c *singleflightJoinedContext) Done() <-chan struct{} {
	select {
	case <-c.joined:
	default:
		close(c.joined)
	}
	return c.Context.Done()
}
