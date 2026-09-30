//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountSearch_PublicFieldsAndPagination(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	fixtures := []*service.Account{
		{Name: "Shared-name"},
		{Name: "site", Credentials: map[string]any{"base_url": "https://shared.example/v1"}},
		{Name: "alias", Credentials: map[string]any{"model_mapping": map[string]any{"shared-alias": "target"}}},
		{Name: "target", Credentials: map[string]any{"model_mapping": map[string]any{"alias": "SHARED-model"}}},
		{Name: "legacy", Credentials: map[string]any{"model_whitelist": []string{"shared-legacy"}}},
		{Name: "unrelated", Credentials: map[string]any{"api_key": "shared-secret", "model_mapping": []string{"shared"}, "model_whitelist": "shared"}},
		{Name: `literal%_\name`},
	}
	for _, fixture := range fixtures {
		mustCreateAccount(t, client, fixture)
	}

	for _, tc := range []struct {
		search string
		count  int
	}{
		{" SHARED ", 5}, {"shared.example", 1}, {"shared-alias", 1},
		{"shared-model", 1}, {"shared-legacy", 1}, {"shared-secret", 0},
		{"%_\\", 1}, {"missing", 0},
	} {
		t.Run(tc.search, func(t *testing.T) {
			all, err := repo.ListAllWithFilters(ctx, "", "", "", tc.search, 0, "")
			require.NoError(t, err)
			require.Len(t, all, tc.count)
			page, meta, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 1}, "", "", "", tc.search, 0, "")
			require.NoError(t, err)
			require.EqualValues(t, tc.count, meta.Total)
			require.Len(t, page, min(tc.count, 1))
			if tc.count > 1 {
				next, _, nextErr := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 2, PageSize: 1}, "", "", "", tc.search, 0, "")
				require.NoError(t, nextErr)
				require.Len(t, next, 1)
				require.NotEqual(t, page[0].ID, next[0].ID)
			}
		})
	}
}

func TestAccountSearch_UsageAggregatesErrorsAndDeletedAccounts(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "account-search@test.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-account-search", Name: "account-search"})
	accounts := []*service.Account{
		mustCreateAccount(t, client, &service.Account{Name: "site", Credentials: map[string]any{"base_url": "https://shared-search.example"}}),
		mustCreateAccount(t, client, &service.Account{Name: "model", Credentials: map[string]any{"model_mapping": map[string]any{"shared-search-model": "upstream"}}}),
		mustCreateAccount(t, client, &service.Account{Name: "unrelated"}),
	}
	now := time.Now().UTC().Truncate(time.Minute)
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	for _, account := range accounts {
		_, err := repo.Create(ctx, &service.UsageLog{
			UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, Model: "test-model",
			InputTokens: 1, OutputTokens: 1, TotalCost: 1, ActualCost: 0.5, CreatedAt: now,
		})
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO ops_error_logs
			(error_phase, error_type, severity, status_code, account_id, user_id, created_at)
			VALUES ('upstream', 'upstream_error', 'error', 500, $1, $2, $3)`, account.ID, user.ID, now)
		require.NoError(t, err)
	}

	filters := usagestats.UsageLogFilters{AccountSearch: "SHARED-search", StartTime: &start, EndTime: &end}
	logs, page, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 1}, filters)
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total)
	require.Len(t, logs, 1)
	stats, err := repo.GetStatsWithFilters(ctx, filters)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.TotalRequests)
	require.EqualValues(t, 2, stats.Endpoints[0].Requests)
	trend, err := repo.GetUsageTrendWithUsageFilters(ctx, start, end, "hour", filters)
	require.NoError(t, err)
	require.Len(t, trend, 1)
	require.EqualValues(t, 2, trend[0].Requests)
	models, err := repo.GetModelStatsWithUsageFiltersBySource(ctx, start, end, filters, usagestats.ModelSourceRequested)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.EqualValues(t, 2, models[0].Requests)
	groups, err := repo.GetGroupStatsWithUsageFilters(ctx, start, end, filters)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.EqualValues(t, 2, groups[0].Requests)
	users, err := repo.GetUserBreakdownStats(ctx, start, end, usagestats.UserBreakdownDimension{AccountSearch: filters.AccountSearch}, 20)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.EqualValues(t, 2, users[0].Requests)

	where, args := buildOpsErrorLogsWhere(&service.OpsErrorLogFilter{AccountSearch: filters.AccountSearch, UserID: &user.ID})
	rows, err := tx.QueryContext(ctx, "SELECT e.account_id FROM ops_error_logs e "+where, args...)
	require.NoError(t, err)
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, []int64{accounts[0].ID, accounts[1].ID}, ids)

	filters.AccountID = accounts[0].ID
	filters.ExactTotal = true
	logs, page, err = repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, filters)
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total)
	require.Len(t, logs, 1)
	filters.AccountSearch = "no-match"
	_, page, err = repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, filters)
	require.NoError(t, err)
	require.Zero(t, page.Total)

	require.NoError(t, client.Account.UpdateOneID(accounts[0].ID).SetDeletedAt(now).Exec(ctx))
	filters.AccountID = 0
	filters.AccountSearch = "shared-search.example"
	logs, page, err = repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, filters)
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total, "historical logs remain searchable after account deletion")
	require.Len(t, logs, 1)
	accountRepo := newAccountRepositoryWithSQL(client, tx, nil)
	visible, _, err := accountRepo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, "", "", "", filters.AccountSearch, 0, "")
	require.NoError(t, err)
	require.Empty(t, visible, "account management retains its soft-delete filter")
}
