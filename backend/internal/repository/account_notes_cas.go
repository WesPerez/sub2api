package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The account identity, note compare-and-swap and cache invalidation are one
// database statement. Checking again in the caller is not sufficient.
func (r *accountRepository) UpdateNotesIfUnchanged(ctx context.Context, expected *service.Account, notes *string) (bool, error) {
	return r.UpdateNotesAndBalanceIfUnchanged(ctx, expected, notes, nil)
}

func (r *accountRepository) UpdateNotesAndBalanceIfUnchanged(ctx context.Context, expected *service.Account, notes *string, balance any) (bool, error) {
	if r == nil || r.sql == nil || expected == nil {
		return false, errors.New("account notes repository unavailable")
	}
	credentials, err := json.Marshal(normalizeJSONMap(expected.Credentials))
	if err != nil {
		return false, err
	}
	var before any
	if value, present := expected.Extra[service.IntegrationBalanceKey]; present {
		encoded, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			return false, encodeErr
		}
		before = string(encoded)
	}
	var update any
	if balance != nil {
		encoded, encodeErr := json.Marshal(map[string]any{service.IntegrationBalanceKey: balance})
		if encodeErr != nil {
			return false, encodeErr
		}
		update = string(encoded)
	}
	result, err := r.sql.ExecContext(ctx, `WITH changed AS (
		UPDATE accounts AS a SET notes=$2,updated_at=NOW(),
		extra=CASE WHEN $11::jsonb IS NULL THEN a.extra ELSE COALESCE(a.extra,'{}'::jsonb) || $11::jsonb END
		WHERE a.id=$1 AND a.deleted_at IS NULL
		AND a.name=$3 AND a.platform=$4 AND a.type=$5
		AND a.credentials=$6::jsonb AND a.notes IS NOT DISTINCT FROM $7
		AND a.parent_account_id IS NOT DISTINCT FROM $8
		AND a.extra->'integration_balance_v1' IS NOT DISTINCT FROM $10::jsonb
		RETURNING a.id
	) INSERT INTO scheduler_outbox (event_type,account_id,group_id,payload)
	SELECT $9,id,NULL,NULL FROM changed`, expected.ID, notes, expected.Name, expected.Platform,
		expected.Type, string(credentials), expected.Notes, expected.ParentAccountID, service.SchedulerOutboxEventAccountChanged, before, update)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, expected.ID)
	return true, nil
}
