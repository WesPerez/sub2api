package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Frozen pre-optimization algorithm: the differential oracle must not delegate
// to the new shared transformation.
func legacyCodexBootstrapOracle(body []byte, isCandidate func(map[string]any) bool, allowHistoricalContext bool) ([]byte, bool) {
	if !hasUniqueJSONMembers(body) {
		return body, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request map[string]any
	if err := decoder.Decode(&request); err != nil {
		return body, false
	}
	if previousResponseID, exists := request["previous_response_id"]; exists {
		value, ok := previousResponseID.(string)
		if !ok || (!allowHistoricalContext && strings.TrimSpace(value) != "") {
			return body, false
		}
	}
	input, ok := request["input"].([]any)
	if !ok {
		return body, false
	}

	// Responses built-ins follow the *_call / *_call_output naming convention,
	// so classify by the wire type shape instead of maintaining an incomplete
	// allowlist. Delegation may coexist with historical anchors only when their
	// IDs make them unambiguous; automation retains the bootstrap-only boundary.
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := stringField(item, "type")
		if isCandidate(item) {
			callIDValue, exists := item["call_id"]
			callID, isString := callIDValue.(string)
			if exists && (!isString || strings.TrimSpace(callID) != "") {
				return body, false
			}
			continue
		}
		if typ == "item_reference" {
			if allowHistoricalContext && strings.TrimSpace(stringField(item, "id")) != "" {
				continue
			}
			return body, false
		}
		if strings.HasSuffix(typ, "_call") || isResponsesCallOutputType(typ) {
			if allowHistoricalContext && strings.TrimSpace(stringField(item, "call_id")) != "" {
				continue
			}
			return body, false
		}
	}

	changed := false
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || !isCandidate(item) {
			continue
		}
		output, ok := item["output"].(string)
		if !ok {
			continue
		}
		input[i] = map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": output,
			}},
		}
		changed = true
	}
	if !changed {
		return body, false
	}
	normalized, err := json.Marshal(request)
	if err != nil {
		return body, false
	}
	return normalized, true
}

func legacyCodexBootstraps(body []byte) ([]byte, bool, bool) {
	first, a := legacyCodexBootstrapOracle(body, isCodexAutomationCandidate, false)
	last, d := legacyCodexBootstrapOracle(first, isCodexDelegationCandidate, true)
	return last, a, d
}

func checkCodexBootstrapEquivalent(t *testing.T, body []byte) {
	t.Helper()
	want, wa, wd := legacyCodexBootstraps(body)
	got, ga, gd := normalizeCodexBootstraps(body)
	if !bytes.Equal(want, got) || wa != ga || wd != gd {
		t.Fatalf("bootstrap mismatch for %.700s: old=%t/%t new=%t/%t", body, wa, wd, ga, gd)
	}
}

func TestCombinedCodexBootstrapDifferential(t *testing.T) {
	automation := `{"type":"function_call_output","namespace":"codex_app","name":"automation_update","output":"<heartbeat><automation_id>wiki</automation_id></heartbeat>"}`
	delegation := `{"type":"function_call_output","namespace":"codex_tui","name":"send_message_to_thread","output":"` + delegationEnvelope + `"}`
	history := []string{`null`, `42`, `"text"`, `{"type":"message","content":"hello"}`, `{"type":"function_call","call_id":"old"}`, `{"type":"function_call"}`, `{"type":"computer_call_output","call_id":"old","output":"done"}`, `{"type":"tool_search_output"}`, `{"type":"item_reference","id":"old"}`, `{"type":"item_reference"}`}
	previous := []string{"", `,"previous_response_id":""`, `,"previous_response_id":"resp_1"`, `,"previous_response_id":17`, `,"previous_response_id":null`}
	callIDs := []string{"", `,"call_id":""`, `,"call_id":"  "`, `,"call_id":"old"`, `,"call_id":4`, `,"call_id":null`}
	for _, candidate := range []string{automation, delegation} {
		for _, callID := range callIDs {
			item := strings.TrimSuffix(candidate, "}") + callID + "}"
			for _, prev := range previous {
				for _, extra := range append([]string{"", automation, delegation}, history...) {
					items := item
					if extra != "" {
						items += "," + extra
					}
					body := []byte(`{"model":"gpt-5","input":[` + items + `]` + prev + `,"numbers":[9007199254740993,1e+09,-0,0.1234567890123456789]}`)
					checkCodexBootstrapEquivalent(t, body)
					checkCodexBootstrapEquivalent(t, []byte(strings.ReplaceAll(string(body), `"type"`, `"\u0074ype"`)))
					checkCodexBootstrapEquivalent(t, []byte(strings.ReplaceAll(string(body), `"input"`, `"\u0069nput"`)))
				}
			}
		}
	}
	for _, candidate := range []string{automation, delegation} {
		for _, replacement := range [][2]string{{`"namespace"`, `"\u006eamespace"`}, {`"name"`, `"\u006eame"`}, {`codex_app`, `codex_\u0061pp`}, {`codex_tui`, `codex_\u0074ui`}, {`send_message_to_thread`, `send_message_to_\u0074hread`}, {`automation_update`, `automation_\u0075pdate`}} {
			body := []byte(`{"input":[` + strings.ReplaceAll(candidate, replacement[0], replacement[1]) + `]}`)
			checkCodexBootstrapEquivalent(t, body)
		}
	}
	edges := []string{
		`{"input":[` + delegation + `],"model":"x","model":"y"}`,
		`{"input":[` + strings.Replace(delegation, `"type":`, `"\u0074ype":"message","type":`, 1) + `]}`,
		`{"input":[` + delegation + `],"nested":{"x":1,"x":2}}`,
		`{"input":[],"input":[` + delegation + `]}`,
		`{"input":[` + delegation + `],"input":[]}`,
		`{"input":[` + delegation + `],"text":"\ud83d\ude00"}`,
		strings.ReplaceAll(`{"input":[`+delegation+`]}`, `function_call_output`, `\u0066unction_call_output`),
		`null`, `[]`, `{}`, `{"input":"text"}`, `{"input":[`, `{"input":[]} trailing`,
		`{"input":[` + delegation + `],"number":1e9999}`,
	}
	for _, body := range edges {
		checkCodexBootstrapEquivalent(t, []byte(body))
	}
	rng := rand.New(rand.NewSource(60))
	choices := append(history, automation, delegation)
	for i := 0; i < 300; i++ {
		parts := make([]string, rng.Intn(8))
		for j := range parts {
			parts[j] = choices[rng.Intn(len(choices))]
		}
		body := []byte(`{"input":[` + strings.Join(parts, ",") + `]` + previous[rng.Intn(len(previous))] + `}`)
		checkCodexBootstrapEquivalent(t, body)
	}
}

func codexBootstrapLargeHistory() []byte {
	item := `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("ordinary history ", 512) + `"}]}`
	return []byte(`{"model":"gpt-5","input":[` + strings.TrimSuffix(strings.Repeat(item+",", 256), ",") + `]}`)
}

func TestCombinedCodexBootstrapLargeHistory(t *testing.T) {
	body := codexBootstrapLargeHistory()
	checkCodexBootstrapEquivalent(t, body)
	oldAllocs := testing.AllocsPerRun(2, func() { legacyCodexBootstraps(body) })
	newAllocs := testing.AllocsPerRun(2, func() { normalizeCodexBootstraps(body) })
	if newAllocs >= oldAllocs/2 {
		t.Fatalf("allocation reduction missing: old=%.0f new=%.0f", oldAllocs, newAllocs)
	}
	start := time.Now()
	for i := 0; i < 3; i++ {
		legacyCodexBootstraps(body)
	}
	oldTime := time.Since(start)
	start = time.Now()
	for i := 0; i < 3; i++ {
		normalizeCodexBootstraps(body)
	}
	newTime := time.Since(start)
	t.Logf("bootstrap comparison bytes=%d old_allocs=%.0f new_allocs=%.0f old_3x=%s new_3x=%s", len(body), oldAllocs, newAllocs, oldTime, newTime)
}

func BenchmarkCombinedCodexBootstrap(b *testing.B) {
	body := codexBootstrapLargeHistory()
	for _, legacy := range []bool{true, false} {
		b.Run(fmt.Sprintf("legacy=%t", legacy), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				if legacy {
					legacyCodexBootstraps(body)
				} else {
					normalizeCodexBootstraps(body)
				}
			}
		})
	}
}
