// Package pool bounds and reuses FTP sessions.
//
// A single FTP session is not safe for concurrent use and supports only one
// in-flight data connection, so every operation must hold a session
// exclusively. Many servers also cap concurrent logins per user, so the number
// of sessions must be bounded and reconnection must be cheap.
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient"
)

// ErrClosed is returned once the pool has been closed.
var ErrClosed = errors.New("ftp: connection pool is closed")

// Defaults for the timing knobs, which are not exposed as component metadata:
// they are implementation detail rather than deployment policy.
const (
	// DefaultMaxIdle sits below vsftpd's idle_session_timeout default of 300s
	// so that we close idle sessions before the server does.
	DefaultMaxIdle      = 4 * time.Minute
	DefaultReapInterval = 30 * time.Second
)

// Options configures a Pool.
type Options struct {
	Max          int           // maximum concurrent sessions; must be >= 1
	MaxIdle      time.Duration // close sessions idle longer than this
	ReapInterval time.Duration // how often the reaper runs
}

func (o *Options) withDefaults() {
	if o.Max < 1 {
		o.Max = 1
	}
	if o.MaxIdle <= 0 {
		o.MaxIdle = DefaultMaxIdle
	}
	if o.ReapInterval <= 0 {
		o.ReapInterval = DefaultReapInterval
	}
}

type entry struct {
	conn     ftpclient.Conn
	lastUsed time.Time
}

// Pool hands out exclusive sessions.
//
// The invariant is: len(tokens) + live == Max, where live counts sessions that
// are either held by a Handle or waiting in idle. A token is spent when a
// session is CREATED and returned when it is CLOSED, never merely when it is
// borrowed. That is what lets idle hold at most Max entries, so returning a
// session to idle can never block, which removes the classic pool deadlock.
type Pool struct {
	dialer ftpclient.Dialer
	opt    Options

	idle   chan *entry   // warm, released sessions
	tokens chan struct{} // permission to own a session
	closed chan struct{} // closed exactly once by Close
	once   sync.Once
	wg     sync.WaitGroup

	// mu guards live and closing. Release makes its discard-or-return decision
	// under this lock, and Close sets closing under it before draining, so the
	// two can never interleave such that a session is pushed into an
	// already-drained pool and never closed.
	mu      sync.Mutex
	live    int
	closing bool
}

// Stats reports pool occupancy, for logging and tests.
type Stats struct {
	Live   int // sessions open right now
	Idle   int // of those, sitting unused
	Tokens int // room to open more
	Max    int
}

// New creates a pool and starts its reaper.
func New(d ftpclient.Dialer, opt Options) *Pool {
	opt.withDefaults()
	p := &Pool{
		dialer: d,
		opt:    opt,
		idle:   make(chan *entry, opt.Max),
		tokens: make(chan struct{}, opt.Max),
		closed: make(chan struct{}),
	}
	for i := 0; i < opt.Max; i++ {
		p.tokens <- struct{}{}
	}
	p.wg.Add(1)
	go p.reap()
	return p
}

// Handle is an exclusive borrow of one session. Release it exactly once.
type Handle struct {
	e      *entry
	p      *Pool
	broken bool
}

// Conn returns the borrowed session.
func (h *Handle) Conn() ftpclient.Conn { return h.e.conn }

// MarkBroken prevents the session from returning to the pool.
func (h *Handle) MarkBroken() { h.broken = true }

// Release returns the session. Calling it more than once is a no-op.
func (h *Handle) Release() {
	if h.p == nil {
		return
	}
	p := h.p
	h.p = nil

	dead := h.broken || h.e.conn.Broken()
	now := time.Now()

	p.mu.Lock()
	if p.closing {
		dead = true
	}
	if !dead {
		h.e.lastUsed = now
		// Cannot block: cap(idle) == Max and live <= Max. Sending under the lock
		// is what makes this atomic with Close's drain.
		p.idle <- h.e
		p.mu.Unlock()
		return
	}
	// discard also takes p.mu, so unwind the bookkeeping inline instead.
	p.live--
	p.mu.Unlock()

	_ = h.e.conn.Close()
	p.tokens <- struct{}{}
}

// Acquire borrows a session, dialling one if the pool has room. It blocks
// until a session is free, the pool closes, or ctx is done.
func (p *Pool) Acquire(ctx context.Context) (*Handle, error) {
	for {
		select {
		case <-p.closed:
			return nil, ErrClosed
		default:
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Prefer a warm session.
		select {
		case e := <-p.idle:
			if h := p.vet(e); h != nil {
				return h, nil
			}
			continue // vet closed it and returned the token
		default:
		}

		// Otherwise grow, if we are under Max.
		select {
		case <-p.tokens:
			return p.born(ctx)
		default:
		}

		// At capacity: take whichever becomes available first.
		select {
		case e := <-p.idle:
			if h := p.vet(e); h != nil {
				return h, nil
			}
		case <-p.tokens:
			return p.born(ctx)
		case <-p.closed:
			return nil, ErrClosed
		case <-ctx.Done():
			return nil, fmt.Errorf(
				"ftp: timed out waiting for a free connection to %s (maxConnections=%d, all in use): %w",
				p.dialer.Target(), p.opt.Max, ctx.Err())
		}
	}
}

// born spends a token and dials.
func (p *Pool) born(ctx context.Context) (*Handle, error) {
	conn, err := p.dialer.Dial(ctx)
	if err != nil {
		p.tokens <- struct{}{} // never blocks
		return nil, fmt.Errorf("ftp: connecting to %s: %w", p.dialer.Target(), err)
	}
	p.mu.Lock()
	p.live++
	p.mu.Unlock()
	return &Handle{e: &entry{conn: conn, lastUsed: time.Now()}, p: p}, nil
}

// vet returns a usable handle, or nil after closing a dead session and
// returning its token.
//
// There is deliberately no liveness probe. Operations are not context-aware, so
// a NOOP against a half-dead peer could block indefinitely and pin the pool
// slot it was meant to protect. A session that died while idle instead surfaces
// as a connection-level failure on first use, which withConn retries once on a
// fresh session — the same approach the built-in SFTP binding takes.
func (p *Pool) vet(e *entry) *Handle {
	if e.conn.Broken() || time.Since(e.lastUsed) >= p.opt.MaxIdle {
		p.discard(e)
		return nil
	}
	return &Handle{e: e, p: p}
}

// discard closes a session and returns its token.
func (p *Pool) discard(e *entry) {
	_ = e.conn.Close()
	p.mu.Lock()
	p.live--
	p.mu.Unlock()
	p.tokens <- struct{}{}
}

// reap closes sessions that have been idle too long, so we drop them before
// the server does.
func (p *Pool) reap() {
	defer p.wg.Done()
	t := time.NewTicker(p.opt.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-p.closed:
			return
		case <-t.C:
			p.reapOnce()
		}
	}
}

func (p *Pool) reapOnce() {
	n := len(p.idle)
	keep := make([]*entry, 0, n)
	for i := 0; i < n; i++ {
		select {
		case e := <-p.idle:
			if e.conn.Broken() || time.Since(e.lastUsed) >= p.opt.MaxIdle {
				p.discard(e)
				continue
			}
			keep = append(keep, e)
		default:
			i = n
		}
	}
	for _, e := range keep {
		select {
		case p.idle <- e:
		default:
			p.discard(e) // should be unreachable given the capacity invariant
		}
	}
}

// Stats reports current occupancy.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	live := p.live
	p.mu.Unlock()
	return Stats{Live: live, Idle: len(p.idle), Tokens: len(p.tokens), Max: p.opt.Max}
}

// Close shuts the pool down and quits every idle session. Sessions currently
// held by a Handle are closed when their Handle is released.
func (p *Pool) Close() error {
	p.once.Do(func() {
		// Set closing before draining, so a Release that has not yet taken the
		// lock discards its session rather than pushing it into a pool nobody
		// will drain again.
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()

		close(p.closed)
		p.wg.Wait()
		for {
			select {
			case e := <-p.idle:
				p.discard(e)
			default:
				return
			}
		}
	})
	return nil
}
