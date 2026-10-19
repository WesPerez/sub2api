package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type budgetSlowCyberSettingRepo struct {
	SettingRepository
	entered chan struct{}
	release chan struct{}
}

func (r *budgetSlowCyberSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if key != SettingKeyCyberSessionBlockEnabled {
		return "", ErrSettingNotFound
	}
	close(r.entered)
	select {
	case <-r.release:
		return "true", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestCyberRuntimeCacheWaitRespectsRequestBudget(t *testing.T) {
	repo := &budgetSlowCyberSettingRepo{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(repo.release)
	svc := NewSettingService(repo, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go func() { enabled, _ := svc.GetCyberSessionBlockRuntime(ctx); done <- enabled }()
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("cyber runtime repository was not entered")
	}
	select {
	case enabled := <-done:
		require.False(t, enabled)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("cyber runtime cache wait exceeded the request deadline")
	}
}
