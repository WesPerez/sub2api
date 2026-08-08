//go:build unit

package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDecodeCachedAccountBindsProxyIdentity(t *testing.T) {
	parentID := int64(42)
	payload, err := json.Marshal(service.Account{
		ID:              99,
		ParentAccountID: &parentID,
		Proxy: &service.Proxy{
			ID:       7,
			Protocol: "socks5h",
			Host:     "172.17.0.1",
			Port:     10834,
			Username: "OpenAI.sub2-{{account_id}}",
			Password: "secret",
		},
	})
	require.NoError(t, err)

	account, err := decodeCachedAccount(payload)
	require.NoError(t, err)
	require.Equal(t, "OpenAI.sub2-42", account.Proxy.Username)
	require.Equal(t, "socks5h://OpenAI.sub2-42:secret@172.17.0.1:10834", account.Proxy.URL())
}
