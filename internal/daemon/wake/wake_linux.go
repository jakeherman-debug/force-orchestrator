//go:build linux

// wake_linux.go — Linux implementation of Subscribe via logind D-Bus.
//
// systemd-logind broadcasts org.freedesktop.login1.Manager.PrepareForSleep
// when the system is about to suspend AND when it has resumed. The
// signal payload is a single boolean:
//
//	true  — system about to sleep
//	false — system has resumed
//
// We connect to the system bus, install a match rule for the signal,
// and translate each delivery into an Event on the returned channel.
//
// Failure modes (any of which return (nil, err) — caller treats as
// "no power-state notifications, continue anyway"):
//
//   - System bus not reachable (containers, distros without systemd).
//   - AddMatch fails (insufficient privileges; rare on a normal user
//     session).
//
// Cleanup is anchored to ctx: when ctx is canceled, the goroutine
// removes the match, closes the bus, and closes the channel.
package wake

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
)

const (
	logindMatchRule = "type='signal',interface='org.freedesktop.login1.Manager',member='PrepareForSleep'"
	logindSignal    = "org.freedesktop.login1.Manager.PrepareForSleep"
)

// Subscribe opens a logind D-Bus subscription and dispatches power
// events on the returned channel. ctx cancellation tears down the
// subscription cleanly.
func Subscribe(ctx context.Context) (<-chan Event, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("wake: dbus.SystemBus: %w", err)
	}

	if call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, logindMatchRule); call.Err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("wake: AddMatch %q: %w", logindMatchRule, call.Err)
	}

	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)

	out := make(chan Event, 4)

	go func() {
		defer close(out)
		// Best-effort cleanup — RemoveMatch may fail if the bus
		// already closed, which is fine.
		defer func() {
			_ = conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, logindMatchRule).Err
			_ = conn.Close()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-signals:
				if !ok {
					return
				}
				if sig == nil || sig.Name != logindSignal || len(sig.Body) == 0 {
					continue
				}
				goingToSleep, ok := sig.Body[0].(bool)
				if !ok {
					continue
				}
				ev := Woke
				if goingToSleep {
					ev = GoingToSleep
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}
