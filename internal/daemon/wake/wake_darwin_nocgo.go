//go:build darwin && !cgo

// wake_darwin_nocgo.go — fallback for `CGO_ENABLED=0` builds on macOS
// (e.g. CI runs that disable cgo for go vet portability). IOKit
// requires cgo, so the no-cgo build is a graceful no-op: returns
// (nil, nil) and the daemon continues without sleep/wake hooks.
package wake

import "context"

// Subscribe is a no-op when cgo is disabled. The daemon proceeds
// without post-wake reconciliation.
func Subscribe(_ context.Context) (<-chan Event, error) {
	return nil, nil
}
