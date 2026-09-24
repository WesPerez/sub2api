package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountBalanceProjectionNeverExposesPrivateStorageOrRevivesInvalidData(t *testing.T) {
	a := &service.Account{ID: 1, Type: service.AccountTypeAPIKey, Extra: map[string]any{
		service.IntegrationBalanceKey: map[string]any{"identity": "private-identity-hash", "schema_version": 99}, "keep": true,
	}}
	view := AccountFromServiceShallow(a)
	require.Equal(t, "invalid", view.BalanceState)
	require.Nil(t, view.BalanceSnapshot)
	require.Equal(t, true, view.Extra["keep"])
	encoded, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), service.IntegrationBalanceKey)
	require.NotContains(t, string(encoded), "private-identity-hash")
	require.Contains(t, a.Extra, service.IntegrationBalanceKey)
}
