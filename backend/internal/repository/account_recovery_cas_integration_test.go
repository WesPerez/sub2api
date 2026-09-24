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
