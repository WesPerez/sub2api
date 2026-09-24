package repository

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/resinrecovery"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type upstreamRecoveryScope struct {
	identity    string
	fingerprint string
}

func upstreamProxyEndpoint(raw string) string {
	_, parsed, err := proxyurl.Parse(raw)
	if err != nil {
		return "invalid"
	}
	if parsed == nil {
		return "direct"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func recoveryScopeFor(attempt *resinrecovery.Attempt) upstreamRecoveryScope {
	if attempt == nil {
		return upstreamRecoveryScope{}
	}
	return upstreamRecoveryScope{identity: attempt.Identity(), fingerprint: attempt.Scope()}
}

func firstRecoveryScope(scopes []upstreamRecoveryScope) upstreamRecoveryScope {
	if len(scopes) == 0 {
		return upstreamRecoveryScope{}
	}
	return scopes[0]
}

func (scope upstreamRecoveryScope) cacheKey(base string) string {
	if scope.fingerprint == "" {
		return base
	}
	return base + ":resin:" + scope.fingerprint
}

func (s *httpUpstreamService) prepareResinRecovery(req *http.Request, proxyURL string, profile service.HTTPUpstreamProfile) *resinrecovery.Attempt {
	if s.resinRecovery == nil || req == nil || req.URL == nil || req.Method != http.MethodPost ||
		profile != service.HTTPUpstreamProfileOpenAI || !strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/responses") {
		return nil
	}
	attempt := s.resinRecovery.Prepare(req.Context(), proxyURL, req.URL)
	if attempt == nil {
		return nil
	}
	attempt.OnRetire(func(scope string) { s.retireResinClients(attempt.Identity(), scope, true) })
	s.retireResinClients(attempt.Identity(), attempt.Scope(), false)
	return attempt
}

// Removing a transport closes its idle connections only. Streams which already
// acquired it keep their own reference and finish normally after a lease change.
func (s *httpUpstreamService) retireResinClients(identity, fingerprint string, matching bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.clients {
		if entry.recoveryScope.identity == identity && (entry.recoveryScope.fingerprint == fingerprint) == matching {
			s.removeClientLocked(key, entry)
		}
	}
}
