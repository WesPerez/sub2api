package service

import (
	"context"
	"errors"
	"log/slog"
)

type ConditionalAccountRecoveryRepository interface {
	ApplyScheduledRecovery(context.Context, *Account, bool) (bool, error)
}

// ApplyScheduledRecovery is the account-state boundary for scheduled API-key
// recovery. OAuth lifecycle stays with its own refresh/recovery services.
func (s *RateLimitService) ApplyScheduledRecovery(ctx context.Context, expected *Account, enabled bool) (bool, error) {
	if expected == nil || expected.Type != AccountTypeAPIKey {
		return false, errors.New("scheduled recovery requires an API-key account")
	}
	repo, ok := s.accountRepo.(ConditionalAccountRecoveryRepository)
	if !ok {
		return false, errors.New("conditional account recovery unavailable")
	}
	changed, err := repo.ApplyScheduledRecovery(ctx, expected, enabled)
	if err != nil || !changed {
		return changed, err
	}
	if enabled {
		if s.tempUnschedCache != nil {
			if err := s.tempUnschedCache.DeleteTempUnsched(ctx, expected.ID); err != nil {
				slog.Warn("scheduled_recovery_cache_delete_failed", "account_id", expected.ID)
			}
		}
		s.ResetOpenAI403Counter(ctx, expected.ID)
		s.notifyAccountSchedulingBlockCleared(expected.ID)
	}
	return true, nil
}
