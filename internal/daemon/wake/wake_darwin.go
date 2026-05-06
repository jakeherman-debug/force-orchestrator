//go:build darwin && cgo

// wake_darwin.go — macOS implementation of Subscribe via IOKit's
// IOPMRegisterForSystemPower.
//
// IOKit dispatches kIOMessageSystemWillSleep + kIOMessageSystemHasPoweredOn
// (and a few related messages) onto a CFRunLoop. We register a callback,
// run the runloop on a dedicated thread (locked via runtime.LockOSThread),
// and translate each callback into an Event on the channel.
//
// IOKit requires us to acknowledge the will-sleep message, otherwise the
// kernel waits up to 30s for our response before forcing sleep anyway.
// We acknowledge immediately — the daemon does NOT block sleep, it just
// observes it.
//
// Cleanup is anchored to ctx: when ctx is canceled we stop the runloop,
// deregister, and close the channel.
package wake

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <stdint.h>
#include <stdlib.h>
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/pwr_mgt/IOPMLib.h>

// Forward declaration of the Go callback bridged via cgo. cgo emits
// the C-side trampoline as `goWakePowerCallback`; we declare it here
// so the C compiler can take its address inside the registration.
extern void goWakePowerCallback(uintptr_t handle, natural_t messageType, void* messageArgument);

// Wrapper that matches IOServiceInterestCallback's signature and
// forwards to the Go side.
static void wakeCallbackTrampoline(void* refCon, io_service_t service, natural_t messageType, void* messageArgument) {
    (void)service;
    goWakePowerCallback((uintptr_t)refCon, messageType, messageArgument);
}

// startWakeNotifier registers for system-power notifications and
// schedules the resulting source on the current runloop. Returns the
// IONotificationPortRef + io_object_t the caller must keep until
// stopWakeNotifier is called.
static int startWakeNotifier(uintptr_t handle, IONotificationPortRef* outPort, io_object_t* outNotifier, io_connect_t* outRoot) {
    io_connect_t root = IORegisterForSystemPower((void*)handle, outPort, wakeCallbackTrampoline, outNotifier);
    if (root == MACH_PORT_NULL) {
        return -1;
    }
    *outRoot = root;
    CFRunLoopAddSource(CFRunLoopGetCurrent(),
                       IONotificationPortGetRunLoopSource(*outPort),
                       kCFRunLoopCommonModes);
    return 0;
}

// stopWakeNotifier tears down the registration. Safe to call with
// zero / null inputs (no-op).
static void stopWakeNotifier(IONotificationPortRef port, io_object_t notifier, io_connect_t root) {
    if (port != NULL) {
        CFRunLoopRemoveSource(CFRunLoopGetCurrent(),
                              IONotificationPortGetRunLoopSource(port),
                              kCFRunLoopCommonModes);
    }
    if (notifier != 0) {
        IODeregisterForSystemPower(&notifier);
    }
    if (port != NULL) {
        IONotificationPortDestroy(port);
    }
    if (root != 0) {
        IOServiceClose(root);
    }
}

// allowSleep acknowledges a will-sleep message so the kernel does not
// have to wait its full 30s timeout. We are observers, never blockers.
static void allowSleep(io_connect_t root, void* messageArgument) {
    IOAllowPowerChange(root, (long)messageArgument);
}

// runRunLoopOnce drives the runloop until CFRunLoopStop is called
// (from a different thread). Returns when the runloop exits.
static void runRunLoop(void) {
    CFRunLoopRun();
}

static void stopRunLoop(CFRunLoopRef rl) {
    if (rl != NULL) {
        CFRunLoopStop(rl);
    }
}

static CFRunLoopRef currentRunLoop(void) {
    return CFRunLoopGetCurrent();
}
*/
import "C"

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Sentinel codes IOKit delivers as `messageType`. Values pulled from
// <IOKit/pwr_mgt/IOPMLib.h>. We only care about the four — the rest are
// ignored.
const (
	kIOMessageSystemWillSleep   = 0xe0000280
	kIOMessageCanSystemSleep    = 0xe0000270
	kIOMessageSystemHasPoweredOn = 0xe0000300
	kIOMessageSystemWillPowerOn  = 0xe0000320
)

// activeNotifiers maps the handle we pass to IOKit (a uintptr) back to
// the Go-side state needed to dispatch a callback. We use a uintptr
// key rather than passing a *Go pointer because cgo (correctly) refuses
// to let us hand a Go pointer into C and back.
var (
	notifiersMu sync.Mutex
	notifiers   = map[uintptr]*notifier{}
	nextHandle  uint64
)

type notifier struct {
	out  chan Event
	root C.io_connect_t
}

// Subscribe registers an IOKit power-state callback and returns a
// channel of Event. ctx cancellation tears down the registration and
// closes the channel.
func Subscribe(ctx context.Context) (<-chan Event, error) {
	out := make(chan Event, 4)

	handle := uintptr(atomic.AddUint64(&nextHandle, 1))
	n := &notifier{out: out}

	// Register the entry BEFORE starting the runloop so a wake event
	// landing immediately can find its bucket.
	notifiersMu.Lock()
	notifiers[handle] = n
	notifiersMu.Unlock()

	// We need a dedicated OS thread so the CFRunLoop has a stable
	// home. CFRunLoops are per-thread.
	type readyResult struct {
		rl  C.CFRunLoopRef
		err error
	}
	ready := make(chan readyResult, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		var port C.IONotificationPortRef
		var notifierObj C.io_object_t
		var root C.io_connect_t
		rc := C.startWakeNotifier(C.uintptr_t(handle), &port, &notifierObj, &root)
		if rc != 0 {
			ready <- readyResult{err: fmt.Errorf("wake: IORegisterForSystemPower failed (rc=%d)", int(rc))}
			return
		}

		notifiersMu.Lock()
		n.root = root
		notifiersMu.Unlock()

		ready <- readyResult{rl: C.currentRunLoop()}
		C.runRunLoop()

		// runloop exited (ctx cancellation triggered stopRunLoop on
		// another thread). Tear down the registration.
		C.stopWakeNotifier(port, notifierObj, root)

		notifiersMu.Lock()
		delete(notifiers, handle)
		notifiersMu.Unlock()

		close(out)
	}()

	res := <-ready
	if res.err != nil {
		// Registration failed — clean up the map entry we speculatively
		// added.
		notifiersMu.Lock()
		delete(notifiers, handle)
		notifiersMu.Unlock()
		close(out)
		return nil, res.err
	}

	// Stop the runloop on ctx cancellation. CFRunLoopStop is
	// thread-safe.
	go func() {
		<-ctx.Done()
		C.stopRunLoop(res.rl)
	}()

	return out, nil
}

//export goWakePowerCallback
func goWakePowerCallback(handle C.uintptr_t, messageType C.natural_t, messageArgument unsafe.Pointer) {
	notifiersMu.Lock()
	n, ok := notifiers[uintptr(handle)]
	notifiersMu.Unlock()
	if !ok {
		return
	}

	switch uint32(messageType) {
	case kIOMessageCanSystemSleep, kIOMessageSystemWillSleep:
		// Acknowledge so the kernel doesn't wait 30s for us. We are
		// observers, not blockers.
		C.allowSleep(n.root, messageArgument)
		select {
		case n.out <- GoingToSleep:
		default:
			// Full channel — drop. Sleep notifications are rare; if
			// we missed one, the corresponding wake will still kick
			// reconciliation.
		}
	case kIOMessageSystemHasPoweredOn:
		select {
		case n.out <- Woke:
		default:
		}
	}
}
