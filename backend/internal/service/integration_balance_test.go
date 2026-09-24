package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func (r *notesTestRepository) UpdateNotesAndBalanceIfUnchanged(_ context.Context, _ *Account, notes *string, balance any) (bool, error) {
	if r.race {
		return false, nil
	}
	r.writes++
	r.account.Notes = notes
	if r.account.Extra == nil {
		r.account.Extra = map[string]any{}
	}
	r.account.Extra[IntegrationBalanceKey] = balance
	return true, nil
}

func TestIntegrationBalanceAtomicNoteContractAndMonotonicSamples(t *testing.T) {
	ctx := context.Background()
	notes := "editable display"
	a := Account{ID: 41, Name: "ar-test-gpt", Type: AccountTypeAPIKey, Platform: PlatformOpenAI,
		Credentials: map[string]any{"base_url": "https://agentrouter.org/v1", "api_key": "fixture-secret"},
		Notes:       &notes, Extra: map[string]any{"unrelated": "keep"}}
	repo := &notesTestRepository{account: a}
	svc := NewAccountNotesService(repo)
	view, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "legacy", view.BalanceState)
	input := &IntegrationBalanceInput{SourceAccountID: 7, Amount: "10.00", Quota: "30.0", ObservedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
	updated, err := svc.UpdateWithBalance(ctx, a.ID, view.Revision, &notes, input)
	require.NoError(t, err)
	require.Equal(t, "valid", updated.BalanceState)
	require.Equal(t, "10", updated.BalanceSnapshot.Amount)
	require.Equal(t, int64(7), updated.BalanceSnapshot.SourceAccountID) // MetAPI IDs belong to a different namespace.
	require.Equal(t, "keep", repo.account.Extra["unrelated"])
	require.NotEqual(t, view.Revision, updated.Revision)
	encoded, err := json.Marshal(updated)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "fixture-secret")
	require.NotContains(t, string(encoded), "identity")
	duplicate, err := svc.UpdateWithBalance(ctx, a.ID, updated.Revision, &notes, input)
	require.NoError(t, err)
	require.Equal(t, updated.Revision, duplicate.Revision)
	older := *input
	older.ObservedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	_, err = svc.UpdateWithBalance(ctx, a.ID, duplicate.Revision, &notes, &older)
	require.ErrorIs(t, err, ErrAccountNotesConflict)
	conflict := *input
	conflict.Amount = "11"
	_, err = svc.UpdateWithBalance(ctx, a.ID, duplicate.Revision, &notes, &conflict)
	require.ErrorIs(t, err, ErrAccountNotesConflict)
	userNotes := "renamed free-form note without a balance prefix"
	current, err := svc.Update(ctx, a.ID, duplicate.Revision, &userNotes)
	require.NoError(t, err)
	require.Equal(t, "10", current.BalanceSnapshot.Amount)
	repo.account.Credentials["api_key"] = "fixture-replaced"
	invalid, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "invalid", invalid.BalanceState)
	require.Nil(t, invalid.BalanceSnapshot)
}

func TestIntegrationBalanceRejectsMalformedAndFutureSamples(t *testing.T) {
	a := &Account{ID: 1, Type: AccountTypeAPIKey}
	valid := IntegrationBalanceInput{SourceAccountID: 2, Amount: "-1.50", Quota: "30", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, amount := range []string{"NaN", "1e3", " 10", "1.123456789", "9999999999999999"} {
		input := valid
		input.Amount = amount
		_, err := prepareIntegrationBalance(a, &input)
		require.ErrorIs(t, err, ErrIntegrationBalanceInvalid)
	}
	for _, at := range []string{"2026-09-22 10:00:00", time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)} {
		input := valid
		input.ObservedAt = at
		_, err := prepareIntegrationBalance(a, &input)
		require.ErrorIs(t, err, ErrIntegrationBalanceInvalid)
	}
}

func TestRecoveryUsesManagedBalanceAndPreservesInvalidOrStaleAccounts(t *testing.T) {
	notes := "METAPI余额 99999 / 99999"
	a := Account{ID: 1, Name: "fixture-gpt", Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Status: StatusActive,
		Credentials: map[string]any{"base_url": "https://agentrouter.org/v1", "api_key": "fixture"}, Notes: &notes}
	input := IntegrationBalanceInput{SourceAccountID: 9, Amount: "5", Quota: "30", ObservedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
	stored, err := prepareIntegrationBalance(&a, &input)
	require.NoError(t, err)
	a.Extra = map[string]any{IntegrationBalanceKey: stored}
	value := recoveryAccountBalance(&a, 6)
	require.NotNil(t, value)
	require.Equal(t, "5", *value)
	before := recoveryIdentity(&a)
	input.ObservedAt = time.Now().Add(-8 * time.Hour).UTC().Format(time.RFC3339Nano)
	a.Extra = nil
	stored, err = prepareIntegrationBalance(&a, &input)
	require.NoError(t, err)
	a.Extra = map[string]any{IntegrationBalanceKey: stored}
	require.NotEqual(t, before, recoveryIdentity(&a))
	plan, err := BuildRecoveryPlan(DefaultRecoveryPolicy(), []Account{a})
	require.NoError(t, err)
	require.Equal(t, "preserve", plan.Accounts[0].Action)
	require.Nil(t, plan.Accounts[0].Balance)
	a.Credentials["api_key"] = "other"
	require.Nil(t, recoveryAccountBalance(&a, 168))
}
