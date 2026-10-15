package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/stats"
)

// idleWatcher reports when the sidecar has gone away -- or never arrived -- so
// the component can exit instead of outliving it.
//
// This exists for Jobs and CronJobs. A pod is only Succeeded once every
// container has terminated, and the Dapr injector adds pluggable components to
// spec.containers -- not, even in 1.18 with dapr.io/enable-native-sidecar, to
// initContainers with restartPolicy: Always. So an app that finishes its work
// and shuts the sidecar down still leaves this process running, and the Job
// never completes. Nothing sends us a signal: the kubelet only does that when
// the pod is being torn down, which is precisely what is not happening.
//
// The signal we use is the sidecar's gRPC connection to our socket. daprd dials
// it while loading the component and holds it open for the life of the process,
// so losing every connection means the sidecar is gone.
//
// A sidecar that never connects at all needs its own answer. If daprd starts
// before our socket exists it fails with "couldn't find binding" and dies,
// leaving us listening on a socket nobody will ever dial -- the disconnect we
// are waiting for cannot happen, because there was never a connection. With no
// connection to go on there is nothing to distinguish that from a daprd that is
// merely slow to start, so the only available signal is the clock: startWait
// bounds how long we hold the socket open for a sidecar that has never shown
// up.
//
// It is off by default, and must be off for Deployments: there a sidecar
// restart is routine, and the component has to still be listening afterwards.
type idleWatcher struct {
	timeout   time.Duration
	startWait time.Duration
	fired     chan string

	mu     sync.Mutex
	active int
	// seen records that the sidecar has connected at least once, which retires
	// startWait: from then on a disconnect is the signal and timeout the clock.
	seen  bool
	timer *time.Timer
}

// newIdleWatcher returns nil when the timeout is zero, which disables the
// behaviour: a nil watcher is never registered with the gRPC server, and its
// Fired channel blocks forever. A zero startWait leaves a component whose
// sidecar never connects running indefinitely.
func newIdleWatcher(timeout, startWait time.Duration) *idleWatcher {
	if timeout <= 0 {
		return nil
	}
	return &idleWatcher{timeout: timeout, startWait: startWait, fired: make(chan string, 1)}
}

// Start begins the wait for the sidecar's first connection. The run loop calls
// it once the listener is up, so the window covers only the time the socket is
// actually dialable.
func (w *idleWatcher) Start() {
	if w == nil || w.startWait <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen || w.timer != nil {
		return
	}
	w.timer = w.arm(w.startWait, fmt.Sprintf("the sidecar never connected within %s", w.startWait))
}

// Fired is closed-over by the run loop and yields the reason to log. A nil
// channel blocks forever in a select, which is exactly what a disabled watcher
// should do.
func (w *idleWatcher) Fired() <-chan string {
	if w == nil {
		return nil
	}
	return w.fired
}

func (w *idleWatcher) HandleConn(_ context.Context, s stats.ConnStats) {
	switch s.(type) {
	case *stats.ConnBegin:
		w.mu.Lock()
		w.active++
		w.seen = true
		// Discovery connects, calls the reflection API and disconnects before
		// the sidecar dials again to initialise the component. Re-arming on
		// every new connection keeps that gap from counting as the sidecar
		// having gone away. This also disarms the startWait timer.
		if w.timer != nil {
			w.timer.Stop()
			w.timer = nil
		}
		w.mu.Unlock()
	case *stats.ConnEnd:
		w.mu.Lock()
		if w.active > 0 {
			w.active--
		}
		if w.active == 0 && w.seen && w.timer == nil {
			w.timer = w.arm(w.timeout, fmt.Sprintf("no sidecar connection for %s", w.timeout))
		}
		w.mu.Unlock()
	}
}

// arm must be called with the mutex held.
func (w *idleWatcher) arm(d time.Duration, reason string) *time.Timer {
	return time.AfterFunc(d, func() { w.expire(reason) })
}

func (w *idleWatcher) expire(reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A connection arriving between the timer firing and this lock means the
	// sidecar is here; Stop() alone cannot rule that out.
	if w.active > 0 {
		return
	}
	select {
	case w.fired <- reason:
	default:
	}
}

// The rest of stats.Handler. Only connection events matter here, and the
// per-RPC ones must stay cheap: they run on every request.
func (w *idleWatcher) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (w *idleWatcher) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (w *idleWatcher) HandleRPC(context.Context, stats.RPCStats)                         {}

var _ stats.Handler = (*idleWatcher)(nil)
