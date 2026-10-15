package main

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/stats"
)

// idleWatcher reports when the sidecar has gone away, so the component can exit
// instead of outliving it.
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
// It is off by default, and must be off for Deployments: there a sidecar
// restart is routine, and the component has to still be listening afterwards.
type idleWatcher struct {
	timeout time.Duration
	fired   chan struct{}

	mu     sync.Mutex
	active int
	// seen suppresses the whole mechanism until the sidecar has connected at
	// least once, so a slow-starting daprd does not find us already gone.
	seen  bool
	timer *time.Timer
}

// newIdleWatcher returns nil when the timeout is zero, which disables the
// behaviour: a nil watcher is never registered with the gRPC server, and its
// Fired channel blocks forever.
func newIdleWatcher(timeout time.Duration) *idleWatcher {
	if timeout <= 0 {
		return nil
	}
	return &idleWatcher{timeout: timeout, fired: make(chan struct{}, 1)}
}

// Fired is closed-over by the run loop. A nil channel blocks forever in a
// select, which is exactly what a disabled watcher should do.
func (w *idleWatcher) Fired() <-chan struct{} {
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
		// having gone away.
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
			w.timer = time.AfterFunc(w.timeout, w.expire)
		}
		w.mu.Unlock()
	}
}

func (w *idleWatcher) expire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A connection arriving between the timer firing and this lock means the
	// sidecar is back; Stop() alone cannot rule that out.
	if w.active > 0 {
		return
	}
	select {
	case w.fired <- struct{}{}:
	default:
	}
}

// The rest of stats.Handler. Only connection events matter here, and the
// per-RPC ones must stay cheap: they run on every request.
func (w *idleWatcher) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (w *idleWatcher) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (w *idleWatcher) HandleRPC(context.Context, stats.RPCStats)                         {}

var _ stats.Handler = (*idleWatcher)(nil)
