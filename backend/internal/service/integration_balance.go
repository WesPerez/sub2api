package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/shopspring/decimal"
)

// IntegrationBalanceKey belongs to this application. Consumers use the public
// projection, never interpret this storage object or the editable notes text.
const IntegrationBalanceKey = "integration_balance_v1"

const IntegrationBalanceMaxClockSkew = time.Minute

var ErrIntegrationBalanceInvalid = errors.New("余额快照格式或采样时间无效")
var balanceDecimal = regexp.MustCompile(`^-?\d{1,15}(?:\.\d{1,8})?$`)

type IntegrationBalanceInput struct {
	SourceAccountID int64  `json:"source_account_id"`
	Amount          string `json:"amount"`
	Quota           string `json:"quota"`
	ObservedAt      string `json:"observed_at"`
}

type IntegrationBalanceSnapshot struct {
	Version         int    `json:"schema_version"`
	Source          string `json:"source"`
	AccountID       int64  `json:"account_id"`
	Platform        string `json:"platform"`
	Type            string `json:"type"`
	BaseURL         string `json:"base_url"`
	SourceAccountID int64  `json:"source_account_id"`
	Amount          string `json:"amount"`
	Quota           string `json:"quota"`
	ObservedAt      string `json:"observed_at"`
}

type storedIntegrationBalance struct {
	IntegrationBalanceSnapshot
	Identity string `json:"identity"`
}

func integrationBalanceIdentity(a *Account) string {
	value, _ := json.Marshal(struct {
		ID             int64
		Platform, Type string
		Parent         *int64
		Credentials    map[string]any
	}{a.ID, a.Platform, a.Type, a.ParentAccountID, a.Credentials})
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// AccountIntegrationBalance keeps "legacy" distinct from invalid managed data.
// Invalid managed data must never fall back to an older editable note.
func AccountIntegrationBalance(a *Account) (*IntegrationBalanceSnapshot, string) {
	if a == nil {
		return nil, "invalid"
	}
	raw, managed := a.Extra[IntegrationBalanceKey]
	if !managed {
		return nil, "legacy"
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, "invalid"
	}
	var value storedIntegrationBalance
	baseURL, _ := a.Credentials["base_url"].(string)
	if json.Unmarshal(encoded, &value) != nil || value.Version != 1 || value.Source != "metapi" ||
		value.Identity != integrationBalanceIdentity(a) || a.Type != AccountTypeAPIKey ||
		value.AccountID != a.ID || value.Platform != a.Platform || value.Type != a.Type || value.BaseURL != baseURL || value.SourceAccountID < 1 ||
		!balanceDecimal.MatchString(value.Amount) || !balanceDecimal.MatchString(value.Quota) {
		return nil, "invalid"
	}
	observed, err := time.Parse(time.RFC3339Nano, value.ObservedAt)
	if err != nil || observed.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || observed.After(time.Now().Add(IntegrationBalanceMaxClockSkew)) {
		return nil, "invalid"
	}
	return &value.IntegrationBalanceSnapshot, "valid"
}

func prepareIntegrationBalance(a *Account, input *IntegrationBalanceInput) (any, error) {
	if a.ParentAccountID != nil || input.SourceAccountID < 1 || !balanceDecimal.MatchString(input.Amount) || !balanceDecimal.MatchString(input.Quota) {
		return nil, ErrIntegrationBalanceInvalid
	}
	amount, _ := decimal.NewFromString(input.Amount)
	quota, _ := decimal.NewFromString(input.Quota)
	canonical := *input
	canonical.Amount, canonical.Quota = amount.String(), quota.String()
	input = &canonical
	observed, err := time.Parse(time.RFC3339Nano, input.ObservedAt)
	if err != nil || observed.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || observed.After(time.Now().Add(IntegrationBalanceMaxClockSkew)) {
		return nil, ErrIntegrationBalanceInvalid
	}
	if previous, state := AccountIntegrationBalance(a); state == "valid" {
		at, _ := time.Parse(time.RFC3339Nano, previous.ObservedAt)
		if observed.Before(at) || observed.Equal(at) && (previous.Amount != input.Amount || previous.Quota != input.Quota || previous.SourceAccountID != input.SourceAccountID) {
			return nil, ErrAccountNotesConflict
		}
	}
	baseURL, _ := a.Credentials["base_url"].(string)
	stored := storedIntegrationBalance{IntegrationBalanceSnapshot: IntegrationBalanceSnapshot{
		Version: 1, Source: "metapi", AccountID: a.ID, Platform: a.Platform, Type: a.Type, BaseURL: baseURL,
		SourceAccountID: input.SourceAccountID, Amount: input.Amount, Quota: input.Quota, ObservedAt: observed.UTC().Format(time.RFC3339Nano),
	}, Identity: integrationBalanceIdentity(a)}
	// Match repository JSON hydration so the write response and the next read
	// hash identical field order in the conditional-write revision.
	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	return object, nil
}
