package main

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"
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
// The signal is daprd's HTTP port on localhost, which every container in a pod
// shares and which daprd listens on for as long as it runs. Our own gRPC
// socket is NOT a usable signal for this, despite looking like one: grpc-go's
// client-side connection idling closes daprd's connection to us after about
// 30 minutes without an Invoke call, then reconnects transparently on the next
// one -- ordinary behaviour that has nothing to do with whether the sidecar is
// still alive. Watching our own socket for "every connection closed" mistakes
// that routine idle-and-reconnect cycle for the sidecar vanishing, and exits
// a component that a live app is about to call again. (dapr-smb-binding hit
// this first; its idle.py documents the same 30-minute figure.)
//
// A sidecar that never shows up at all is folded into the same signal: if
// daprd never starts, or dies before reaching us, its HTTP port never answers
// either, so "never seen running" and "seen running, then gone" are just two
// branches of one poll loop.
//
// It is off by default, and must be off for Deployments: there a sidecar
// restart is routine, and the component has to still be listening afterwards.
type idleWatcher struct {
	timeout   time.Duration
	startWait time.Duration
	fired     chan string

	// running reports whether daprd looks alive. A field rather than a free
	// function so tests can substitute a fake without touching a real port.
	running func() bool
	// pollInterval paces the check. A field for the same reason: tests shrink
	// it so the suite does not take minutes to run the real intervals.
	pollInterval time.Duration

	once sync.Once
}

// daprHTTPPortEnv is how the Dapr injector tells every container in the pod
// which port daprd's HTTP API listens on.
const daprHTTPPortEnv = "DAPR_HTTP_PORT"

// defaultDaprHTTPPort is what the injector uses when it has not overridden it.
const defaultDaprHTTPPort = "3500"

const (
	defaultPollInterval = 1 * time.Second
	dialTimeout         = 1 * time.Second
)

// newIdleWatcher returns nil when the timeout is zero, which disables the
// behaviour: a nil watcher's Fired channel blocks forever. A zero startWait
// leaves a component whose sidecar never shows up running indefinitely.
func newIdleWatcher(timeout, startWait time.Duration) *idleWatcher {
	if timeout <= 0 {
		return nil
	}
	return &idleWatcher{
		timeout:      timeout,
		startWait:    startWait,
		fired:        make(chan string, 1),
		running:      sidecarRunning,
		pollInterval: defaultPollInterval,
	}
}

// Start begins polling for daprd. Safe to call on a nil watcher, and more than
// once on the same one.
func (w *idleWatcher) Start() {
	if w == nil {
		return
	}
	w.once.Do(func() { go w.watch() })
}

// Fired yields the reason to log once the watcher decides the sidecar is
// gone. A nil channel blocks forever in a select, which is exactly what a
// disabled watcher should do.
func (w *idleWatcher) Fired() <-chan string {
	if w == nil {
		return nil
	}
	return w.fired
}

// watch polls daprd until it decides the sidecar is gone for good, then exits.
//
// seen retires the startup wait: from then on, a quiet daprd is the idle
// timeout's problem, not the startup wait's.
func (w *idleWatcher) watch() {
	t := time.NewTicker(w.pollInterval)
	defer t.Stop()

	start := time.Now()
	var seen bool
	var goneSince time.Time

	for range t.C {
		if w.running() {
			seen = true
			goneSince = time.Time{}
			continue
		}

		if !seen {
			if w.startWait > 0 && time.Since(start) >= w.startWait {
				w.fire(fmt.Sprintf("the sidecar never connected within %s", w.startWait))
				return
			}
			continue
		}

		if goneSince.IsZero() {
			goneSince = time.Now()
			continue
		}
		if time.Since(goneSince) >= w.timeout {
			w.fire(fmt.Sprintf("no sidecar connection for %s", w.timeout))
			return
		}
	}
}

func (w *idleWatcher) fire(reason string) {
	select {
	case w.fired <- reason:
	default:
	}
}

// sidecarRunning reports whether daprd's HTTP API port is accepting
// connections on localhost. That port is up for as long as daprd runs,
// independent of whether it currently holds a connection to our socket.
func sidecarRunning() bool {
	port := os.Getenv(daprHTTPPortEnv)
	if port == "" {
		port = defaultDaprHTTPPort
	}
	//nolint:gosec // G704: the host is the literal "127.0.0.1"; port comes from the
	// pod's own DAPR_HTTP_PORT, set by the Dapr injector, not external input.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), dialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
