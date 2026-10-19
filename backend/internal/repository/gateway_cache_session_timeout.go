package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var _ service.OpenAISessionTimeoutCache = (*gatewayCache)(nil)

func buildOpenAISessionTimeoutKey(groupID int64, sessionHash, model string) string {
	// No prompt, credential, raw session ID or unbounded model name in Redis keys.
	digest := sha256.Sum256([]byte(sessionHash + "\x00" + model))
	return fmt.Sprintf("openai_session_timeout:%d:%x", groupID, digest)
}

var recordOpenAISessionTimeoutScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[2]), ARGV[1])
local count = redis.call('ZCARD', KEYS[1])
if count > 128 then redis.call('ZREMRANGEBYRANK', KEYS[1], 0, count - 129) end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

var getOpenAISessionTimeoutScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
return redis.call('ZRANGEBYSCORE', KEYS[1], '(' .. now, '+inf', 'LIMIT', 0, 128)
`)

func (c *gatewayCache) RecordOpenAISessionTimeout(ctx context.Context, groupID int64, sessionHash, model string, accountID int64, ttl time.Duration) error {
	if sessionHash == "" || model == "" || accountID <= 0 || ttl <= 0 {
		return nil
	}
	// Keep this best-effort bookkeeping short even when the caller has more
	// time. The clone shares the existing pool and must not be closed.
	return recordOpenAISessionTimeoutScript.Run(ctx, c.rdb.WithTimeout(200*time.Millisecond), []string{buildOpenAISessionTimeoutKey(groupID, sessionHash, model)}, accountID, ttl.Milliseconds()).Err()
}

func (c *gatewayCache) GetOpenAISessionTimeoutAccounts(ctx context.Context, groupID int64, sessionHash, model string) ([]int64, error) {
	if sessionHash == "" || model == "" {
		return nil, nil
	}
	values, err := getOpenAISessionTimeoutScript.Run(ctx, c.rdb.WithTimeout(100*time.Millisecond), []string{buildOpenAISessionTimeoutKey(groupID, sessionHash, model)}).StringSlice()
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
