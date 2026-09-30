package dto

import "testing"

func TestRetiredBalanceMetadataIsNotReturned(t *testing.T) {
	out := redactAccountManagedExtra(map[string]any{"integration_balance_v1": map[string]any{"identity": "private"}, "keep": true})
	if _, ok := out["integration_balance_v1"]; ok {
		t.Fatal("retired private metadata exposed")
	}
	if out["keep"] != true {
		t.Fatal("unrelated extra removed")
	}
}
