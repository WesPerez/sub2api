package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

const openAIAccountScheduleLayerSameAccountRetry = "same_account_retry"

func consumeOpenAISameAccountRetrySelection(
	pending **service.AccountSelectionResult,
) (*service.AccountSelectionResult, service.OpenAIAccountScheduleDecision, bool) {
	if pending == nil || *pending == nil {
		return nil, service.OpenAIAccountScheduleDecision{}, false
	}

	selection := *pending
	*pending = nil
	decision := service.OpenAIAccountScheduleDecision{
		Layer: openAIAccountScheduleLayerSameAccountRetry,
	}
	if selection.Account != nil {
		decision.SelectedAccountID = selection.Account.ID
		decision.SelectedAccountType = selection.Account.Type
	}
	return selection, decision, true
}
