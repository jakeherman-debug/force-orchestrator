package dashboard

// D12 P1 — bundled-dashboard smoke check: spin up RunDashboardCtx on
// an ephemeral loopback port, hit /api/health, confirm 200.
//
// This stands in for the acceptance-bar item #7 ("curl http://127.0.0.1:41977/api/health
// returns 200 within 2 seconds"): we can't bind 41977 inside CI because
// the port may collide with a developer's running daemon; instead we
// pick a free port via net.Listen-then-close trick + call RunDashboardCtx
// with that port.

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

func TestBundledDashboard_HealthEndpoint(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Find a free port. Listen-and-close gives us one with high
	// probability that it's still free a moment later (nothing else
	// in this test grabs it).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunDashboardCtx(ctx, db, port)

	// Wait up to 2s for the server to come up.
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/health"
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var lastErr error
	for time.Now().Before(deadline) {
		resp, lastErr = http.Get(url)
		if lastErr == nil && resp != nil && resp.StatusCode == 200 {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET %s: %v", url, lastErr)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %v", url, resp)
	}
	resp.Body.Close()

	// Also probe /healthz to confirm the alias works on both paths.
	url2 := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	resp2, err := http.Get(url2)
	if err != nil {
		t.Fatalf("GET %s: %v", url2, err)
	}
	if resp2.StatusCode != 200 {
		t.Errorf("GET %s: status %d", url2, resp2.StatusCode)
	}
	resp2.Body.Close()
}

// TestBundledDashboard_LoopbackOnly: confirm the server bound on
// 127.0.0.1 only, NOT on the wildcard. We can't easily probe other
// interfaces from a unit test, so we verify by pinging the bind addr
// stamp produced by loopbackBindAddr.
func TestBundledDashboard_LoopbackOnly(t *testing.T) {
	addr := loopbackBindAddr(41977)
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("loopbackBindAddr = %q, want 127.0.0.1: prefix (D12 P1 + AUDIT-001)", addr)
	}
	if !strings.HasSuffix(addr, ":41977") {
		t.Errorf("loopbackBindAddr port suffix = %q, want :41977", addr)
	}
}

// TestBundledDashboard_CtxCancelStopsServer: cancelling the ctx should
// cause RunDashboardCtx to return within ~5s (the shutdown deadline).
func TestBundledDashboard_CtxCancelStopsServer(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunDashboardCtx(ctx, db, port)
		close(done)
	}()

	// Wait for server up.
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Cancel and wait for the goroutine to return.
	cancel()
	select {
	case <-done:
		// good
	case <-time.After(10 * time.Second):
		t.Errorf("RunDashboardCtx did not return within 10s of ctx cancel")
	}
}
