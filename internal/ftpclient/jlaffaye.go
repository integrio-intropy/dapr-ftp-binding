package ftpclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftperr"
)

// TLSMode mirrors config.TLSMode without importing it, keeping this package
// free of a dependency on configuration parsing.
type TLSMode uint8

// These must stay in the same order as config.TLSMode's constants: the command
// wiring converts one to the other numerically.
const (
	TLSNone TLSMode = iota
	TLSExplicit
	TLSImplicit
)

// keepAlive asks the OS to probe idle connections. With no per-operation
// deadlines this is the only thing that eventually frees a session whose peer
// has gone away silently; the system default is measured in hours.
const keepAlive = 30 * time.Second

// DialerConfig is everything the adapter needs to open a session.
type DialerConfig struct {
	Address     string
	Username    string
	Password    string
	TLSMode     TLSMode
	TLSConfig   *tls.Config
	Timeout     time.Duration
	DisableEPSV bool
	TrustPASVIP bool
	TargetName  string
}

// NewDialer returns a Dialer backed by github.com/jlaffaye/ftp.
//
// This is the only place in the module that imports that library.
func NewDialer(cfg DialerConfig) Dialer {
	if cfg.Username == "" {
		cfg.Username = "anonymous"
		if cfg.Password == "" {
			cfg.Password = "anonymous@"
		}
	}
	return &jlDialer{cfg: cfg}
}

type jlDialer struct{ cfg DialerConfig }

func (d *jlDialer) Target() string { return d.cfg.TargetName }

func (d *jlDialer) Dial(ctx context.Context) (Conn, error) {
	// The library handles TLS on both the control and the data channel itself.
	// That only holds while we do NOT pass DialWithDialFunc: openDataConn
	// (ftp.go:595) hands back a custom dial func's socket before reaching its own
	// tls.Client wrapping, which would leave the data channel in cleartext with
	// no error. DialWithDialer gives us dial timeouts and keepalive for both
	// channels (ftp.go:612, 620) without that hazard.
	opts := []ftp.DialOption{
		ftp.DialWithContext(ctx),
		ftp.DialWithDialer(net.Dialer{Timeout: d.cfg.Timeout, KeepAlive: keepAlive}),
		ftp.DialWithTimeout(d.cfg.Timeout),
		ftp.DialWithDisabledEPSV(d.cfg.DisableEPSV),
		ftp.DialWithTrustPasvIP(d.cfg.TrustPASVIP),
	}
	switch d.cfg.TLSMode {
	case TLSExplicit:
		opts = append(opts, ftp.DialWithExplicitTLS(d.cfg.TLSConfig))
	case TLSImplicit:
		opts = append(opts, ftp.DialWithTLS(d.cfg.TLSConfig))
	}

	sc, err := ftp.Dial(d.cfg.Address, opts...)
	if err != nil {
		return nil, err
	}
	if err := sc.Login(d.cfg.Username, d.cfg.Password); err != nil {
		_ = sc.Quit()
		// Name the account: a 530 otherwise gives the operator nothing to check.
		return nil, fmt.Errorf("login as %q failed: %w", d.cfg.Username, err)
	}
	return &jlConn{sc: sc}, nil
}

type jlConn struct {
	sc     *ftp.ServerConn
	broken atomic.Bool
	closed atomic.Bool
}

// Broken reports that the session is no longer usable, so the pool closes it
// rather than handing it to another request.
func (c *jlConn) Broken() bool { return c.broken.Load() }

func (c *jlConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.sc.Quit()
}

// begin rejects work on a dead session and honours a context that has already
// been cancelled.
//
// Operations are not interruptible once started: the FTP library's calls are
// not context-aware, and the only way to interrupt them is to hold the raw
// sockets, which in turn disables the library's TLS handling. Cancellation
// therefore takes effect between operations, and a peer that has gone away
// silently is caught by TCP keepalive. The SFTP binding ignores context
// entirely, so this is already a little stricter than the built-in equivalent.
func (c *jlConn) begin(ctx context.Context) error {
	if c.closed.Load() || c.broken.Load() {
		return ftperr.ErrBrokenConn
	}
	return ctx.Err()
}

// end marks the session unusable when the failure was at the connection level,
// so the pool discards it and withConn retries on a fresh one.
func (c *jlConn) end(err error) error {
	if err != nil && ftperr.IsConnectionLevel(err) {
		c.broken.Store(true)
	}
	return err
}

func (c *jlConn) Store(ctx context.Context, remotePath string, r io.Reader) error {
	if err := c.begin(ctx); err != nil {
		return err
	}
	if err := c.sc.Type(ftp.TransferTypeBinary); err != nil {
		return c.end(err)
	}
	// StorFrom already joins the data-connection close error with the server's
	// post-transfer status, so no status is lost here.
	return c.end(c.sc.Stor(remotePath, r))
}

func (c *jlConn) Retrieve(ctx context.Context, remotePath string, w io.Writer) (int64, error) {
	if err := c.begin(ctx); err != nil {
		return 0, err
	}
	if err := c.sc.Type(ftp.TransferTypeBinary); err != nil {
		return 0, c.end(err)
	}
	resp, err := c.sc.Retr(remotePath)
	if err != nil {
		return 0, c.end(err)
	}

	n, copyErr := io.Copy(w, resp)

	// Response.Close carries the server's post-transfer status. Discarding it
	// makes a transfer that failed server-side look successful, which is the
	// defect the upstream contrib PR shipped.
	closeErr := resp.Close()

	if copyErr != nil {
		// We stopped reading early, so the session state is undefined.
		c.broken.Store(true)
		return n, copyErr
	}
	return n, c.end(closeErr)
}

func (c *jlConn) Delete(ctx context.Context, remotePath string) error {
	if err := c.begin(ctx); err != nil {
		return err
	}
	return c.end(c.sc.Delete(remotePath))
}

func (c *jlConn) List(ctx context.Context, remotePath string) ([]Entry, error) {
	if err := c.begin(ctx); err != nil {
		return nil, err
	}
	entries, err := c.sc.List(remotePath)
	if err != nil {
		return nil, c.end(err)
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		out = append(out, Entry{
			Name:    e.Name,
			Type:    entryType(e.Type),
			Size:    e.Size,
			ModTime: e.Time,
		})
	}
	return out, nil
}

func entryType(t ftp.EntryType) EntryType {
	switch t {
	case ftp.EntryTypeFolder:
		return EntryDir
	case ftp.EntryTypeLink:
		return EntryLink
	default:
		return EntryFile
	}
}
