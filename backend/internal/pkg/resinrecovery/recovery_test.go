package resinrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recoveryRoundTripper func(*http.Request) (*http.Response, error)

func (f recoveryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type recoveryHarness struct {
	m             *Manager
	mu            sync.Mutex
	current       lease
	acquires      int
	reported      []map[string]string
	failAcquire   bool
	reportStatus  string
	reportStarted chan struct{}
	reportRelease <-chan struct{}
	clock         atomic.Int64
}

func newRecoveryHarness(t *testing.T, threshold int) *recoveryHarness {
	t.Helper()
	h := &recoveryHarness{current: lease{NodeHash: strings.Repeat("1", 32), CreatedAtNs: "1790000000000000001", EgressIP: "198.51.100.1"}}
	h.clock.Store(time.Now().UnixNano())
	h.m = New(Config{Enabled: true, ProxyEndpoints: []string{"proxy.test:10834"}, FailureThreshold: threshold, WindowSeconds: 60})
	h.m.now = func() time.Time { return time.Unix(0, h.clock.Load()) }
	h.m.client.Transport = recoveryRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.String(), "test-secret") || r.Header.Get("Authorization") != "Bearer test-secret" || r.Method != http.MethodPost {
			t.Error("control request violated authentication contract")
			return nil, errors.New("bad test control request")
		}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 8 || parts[1] != "proxy-api" || parts[2] != "v1" || parts[4] != "leases" || parts[6] != "actions" {
			t.Errorf("unexpected control path: %s", r.URL.Path)
			return nil, errors.New("unexpected control path")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		h.mu.Lock()
		result := controlResponse{RecoveryVersion: 1, Status: "available"}
		switch parts[7] {
		case "acquire":
			h.acquires++
			if h.failAcquire {
				h.mu.Unlock()
				return nil, errors.New("synthetic unavailable control")
			}
		case "report-failure":
			h.reported = append(h.reported, body)
			result.Status = h.reportStatus
			if result.Status == "" {
				result.Status = "rotated"
			}
			if body["expected_node_hash"] != h.current.NodeHash || body["expected_created_at_ns"] != h.current.CreatedAtNs {
				result.Status = "stale_lease"
			}
			started, release := h.reportStarted, h.reportRelease
			h.reportStarted = nil
			h.mu.Unlock()
			if started != nil {
				close(started)
			}
			if release != nil {
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			h.mu.Lock()
			if result.Status == "rotated" {
				h.current.NodeHash = strings.Repeat("2", 32)
				h.current.CreatedAtNs = "1790000000000000002"
				h.current.EgressIP = "198.51.100.2"
			}
		default:
			h.mu.Unlock()
			return nil, errors.New("business request replayed through control")
		}
		value := h.current
		value.Account = parts[5]
		result.Lease = &value
		h.mu.Unlock()
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	})
	t.Cleanup(func() { h.m.reports.Wait(); h.m.client.CloseIdleConnections() })
	return h
}

func (h *recoveryHarness) prepare(t *testing.T, ctx context.Context, account, target string) *Attempt {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	a := h.m.Prepare(ctx, "socks5h://Global."+account+":test-secret@proxy.test:10834", u)
	if a == nil {
		t.Fatal("expected a managed attempt")
	}
	return a
}

func (h *recoveryHarness) reportCount() int {
	h.m.reports.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reported)
}

func TestRecoveryThresholdAndNewGeneration(t *testing.T) {
	h := newRecoveryHarness(t, 3)
	var oldScope string
	var retired atomic.Bool
	for i := range 3 {
		a := h.prepare(t, context.Background(), "sub2-test", "https://example.com/v1/responses")
		oldScope = a.Scope()
		a.OnRetire(func(scope string) { retired.Store(scope == oldScope) })
		a.Complete(EmptyStream)
		a.Complete(EmptyStream)
		if got := h.reportCount(); got != (i+1)/3 {
			t.Fatalf("reports=%d after %d failures", got, i+1)
		}
	}
	if !retired.Load() {
		t.Fatal("old transport was not retired")
	}
	if h.reported[0]["expected_created_at_ns"] != "1790000000000000001" || h.reported[0]["target_host"] != "example.com:443" {
		t.Fatalf("CAS precision/target lost: %v", h.reported[0])
	}
	next := h.prepare(t, context.Background(), "sub2-test", "https://example.com/v1/responses")
	if next.Scope() == oldScope {
		t.Fatal("replacement reused old connection scope")
	}
	if h.acquires != 4 {
		t.Fatalf("unexpected replay/acquire count: %d", h.acquires)
	}
}

func TestRecoveryConcurrentFailuresCollapseAndSuccessIsBarrier(t *testing.T) {
	h := newRecoveryHarness(t, 3)
	var attempts []*Attempt
	for range 8 {
		attempts = append(attempts, h.prepare(t, context.Background(), "account", "https://example.com/responses"))
	}
	var wg sync.WaitGroup
	for _, a := range attempts {
		wg.Add(1)
		go func() { defer wg.Done(); a.Complete(InvalidStream) }()
	}
	wg.Wait()
	if h.reportCount() != 0 || len(h.m.entries[attempts[0].Scope()].failures) != 1 {
		t.Fatal("one concurrent incident became a failure burst")
	}
	late := h.prepare(t, context.Background(), "account", "https://example.com/responses")
	success := h.prepare(t, context.Background(), "account", "https://example.com/responses")
	success.Complete(Reachable)
	late.Complete(EmptyStream)
	if len(h.m.entries[success.Scope()].failures) != 0 {
		t.Fatal("late failure crossed the success barrier")
	}
	for range 3 {
		h.prepare(t, context.Background(), "account", "https://example.com/responses").Complete(EmptyStream)
	}
	if h.reportCount() != 1 {
		t.Fatal("sequential fast failures failed to reach threshold")
	}
}

func TestRecoveryTargetAccountAndABAGenerationIsolation(t *testing.T) {
	h := newRecoveryHarness(t, 20)
	old := h.prepare(t, context.Background(), "parent", "https://a.example/responses")
	oldLate := h.prepare(t, context.Background(), "parent", "https://a.example/responses")
	old.Complete(EmptyStream)
	otherTarget := h.prepare(t, context.Background(), "parent", "https://b.example/responses")
	otherTarget.Complete(Reachable)
	child := h.prepare(t, context.Background(), "parent-child", "https://a.example/responses")
	if otherTarget.Scope() == old.Scope() || child.Identity() == old.Identity() {
		t.Fatal("target or expanded account was not isolated")
	}
	if len(h.m.entries[old.Scope()].failures) != 1 {
		t.Fatal("other target cleared failures")
	}
	h.mu.Lock()
	h.current.NodeHash = strings.Repeat("2", 32)
	h.current.CreatedAtNs = "1790000000000000002"
	h.mu.Unlock()
	b := h.prepare(t, context.Background(), "parent", "https://a.example/responses")
	h.mu.Lock()
	h.current.NodeHash = strings.Repeat("1", 32)
	h.current.CreatedAtNs = "1790000000000000003"
	h.mu.Unlock()
	newA := h.prepare(t, context.Background(), "parent", "https://a.example/responses")
	if old.Scope() == newA.Scope() || b.Scope() == newA.Scope() {
		t.Fatal("A-B-A reused a generation")
	}
	newA.Complete(EmptyStream)
	oldLate.Complete(Reachable)
	if len(h.m.entries[newA.Scope()].failures) != 1 {
		t.Fatal("old generation cleared new failures")
	}
}

func TestRecoveryWindowExpiresAndNoAlternativeBacksOff(t *testing.T) {
	h := newRecoveryHarness(t, 2)
	h.reportStatus = "no_alternative"
	h.prepare(t, context.Background(), "account", "https://example.com/responses").Complete(EmptyStream)
	h.clock.Add(int64(61 * time.Second))
	h.prepare(t, context.Background(), "account", "https://example.com/responses").Complete(EmptyStream)
	if h.reportCount() != 0 {
		t.Fatal("expired failures counted")
	}
	h.prepare(t, context.Background(), "account", "https://example.com/responses").Complete(EmptyStream)
	if h.reportCount() != 1 {
		t.Fatal("expected first report")
	}
	for range 3 {
		h.prepare(t, context.Background(), "account", "https://example.com/responses").Complete(EmptyStream)
	}
	if h.reportCount() != 1 {
		t.Fatal("no-alternative report storm")
	}
	h.clock.Add(int64(31 * time.Second))
	h.prepare(t, context.Background(), "account", "https://example.com/responses").Complete(EmptyStream)
	if h.reportCount() != 2 {
		t.Fatal("recovery never retried after backoff")
	}
}

func TestRecoveryReportIsAsyncAndSurvivesHandlerTeardown(t *testing.T) {
	h := newRecoveryHarness(t, 1)
	release := make(chan struct{})
	started := make(chan struct{})
	h.reportRelease, h.reportStarted = release, started
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := h.prepare(t, ctx, "account", "https://example.com/responses")
	done := make(chan struct{})
	go func() { a.Complete(EmptyStream); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failure report blocked the business response")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("report did not start")
	}
	cancel()
	releaseOnce.Do(func() { close(release) })
	if h.reportCount() != 1 || !h.m.entries[a.Scope()].retired {
		t.Fatal("handler cancellation aborted the final verdict")
	}
}

func TestRecoveryUnavailableControlDoesNotGuessLease(t *testing.T) {
	h := newRecoveryHarness(t, 3)
	h.failAcquire = true
	target, _ := url.Parse("https://example.com/responses")
	proxy := "socks5h://Global.account:test-secret@proxy.test:10834"
	for range 3 {
		if h.m.Prepare(context.Background(), proxy, target) != nil {
			t.Fatal("guessed a lease without metadata")
		}
	}
	if h.acquires != 1 {
		t.Fatal("unavailable control did not back off")
	}
	h.clock.Add(int64(31 * time.Second))
	h.failAcquire = false
	if h.m.Prepare(context.Background(), proxy, target) == nil {
		t.Fatal("control did not recover")
	}
	for _, raw := range []string{"", "http://Global.account:test-secret@proxy.test:10834", "socks5h://Global.account:test-secret@other.test:10834", "socks5h://Global.account~r1~bad:test-secret@proxy.test:10834"} {
		if h.m.Prepare(context.Background(), raw, target) != nil {
			t.Fatalf("managed unsupported proxy: %s", raw)
		}
	}
	if New(Config{}) != nil {
		t.Fatal("recovery enabled by default")
	}
}

func TestRecoveryCancellationUnreadBodyAndBounds(t *testing.T) {
	h := newRecoveryHarness(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	a := h.prepare(t, ctx, "account", "https://example.com/responses")
	cancel()
	a.Complete(TransportError)
	if len(h.m.entries) != 0 {
		t.Fatal("client cancellation counted")
	}
	a = h.prepare(t, context.Background(), "account", "https://example.com/responses")
	_ = BindBody(io.NopCloser(strings.NewReader("unread")), a).Close()
	if len(h.m.entries) != 0 {
		t.Fatal("body close counted as failure")
	}
	now := h.m.now()
	for i := range maxEntries {
		h.m.entries[fmt.Sprint(i)] = &entry{reporting: true, touched: now}
	}
	a.Complete(EmptyStream)
	if len(h.m.entries) != maxEntries {
		t.Fatal("entries exceeded hard cap")
	}
	h.m.entries["0"].reporting = false
	h.prepare(t, context.Background(), "account", "https://example.com/responses").Complete(EmptyStream)
	if len(h.m.entries) != maxEntries || h.m.entries["0"] != nil {
		t.Fatal("bounded eviction failed")
	}
}

func TestRecoveryWrappedBodyUsesConsumerCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("consumer-canceled-%t", canceled), func(t *testing.T) {
			h := newRecoveryHarness(t, 3)
			upstreamCtx, closeUpstream := context.WithCancel(context.Background())
			defer closeUpstream()
			a := h.prepare(t, upstreamCtx, "account", "https://example.com/responses")
			source := BindBody(io.NopCloser(strings.NewReader("")), a)
			body := PreserveBodyObservation(io.NopCloser(strings.NewReader("")), source)
			body = PreserveBodyObservation(io.NopCloser(strings.NewReader("")), body)
			if !IsManaged(body) {
				t.Fatal("wrappers lost the observation")
			}
			// A transformer can close the private upstream context before the
			// handler classifies its final EOF.
			closeUpstream()
			consumerCtx, cancelConsumer := context.WithCancel(context.Background())
			defer cancelConsumer()
			if canceled {
				cancelConsumer()
			}
			Report(consumerCtx, body, EmptyStream)
			Report(consumerCtx, body, EmptyStream)
			e := h.m.entries[a.Scope()]
			if canceled {
				if e != nil {
					t.Fatal("consumer cancellation counted as a failed node")
				}
			} else if e == nil || len(e.failures) != 1 {
				t.Fatal("upstream cleanup discarded or duplicated the final verdict")
			}
		})
	}
	ordinary := io.NopCloser(strings.NewReader("ordinary"))
	wrapped := PreserveBodyObservation(io.NopCloser(strings.NewReader("ordinary")), ordinary)
	if IsManaged(wrapped) {
		t.Fatal("ordinary wrapper incorrectly disabled shared proxy protection")
	}
}

func TestRecoveryBusinessResponseRequiresSemanticEvidence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		media, body string
		want        Outcome
	}{
		{"quota-json", 402, "application/json", `{"error":{"message":"quota exhausted"}}`, Reachable},
		{"quota-text", 402, "text/plain", "quota exhausted", Reachable},
		{"rate-limit", 429, "", "rate limited", Reachable},
		{"bad-key", 401, "application/json", `{"error":{"message":"invalid key"}}`, Reachable},
		{"bad-request", 400, "application/problem+json", `{"error":"invalid input"}`, Reachable},
		{"gateway-json", 502, "application/json", `{"error":{"message":"bad gateway"}}`, ""},
		{"invalid-json", 403, "application/json", `{"error":`, ""},
		{"untyped-error", 403, "application/json", `{"error":true}`, ""},
		{"challenge", 403, "text/html", "<!DOCTYPE html><html>challenge</html>", InvalidStream},
		{"gateway-html", 502, "text/html", "<html>bad gateway</html>", InvalidStream},
		{"oversized", 401, "application/json", `{"error":{"message":"` + strings.Repeat("x", 17<<10) + `"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryHarness(t, 20)
			a := h.prepare(t, context.Background(), "account", "https://example.com/responses")
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.media}}, Body: BindBody(io.NopCloser(strings.NewReader(tc.body)), a)}
			ReportBusinessResponse(resp)
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(data) != tc.body {
				t.Fatal("observer changed response bytes")
			}
			e := h.m.entries[a.Scope()]
			if tc.want == "" {
				if e != nil {
					t.Fatal("untrusted response cleared or recorded failures")
				}
				return
			}
			if e == nil {
				t.Fatal("missing verdict")
			}
			if tc.want == Reachable && (e.successSequence == 0 || len(e.failures) != 0) {
				t.Fatal("business response not reachable")
			}
			if tc.want == InvalidStream && len(e.failures) != 1 {
				t.Fatal("HTML challenge not classified")
			}
		})
	}
}

func TestRecoveryLeaseValidationRequiresPreciseGeneration(t *testing.T) {
	good := lease{Account: "account", NodeHash: strings.Repeat("a", 32), CreatedAtNs: "1790000000000000001", EgressIP: "198.51.100.1"}
	if !validLease(&good, "account") {
		t.Fatal("valid lease rejected")
	}
	for _, generation := range []string{"", "0", "-1", "1.79e18", "1790000000000000001.0"} {
		value := good
		value.CreatedAtNs = generation
		if validLease(&value, "account") {
			t.Fatalf("invalid generation accepted: %s", generation)
		}
	}
	if validLease(&good, "other") || validLease(nil, "account") {
		t.Fatal("wrong identity accepted")
	}
}
