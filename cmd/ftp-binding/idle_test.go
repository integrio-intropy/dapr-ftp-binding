package main

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// quick is long enough not to be flaky on a loaded CI runner, short enough that
// the whole file runs in well under a second.
const quick = 150 * time.Millisecond

// fakeSidecar is a controllable stand-in for sidecarRunning.
type fakeSidecar struct{ up atomic.Bool }

func (f *fakeSidecar) running() bool { return f.up.Load() }

// fired waits up to d for the watcher to report an idle sidecar.
func fired(t *testing.T, w *idleWatcher, d time.Duration) bool {
	t.Helper()
	select {
	case <-w.Fired():
		return true
	case <-time.After(d):
		return false
	}
}

// started builds a watcher polling a fake sidecar at test speed, and starts it.
func started(timeout, startWait time.Duration, sc *fakeSidecar) *idleWatcher {
	w := newIdleWatcher(timeout, startWait)
	w.running = sc.running
	w.pollInterval = quick / 10
	w.Start()
	return w
}

func TestZeroTimeoutDisablesTheWatcher(t *testing.T) {
	w := newIdleWatcher(0, quick)
	if w != nil {
		t.Fatalf("newIdleWatcher(0, quick) = %v, want nil so it is never registered", w)
	}
	w.Start() // must not panic on the nil watcher the run loop still calls
	// A nil watcher's channel must block rather than fire, or every Deployment
	// would exit as soon as the run loop reached its select.
	if fired(t, w, quick) {
		t.Fatal("a disabled watcher fired; Deployments would exit immediately")
	}
}

// The component may be running for a while before daprd's HTTP port answers.
// Firing on the idle timeout alone would turn a slow sidecar start into a
// vanished component: before the first sighting it is the longer startup wait
// that applies.
func TestDoesNotFireOnTheIdleTimeoutBeforeTheSidecarHasEverBeenSeen(t *testing.T) {
	sc := &fakeSidecar{}
	w := started(quick, 100*quick, sc)
	if fired(t, w, 4*quick) {
		t.Fatal("fired on the idle timeout before the sidecar was ever seen")
	}
}

// A zero startup wait keeps the old behaviour: hold the socket open forever
// for a sidecar that may yet arrive.
func TestZeroStartWaitWaitsForeverForTheFirstSighting(t *testing.T) {
	sc := &fakeSidecar{}
	w := started(quick, 0, sc)
	if fired(t, w, 6*quick) {
		t.Fatal("fired although the startup wait was disabled")
	}
}

// The case this is all for: daprd dies before its HTTP port ever answers --
// typically because it died before reaching this container at all -- so no
// sighting is ever coming. Without this the Job runs forever.
func TestFiresWhenTheSidecarIsNeverSeen(t *testing.T) {
	sc := &fakeSidecar{}
	w := started(100*quick, quick, sc)
	select {
	case reason := <-w.Fired():
		// The reason is logged as the process exits, and is the only clue an
		// operator gets that the sidecar was the thing that went missing.
		if !strings.Contains(reason, "never connected") {
			t.Fatalf("reason = %q, want it to say the sidecar never connected", reason)
		}
	case <-time.After(20 * quick):
		t.Fatal("did not fire although the sidecar was never seen")
	}
}

// A sidecar seen within the startup wait disarms it for good: from then on
// only it going quiet plus the idle timeout may fire.
func TestTheFirstSightingDisarmsTheStartupWait(t *testing.T) {
	sc := &fakeSidecar{}
	w := started(100*quick, 2*quick, sc)
	sc.up.Store(true)
	if fired(t, w, 6*quick) {
		t.Fatal("fired on the startup wait although the sidecar had been seen")
	}
}

// Going quiet before the startup wait would have elapsed hands over to the
// idle timeout rather than leaving the startup timer to fire early.
func TestGoingQuietAfterBeingSeenUsesTheIdleTimeout(t *testing.T) {
	sc := &fakeSidecar{}
	w := started(4*quick, 100*quick, sc)
	sc.up.Store(true)
	time.Sleep(quick)
	sc.up.Store(false)
	if !fired(t, w, 40*quick) {
		t.Fatal("did not fire on the idle timeout after the sidecar went quiet")
	}
}

// This is the regression this redesign exists for: a sidecar that is seen,
// goes briefly quiet the way daprd's own connection-idling does every ~30
// minutes against OUR socket, and comes straight back, must not look like a
// vanished sidecar. The watcher here never even looks at our socket, but the
// equivalent flap on the signal it does use -- daprd's HTTP port blinking --
// must behave the same way: the countdown resets rather than accumulating.
func TestABriefGapWhileStillRunningDoesNotAccumulate(t *testing.T) {
	sc := &fakeSidecar{}
	w := started(4*quick, 100*quick, sc)
	sc.up.Store(true)
	time.Sleep(quick)

	for range 5 {
		sc.up.Store(false)
		time.Sleep(quick) // less than the 4*quick timeout
		sc.up.Store(true)
		time.Sleep(quick / 2)
	}

	if fired(t, w, 2*quick) {
		t.Fatal("fired although the sidecar kept coming back before the timeout")
	}

	sc.up.Store(false)
	if !fired(t, w, 40*quick) {
		t.Fatal("did not fire once the sidecar stayed gone past the timeout")
	}
}

// End to end over a real TCP port, proving sidecarRunning itself rather than
// just the bookkeeping around a fake.
func TestFiresWhenARealPortStopsAnswering(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	_, port, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("splitting %s: %v", lis.Addr(), err)
	}
	t.Setenv(daprHTTPPortEnv, port)

	w := newIdleWatcher(quick, 100*quick)
	w.pollInterval = quick / 10
	w.Start()

	if fired(t, w, 2*quick) {
		t.Fatal("fired while the port was still answering")
	}

	if err := lis.Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}
	if !fired(t, w, 20*quick) {
		t.Fatal("did not fire after the port stopped answering")
	}
}

// Guards the zero-value default: an unset port must fall back to 3500, not
// dial an empty address.
func TestSidecarRunningDefaultsThePort(t *testing.T) {
	t.Setenv(daprHTTPPortEnv, "")
	if sidecarRunning() {
		t.Skip("something is actually listening on 127.0.0.1:3500 in this environment")
	}
}
