package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSessionTimeoutCacheExpiryIsolationAndBound(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewGatewayCache(client).(service.OpenAISessionTimeoutCache)
	require.True(t, ok)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	server.SetTime(start)
	require.NoError(t, store.RecordOpenAISessionTimeout(ctx, 11, "session", "model", 2290, 10*time.Minute))
	server.SetTime(start.Add(6 * time.Minute))
	require.NoError(t, store.RecordOpenAISessionTimeout(ctx, 11, "session", "model", 2265, 10*time.Minute))
	server.SetTime(start.Add(11 * time.Minute))
	ids, err := store.GetOpenAISessionTimeoutAccounts(ctx, 11, "session", "model")
	require.NoError(t, err)
	require.Equal(t, []int64{2265}, ids, "a new failure must not prolong an old member's expiry")
	for _, scope := range []struct {
		group          int64
		session, model string
	}{{12, "session", "model"}, {11, "other", "model"}, {11, "session", "other"}} {
		ids, err := store.GetOpenAISessionTimeoutAccounts(ctx, scope.group, scope.session, scope.model)
		require.NoError(t, err)
		require.Empty(t, ids)
	}
	server.SetTime(start.Add(17 * time.Minute))
	ids, err = store.GetOpenAISessionTimeoutAccounts(ctx, 11, "session", "model")
	require.NoError(t, err)
	require.Empty(t, ids)
	for id := int64(1); id <= 140; id++ {
		require.NoError(t, store.RecordOpenAISessionTimeout(ctx, 11, "bounded", "model", id, 10*time.Minute))
	}
	ids, err = store.GetOpenAISessionTimeoutAccounts(ctx, 11, "bounded", "model")
	require.NoError(t, err)
	require.Len(t, ids, 128)
	server.FastForward(11 * time.Minute)
	require.False(t, server.Exists(buildOpenAISessionTimeoutKey(11, "bounded", "model")))
}
