package repository

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestBuildRedisOptions(t *testing.T) {
	cfg := &config.Config{
		Redis: config.RedisConfig{
			Host:                "localhost",
			Port:                6379,
			Username:            "app-user",
			Password:            "secret",
			DB:                  2,
			DialTimeoutSeconds:  5,
			ReadTimeoutSeconds:  3,
			WriteTimeoutSeconds: 4,
			PoolSize:            100,
			MinIdleConns:        10,
		},
	}

	opts := buildRedisOptions(cfg)
	require.Equal(t, "localhost:6379", opts.Addr)
	require.Equal(t, "app-user", opts.Username)
	require.Equal(t, "secret", opts.Password)
	require.Equal(t, 2, opts.DB)
	require.Equal(t, 5*time.Second, opts.DialTimeout)
	require.Equal(t, 3*time.Second, opts.ReadTimeout)
	require.Equal(t, 4*time.Second, opts.WriteTimeout)
	require.Equal(t, 100, opts.PoolSize)
	require.Equal(t, 10, opts.MinIdleConns)
	require.True(t, opts.ContextTimeoutEnabled)
	require.Nil(t, opts.TLSConfig)

	// Test case with TLS enabled
	cfgTLS := &config.Config{
		Redis: config.RedisConfig{
			Host:      "localhost",
			EnableTLS: true,
		},
	}
	optsTLS := buildRedisOptions(cfgTLS)
	require.NotNil(t, optsTLS.TLSConfig)
	require.Equal(t, "localhost", optsTLS.TLSConfig.ServerName)
}

func TestRedisRequestDeadlineBoundsUnresponsivePeer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	done := make(chan struct{})
	readFrame := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1024)
		first := true
		for {
			if _, readErr := conn.Read(buf); readErr != nil {
				return
			}
			if first {
				close(readFrame)
				first = false
			}
		}
	}()
	opts := buildRedisOptions(&config.Config{Redis: config.RedisConfig{ReadTimeoutSeconds: 3, WriteTimeoutSeconds: 3}})
	opts.Addr = listener.Addr().String()
	opts.MaxRetries = -1
	opts.MinIdleConns = 0
	client := redis.NewClient(opts)
	defer func() { _ = listener.Close(); _ = client.Close(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	require.Error(t, client.Ping(ctx).Err())
	select {
	case <-readFrame:
	default:
		t.Fatal("must exercise an in-flight Redis handshake")
	}
	require.Less(t, time.Since(started), 500*time.Millisecond, "Redis socket ignored request deadline")
}
