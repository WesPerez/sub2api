package repository

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ApplyScheduledRecovery changes runtime state and its durable invalidation in
// one statement, only while the account still matches the validated snapshot.
func (r *accountRepository) ApplyScheduledRecovery(ctx context.Context, expected *service.Account, enabled bool) (bool, error) {
	if r == nil || r.sql == nil || expected == nil {
		return false, errors.New("account recovery repository unavailable")
	}
	credentials, err := json.Marshal(normalizeJSONMap(expected.Credentials))
	if err != nil {
		return false, err
	}
	extra, err := json.Marshal(normalizeJSONMap(expected.Extra))
	if err != nil {
		return false, err
	}
	groups := append([]int64{}, expected.GroupIDs...)
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	groupJSON, err := json.Marshal(groups)
	if err != nil {
		return false, err
	}
	var balance any
	if value, present := expected.Extra[service.IntegrationBalanceKey]; present {
		encoded, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			return false, encodeErr
		}
		balance = string(encoded)
	}
	result, err := r.sql.ExecContext(ctx, `WITH changed AS (
		UPDATE accounts AS a SET schedulable=$2,
		status='active', error_message=NULL,
		rate_limited_at=NULL, rate_limit_reset_at=NULL, overload_until=NULL,
		temp_unschedulable_until=NULL, temp_unschedulable_reason=NULL,
		extra=COALESCE(a.extra,'{}'::jsonb)-'model_rate_limits'-'antigravity_quota_scopes',
		updated_at=NOW()
		WHERE a.id=$1 AND a.deleted_at IS NULL AND a.parent_account_id IS NULL
		AND a.name=$3 AND a.platform=$4 AND a.type=$5 AND a.credentials=$6::jsonb
		AND (NOT $2 OR a.notes IS NOT DISTINCT FROM $7) AND a.proxy_id IS NOT DISTINCT FROM $8
		AND a.status=$9 AND a.status IN ('active','error') AND a.schedulable=$10
		AND (NOT $2 OR a.extra->'integration_balance_v1' IS NOT DISTINCT FROM $13::jsonb)
		AND (NOT $2 OR (a.rate_limited_at IS NOT DISTINCT FROM $14 AND a.rate_limit_reset_at IS NOT DISTINCT FROM $15
		AND a.overload_until IS NOT DISTINCT FROM $16 AND a.temp_unschedulable_until IS NOT DISTINCT FROM $17
		AND COALESCE(a.temp_unschedulable_reason,'')=$18
		AND a.extra->'model_rate_limits' IS NOT DISTINCT FROM $19::jsonb->'model_rate_limits'
		AND a.extra->'antigravity_quota_scopes' IS NOT DISTINCT FROM $19::jsonb->'antigravity_quota_scopes'))
		AND COALESCE((SELECT jsonb_agg(ag.group_id ORDER BY ag.group_id) FROM account_groups ag WHERE ag.account_id=a.id),'[]'::jsonb)=$11::jsonb
		RETURNING a.id
	) INSERT INTO scheduler_outbox (event_type,account_id,group_id,payload)
	SELECT $12,id,NULL,NULL FROM changed`, expected.ID, enabled, expected.Name, expected.Platform, expected.Type, string(credentials), expected.Notes, expected.ProxyID, expected.Status, expected.Schedulable, string(groupJSON), service.SchedulerOutboxEventAccountChanged, balance,
		expected.RateLimitedAt, expected.RateLimitResetAt, expected.OverloadUntil, expected.TempUnschedulableUntil, expected.TempUnschedulableReason,
		string(extra))
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
