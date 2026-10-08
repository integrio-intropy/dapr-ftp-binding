package pool

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient/fake"
)

func newTestPool(t *testing.T, b *fake.Backend, maxConns int) *Pool {
	t.Helper()
	p := New(b.Dialer(), Options{Max: maxConns, ReapInterval: time.Hour})
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// assertInvariant checks live + tokens == Max, the property that keeps the
// pool from leaking or over-dialling.
func assertInvariant(t *testing.T, p *Pool) {
	t.Helper()
	s := p.Stats()
	if s.Live+s.Tokens != s.Max {
		t.Fatalf("invariant violated: live=%d + tokens=%d != max=%d", s.Live, s.Tokens, s.Max)
	}
}

func TestAcquireReleaseReuses(t *testing.T) {
	b := fake.NewBackend()
	p := newTestPool(t, b, 2)
	ctx := context.Background()

	h, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.Release()
	assertInvariant(t, p)

	h2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h2.Release()

	if got := b.Dials(); got != 1 {
		t.Errorf("Dials = %d, want 1 (the session should have been reused)", got)
	}
	assertInvariant(t, p)
}

func TestPoolIsBoundedByMax(t *testing.T) {
	b := fake.NewBackend()
	p := newTestPool(t, b, 3)
	ctx := context.Background()

	var handles []*Handle
	for i := 0; i < 3; i++ {
		h, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		handles = append(handles, h)
	}
	if s := p.Stats(); s.Tokens != 0 || s.Live != 3 {
		t.Fatalf("stats = %+v, want live=3 tokens=0", s)
	}

	// A fourth acquire must block until one is released.
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(tctx); err == nil {
		t.Fatal("expected the fourth acquire to block and time out")
	}

	for _, h := range handles {
		h.Release()
	}
	assertInvariant(t, p)
}

// Exhaustion must name the knob the operator has to turn.
func TestExhaustionErrorNamesMaxConnections(t *testing.T) {
	b := fake.NewBackend()
	p := newTestPool(t, b, 1)
	ctx := context.Background()

	h, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer h.Release()

	tctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(tctx)
	if err == nil {
		t.Fatal("expected an exhaustion error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should wrap the context error, got %v", err)
	}
	if want := "maxConnections=1"; !contains(err.Error(), want) {
		t.Errorf("error %q should mention %q", err, want)
	}
}

func TestBrokenConnectionIsClosedAndTokenReturned(t *testing.T) {
	b := fake.NewBackend()
	p := newTestPool(t, b, 2)
	ctx := context.Background()

	h, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.MarkBroken()
	h.Release()

	assertInvariant(t, p)
	if s := p.Stats(); s.Live != 0 {
		t.Errorf("live = %d, want 0 after releasing a broken session", s.Live)
	}
	if b.LiveSessions() != 0 {
		t.Errorf("backend still has %d live sessions", b.LiveSessions())
	}
}

func TestDialFailureReturnsToken(t *testing.T) {
	b := fake.NewBackend()
	b.DialErr = errors.New("connection refused")
	p := newTestPool(t, b, 2)

	_, err := p.Acquire(context.Background())
	if err == nil {
		t.Fatal("expected a dial error")
	}
	if !contains(err.Error(), "connection refused") {
		t.Errorf("error should include the cause, got %v", err)
	}
	assertInvariant(t, p)
	if s := p.Stats(); s.Tokens != 2 {
		t.Errorf("tokens = %d, want 2 after a failed dial", s.Tokens)
	}
}

func TestDoubleReleaseIsNoop(t *testing.T) {
	b := fake.NewBackend()
	p := newTestPool(t, b, 1)

	h, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.Release()
	h.Release() // must not panic or corrupt the pool
	assertInvariant(t, p)
}

func TestClosedPoolRejects(t *testing.T) {
	b := fake.NewBackend()
	p := New(b.Dialer(), Options{Max: 1})
	h, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.Release()

	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second close should be a no-op: %v", err)
	}
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("acquire after close = %v, want ErrClosed", err)
	}
	if b.LiveSessions() != 0 {
		t.Errorf("close left %d sessions open", b.LiveSessions())
	}
}

func TestCloseWhileHandleOutstanding(t *testing.T) {
	b := fake.NewBackend()
	p := New(b.Dialer(), Options{Max: 2})
	h, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	h.Release() // the session must be closed, not returned to a closed pool
	if b.LiveSessions() != 0 {
		t.Errorf("backend still has %d live sessions", b.LiveSessions())
	}
}

// The soak test is the one that proves a single session is never used by two
// goroutines at once, and that the pool never exceeds its bound.
func TestConcurrentUseIsSerializedPerConnection(t *testing.T) {
	const (
		maxConns   = 3
		goroutines = 50
		iterations = 40
	)
	b := fake.NewBackend()
	p := newTestPool(t, b, maxConns)
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				h, err := p.Acquire(ctx)
				if err != nil {
					errCh <- err
					return
				}
				if err := h.Conn().Store(ctx, "/f", bytes.NewReader([]byte("x"))); err != nil {
					h.MarkBroken()
					h.Release()
					errCh <- err
					return
				}
				h.Release()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("worker failed: %v", err)
	}

	if b.SawConcurrentUse() {
		t.Fatal("a single Conn was used by two goroutines at once")
	}
	if got := b.MaxLiveSessions(); got > maxConns {
		t.Errorf("peak sessions = %d, exceeds max of %d", got, maxConns)
	}
	assertInvariant(t, p)
}

func TestReaperClosesIdleConnections(t *testing.T) {
	b := fake.NewBackend()
	p := New(b.Dialer(), Options{
		Max: 2, MaxIdle: time.Millisecond, ReapInterval: 5 * time.Millisecond,
	})
	t.Cleanup(func() { _ = p.Close() })

	h, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.Release()

	deadline := time.After(2 * time.Second)
	for p.Stats().Live != 0 {
		select {
		case <-deadline:
			t.Fatalf("reaper did not close the idle session: %+v", p.Stats())
		case <-time.After(5 * time.Millisecond):
		}
	}
	assertInvariant(t, p)
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Release must not hand a session to a pool that Close has already drained.
//
// The decision to discard or return has to be atomic with the drain: checking
// "is the pool closed?" and then sending on the idle channel leaves a window
// where Close drains an empty channel and the session is pushed in afterwards,
// never closed, so the server sees a dropped connection instead of QUIT.
func TestConcurrentReleaseDuringClose(t *testing.T) {
	for attempt := 0; attempt < 400; attempt++ {
		b := fake.NewBackend()
		p := New(b.Dialer(), Options{Max: 8, ReapInterval: time.Hour})
		ctx := context.Background()

		// Hold a few sessions, as in-flight handlers would during shutdown.
		var handles []*Handle
		for i := 0; i < 8; i++ {
			h, err := p.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			handles = append(handles, h)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, h := range handles {
			wg.Add(1)
			go func(h *Handle) {
				defer wg.Done()
				<-start
				h.Release()
			}(h)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = p.Close()
		}()

		close(start)
		wg.Wait()

		if live := b.LiveSessions(); live != 0 {
			t.Fatalf("attempt %d: %d sessions left open after Close; a release raced the drain",
				attempt, live)
		}
	}
}
