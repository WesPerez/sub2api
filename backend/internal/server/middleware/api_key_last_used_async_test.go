//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSlowMetadataDoesNotBlockAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{config.RunModeSimple, config.RunModeStandard} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			key := &service.APIKey{ID: 1, Key: "slow-metadata", Status: service.StatusActive,
				User: &service.User{ID: 1, Status: service.StatusActive, Balance: 10}}
			repo := &stubApiKeyRepo{
				getByKey: func(context.Context, string) (*service.APIKey, error) { clone := *key; return &clone, nil },
				updateLastUsed: func(context.Context, int64, time.Time) error {
					close(entered)
					<-release
					return nil
				},
			}
			cfg := &config.Config{RunMode: mode}
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			router := newAuthTestRouter(svc, nil, cfg)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/t", nil)
			request.Header.Set("x-api-key", key.Key)
			done := make(chan struct{})
			go func() { router.ServeHTTP(response, request); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("authenticated request waited for metadata write")
			}
			require.Equal(t, http.StatusOK, response.Code)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("metadata update was dropped instead of scheduled")
			}
		})
	}
}
