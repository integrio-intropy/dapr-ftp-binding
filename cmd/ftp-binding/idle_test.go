package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
)

// quick is long enough not to be flaky on a loaded CI runner, short enough that
// the whole file runs in well under a second.
const quick = 150 * time.Millisecond

func begin(w *idleWatcher) { w.HandleConn(context.Background(), &stats.ConnBegin{}) }
func end(w *idleWatcher)   { w.HandleConn(context.Background(), &stats.ConnEnd{}) }

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

// started builds a watcher with the startup wait armed, as the run loop does
// once the socket is listening.
func started(timeout, startWait time.Duration) *idleWatcher {
	w := newIdleWatcher(timeout, startWait)
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

// The component may be running for a while before daprd dials it. Firing after
// the idle timeout alone would turn a slow sidecar start into a vanished
// component: before the first connection it is the longer startup wait that
// applies.
func TestDoesNotFireOnTheIdleTimeoutBeforeTheSidecarHasEverConnected(t *testing.T) {
	w := started(quick, 100*quick)
	if fired(t, w, 4*quick) {
		t.Fatal("fired on the idle timeout before any connection was seen")
	}
}

// A zero startup wait keeps the old behaviour: hold the socket open forever for
// a sidecar that may yet arrive.
func TestZeroStartWaitWaitsForeverForTheFirstConnection(t *testing.T) {
	w := started(quick, 0)
	if fired(t, w, 6*quick) {
		t.Fatal("fired although the startup wait was disabled")
	}
}

// The case this is all for: daprd dies before it ever dials us -- typically
// because it scanned the socket folder before our socket existed -- so no
// disconnect is ever coming. Without this the Job runs forever.
func TestFiresWhenTheSidecarNeverConnects(t *testing.T) {
	w := started(100*quick, quick)
	select {
	case reason := <-w.Fired():
		// The reason is logged as the process exits, and is the only clue an
		// operator gets that the sidecar was the thing that went missing.
		if !strings.Contains(reason, "never connected") {
			t.Fatalf("reason = %q, want it to say the sidecar never connected", reason)
		}
	case <-time.After(20 * quick):
		t.Fatal("did not fire although no sidecar ever connected")
	}
}

// A sidecar that arrives within the startup wait disarms it for good: from then
// on only a disconnect plus the idle timeout may fire.
func TestTheFirstConnectionDisarmsTheStartupWait(t *testing.T) {
	w := started(100*quick, 2*quick)
	begin(w)
	if fired(t, w, 6*quick) {
		t.Fatal("fired on the startup wait although the sidecar had connected")
	}
}

// Disconnecting before the startup wait would have elapsed hands over to the
// idle timeout rather than leaving the startup timer to fire early.
func TestDisconnectAfterConnectingUsesTheIdleTimeout(t *testing.T) {
	w := started(4*quick, 100*quick)
	begin(w)
	end(w)
	if !fired(t, w, 40*quick) {
		t.Fatal("did not fire on the idle timeout after the sidecar disconnected")
	}
}

func TestFiresAfterTheLastConnectionCloses(t *testing.T) {
	w := started(quick, 100*quick)
	begin(w)
	if fired(t, w, 2*quick) {
		t.Fatal("fired while the sidecar was still connected")
	}
	end(w)
	if !fired(t, w, 20*quick) {
		t.Fatal("did not fire after the last connection closed")
	}
}

func TestAnotherLiveConnectionKeepsItQuiet(t *testing.T) {
	w := started(quick, 100*quick)
	begin(w)
	begin(w)
	end(w)
	if fired(t, w, 4*quick) {
		t.Fatal("fired while one of two connections was still open")
	}
}

// Discovery dials, calls the reflection API, hangs up, and only then does the
// sidecar connect again to initialise the component. That gap must not count.
func TestReconnectingDisarmsTheTimer(t *testing.T) {
	w := started(quick, 100*quick)
	begin(w)
	end(w)
	time.Sleep(quick / 3)
	begin(w)

	if fired(t, w, 4*quick) {
		t.Fatal("fired although the sidecar had reconnected")
	}

	end(w)
	if !fired(t, w, 20*quick) {
		t.Fatal("did not fire after the reconnected session closed")
	}
}

// End to end over the real thing: a gRPC server on a Unix socket, the watcher
// installed as its stats handler, and a client that connects and goes away.
// This is what proves the wiring, not just the bookkeeping.
func TestFiresWhenARealClientDisconnects(t *testing.T) {
	// Not t.TempDir(): a Unix socket path is capped at 104 bytes on macOS and
	// 108 on Linux, and the per-test temporary directory alone overruns that.
	dir, err := os.MkdirTemp("", "ftpsock")
	if err != nil {
		t.Fatalf("creating a socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "ftp.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}

	w := started(quick, 100*quick)
	server := grpc.NewServer(grpc.StatsHandler(w))
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("creating the client: %v", err)
	}
	conn.Connect()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for conn.GetState() != connectivity.Ready {
		if !conn.WaitForStateChange(ctx, conn.GetState()) {
			t.Fatalf("client never reached Ready: %v", ctx.Err())
		}
	}

	if fired(t, w, 2*quick) {
		t.Fatal("fired while the client was connected")
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("closing the client: %v", err)
	}
	if !fired(t, w, 20*quick) {
		t.Fatal("did not fire after the client disconnected")
	}
}
