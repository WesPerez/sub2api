package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const openAISessionBindingInvalidatedReason = GatewayFailureReason("openai_session_binding_invalidated")

// Only the structured upstream rejection is evidence that this session's
// binding was rejected. Other conflicts and text echoed from input retain
// their existing behavior.
func isOpenAISessionBindingInvalidated(status int, body []byte) bool {
	if status != http.StatusConflict || !gjson.ValidBytes(body) {
		return false
	}
	return gjson.GetBytes(body, "error.type").String() == "invalid_request_error" &&
		gjson.GetBytes(body, "error.code").String() == "session_binding_invalidated"
}

// RecordOpenAISessionBindingInvalidation reuses the bounded session/model
// exclusion store. Do not delete the sticky key: a late rejection could race
// with a successful request and delete its newer binding. Both schedulers
// already ignore excluded accounts and can select and bind a replacement.
func (s *OpenAIGatewayService) RecordOpenAISessionBindingInvalidation(ctx context.Context, groupID *int64, sessionHash, model string, account *Account, err error) {
	var failure *UpstreamFailoverError
	if s == nil || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey ||
		!errors.As(err, &failure) || failure.Reason != openAISessionBindingInvalidatedReason ||
		!isOpenAISessionBindingInvalidated(failure.StatusCode, failure.ResponseBody) ||
		strings.TrimSpace(sessionHash) == "" || strings.TrimSpace(model) == "" {
		return
	}
	cache, ok := s.cache.(OpenAISessionTimeoutCache)
	if !ok {
		return
	}
	maintenanceCtx, cancel := openAIResponsesMaintenanceContext(ctx, 3*time.Second)
	defer cancel()
	if recordErr := cache.RecordOpenAISessionTimeout(maintenanceCtx, derefGroupID(groupID), sessionHash, model, account.ID, openAISessionTimeoutExclusionTTL); recordErr != nil {
		logger.L().Warn("openai.session_binding_exclusion_failed", zap.Int64("account_id", account.ID), zap.Error(recordErr))
	}
}
