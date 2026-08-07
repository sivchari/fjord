package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForAgentReturnsOnceTheAgentAnswers(t *testing.T) {
	t.Parallel()

	// Refuse the first two requests the way a tunnel to an agent that is not
	// up yet does -- the connection closes mid-request -- then answer.
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer does not support hijacking")

				return
			}

			hijacked, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)

				return
			}

			_ = hijacked.Close()

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := waitForAgent(t.Context(), server.URL); err != nil {
		t.Fatalf("waitForAgent: %v", err)
	}

	if got := requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (two refused, one answered)", got)
	}
}

func TestWaitForAgentFailsWhenNothingAnswers(t *testing.T) {
	t.Parallel()

	// A server that is closed before the wait starts: connections to its
	// address are refused, which is the never-ready case.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL

	server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := waitForAgent(ctx, url)
	if err == nil {
		t.Fatal("waitForAgent succeeded against a closed server, want an error")
	}
}
