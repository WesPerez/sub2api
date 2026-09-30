//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestScheduledRecoveryCASRejectsRepurposedAccounts(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	r := newAccountRepositoryWithSQL(client, integrationDB, nil)
	account := &service.Account{Name: fmt.Sprintf("recovery-cas-%d-gpt", time.Now().UnixNano()), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusError, Schedulable: false, Credentials: map[string]any{"base_url": "https://agentrouter.org/v1", "api_key": "synthetic-secret"}, Extra: map[string]any{}, Concurrency: 1, Priority: 1}
	require.NoError(t, r.Create(ctx, account))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id=$1`, account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, account.ID)
	})
	snapshot, err := r.GetByID(ctx, account.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET name=name||'-edited' WHERE id=$1`, account.ID)
	require.NoError(t, err)
	changed, err := r.ApplyScheduledRecovery(ctx, snapshot, true)
	require.NoError(t, err)
	require.False(t, changed)
	current, err := r.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.False(t, current.Schedulable)
	require.Equal(t, service.StatusError, current.Status)
	changed, err = r.ApplyScheduledRecovery(ctx, current, true)
	require.NoError(t, err)
	require.True(t, changed)
	updated, err := r.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, updated.Schedulable)
	require.Equal(t, service.StatusActive, updated.Status)
	require.Equal(t, current.Name, updated.Name)
	require.Equal(t, current.Credentials, updated.Credentials)
	changed, err = r.ApplyScheduledRecovery(ctx, current, false)
	require.NoError(t, err)
	require.False(t, changed, "stale observed status must not disable a recovered account")
}

func TestScheduledRecoveryClearsAllStateWhileKeepingSchedulingDisabled(t *testing.T) {
	ctx := context.Background()
	r := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	a := &service.Account{Name: fmt.Sprintf("recovery-reset-%d-gpt", time.Now().UnixNano()), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusError, Schedulable: true, Credentials: map[string]any{"base_url": "https://agentrouter.org/v1", "api_key": "synthetic-secret"}, Extra: map[string]any{}, Concurrency: 1, Priority: 1}
	require.NoError(t, r.Create(ctx, a))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id=$1`, a.ID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, a.ID)
	})
	_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET error_message='old quota error',rate_limited_at=NOW(),rate_limit_reset_at=NOW()+INTERVAL '1 hour',
		overload_until=NOW()+INTERVAL '1 hour',temp_unschedulable_until=NOW()+INTERVAL '1 hour',temp_unschedulable_reason='old 403',
		extra='{"keep":"unrelated","model_rate_limits":{"gpt":{}},"antigravity_quota_scopes":{"scope":{}},"integration_balance_v1":{"amount":"5"}}'::jsonb WHERE id=$1`, a.ID)
	require.NoError(t, err)
	expected, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	// A concurrent balance refresh must not leave an old error behind during reset.
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET notes='fresh notes',extra=jsonb_set(extra,'{integration_balance_v1,amount}','"6"'::jsonb) WHERE id=$1`, a.ID)
	require.NoError(t, err)
	changed, err := r.ApplyScheduledRecovery(ctx, expected, false)
	require.NoError(t, err)
	require.True(t, changed)
	current, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, current.Status)
	require.False(t, current.Schedulable)
	require.Empty(t, current.ErrorMessage)
	require.Nil(t, current.RateLimitedAt)
	require.Nil(t, current.RateLimitResetAt)
	require.Nil(t, current.OverloadUntil)
	require.Nil(t, current.TempUnschedulableUntil)
	require.Empty(t, current.TempUnschedulableReason)
	require.NotContains(t, current.Extra, "model_rate_limits")
	require.NotContains(t, current.Extra, "antigravity_quota_scopes")
	require.Equal(t, "unrelated", current.Extra["keep"])
	require.Equal(t, "6", current.Extra[service.IntegrationBalanceKey].(map[string]any)["amount"])
	require.Equal(t, "fresh notes", *current.Notes)
	require.Equal(t, expected.Credentials, current.Credentials)
	var outbox int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, a.ID).Scan(&outbox))
	require.Positive(t, outbox)
	// A late in-flight failure between read and enable must not be erased.
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET rate_limit_reset_at=NOW()+INTERVAL '1 hour' WHERE id=$1`, a.ID)
	require.NoError(t, err)
	changed, err = r.ApplyScheduledRecovery(ctx, current, true)
	require.NoError(t, err)
	require.False(t, changed)
}
