// Package fake is an in-memory ftpclient implementation for tests.
//
// It deliberately implements the ftpclient interfaces rather than mimicking
// jlaffaye's types, so everything above the seam is testable without a socket.
//
// Every session asserts that no two of its methods ever run concurrently. That
// single check is what proves the pool honours the one-operation-per-connection
// rule, and it runs in every test that touches a pool, for free.
package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftperr"
)

// ErrConcurrentUse is reported when two operations overlap on one session.
var ErrConcurrentUse = errors.New("fake: concurrent use of a single Conn")

// Backend is the shared in-memory server behind every session.
type Backend struct {
	mu    sync.Mutex
	files map[string][]byte

	// DialErr, when set, makes every Dial fail.
	DialErr error
	// MaxSessions caps concurrent logins, mimicking a real server's per-user
	// limit. Zero means unlimited.
	MaxSessions int
	// Latency is slept inside every operation, for deadline tests.
	Latency time.Duration
	// FailNext returns a non-nil error to fail one operation. It is consulted
	// under the backend lock and may be replaced between calls.
	FailNext func(op, path string) error
	// StaleAfter makes a session report a connection-level failure on its nth
	// operation onwards (1-based). Zero disables it.
	StaleAfter int
	// FailRetrieveClose simulates a server reporting a transfer failure only
	// after the data stream ends, which is the bug upstream shipped.
	FailRetrieveClose bool

	dials        atomic.Int64
	liveSessions atomic.Int64
	maxLive      atomic.Int64
	concurrent   atomic.Bool
}

// NewBackend returns an empty backend.
func NewBackend() *Backend {
	return &Backend{files: map[string][]byte{}}
}

// Put seeds a file.
func (b *Backend) Put(p string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.files[p] = append([]byte(nil), data...)
}

// Get returns a stored file.
func (b *Backend) Get(p string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.files[p]
	return append([]byte(nil), d...), ok
}

// Paths returns every stored path, sorted.
func (b *Backend) Paths() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.files))
	for p := range b.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Dials is the number of sessions ever opened.
func (b *Backend) Dials() int64 { return b.dials.Load() }

// LiveSessions is the number of sessions currently open.
func (b *Backend) LiveSessions() int64 { return b.liveSessions.Load() }

// MaxLiveSessions is the high-water mark of concurrent sessions.
func (b *Backend) MaxLiveSessions() int64 { return b.maxLive.Load() }

// SawConcurrentUse reports whether any single Conn was ever used by two
// goroutines at once.
func (b *Backend) SawConcurrentUse() bool { return b.concurrent.Load() }

// Dialer returns a Dialer backed by b.
func (b *Backend) Dialer() ftpclient.Dialer { return &dialer{b: b} }

type dialer struct{ b *Backend }

func (d *dialer) Target() string { return "fake://memory" }

func (d *dialer) Dial(ctx context.Context) (ftpclient.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.b.DialErr != nil {
		return nil, d.b.DialErr
	}
	if d.b.MaxSessions > 0 && d.b.liveSessions.Load() >= int64(d.b.MaxSessions) {
		return nil, fmt.Errorf("fake: too many sessions (limit %d)", d.b.MaxSessions)
	}
	d.b.dials.Add(1)
	live := d.b.liveSessions.Add(1)
	for {
		old := d.b.maxLive.Load()
		if live <= old || d.b.maxLive.CompareAndSwap(old, live) {
			break
		}
	}
	return &conn{b: d.b}, nil
}

type conn struct {
	b      *Backend
	inUse  atomic.Int32
	ops    atomic.Int64
	broken atomic.Bool
	closed atomic.Bool
}

func (c *conn) Broken() bool { return c.broken.Load() }

func (c *conn) Close() error {
	if c.closed.Swap(true) {
		return nil // idempotent
	}
	c.b.liveSessions.Add(-1)
	return nil
}

// enter asserts single-threaded use and applies the configured faults.
func (c *conn) enter(ctx context.Context, op, p string) error {
	if c.inUse.Add(1) != 1 {
		c.b.concurrent.Store(true)
		c.inUse.Add(-1)
		return ErrConcurrentUse
	}
	if c.closed.Load() {
		c.inUse.Add(-1)
		return ftperr.ErrBrokenConn
	}
	if c.broken.Load() {
		c.inUse.Add(-1)
		return ftperr.ErrBrokenConn
	}

	if c.b.Latency > 0 {
		select {
		case <-time.After(c.b.Latency):
		case <-ctx.Done():
			c.broken.Store(true)
			c.inUse.Add(-1)
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		c.broken.Store(true)
		c.inUse.Add(-1)
		return err
	}

	n := c.ops.Add(1)
	if c.b.StaleAfter > 0 && n >= int64(c.b.StaleAfter) {
		c.broken.Store(true)
		c.inUse.Add(-1)
		return fmt.Errorf("fake: session went stale: %w", io.ErrUnexpectedEOF)
	}
	c.b.mu.Lock()
	fail := c.b.FailNext
	c.b.mu.Unlock()
	if fail != nil {
		if err := fail(op, p); err != nil {
			c.inUse.Add(-1)
			if ftperr.IsConnectionLevel(err) {
				c.broken.Store(true)
			}
			return err
		}
	}
	return nil
}

func (c *conn) exit() { c.inUse.Add(-1) }

func (c *conn) Store(ctx context.Context, p string, r io.Reader) error {
	if err := c.enter(ctx, "store", p); err != nil {
		return err
	}
	defer c.exit()
	data, err := io.ReadAll(r)
	if err != nil {
		c.broken.Store(true)
		return err
	}
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	c.b.files[p] = data
	return nil
}

func (c *conn) Retrieve(ctx context.Context, p string, w io.Writer) (int64, error) {
	if err := c.enter(ctx, "retrieve", p); err != nil {
		return 0, err
	}
	defer c.exit()
	c.b.mu.Lock()
	data, ok := c.b.files[p]
	failClose := c.b.FailRetrieveClose
	c.b.mu.Unlock()
	if !ok {
		return 0, &textproto.Error{Code: ftperr.CodeFileUnavailable, Msg: "Failed to open file"}
	}
	n, err := w.Write(data)
	if err != nil {
		// We stopped reading early, so the session state is undefined.
		c.broken.Store(true)
		return int64(n), err
	}
	if failClose {
		// The server reported the failure only after the data stream ended.
		c.broken.Store(true)
		return int64(n), &textproto.Error{Code: ftperr.CodeTransferAborted, Msg: "Transfer aborted"}
	}
	return int64(n), nil
}

func (c *conn) Delete(ctx context.Context, p string) error {
	if err := c.enter(ctx, "delete", p); err != nil {
		return err
	}
	defer c.exit()
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	if _, ok := c.b.files[p]; !ok {
		return &textproto.Error{Code: ftperr.CodeFileUnavailable, Msg: "No such file or directory"}
	}
	delete(c.b.files, p)
	return nil
}

func (c *conn) List(ctx context.Context, p string) ([]ftpclient.Entry, error) {
	if err := c.enter(ctx, "list", p); err != nil {
		return nil, err
	}
	defer c.exit()
	c.b.mu.Lock()
	defer c.b.mu.Unlock()

	prefix := strings.TrimSuffix(p, "/") + "/"
	seen := map[string]ftpclient.Entry{}
	for fp, data := range c.b.files {
		if fp == p {
			seen[path.Base(fp)] = ftpclient.Entry{Name: path.Base(fp), Type: ftpclient.EntryFile, Size: uint64(len(data))}
			continue
		}
		if !strings.HasPrefix(fp, prefix) {
			continue
		}
		rest := strings.TrimPrefix(fp, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			name := rest[:i]
			seen[name] = ftpclient.Entry{Name: name, Type: ftpclient.EntryDir}
			continue
		}
		seen[rest] = ftpclient.Entry{Name: rest, Type: ftpclient.EntryFile, Size: uint64(len(data))}
	}
	out := make([]ftpclient.Entry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
