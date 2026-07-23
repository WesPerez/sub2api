//go:build unit

package service

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// deterministicReader yields successive big-endian uint64 values from a counter.
type deterministicReader struct {
	n uint64
}

func (r *deterministicReader) Read(p []byte) (int, error) {
	for i := 0; i < len(p); {
		r.n++
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], r.n*0x9e3779b97f4a7c15)
		n := copy(p[i:], buf[:])
		i += n
	}
	return len(p), nil
}

func TestNewAccountTestProbeWithEntropy_Deterministic(t *testing.T) {
	t.Parallel()
	r1 := &deterministicReader{}
	r2 := &deterministicReader{}
	p1, err := NewAccountTestProbeWithEntropy(r1)
	require.NoError(t, err)
	p2, err := NewAccountTestProbeWithEntropy(r2)
	require.NoError(t, err)
	require.Equal(t, p1.Family, p2.Family)
	require.Equal(t, p1.Prompt, p2.Prompt)
	require.Equal(t, p1.Expected, p2.Expected)
	require.NotEmpty(t, p1.Prompt)
	require.False(t, regexp.MustCompile(`(?i)\b(?:hi|hello)\b`).MatchString(p1.Prompt))
	require.Contains(t, strings.ToLower(p1.Prompt), "json")
}

func TestNewAccountTestProbeWithEntropy_VariesWithEntropy(t *testing.T) {
	t.Parallel()
	p1, err := NewAccountTestProbeWithEntropy(bytes.NewReader(make([]byte, 64)))
	require.NoError(t, err)
	p2, err := NewAccountTestProbeWithEntropy(bytes.NewReader(bytes.Repeat([]byte{0xff}, 64)))
	require.NoError(t, err)
	require.NotEqual(t, p1.Prompt, p2.Prompt)
}

func TestAccountTestProbe_ValidateResponse_TolerantJSONAndStrictSemantics(t *testing.T) {
	t.Parallel()
	probe := &AccountTestProbe{
		Family: "order",
		Expected: map[string]any{
			"names": []string{"eraser", "pencil"},
			"sum":   9,
			"count": 2,
		},
	}

	okText := "Sure.\n```json\n{\"names\":[\"eraser\",\"pencil\"],\"sum\":9,\"count\":2}\n```\n"
	require.NoError(t, probe.ValidateResponse(okText))

	// Wrong semantics must fail even with valid JSON.
	err := probe.ValidateResponse(`{"names":["pencil","eraser"],"sum":9,"count":2}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "names")

	// Empty / non-JSON must fail.
	require.Error(t, probe.ValidateResponse("ok"))
	require.Error(t, probe.ValidateResponse(""))
}

func TestAccountTestProbe_InventoryFamilySemantics(t *testing.T) {
	t.Parallel()
	// Force inventory family by crafting expected values manually via builder with controlled entropy.
	// Family index 0 = inventory when first rand chooses 0.
	seed := make([]byte, 64)
	// first uint64 % 3 == 0 => inventory
	binary.BigEndian.PutUint64(seed[0:8], 0)
	// subsequent picks for fruits/counts
	for i := 8; i < len(seed); i += 8 {
		binary.BigEndian.PutUint64(seed[i:i+8], uint64(i+3))
	}
	probe, err := NewAccountTestProbeWithEntropy(bytes.NewReader(seed))
	require.NoError(t, err)
	require.Equal(t, "inventory", probe.Family)
	require.NoError(t, probe.ValidateResponse(expectedJSONForProbe(probe)))
}

func TestAccountTestProbe_TemperatureAndOrderFamilySemantics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		familyIndex uint64
	}{
		{name: "temperature", familyIndex: 1},
		{name: "order", familyIndex: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entropy := make([]byte, 128)
			binary.BigEndian.PutUint64(entropy[:8], tc.familyIndex)
			for offset := 8; offset+8 <= len(entropy); offset += 8 {
				binary.BigEndian.PutUint64(entropy[offset:offset+8], uint64(offset+3))
			}
			probe, err := NewAccountTestProbeWithEntropy(bytes.NewReader(entropy))
			require.NoError(t, err)
			require.Equal(t, tc.name, probe.Family)
			require.NoError(t, probe.ValidateResponse(expectedJSONForProbe(probe)))
			if tc.name == "temperature" {
				require.NotEqual(t, probe.Expected["hottest"], probe.Expected["coldest"])
			}
		})
	}
}

func TestAccountTestServiceFinishTextProbeRequiresContextProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

	err := (&AccountTestService{}).finishTextProbe(c, `{"sum":9}`)

	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "probe context is missing")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestServiceProviderTextStreamsValidateProbeSemantics(t *testing.T) {
	t.Parallel()
	type processor func(*AccountTestService, *gin.Context, io.Reader) error
	cases := []struct {
		name    string
		stream  func(string) string
		process processor
	}{
		{
			name: "claude",
			stream: func(text string) string {
				return "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":" + jsonStringForAccountTest(text) + "}}\n\n" +
					"data: {\"type\":\"message_stop\"}\n\n"
			},
			process: func(svc *AccountTestService, c *gin.Context, body io.Reader) error {
				return svc.processClaudeStream(c, body)
			},
		},
		{
			name: "gemini",
			stream: func(text string) string {
				return "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":" + jsonStringForAccountTest(text) + "}]},\"finishReason\":\"STOP\"}]}\n\n"
			},
			process: func(svc *AccountTestService, c *gin.Context, body io.Reader) error {
				return svc.processGeminiStream(c, body)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"_accepts_expected_json", func(t *testing.T) {
			probe := newFixedAccountTestProbe()
			c, recorder := newAccountTestProbeContext(probe)
			err := tc.process(&AccountTestService{}, c, strings.NewReader(tc.stream(expectedJSONForProbe(probe))))
			require.NoError(t, err)
			require.Contains(t, recorder.Body.String(), `"success":true`)
		})
		t.Run(tc.name+"_rejects_meaningless_output", func(t *testing.T) {
			c, recorder := newAccountTestProbeContext(newFixedAccountTestProbe())
			err := tc.process(&AccountTestService{}, c, strings.NewReader(tc.stream("ok")))
			require.Error(t, err)
			require.Contains(t, recorder.Body.String(), "semantic check failed")
			require.NotContains(t, recorder.Body.String(), `"success":true`)
		})
		t.Run(tc.name+"_requires_context_probe", func(t *testing.T) {
			c, recorder := newAccountTestProbeContext(nil)
			err := tc.process(&AccountTestService{}, c, strings.NewReader(tc.stream(`{"status":"available"}`)))
			require.Error(t, err)
			require.Contains(t, recorder.Body.String(), "probe context is missing")
			require.NotContains(t, recorder.Body.String(), `"success":true`)
		})
	}
}

func newAccountTestProbeContext(probe *AccountTestProbe) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)
	c.Request = c.Request.WithContext(withAccountTestProbe(c.Request.Context(), probe))
	return c, recorder
}

func jsonStringForAccountTest(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func newFixedAccountTestProbe() *AccountTestProbe {
	return &AccountTestProbe{
		Family: "order",
		Prompt: "fixed-account-test-probe",
		Expected: map[string]any{
			"names": []string{"eraser", "pencil"},
			"sum":   9,
			"count": 2,
		},
	}
}

func openAIProbeSuccessStream(probe *AccountTestProbe) string {
	delta, err := json.Marshal(expectedJSONForProbe(probe))
	if err != nil {
		panic(err)
	}
	return "data: {\"type\":\"response.output_text.delta\",\"delta\":" + string(delta) + "}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
}

func expectedJSONForProbe(probe *AccountTestProbe) string {
	if probe == nil {
		return "{}"
	}
	b, err := json.Marshal(probe.Expected)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func fixedAccountTestProbeFactory(probe *AccountTestProbe) func() (*AccountTestProbe, error) {
	return func() (*AccountTestProbe, error) {
		return cloneAccountTestProbeForTest(probe), nil
	}
}

func cloneAccountTestProbeForTest(probe *AccountTestProbe) *AccountTestProbe {
	if probe == nil {
		return nil
	}
	expected := make(map[string]any, len(probe.Expected))
	for key, value := range probe.Expected {
		switch typed := value.(type) {
		case []string:
			expected[key] = append([]string(nil), typed...)
		case []any:
			expected[key] = append([]any(nil), typed...)
		default:
			expected[key] = typed
		}
	}
	return &AccountTestProbe{Family: probe.Family, Prompt: probe.Prompt, Expected: expected}
}
