package repository

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/resinrecovery"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type resinTestTransport struct {
	requests   atomic.Int64
	idleCloses atomic.Int64
}

func (t *resinTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.requests.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func (t *resinTestTransport) CloseIdleConnections() { t.idleCloses.Add(1) }

func TestResinRecoveryOrdinaryAndTLSCacheIsolation(t *testing.T) {
	s, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
	require.True(t, ok)
	proxy := "socks5h://Global.account:test-secret@127.0.0.1:1080"
	a := upstreamRecoveryScope{identity: "account-origin", fingerprint: "generation-a"}
	b := upstreamRecoveryScope{identity: "account-origin", fingerprint: "generation-b"}
	for _, tls := range []bool{false, true} {
		get := func(scope upstreamRecoveryScope) *upstreamClientEntry {
			var value *upstreamClientEntry
			var err error
			if tls {
				value, err = s.getClientEntryWithTLS(proxy, 1, 1, &tlsfingerprint.Profile{Name: "test"}, service.HTTPUpstreamProfileOpenAI, false, false, scope)
			} else {
				value, err = s.getClientEntry(proxy, 1, 1, service.HTTPUpstreamProfileOpenAI, false, false, scope)
			}
			require.NoError(t, err)
			return value
		}
		first := get(a)
		require.Same(t, first, get(a))
		require.NotSame(t, first, get(b), "new lease must not reuse the old transport")
	}
	require.Len(t, s.clients, 4, "TLS and normal clients must stay separately keyed")
	for key, entry := range s.clients {
		s.removeClientLocked(key, entry)
	}
}

func TestResinRecoveryRetiresIdleConnectionsWithoutLosingActiveReferences(t *testing.T) {
	s, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
	require.True(t, ok)
	oldTransport, newTransport, otherTransport := &resinTestTransport{}, &resinTestTransport{}, &resinTestTransport{}
	old := &upstreamClientEntry{client: &http.Client{Transport: oldTransport}, recoveryScope: upstreamRecoveryScope{"account", "old"}}
	atomic.StoreInt64(&old.inFlight, 1)
	s.clients["old"] = old
	s.clients["new"] = &upstreamClientEntry{client: &http.Client{Transport: newTransport}, recoveryScope: upstreamRecoveryScope{"account", "new"}}
	s.clients["child"] = &upstreamClientEntry{client: &http.Client{Transport: otherTransport}, recoveryScope: upstreamRecoveryScope{"child", "old"}}
	s.retireResinClients("account", "new", false)
	require.Len(t, s.clients, 2)
	require.Equal(t, int64(1), oldTransport.idleCloses.Load())
	require.Zero(t, newTransport.idleCloses.Load())
	require.Zero(t, otherTransport.idleCloses.Load())
	require.Equal(t, int64(1), atomic.LoadInt64(&old.inFlight), "active requests retain their entry reference")
	s.retireResinClients("account", "old", true)
	require.Len(t, s.clients, 2, "late retirement must not remove the new generation")
}

func TestResinRecoveryHTTPAndTLSShareFailureThreshold(t *testing.T) {
	reported := make(chan map[string]string, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" || strings.Contains(r.URL.String(), "test-secret") {
			t.Error("credential transport contract violated")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		status := "available"
		if strings.HasSuffix(r.URL.Path, "/report-failure") {
			var value map[string]string
			if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
				t.Error(err)
			}
			reported <- value
			status = "rotated"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"recovery_version": 1, "status": status, "lease": map[string]string{
			"account": "sub2-test", "node_hash": strings.Repeat("1", 32), "created_at_ns": "1790000000000000001", "egress_ip": "198.51.100.1",
		}})
	}))
	defer control.Close()
	u, err := url.Parse(control.URL)
	require.NoError(t, err)
	proxy := "socks5h://Global.sub2-test:test-secret@" + u.Host
	s, ok := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{ResinRecovery: config.GatewayResinRecoveryConfig{
		Enabled: true, ProxyEndpoints: []string{u.Host}, FailureThreshold: 3, WindowSeconds: 60,
	}}}).(*httpUpstreamService)
	require.True(t, ok)
	ctx := service.WithHTTPUpstreamProfile(context.Background(), service.HTTPUpstreamProfileOpenAI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.test/v1/responses", strings.NewReader(`{"stream":true}`))
	require.NoError(t, err)
	attempt := s.prepareResinRecovery(req, proxy, service.HTTPUpstreamProfileOpenAI)
	require.NotNil(t, attempt)
	scope := recoveryScopeFor(attempt)
	profile := &tlsfingerprint.Profile{Name: "test"}
	plain, err := s.getClientEntry(proxy, 1, 1, service.HTTPUpstreamProfileOpenAI, false, false, scope)
	require.NoError(t, err)
	fingerprint, err := s.getClientEntryWithTLS(proxy, 1, 1, profile, service.HTTPUpstreamProfileOpenAI, false, false, scope)
	require.NoError(t, err)
	plainTransport, tlsTransport := &resinTestTransport{}, &resinTestTransport{}
	plain.client.Transport, fingerprint.client.Transport = plainTransport, tlsTransport
	for i := range 3 {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL.String(), strings.NewReader(`{"stream":true}`))
		require.NoError(t, err)
		var resp *http.Response
		if i == 1 {
			resp, err = s.DoWithTLS(request, proxy, 1, 1, profile)
		} else {
			resp, err = s.Do(request, proxy, 1, 1)
		}
		require.NoError(t, err)
		require.True(t, resinrecovery.IsManaged(resp.Body))
		resinrecovery.Report(ctx, resp.Body, resinrecovery.EmptyStream)
		require.NoError(t, resp.Body.Close())
	}
	select {
	case value := <-reported:
		require.Equal(t, "empty_stream", value["reason"])
		require.Equal(t, "1790000000000000001", value["expected_created_at_ns"])
	case <-time.After(3 * time.Second):
		t.Fatal("failure threshold never reached Resin")
	}
	require.Eventually(t, func() bool { s.mu.RLock(); defer s.mu.RUnlock(); return len(s.clients) == 0 }, 3*time.Second, time.Millisecond)
	require.Equal(t, int64(3), plainTransport.requests.Load()+tlsTransport.requests.Load(), "recovery must not replay the business POST")
	require.Equal(t, int64(1), plainTransport.idleCloses.Load())
	require.Equal(t, int64(1), tlsTransport.idleCloses.Load())
}

func TestResinRecoveryEndpointLoggingNeverIncludesCredentials(t *testing.T) {
	require.Equal(t, "socks5h://proxy.test:10834", upstreamProxyEndpoint("socks5h://Global.account:test-secret@proxy.test:10834"))
	require.Equal(t, "invalid", upstreamProxyEndpoint("://bad:test-secret"))
	require.Equal(t, "direct", upstreamProxyEndpoint(""))
}
