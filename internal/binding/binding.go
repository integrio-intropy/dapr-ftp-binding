// Package binding implements the Dapr output binding for FTP and FTPS.
package binding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"sync"

	"github.com/dapr/components-contrib/bindings"
	contribmd "github.com/dapr/components-contrib/metadata"
	"github.com/dapr/kit/logger"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/config"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftperr"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/pool"
)

// Request metadata keys, matching the SFTP binding's convention.
const (
	metadataFileName = "fileName"

	contentTypeJSON   = "application/json"
	contentTypeBinary = "application/octet-stream"
)

// DialerFactory builds the dialer for a parsed config. It is a field so tests
// can substitute an in-memory backend.
type DialerFactory func(*config.Config) (ftpclient.Dialer, error)

// Binding is the Dapr output binding component.
type Binding struct {
	log     logger.Logger
	newDial DialerFactory

	mu   sync.RWMutex
	cfg  *config.Config
	root string
	pool *pool.Pool
}

// New returns a Binding that talks to real FTP servers.
func New(log logger.Logger, factory DialerFactory) *Binding {
	return &Binding{log: log, newDial: factory}
}

type createResponse struct {
	FileName string `json:"fileName"`
}

// listEntry matches the built-in SFTP binding's listing shape.
type listEntry struct {
	FileName    string `json:"fileName"`
	IsDirectory bool   `json:"isDirectory"`
}

// Init parses metadata, verifies the server is reachable and builds the pool.
func (b *Binding) Init(ctx context.Context, meta bindings.Metadata) error {
	cfg, warns, err := config.Parse(meta.Properties)
	if err != nil {
		return err
	}
	for _, w := range warns {
		b.log.Warn(w)
	}

	dialer, err := b.newDial(cfg)
	if err != nil {
		return err
	}

	root := cfg.RootPath
	// Connect once so a bad address or bad credentials fail at startup rather
	// than at the first request, as the built-in SFTP binding's newClient does.
	// The connection is closed rather than pooled.
	probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	conn, err := dialer.Dial(probeCtx)
	if err != nil {
		return fmt.Errorf("ftp: connecting to %s: %w", cfg.Target(), err)
	}
	_ = conn.Close()

	// Init is a gRPC method the sidecar may call more than once (restart, hot
	// reload), and the component instance is cached for the life of the process,
	// so the previous pool has to be closed or its sessions stay logged in and
	// its reaper goroutine keeps running with nothing able to reach them.
	b.mu.Lock()
	old := b.pool
	b.cfg = cfg
	b.root = root
	b.pool = pool.New(dialer, pool.Options{Max: cfg.MaxConnections})
	b.mu.Unlock()

	// Closed outside the lock: it sends QUIT on every pooled session.
	if old != nil {
		if err := old.Close(); err != nil {
			b.log.Warnf("closing the previous connection pool: %v", err)
		}
	}

	b.log.Infof("ftp binding ready: target=%s rootPath=%s maxConnections=%d timeout=%s",
		cfg.Target(), root, cfg.MaxConnections, cfg.Timeout)
	return nil
}

// Operations lists what this binding supports. Note that rename is not
// included; it is deliberately out of scope for this release.
func (b *Binding) Operations() []bindings.OperationKind {
	return []bindings.OperationKind{
		bindings.CreateOperation,
		bindings.GetOperation,
		bindings.ListOperation,
		bindings.DeleteOperation,
	}
}

// GetComponentMetadata describes the metadata fields for tooling.
func (b *Binding) GetComponentMetadata() map[string]string {
	m := map[string]string{}
	_ = contribmd.GetMetadataInfoFromStructType(reflect.TypeOf(config.RawType()), &m, contribmd.BindingType)
	return m
}

// Close releases the pool. The SDK never calls this, so cmd wires it to the
// process signal handler instead.
func (b *Binding) Close() error {
	b.mu.Lock()
	p := b.pool
	b.pool = nil
	b.mu.Unlock()
	if p != nil {
		return p.Close()
	}
	return nil
}

// Invoke dispatches an operation.
func (b *Binding) Invoke(ctx context.Context, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error) {
	if req == nil {
		return nil, errors.New("ftp: nil request")
	}
	b.mu.RLock()
	ready := b.pool != nil
	b.mu.RUnlock()
	if !ready {
		return nil, errors.New("ftp: binding is not initialised")
	}

	switch req.Operation {
	case bindings.CreateOperation:
		return b.create(ctx, req)
	case bindings.GetOperation:
		return b.get(ctx, req)
	case bindings.ListOperation:
		return b.list(ctx, req)
	case bindings.DeleteOperation:
		return b.delete(ctx, req)
	default:
		return nil, fmt.Errorf(
			"ftp: unsupported operation %q; this binding supports create, get, list and delete",
			req.Operation)
	}
}

func (b *Binding) snapshot() (*config.Config, string, *pool.Pool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cfg, b.root, b.pool
}

// resolve joins the requested fileName to rootPath.
//
// Like the built-in SFTP binding, this does not confine the result: the FTP
// server decides what the account can reach, via its own chroot and the
// permissions of the user we log in as.
func (b *Binding) resolve(req *bindings.InvokeRequest, allowRoot bool) (string, error) {
	_, root, _ := b.snapshot()
	name, _ := contribmd.GetMetadataProperty(req.Metadata, metadataFileName)
	if name == "" {
		if !allowRoot {
			return "", errors.New("required metadata fileName is missing")
		}
		return root, nil
	}
	return path.Join(root, name), nil
}

// withConn borrows a session and runs fn, retrying once on a fresh session if
// the failure was connection-level rather than the server refusing the command.
//
// fn must be safe to run twice.
func (b *Binding) withConn(ctx context.Context, op, remotePath string, fn func(context.Context, ftpclient.Conn) error) error {
	cfg, _, p := b.snapshot()
	if p == nil {
		return errors.New("ftp: binding is not initialised")
	}

	// Bound the whole operation even when the caller supplied no deadline.
	if _, ok := ctx.Deadline(); !ok && cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	var last error
	for attempt := 0; attempt < 2; attempt++ {
		h, err := p.Acquire(ctx)
		if err != nil {
			return err
		}
		err = fn(ctx, h.Conn())
		if err == nil {
			h.Release()
			return nil
		}
		connLevel := ftperr.IsConnectionLevel(err) || h.Conn().Broken()
		if connLevel {
			h.MarkBroken()
		}
		h.Release()
		last = ftperr.Wrap(op, remotePath, cfg.Target(), err)

		if connLevel && attempt == 0 && ctx.Err() == nil {
			b.log.Debugf("ftp: %s on %s failed at the connection level, retrying once on a fresh connection: %v",
				op, remotePath, err)
			continue
		}
		return annotate(last)
	}
	return annotate(last)
}

// annotate appends a short clarification for reply codes whose standard text
// is unhelpful on its own.
func annotate(err error) error {
	var fe *ftperr.Error
	if !errors.As(err, &fe) || fe.Code == 0 {
		return err
	}
	if note := ftperr.Explain(fe.Code); note != "" {
		return fmt.Errorf("%w (%s)", err, note)
	}
	return err
}

func (b *Binding) create(ctx context.Context, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error) {
	remotePath, err := b.resolve(req, false)
	if err != nil {
		return nil, fmt.Errorf("ftp: create: %w", err)
	}

	err = b.withConn(ctx, "create", remotePath, func(ctx context.Context, c ftpclient.Conn) error {
		// A fresh reader each attempt, because withConn may run fn twice.
		return c.Store(ctx, remotePath, bytes.NewReader(req.Data))
	})
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(createResponse{FileName: remotePath})
	if err != nil {
		return nil, fmt.Errorf("ftp: create %q: encoding the response failed: %w", remotePath, err)
	}
	ct := contentTypeJSON
	return &bindings.InvokeResponse{
		Data:        body,
		Metadata:    map[string]string{metadataFileName: remotePath},
		ContentType: &ct,
	}, nil
}

func (b *Binding) get(ctx context.Context, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error) {
	remotePath, err := b.resolve(req, false)
	if err != nil {
		return nil, fmt.Errorf("ftp: get: %w", err)
	}

	var buf bytes.Buffer
	err = b.withConn(ctx, "get", remotePath, func(ctx context.Context, c ftpclient.Conn) error {
		buf.Reset() // withConn may run this twice
		_, rerr := c.Retrieve(ctx, remotePath, &buf)
		return rerr
	})
	if err != nil {
		return nil, err
	}

	ct := contentTypeBinary
	data := buf.Bytes()
	return &bindings.InvokeResponse{
		Data:        data,
		Metadata:    map[string]string{metadataFileName: remotePath, "size": strconv.Itoa(len(data))},
		ContentType: &ct,
	}, nil
}

func (b *Binding) list(ctx context.Context, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error) {
	// allowRoot: an omitted fileName lists rootPath itself.
	remotePath, err := b.resolve(req, true)
	if err != nil {
		return nil, fmt.Errorf("ftp: list: %w", err)
	}

	var entries []ftpclient.Entry
	err = b.withConn(ctx, "list", remotePath, func(ctx context.Context, c ftpclient.Conn) error {
		var lerr error
		entries, lerr = c.List(ctx, remotePath)
		return lerr
	})
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(toListResponse(entries))
	if err != nil {
		return nil, fmt.Errorf("ftp: list %q: encoding the response failed: %w", remotePath, err)
	}

	ct := contentTypeJSON
	return &bindings.InvokeResponse{
		Data:        body,
		Metadata:    map[string]string{metadataFileName: remotePath},
		ContentType: &ct,
	}, nil
}

// toListResponse renders a listing in the server's own order, as the built-in
// SFTP binding does. The "." and ".." entries some servers return are dropped.
func toListResponse(entries []ftpclient.Entry) []listEntry {
	out := make([]listEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." {
			continue
		}
		out = append(out, listEntry{
			FileName:    e.Name,
			IsDirectory: e.Type == ftpclient.EntryDir,
		})
	}
	return out
}

func (b *Binding) delete(ctx context.Context, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error) {
	// allowRoot is false, so rootPath itself can never be targeted.
	remotePath, err := b.resolve(req, false)
	if err != nil {
		return nil, fmt.Errorf("ftp: delete: %w", err)
	}

	err = b.withConn(ctx, "delete", remotePath, func(ctx context.Context, c ftpclient.Conn) error {
		return c.Delete(ctx, remotePath)
	})
	if err != nil {
		return nil, err
	}
	// An empty, non-nil response. The sidecar answers HTTP 200 with an empty
	// body. Returning nil risks a nil dereference in the SDK's response
	// marshalling, so always return a value.
	return &bindings.InvokeResponse{}, nil
}
