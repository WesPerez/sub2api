package requestbudget

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIngressSlowBodyUsesSocketDeadline(t *testing.T) {
	for _, path := range []string{"/responses", "/v1/responses", "/backend-api/codex/responses", "/v1/responses/", "/responses/custom", "/v1/responses/custom", "/backend-api/codex/responses/custom"} {
		t.Run(path, func(t *testing.T) {
			server := httptest.NewServer(Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.WithContext(RestoreLifecycle(r.Context()))
				_, err := io.ReadAll(r.Body)
				if err != nil && Expired(r.Context()) {
					w.Header().Set("Connection", "close")
					w.WriteHeader(http.StatusGatewayTimeout)
					return
				}
				w.WriteHeader(http.StatusOK)
			}), 100*time.Millisecond))
			defer server.Close()
			conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
			started := time.Now()
			_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\n{", path)
			require.NoError(t, err)
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
			require.Less(t, time.Since(started), 750*time.Millisecond)
		})
	}
}

func TestIngressAuthDeadlineAndLifecycleValues(t *testing.T) {
	type key struct{}
	base, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotNil(t, r.Context().Done())
		ctx := context.WithValue(r.Context(), key{}, "user-and-group")
		ctx = RestoreLifecycle(ctx)
		require.Equal(t, "user-and-group", ctx.Value(key{}))
		require.NoError(t, ctx.Err())
		child, cancel := context.WithCancel(ctx)
		defer cancel()
		require.NoError(t, child.Err(), "child must not bind the canceled auth context")
		time.Sleep(80 * time.Millisecond)
		require.NoError(t, ctx.Err(), "upload/auth deadline must not cancel a long response")
		cancelClient()
		require.Eventually(t, func() bool { return child.Err() != nil }, time.Second, time.Millisecond)
	}), 30*time.Millisecond).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/responses", nil).WithContext(base))

	Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		require.ErrorIs(t, r.Context().Err(), context.DeadlineExceeded)
		require.True(t, Expired(r.Context()))
	}), 20*time.Millisecond).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/responses", nil))
}

func TestIngressDoesNotApplyToCompactOrWebsocket(t *testing.T) {
	for _, path := range []string{"/responses/compact", "/v1/responses/input_tokens", "/backend-api/codex/responses/compact/", "/responses-other", "/messages"} {
		Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, enabled := r.Context().Deadline()
			require.False(t, enabled)
		}), time.Millisecond).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil))
	}
	Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, enabled := r.Context().Deadline()
		require.False(t, enabled)
	}), time.Millisecond).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/responses", nil))
}

func TestIngressLongResponseAfterBodyKeepsConnectionUsable(t *testing.T) {
	server := httptest.NewServer(Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(RestoreLifecycle(r.Context()))
		_, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		FinishRead(r.Context())
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: first\n\n")
		require.NoError(t, http.NewResponseController(w).Flush())
		time.Sleep(120 * time.Millisecond)
		if r.Context().Err() == nil {
			_, _ = io.WriteString(w, "data: done\n\n")
		}
	}), 40*time.Millisecond))
	defer server.Close()
	client := server.Client()
	for i := 0; i < 2; i++ {
		resp, err := client.Post(server.URL+"/responses", "application/json", strings.NewReader(`{"stream":true}`))
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Contains(t, string(body), "data: done")
	}
}
