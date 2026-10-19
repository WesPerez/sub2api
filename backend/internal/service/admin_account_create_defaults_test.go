package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildAccountForCreateDefaultsNewAccountsUnschedulable(t *testing.T) {
	account, err := buildAccountForCreate(&CreateAccountInput{
		Name:     "new account",
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "test-key",
		},
	}, nil)

	require.NoError(t, err)
	require.False(t, account.Schedulable)
}

func TestBuildAccountForCreateOpenAIAPIKeyModelDefaults(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]any
		extra       map[string]any
		wantMapping any
		wantPresent bool
	}{
		{
			name:        "missing mapping gets gpt-6.1-sol",
			credentials: map[string]any{"api_key": "test-key"},
			wantMapping: map[string]any{"gpt-6.1-sol": "gpt-6.1-sol"},
			wantPresent: true,
		},
		{
			name: "explicit custom mapping is preserved",
			credentials: map[string]any{
				"api_key":       "test-key",
				"model_mapping": map[string]any{"public-model": "upstream-model"},
			},
			wantMapping: map[string]any{"public-model": "upstream-model"},
			wantPresent: true,
		},
		{
			name: "explicit empty mapping is preserved",
			credentials: map[string]any{
				"api_key":       "test-key",
				"model_mapping": map[string]any{},
			},
			wantMapping: map[string]any{},
			wantPresent: true,
		},
		{
			name:        "passthrough skips default mapping",
			credentials: map[string]any{"api_key": "test-key"},
			extra:       map[string]any{"openai_passthrough": true},
			wantPresent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := &CreateAccountInput{
				Name:        "openai api key",
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: tt.credentials,
				Extra:       tt.extra,
			}

			account, err := buildAccountForCreate(input, tt.extra)
			require.NoError(t, err)
			require.False(t, account.Schedulable)

			mapping, present := account.Credentials["model_mapping"]
			require.Equal(t, tt.wantPresent, present)
			if tt.wantPresent {
				require.Equal(t, tt.wantMapping, mapping)
			}
		})
	}
}

func TestBuildAccountForCreateOpenAIAPIKeyModelDefaultDoesNotApplyToOAuth(t *testing.T) {
	account, err := buildAccountForCreate(&CreateAccountInput{
		Name:        "openai oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "test-refresh-token"},
	}, nil)

	require.NoError(t, err)
	require.NotContains(t, account.Credentials, "model_mapping")
	require.False(t, account.Schedulable)
}
