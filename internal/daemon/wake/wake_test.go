package wake

import (
	"context"
	"testing"
	"time"
)

// TestEvent_String confirms the Event stringer produces the labels
// Subscribers (and operator log lines) expect.
func TestEvent_String(t *testing.T) {
	cases := map[Event]string{
		GoingToSleep: "going-to-sleep",
		Woke:         "woke",
		Event(99):    "unknown",
	}
	for ev, want := range cases {
		if got := ev.String(); got != want {
			t.Errorf("Event(%d).String() = %q, want %q", ev, got, want)
		}
	}
}

// TestSubscribe_ContextCancellation confirms that Subscribe respects
// ctx cancellation across every platform implementation: the channel
// (when non-nil) closes after ctx is cancelled, and Subscribe never
// blocks indefinitely.
//
// The test tolerates two valid outcomes per platform:
//
//   - (nil, nil)        — graceful no-op (other / no-cgo darwin /
//                         CI without a system bus on linux).
//   - (chan, nil)       — real subscription; channel closes within
//                         the deadline after ctx cancellation.
//   - (nil, non-nil)    — subscription failed (e.g. no system bus
//                         in a sandboxed CI). Treated as a tolerated
//                         outcome — the daemon's wiring code logs and
//                         continues without hooks.
func TestSubscribe_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := Subscribe(ctx)

	if err != nil {
		// Tolerated — platform / sandbox can't subscribe. Cancel and
		// return.
		cancel()
		t.Logf("Subscribe returned err=%v (tolerated as graceful degradation)", err)
		return
	}
	if ch == nil {
		// Graceful no-op platform. Nothing more to verify.
		cancel()
		return
	}

	// Real subscription: cancel ctx and confirm the channel closes
	// within a reasonable deadline.
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
			// drain any in-flight events
		case <-deadline:
			t.Fatalf("Subscribe channel did not close within 5s after ctx cancellation")
		}
	}
}

// TestSubscribe_NoOpPlatformReturnsNilNil documents the "graceful
// degradation" contract on platforms without a real power hook. The
// no-cgo darwin and wake_other.go files MUST return (nil, nil) — the
// daemon's wiring code uses that exact shape to decide whether to
// spawn the reconcile goroutine.
//
// On the platforms where Subscribe IS implemented (cgo darwin, linux),
// we just confirm the call doesn't panic and either returns a channel
// or a transient error (e.g. linux without a system bus). We do NOT
// drive synthetic events here — those are exercised through
// reconcilePostWake's unit test (which doesn't require a live OS hook).
func TestSubscribe_NoOpPlatformReturnsNilNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := Subscribe(ctx)

	// All platforms: panic-free, well-defined return shape.
	switch {
	case ch == nil && err == nil:
		// Graceful no-op (wake_other.go / wake_darwin_nocgo.go OR a
		// platform-specific impl that decided no support is available
		// at runtime).
		t.Logf("Subscribe returned (nil, nil) — graceful no-op platform")
	case ch != nil && err == nil:
		t.Logf("Subscribe returned a real channel — supported platform")
	case ch == nil && err != nil:
		t.Logf("Subscribe returned (nil, %v) — tolerated transient error", err)
	default:
		t.Fatalf("Subscribe returned both ch=%v AND err=%v (must be one or the other)", ch, err)
	}
}
