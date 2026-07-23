package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type missingWebSearchSettingRepo struct{}

func (r *missingWebSearchSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}

func (r *missingWebSearchSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", ErrSettingNotFound
}

func (r *missingWebSearchSettingRepo) Set(context.Context, string, string) error {
	return nil
}

func (r *missingWebSearchSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *missingWebSearchSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (r *missingWebSearchSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *missingWebSearchSettingRepo) Delete(context.Context, string) error {
	return nil
}

func TestGetWebSearchEmulationConfigMissingSettingUsesDisabledDefault(t *testing.T) {
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{expiresAt: time.Now().Add(-time.Second).UnixNano()})
	webSearchEmulationSF.Forget(sfKeyWebSearchConfig)

	service := NewSettingService(&missingWebSearchSettingRepo{}, &config.Config{})
	cfg, err := service.GetWebSearchEmulationConfig(context.Background())

	require.NoError(t, err)
	require.False(t, cfg.Enabled)
	require.Empty(t, cfg.Providers)
}
