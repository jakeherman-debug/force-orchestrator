// Package wake abstracts platform-specific power-state notifications so
// the force daemon can survive sleep/wake transitions cleanly.
//
// Why a dedicated abstraction?
//
//   - macOS uses IOKit's IOPMRegisterForSystemPower (cgo).
//   - Linux uses logind's PrepareForSleep D-Bus signal.
//   - Other platforms (Windows, *BSD, no-cgo macOS) have no support;
//     Subscribe returns (nil, nil) so the daemon still runs but skips
//     post-wake reconciliation. "Graceful degradation" — never block
//     the daemon over a missing power-state hook.
//
// The Event channel is consumed by cmd/force/daemon_wake.go, which
// runs reconcilePostWake when a Woke event lands. Idempotence of the
// reconciler is the package's correctness gate (see D12 P2 spec).
//
// Public API:
//
//	wake.Subscribe(ctx) (<-chan wake.Event, error)
//
// Each platform-specific implementation lives in its own file with the
// matching build tag (wake_darwin.go, wake_linux.go, wake_other.go,
// wake_darwin_nocgo.go).
package wake

// Event is the kind of power-state notification the OS delivered.
type Event int

const (
	// GoingToSleep indicates the system is about to suspend. Subscribers
	// have a short, non-blocking window before the kernel actually
	// freezes the process — DO NOT block in this branch (snapshotting
	// the holocron, flushing logs is fine; making an HTTP call is not).
	GoingToSleep Event = iota

	// Woke indicates the system has resumed from sleep. May arrive
	// slightly delayed (the OS schedules userland notifications after
	// the kernel + driver wake sequence completes). Reconciliation
	// runs in this branch.
	Woke
)

// String renders the Event for log lines.
func (e Event) String() string {
	switch e {
	case GoingToSleep:
		return "going-to-sleep"
	case Woke:
		return "woke"
	default:
		return "unknown"
	}
}
