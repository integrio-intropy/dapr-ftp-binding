package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
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

func TestZeroTimeoutDisablesTheWatcher(t *testing.T) {
	w := newIdleWatcher(0)
	if w != nil {
		t.Fatalf("newIdleWatcher(0) = %v, want nil so it is never registered", w)
	}
	// A nil watcher's channel must block rather than fire, or every Deployment
	// would exit as soon as the run loop reached its select.
	if fired(t, w, quick) {
		t.Fatal("a disabled watcher fired; Deployments would exit immediately")
	}
}

// The component may be running for a while before daprd dials it. Firing in
// that window would turn a slow sidecar start into a vanished component.
func TestDoesNotFireBeforeTheSidecarHasEverConnected(t *testing.T) {
	w := newIdleWatcher(quick)
	if fired(t, w, 4*quick) {
		t.Fatal("fired before any connection was seen")
	}
}

func TestFiresAfterTheLastConnectionCloses(t *testing.T) {
	w := newIdleWatcher(quick)
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
	w := newIdleWatcher(quick)
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
	w := newIdleWatcher(quick)
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

	w := newIdleWatcher(quick)
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
