//go:build !darwin && !linux

// wake_other.go — fallback for platforms with no power-state hook
// (Windows, *BSD, anything that's neither macOS nor Linux). Returns
// (nil, nil) so the daemon still runs but skips post-wake
// reconciliation. Graceful degradation per D12 P2 spec.
package wake

import "context"

// Subscribe is a no-op on unsupported platforms.
func Subscribe(_ context.Context) (<-chan Event, error) {
	return nil, nil
}
