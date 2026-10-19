//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type asyncLastUsedRepo struct {
	APIKeyRepository
	write func(context.Context, int64, time.Time) error
}

func (r asyncLastUsedRepo) UpdateLastUsed(ctx context.Context, id int64, at time.Time) error {
	return r.write(ctx, id, at)
}

func TestLastUsedAsyncDoesNotWaitAndDeduplicatesBlockedWrite(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	svc := &APIKeyService{apiKeyRepo: asyncLastUsedRepo{write: func(context.Context, int64, time.Time) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}}}
	defer close(release)
	returned := make(chan struct{})
	go func() { svc.TouchLastUsedAsync(context.Background(), 1); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("metadata write blocked admission")
	}
	<-entered
	var callers sync.WaitGroup
	for range 100 {
		callers.Add(1)
		go func() { defer callers.Done(); svc.TouchLastUsedAsync(context.Background(), 1) }()
	}
	callers.Wait()
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, int32(1), svc.lastUsedTouchActive.Load())
}

func TestLastUsedAsyncBoundsWorkersEvenWhenRepositoryIgnoresCancellation(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	svc := &APIKeyService{apiKeyRepo: asyncLastUsedRepo{write: func(context.Context, int64, time.Time) error {
		calls.Add(1)
		<-release
		return nil
	}}}
	defer close(release)
	for id := int64(1); id <= 100; id++ {
		svc.TouchLastUsedAsync(context.Background(), id)
	}
	require.Eventually(t, func() bool { return calls.Load() == apiKeyLastUsedMaxConcurrent }, time.Second, time.Millisecond)
	require.Equal(t, int32(apiKeyLastUsedMaxConcurrent), svc.lastUsedTouchActive.Load())
	pending := 0
	svc.lastUsedTouchPending.Range(func(any, any) bool { pending++; return true })
	require.Equal(t, apiKeyLastUsedMaxConcurrent, pending)
}

func TestLastUsedAsyncUsesIndependentDeadlineAndFailureBackoff(t *testing.T) {
	entered := make(chan context.Context, 1)
	var calls atomic.Int32
	svc := &APIKeyService{apiKeyRepo: asyncLastUsedRepo{write: func(ctx context.Context, _ int64, _ time.Time) error {
		calls.Add(1)
		entered <- ctx
		<-ctx.Done()
		return ctx.Err()
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	svc.TouchLastUsedAsync(ctx, 7)
	writeCtx := <-entered
	cancel()
	require.NoError(t, writeCtx.Err(), "finishing an accepted request must not cancel its metadata write")
	deadline, ok := writeCtx.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, time.Until(deadline), apiKeyLastUsedWriteTimeout)
	require.Eventually(t, func() bool { return svc.lastUsedTouchActive.Load() == 0 }, 3*time.Second, time.Millisecond)
	require.ErrorIs(t, writeCtx.Err(), context.DeadlineExceeded)
	svc.TouchLastUsedAsync(context.Background(), 7)
	require.Equal(t, int32(1), calls.Load(), "failed writes must retain their backoff")
	svc.TouchLastUsedAsync(ctx, 8)
	require.Equal(t, int32(1), calls.Load(), "already canceled requests must not schedule metadata")
}

func TestLastUsedAsyncRecoversPanicAndReleasesCapacity(t *testing.T) {
	var calls atomic.Int32
	svc := &APIKeyService{apiKeyRepo: asyncLastUsedRepo{write: func(context.Context, int64, time.Time) error {
		if calls.Add(1) == 1 {
			panic("metadata repository failed")
		}
		return nil
	}}}
	svc.TouchLastUsedAsync(context.Background(), 1)
	require.Eventually(t, func() bool { return svc.lastUsedTouchActive.Load() == 0 }, time.Second, time.Millisecond)
	_, pending := svc.lastUsedTouchPending.Load(int64(1))
	require.False(t, pending)
	svc.TouchLastUsedAsync(context.Background(), 1)
	require.Equal(t, int32(1), calls.Load(), "panic must retain failure backoff")
	svc.TouchLastUsedAsync(context.Background(), 2)
	require.Eventually(t, func() bool { return calls.Load() == 2 && svc.lastUsedTouchActive.Load() == 0 }, time.Second, time.Millisecond)
}
