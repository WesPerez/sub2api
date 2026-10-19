package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// OpenAISessionTimeoutCache keeps confirmed first-output failures scoped to one
// group/session/model. It does not change account health or upstream identities.
// Optional, like the other specialized GatewayCache capabilities.
type OpenAISessionTimeoutCache interface {
	RecordOpenAISessionTimeout(context.Context, int64, string, string, int64, time.Duration) error
	GetOpenAISessionTimeoutAccounts(context.Context, int64, string, string) ([]int64, error)
}

// Two ordinary five-minute attempts: a retry that was already selected when
// the previous timeout was recorded must not outlive the exclusion immediately.
const openAISessionTimeoutExclusionTTL = 10 * time.Minute

var ErrOpenAISessionTimeoutCapacity = errors.New("session first-output timeout exclusions exhausted available capacity")

func openAISessionTimeoutSelectionError(err error, hadSessionExclusions bool) error {
	if hadSessionExclusions && (errors.Is(err, ErrNoAvailableAccounts) || errors.Is(err, ErrNoAvailableCompactAccounts)) {
		return fmt.Errorf("%w: %w", ErrOpenAISessionTimeoutCapacity, err)
	}
	return err
}

func (s *OpenAIGatewayService) RecordOpenAISessionFirstOutputTimeout(ctx context.Context, groupID *int64, sessionHash, model string, account *Account, err error) {
	var failure *UpstreamFailoverError
	if s == nil || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey ||
		!errors.As(err, &failure) || !failure.FirstOutputTimedOut || strings.TrimSpace(sessionHash) == "" || strings.TrimSpace(model) == "" {
		return
	}
	cache, ok := s.cache.(OpenAISessionTimeoutCache)
	if !ok {
		return
	}
	maintenanceCtx, cancel := openAIResponsesMaintenanceContext(ctx, 3*time.Second)
	defer cancel()
	if recordErr := cache.RecordOpenAISessionTimeout(maintenanceCtx, derefGroupID(groupID), sessionHash, model, account.ID, openAISessionTimeoutExclusionTTL); recordErr != nil {
		logger.L().Warn("openai.session_timeout_record_failed", zap.Int64("account_id", account.ID), zap.Error(recordErr))
		return
	}
	logger.L().Info("openai.session_timeout_recorded", zap.Int64("account_id", account.ID), zap.String("model", model), zap.Duration("ttl", openAISessionTimeoutExclusionTTL))
}

func (s *OpenAIGatewayService) withOpenAISessionTimeoutExclusions(ctx context.Context, groupID *int64, sessionHash, model string, excluded map[int64]struct{}) map[int64]struct{} {
	if s == nil || strings.TrimSpace(sessionHash) == "" || strings.TrimSpace(model) == "" {
		return excluded
	}
	cache, ok := s.cache.(OpenAISessionTimeoutCache)
	if !ok {
		return excluded
	}
	readCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	ids, err := cache.GetOpenAISessionTimeoutAccounts(readCtx, derefGroupID(groupID), sessionHash, model)
	if err != nil {
		// Redis availability must not prevent an otherwise valid request.
		logger.L().Debug("openai.session_timeout_read_failed", zap.Error(err))
		return excluded
	}
	if len(ids) == 0 {
		return excluded
	}
	merged := make(map[int64]struct{}, len(excluded)+len(ids))
	for id := range excluded {
		merged[id] = struct{}{}
	}
	for _, id := range ids {
		if id > 0 {
			merged[id] = struct{}{}
		}
	}
	return merged
}
