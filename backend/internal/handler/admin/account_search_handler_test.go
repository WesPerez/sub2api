package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminUsageAccountSearchListAndStatsCache(t *testing.T) {
	repo := &adminUsageRepoCapture{}
	router := newAdminUsageRequestTypeTestRouter(repo)
	for _, keyword := range []string{"windhub", "glm-5.3"} {
		query := "?account_id=7&account_search=" + url.QueryEscape(" "+keyword+" ")
		for _, endpoint := range []string{"/admin/usage", "/admin/usage/stats"} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, endpoint+query, nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
		require.Equal(t, keyword, repo.listFilters.AccountSearch)
		require.Equal(t, keyword, repo.statsFilters.AccountSearch, "different keywords must not share a cached total")
		require.EqualValues(t, 7, repo.listFilters.AccountID)
		require.EqualValues(t, 7, repo.statsFilters.AccountID)
	}
}

func TestDashboardAccountSearchCachesAndSnapshot(t *testing.T) {
	for _, endpoint := range []string{"trend", "models", "groups", "snapshot-v2"} {
		t.Run(endpoint, func(t *testing.T) {
			repo := &dashboardUsageRepoCapture{}
			router := newDashboardRequestTypeTestRouter(repo)
			for _, keyword := range []string{endpoint + "-site", endpoint + "-model"} {
				query := "?account_search=" + url.QueryEscape(" "+keyword+" ") + "&include_stats=false&include_group_stats=true"
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/dashboard/"+endpoint+query, nil))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				if endpoint == "trend" || endpoint == "snapshot-v2" {
					require.Equal(t, keyword, repo.trendAccountSearch)
				}
				if endpoint == "models" || endpoint == "snapshot-v2" {
					require.Equal(t, keyword, repo.modelAccountSearch)
				}
				if endpoint == "groups" || endpoint == "snapshot-v2" {
					require.Equal(t, keyword, repo.groupAccountSearch)
				}
			}
		})
	}
}
