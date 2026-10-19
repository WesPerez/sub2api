// Package requestbudget bounds Responses authentication and request upload
// without attaching their deadline to the subsequent response stream.
package requestbudget

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ingressKey struct{}
type ingress struct {
	base     context.Context
	deadline time.Time
	cancel   context.CancelFunc
	readDone func()
	once     sync.Once
}

// Wrap must run outside Gin so ResponseController can reach the actual HTTP/1
// or HTTP/2 writer, including versions of Gin without ResponseWriter.Unwrap.
func Wrap(next http.Handler, timeout time.Duration) http.Handler {
	if timeout <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if !responsesGenerationPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		deadline := time.Now().Add(timeout)
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(deadline); err != nil {
			slog.Warn("responses_ingress_read_deadline_unavailable", "error", err)
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		state := &ingress{base: r.Context(), deadline: deadline, cancel: cancel,
			readDone: func() { _ = controller.SetReadDeadline(time.Time{}) }}
		ctx = context.WithValue(ctx, ingressKey{}, state)
		defer cancel()
		defer FinishRead(ctx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func responsesGenerationPath(path string) bool {
	path = strings.TrimRight(path, "/")
	for _, prefix := range []string{"/responses", "/v1/responses", "/backend-api/codex/responses"} {
		if path == prefix {
			return true
		}
		if strings.HasPrefix(path, prefix+"/") {
			suffix := strings.TrimPrefix(path, prefix)
			return suffix != "/compact" && suffix != "/input_tokens"
		}
	}
	return false
}

func Expired(ctx context.Context) bool {
	state, _ := ctx.Value(ingressKey{}).(*ingress)
	return state != nil && !time.Now().Before(state.deadline)
}

func Deadline(ctx context.Context) (time.Time, bool) {
	state, ok := ctx.Value(ingressKey{}).(*ingress)
	if !ok {
		return time.Time{}, false
	}
	return state.deadline, true
}

func FinishRead(ctx context.Context) {
	if state, _ := ctx.Value(ingressKey{}).(*ingress); state != nil {
		// After an upload timeout leave the read deadline expired until the
		// connection closes; net/http must not block draining an incomplete body.
		state.once.Do(func() {
			if time.Now().Before(state.deadline) {
				state.readDone()
			}
		})
	}
}

// Preserve every value added by authentication (user, group, tracing), while
// restoring the original client's cancellation and deadline for long streams.
type lifecycleContext struct {
	context.Context
	values context.Context
}

func (c lifecycleContext) Value(key any) any { return c.values.Value(key) }

func RestoreLifecycle(ctx context.Context) context.Context {
	if state, _ := ctx.Value(ingressKey{}).(*ingress); state != nil {
		state.cancel()
		return lifecycleContext{Context: state.base, values: context.WithoutCancel(ctx)}
	}
	return ctx
}
