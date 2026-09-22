package resinrecovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
)

type Outcome string

const (
	Reachable      Outcome = "reachable"
	EmptyStream    Outcome = "empty_stream"
	InvalidStream  Outcome = "invalid_stream"
	TransportError Outcome = "transport_error"
	maxEntries             = 4096
	entryTTL               = 15 * time.Minute
	controlTimeout         = 2 * time.Second
	reportBackoff          = 30 * time.Second
)

type Config struct {
	Enabled          bool
	ProxyEndpoints   []string
	FailureThreshold int
	WindowSeconds    int
}

type lease struct {
	Account     string `json:"account"`
	NodeHash    string `json:"node_hash"`
	CreatedAtNs string `json:"created_at_ns"`
	EgressIP    string `json:"egress_ip"`
}

type controlResponse struct {
	RecoveryVersion int    `json:"recovery_version"`
	Status          string `json:"status"`
	Lease           *lease `json:"lease"`
}

type failure struct {
	sequence uint64
	at       time.Time
}

type entry struct {
	failures        []failure
	successSequence uint64
	reporting       bool
	retired         bool
	retryAfter      time.Time
	touched         time.Time
	failureBarrier  uint64
}

// Manager shares response observations across account tests and real requests.
// The lease fingerprint is part of every key, so stale outcomes cannot affect a
// newer lease. Business requests are never replayed by recovery.
type Manager struct {
	endpoints         map[string]struct{}
	threshold         int
	window            time.Duration
	client            *http.Client
	now               func() time.Time
	sequence          atomic.Uint64
	mu                sync.Mutex
	entries           map[string]*entry
	controlRetryAfter map[string]time.Time
	reportSlots       chan struct{}
	reports           sync.WaitGroup
	warnAfter         time.Time
}

func New(cfg Config) *Manager {
	if !cfg.Enabled || len(cfg.ProxyEndpoints) == 0 {
		return nil
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: controlTimeout}).DialContext,
		MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: time.Minute}
	m := &Manager{
		endpoints: make(map[string]struct{}), threshold: cfg.FailureThreshold,
		window: time.Duration(cfg.WindowSeconds) * time.Second, now: time.Now,
		entries:           make(map[string]*entry),
		controlRetryAfter: make(map[string]time.Time), reportSlots: make(chan struct{}, 8),
		client: &http.Client{Transport: transport, Timeout: controlTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	for _, endpoint := range cfg.ProxyEndpoints {
		m.endpoints[strings.ToLower(strings.TrimSpace(endpoint))] = struct{}{}
	}
	if m.threshold <= 0 {
		m.threshold = 3
	}
	if m.window <= 0 {
		m.window = time.Minute
	}
	return m
}

type Attempt struct {
	manager  *Manager
	ctx      context.Context
	proxy    *url.URL
	platform string
	account  string
	target   string
	lease    lease
	scope    string
	identity string
	sequence uint64
	onRetire func(string)
	once     sync.Once
}

func (a *Attempt) Scope() string {
	if a == nil {
		return ""
	}
	return a.scope
}

func (a *Attempt) Identity() string {
	if a == nil {
		return ""
	}
	return a.identity
}

func (a *Attempt) OnRetire(fn func(string)) {
	if a != nil {
		a.onRetire = fn
	}
}

// Prepare is opt-in for exact SOCKS endpoints. Unavailable control-plane
// metadata leaves the existing proxy path usable without guessing a generation.
func (m *Manager) Prepare(ctx context.Context, proxyURL string, target *url.URL) *Attempt {
	if m == nil || target == nil || target.Hostname() == "" || ctx.Err() != nil {
		return nil
	}
	_, p, err := proxyurl.Parse(proxyURL)
	if err != nil || p == nil || (p.Scheme != "socks5h" && p.Scheme != "socks5") || p.User == nil ||
		p.RawQuery != "" || p.Fragment != "" || (p.Path != "" && p.Path != "/") {
		return nil
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil
	}
	if _, allowed := m.endpoints[strings.ToLower(p.Host)]; !allowed {
		return nil
	}
	m.mu.Lock()
	backingOff := m.now().Before(m.controlRetryAfter[strings.ToLower(p.Host)])
	m.mu.Unlock()
	if backingOff {
		return nil
	}
	password, ok := p.User.Password()
	platform, account, hasAccount := strings.Cut(p.User.Username(), ".")
	if !ok || password == "" || !hasAccount || platform == "" || account == "" ||
		strings.ContainsAny(account+platform, "/\\?#@~ \t\r\n") || len(account) > 128 || len(platform) > 128 {
		return nil
	}
	port := target.Port()
	if port == "" {
		switch target.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return nil
		}
	}
	a := &Attempt{manager: m, ctx: ctx, proxy: p, platform: platform, account: account,
		target:   net.JoinHostPort(strings.TrimSuffix(strings.ToLower(target.Hostname()), "."), port),
		sequence: m.sequence.Add(1)}
	result, err := a.control(ctx, "acquire", map[string]string{"target_host": a.target})
	if err != nil || (result.Status != "available" && result.Status != "no_alternative") || !validLease(result.Lease, account) {
		m.warnControlUnavailable(p.Host)
		return nil
	}
	a.lease = *result.Lease
	identity := strings.Join([]string{p.String(), platform, account, target.Scheme, a.target}, "\x00")
	identityDigest := sha256.Sum256([]byte(identity))
	a.identity = hex.EncodeToString(identityDigest[:])
	fingerprint := strings.Join([]string{identity, a.lease.NodeHash, a.lease.CreatedAtNs, a.lease.EgressIP}, "\x00")
	digest := sha256.Sum256([]byte(fingerprint))
	a.scope = hex.EncodeToString(digest[:])
	return a
}

func validLease(value *lease, account string) bool {
	if value == nil || value.Account != account || len(value.NodeHash) != 32 {
		return false
	}
	hash, err := hex.DecodeString(value.NodeHash)
	if err != nil || bytes.Equal(hash, make([]byte, 16)) {
		return false
	}
	created, err := strconv.ParseInt(value.CreatedAtNs, 10, 64)
	return err == nil && created > 0 && net.ParseIP(value.EgressIP) != nil
}

func (a *Attempt) control(ctx context.Context, action string, body any) (*controlResponse, error) {
	password, _ := a.proxy.User.Password()
	endpoint, err := url.JoinPath("http://"+a.proxy.Host, "proxy-api", "v1",
		url.PathEscape(a.platform), "leases", url.PathEscape(a.account), "actions", action)
	if err != nil {
		return nil, errors.New("invalid recovery endpoint")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("invalid recovery body")
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, errors.New("invalid recovery request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+password)
	// The destination matches the configured trusted endpoint allowlist; redirects are disabled.
	resp, err := a.manager.client.Do(req) //nolint:gosec // G704: only explicitly configured Resin endpoints receive control requests.
	if err != nil {
		return nil, errors.New("recovery control request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("recovery control status %d", resp.StatusCode)
	}
	var result controlResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16385))
	if decoder.Decode(&result) != nil || result.RecoveryVersion != 1 {
		return nil, errors.New("invalid recovery response")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid recovery response suffix")
	}
	return &result, nil
}

func (m *Manager) warnControlUnavailable(endpoint string) {
	m.mu.Lock()
	now := m.now()
	m.controlRetryAfter[strings.ToLower(endpoint)] = now.Add(reportBackoff)
	logNow := !now.Before(m.warnAfter)
	if logNow {
		m.warnAfter = now.Add(reportBackoff)
	}
	m.mu.Unlock()
	if logNow {
		slog.Warn("resin_recovery_control_unavailable", "proxy_endpoint", endpoint)
	}
}

// Complete records only an explicit semantic verdict. Closing a Body, a
// cancelled client, or an unread response must never look like a node failure.
func (a *Attempt) Complete(outcome Outcome) {
	if a == nil || a.ctx.Err() != nil {
		return
	}
	a.complete(outcome)
}

func (a *Attempt) complete(outcome Outcome) {
	switch outcome {
	case Reachable, EmptyStream, InvalidStream, TransportError:
	default:
		return
	}
	a.once.Do(func() { a.manager.record(a, outcome) })
}

func (m *Manager) record(a *Attempt, outcome Outcome) {
	m.mu.Lock()
	now := m.now()
	e := m.entries[a.scope]
	if e == nil {
		if !m.makeRoomLocked(now) {
			m.mu.Unlock()
			return
		}
		e = &entry{}
		m.entries[a.scope] = e
	}
	e.touched = now
	if e.retired {
		m.mu.Unlock()
		return
	}
	kept := e.failures[:0]
	if outcome == Reachable {
		e.successSequence = max(e.successSequence, a.sequence)
	}
	for _, item := range e.failures {
		if item.sequence > e.successSequence && now.Sub(item.at) <= m.window {
			kept = append(kept, item)
		}
	}
	e.failures = kept
	if outcome == Reachable || a.sequence <= e.successSequence {
		m.mu.Unlock()
		return
	}
	// Coalesce requests already in flight when an incident was observed, while
	// still counting sequential failures even if a test finishes very quickly.
	if a.sequence > e.failureBarrier {
		if len(e.failures) < m.threshold {
			e.failures = append(e.failures, failure{a.sequence, now})
		}
		e.failureBarrier = m.sequence.Load()
	}
	if len(e.failures) < m.threshold || e.reporting || now.Before(e.retryAfter) {
		m.mu.Unlock()
		return
	}
	select {
	case m.reportSlots <- struct{}{}:
	default:
		m.mu.Unlock()
		return
	}
	e.reporting = true
	m.reports.Add(1)
	m.mu.Unlock()
	go m.reportFailure(a, outcome, e)
}

func (m *Manager) reportFailure(a *Attempt, outcome Outcome, e *entry) {
	defer m.reports.Done()
	defer func() { <-m.reportSlots }()
	m.mu.Lock()
	if e.retired || len(e.failures) < m.threshold || a.sequence <= e.successSequence {
		e.reporting = false
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// The semantic verdict is final; handler teardown must not cancel its report.
	// The control request has its own short deadline and never replays the POST.
	result, err := a.control(context.WithoutCancel(a.ctx), "report-failure", map[string]string{
		"target_host": a.target, "expected_node_hash": a.lease.NodeHash,
		"expected_created_at_ns": a.lease.CreatedAtNs, "reason": string(outcome),
	})
	status := "control_unavailable"
	if err == nil {
		status = result.Status
	}
	retired := status == "rotated" || status == "stale_lease"
	m.mu.Lock()
	e.reporting = false
	e.retired = retired
	e.retryAfter = m.now().Add(reportBackoff)
	m.mu.Unlock()
	if retired && a.onRetire != nil {
		a.onRetire(a.scope)
	}
	slog.Info("resin_lease_recovery", "platform", a.platform, "account", a.account,
		"target_host", a.target, "observed_node", a.lease.NodeHash, "reason", outcome, "status", status)
}

func (m *Manager) makeRoomLocked(now time.Time) bool {
	for key, value := range m.entries {
		if !value.reporting && now.Sub(value.touched) >= entryTTL {
			delete(m.entries, key)
		}
	}
	if len(m.entries) < maxEntries {
		return true
	}
	var oldestKey string
	var oldest time.Time
	for key, value := range m.entries {
		if !value.reporting && (oldest.IsZero() || value.touched.Before(oldest)) {
			oldestKey, oldest = key, value.touched
		}
	}
	if oldestKey != "" {
		delete(m.entries, oldestKey)
	}
	return len(m.entries) < maxEntries
}

type observedBody struct {
	io.ReadCloser
	attempt         *Attempt
	inspectBusiness bool
	status          int
	oversized       bool
	errorBody       []byte
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.inspectBusiness && !b.oversized {
		if len(b.errorBody)+n > 16<<10 {
			b.oversized = true
			b.errorBody = nil
		} else {
			b.errorBody = append(b.errorBody, p[:n]...)
		}
		if err == io.EOF && !b.oversized {
			var value map[string]json.RawMessage
			if b.status == http.StatusPaymentRequired || b.status == http.StatusTooManyRequests {
				b.attempt.Complete(Reachable)
			} else if b.status < 500 && json.Unmarshal(b.errorBody, &value) == nil && validBusinessError(value["error"]) {
				b.attempt.Complete(Reachable)
			} else if bytes.HasPrefix(bytes.ToLower(bytes.TrimSpace(b.errorBody)), []byte("<!doctype html")) ||
				bytes.HasPrefix(bytes.ToLower(bytes.TrimSpace(b.errorBody)), []byte("<html")) {
				b.attempt.Complete(InvalidStream)
			}
			b.errorBody = nil
			b.inspectBusiness = false
		}
	}
	return n, err
}

func (b *observedBody) ReportResinOutcome(outcome Outcome) { b.attempt.complete(outcome) }

type outcomeReporter interface {
	ReportResinOutcome(Outcome)
}

type forwardingBody struct {
	io.ReadCloser
	outcomeReporter
}

func BindBody(body io.ReadCloser, attempt *Attempt) io.ReadCloser {
	if body == nil || attempt == nil {
		return body
	}
	return &observedBody{ReadCloser: body, attempt: attempt}
}

// PreserveBodyObservation keeps semantic reporting across stream transforms,
// without marking ordinary response bodies as managed.
func PreserveBodyObservation(body, source io.ReadCloser) io.ReadCloser {
	if observed, ok := source.(outcomeReporter); ok && body != nil {
		return &forwardingBody{ReadCloser: body, outcomeReporter: observed}
	}
	return body
}

// Report uses the consumer's context: an upstream wrapper may already have
// closed its source and cancelled its private request context after reaching EOF.
func Report(ctx context.Context, body io.Reader, outcome Outcome) {
	if ctx.Err() != nil {
		return
	}
	if observed, ok := body.(outcomeReporter); ok {
		observed.ReportResinOutcome(outcome)
	}
}

func IsManaged(body io.Reader) bool {
	_, ok := body.(outcomeReporter)
	return ok
}

func validBusinessError(raw json.RawMessage) bool {
	var value struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(raw, &value) == nil && (value.Message != "" || value.Type != "") {
		return true
	}
	var message string
	return json.Unmarshal(raw, &message) == nil && message != ""
}

// A fully read JSON error proves the upstream answered. Merely receiving HTTP
// headers or an intermediary HTML page does not clear an existing failure streak.
func ReportBusinessResponse(resp *http.Response) {
	if resp == nil || resp.StatusCode < 400 {
		return
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if (err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") || mediaType == "text/html")) ||
		resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
		if body, ok := resp.Body.(*observedBody); ok {
			body.inspectBusiness = true
			body.status = resp.StatusCode
		}
	}
}
