package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountNotesRequiresSpecificPrecondition(t *testing.T) {
	for _, test := range []struct {
		etag   string
		status int
	}{{"", 428}, {"*", 400}, {`W/"weak"`, 400}} {
		r := gin.New()
		h := &AccountNotesHandler{}
		r.PUT("/notes/:id", h.Update)
		request := httptest.NewRequest(http.MethodPut, "/notes/1", strings.NewReader(`{"notes":"new"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Match", test.etag)
		result := httptest.NewRecorder()
		r.ServeHTTP(result, request)
		require.Equal(t, test.status, result.Code)
	}
}

func TestRecoveryInfrastructureErrorsAreServerErrorsWithoutDetails(t *testing.T) {
	result := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(result)
	recoveryError(c, errors.New("postgres storage synthetic-secret"))
	require.Equal(t, http.StatusInternalServerError, result.Code)
	require.NotContains(t, result.Body.String(), "synthetic-secret")
	result = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(result)
	recoveryError(c, service.ErrRecoveryInvalid)
	require.Equal(t, http.StatusBadRequest, result.Code)
}
