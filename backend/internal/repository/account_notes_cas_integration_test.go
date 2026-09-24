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

func TestNotesCASPreservesConcurrentEditsAndAccountIdentity(t *testing.T) {
	ctx := context.Background()
	r := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	old, userEdit, synced := "old", "edited by user", "balance snapshot"
	a := &service.Account{Name: fmt.Sprintf("notes-cas-%d", time.Now().UnixNano()), Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Notes: &old, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"base_url": "https://example.test", "api_key": "synthetic-secret"}, Extra: map[string]any{}, Concurrency: 1, Priority: 1}
	require.NoError(t, r.Create(ctx, a))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id=$1`, a.ID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, a.ID)
	})
	snapshot, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET notes=$2 WHERE id=$1`, a.ID, userEdit)
	require.NoError(t, err)
	changed, err := r.UpdateNotesIfUnchanged(ctx, snapshot, &synced)
	require.NoError(t, err)
	require.False(t, changed)
	current, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, &userEdit, current.Notes)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET name=name||'-renamed' WHERE id=$1`, a.ID)
	require.NoError(t, err)
	changed, err = r.UpdateNotesIfUnchanged(ctx, current, &synced)
	require.NoError(t, err)
	require.False(t, changed)
	current, err = r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	changed, err = r.UpdateNotesIfUnchanged(ctx, current, &synced)
	require.NoError(t, err)
	require.True(t, changed)
	updated, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, &synced, updated.Notes)
	require.Equal(t, current.Credentials, updated.Credentials)
	require.Equal(t, current.Name, updated.Name)
	require.True(t, updated.Schedulable)
}

func TestBalanceAndNotesCommitTogetherAndRejectConcurrentSnapshot(t *testing.T) {
	ctx := context.Background()
	r := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	notes := "original note"
	a := &service.Account{Name: fmt.Sprintf("balance-cas-%d", time.Now().UnixNano()), Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Notes: &notes, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"base_url": "https://example.test", "api_key": "fixture"}, Extra: map[string]any{"keep": "unrelated"}, Concurrency: 1, Priority: 1}
	require.NoError(t, r.Create(ctx, a))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id=$1`, a.ID)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, a.ID)
	})
	svc := service.NewAccountNotesService(r)
	view, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	input := &service.IntegrationBalanceInput{SourceAccountID: 8, Amount: "5.50", Quota: "30", ObservedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
	next := "human-readable balance"
	updated, err := svc.UpdateWithBalance(ctx, a.ID, view.Revision, &next, input)
	require.NoError(t, err)
	require.Equal(t, "5.5", updated.BalanceSnapshot.Amount)
	reread, err := svc.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, updated.Revision, reread.Revision)
	duplicate, err := svc.UpdateWithBalance(ctx, a.ID, updated.Revision, &next, input)
	require.NoError(t, err)
	require.Equal(t, updated.Revision, duplicate.Revision)
	current, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "unrelated", current.Extra["keep"])
	require.Equal(t, &next, current.Notes)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{integration_balance_v1,amount}','"9"'::jsonb) WHERE id=$1`, a.ID)
	require.NoError(t, err)
	changed, err := r.UpdateNotesAndBalanceIfUnchanged(ctx, current, &notes, current.Extra[service.IntegrationBalanceKey])
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = r.ApplyScheduledRecovery(ctx, current, false)
	require.NoError(t, err)
	require.False(t, changed)
	final, err := r.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, final.Schedulable)
	require.Equal(t, &next, final.Notes)
}
