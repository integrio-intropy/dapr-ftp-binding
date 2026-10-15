//go:build integration

// Package integration exercises the binding against real FTP servers.
//
// Run with:
//
//	docker compose -f tests/integration/docker-compose.yaml up -d --build --wait
//	go test -tags=integration ./tests/integration/...
//
// Passive-mode ports cannot be randomised, so the compose file pins them and
// the addresses below must match it.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	contribbindings "github.com/dapr/components-contrib/bindings"
	contribmd "github.com/dapr/components-contrib/metadata"
	"github.com/dapr/kit/logger"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/binding"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/config"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient"
)

type server struct {
	name     string
	address  string
	tlsMode  string
	rootPath string
}

// Addresses must match tests/integration/docker-compose.yaml.
//
// All accounts are chrooted to their home directory, so rootPath is relative to
// that, not to the container filesystem root.
var servers = []server{
	{name: "vsftpd-plain", address: "127.0.0.1:2121", tlsMode: "none", rootPath: "/upload"},
	{name: "pureftpd-plain", address: "127.0.0.1:2122", tlsMode: "none", rootPath: "/upload"},
	{name: "proftpd-explicit-tls", address: "127.0.0.1:2123", tlsMode: "explicit", rootPath: "/upload"},
	{name: "proftpd-implicit-tls", address: "127.0.0.1:9990", tlsMode: "implicit", rootPath: "/upload"},
}

// serverReuse demands that the data connection resume the control connection's
// TLS session, which Go only does when tls.Config carries a ClientSessionCache.
var serverReuse = server{name: "proftpd-ssl-reuse", address: "127.0.0.1:2125", tlsMode: "explicit", rootPath: "/upload"}

// serverIdle has idle_session_timeout=5, for stale-connection recovery.
var serverIdle = server{name: "vsftpd-idle", address: "127.0.0.1:2124", tlsMode: "none", rootPath: "/upload"}

func newBinding(t *testing.T, s server, overrides map[string]string) *binding.Binding {
	t.Helper()
	requireServer(t, s.address)

	props := map[string]string{
		"address":  s.address,
		"username": "ftpuser",
		"password": "ftppass",
		"rootPath": s.rootPath,
		"tlsMode":  s.tlsMode,
		"timeout":  "20s",
		// The test servers use self-signed certificates. A dedicated test below
		// covers verification being enforced by default.
		"insecureSkipVerify": "true",
		// The servers advertise a container-internal PASV address.
		"trustPASVIP": "true",
	}
	for k, v := range overrides {
		props[k] = v
	}

	b := binding.New(logger.NewLogger("integration"), func(c *config.Config) (ftpclient.Dialer, error) {
		return ftpclient.NewDialer(ftpclient.DialerConfig{
			Address: c.Address, Username: c.Username, Password: c.Password,
			TLSMode: ftpclient.TLSMode(c.TLSMode), TLSConfig: c.TLSConfig,
			Timeout: c.Timeout, DisableEPSV: c.DisableEPSV, TrustPASVIP: c.TrustPASVIP,
			TargetName: c.Target(),
		}), nil
	})
	if err := b.Init(context.Background(), contribbindings.Metadata{Base: contribmd.Base{Properties: props}}); err != nil {
		t.Fatalf("Init against %s: %v", s.name, err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// requireEnv, when set, makes an unreachable server fatal rather than a skip.
// Set it in CI: skipping is the right default for a developer who has not
// started the containers, but in CI a container that failed to start would
// otherwise skip every test and report the job green, which is the one failure
// mode nobody notices.
const requireEnv = "FTP_INTEGRATION_REQUIRE_SERVERS"

func requireServer(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		msg := fmt.Sprintf("no FTP server on %s: %v", addr, err)
		if os.Getenv(requireEnv) != "" {
			t.Fatalf("%s (%s is set, so this is a failure rather than a skip)", msg, requireEnv)
		}
		t.Skipf("%s; run docker compose up -d --build --wait first", msg)
	}
	_ = conn.Close()
}

func invoke(t *testing.T, b *binding.Binding, op contribbindings.OperationKind, data []byte, md map[string]string) (*contribbindings.InvokeResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return b.Invoke(ctx, &contribbindings.InvokeRequest{Operation: op, Data: data, Metadata: md})
}

// Binary content with CRLF and NUL: an ASCII-mode transfer would corrupt it.
var payload = []byte("alpha\r\nbeta\n\x00\x01\xfe\xff omega\r\n")

func TestRoundTripAllServers(t *testing.T) {
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) {
			b := newBinding(t, s, nil)
			name := fmt.Sprintf("roundtrip-%d.bin", time.Now().UnixNano())

			if _, err := invoke(t, b, contribbindings.CreateOperation, payload, map[string]string{"fileName": name}); err != nil {
				t.Fatalf("create: %v", err)
			}

			got, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if !bytes.Equal(got.Data, payload) {
				t.Fatalf("payload corrupted in transit:\n got %q\nwant %q", got.Data, payload)
			}

			listed, err := invoke(t, b, contribbindings.ListOperation, nil, nil)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var entries []struct {
				FileName    string `json:"fileName"`
				IsDirectory bool   `json:"isDirectory"`
			}
			if err := json.Unmarshal(listed.Data, &entries); err != nil {
				t.Fatalf("list response is not a JSON array: %v", err)
			}
			var found bool
			for _, e := range entries {
				if e.FileName == name {
					found = true
				}
			}
			if !found {
				t.Errorf("list %+v does not include %q", entries, name)
			}

			if _, err := invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name}); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name}); err == nil {
				t.Error("get after delete should fail")
			}
		})
	}
}

func TestEmptyFile(t *testing.T) {
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) {
			b := newBinding(t, s, nil)
			name := fmt.Sprintf("empty-%d.bin", time.Now().UnixNano())

			// Zero-byte uploads take a different path in the FTP library, which
			// forces the TLS handshake explicitly because Write is never called.
			if _, err := invoke(t, b, contribbindings.CreateOperation, nil, map[string]string{"fileName": name}); err != nil {
				t.Fatalf("create empty: %v", err)
			}
			got, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
			if err != nil {
				t.Fatalf("get empty: %v", err)
			}
			if len(got.Data) != 0 {
				t.Errorf("got %d bytes, want 0", len(got.Data))
			}
			_, _ = invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name})
		})
	}
}

// Certificate verification must be on unless explicitly disabled.
// The ProFTPD containers run "TLSRequired on", which refuses a cleartext data
// connection. So every FTPS case below passing is itself the assertion that the
// data channel is encrypted -- we no longer inspect the socket, because we no
// longer supply the dial func that would let us.
func TestTLSVerificationEnforcedByDefault(t *testing.T) {
	for _, s := range servers {
		if s.tlsMode == "none" {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			requireServer(t, s.address)
			props := map[string]string{
				"address": s.address, "username": "ftpuser", "password": "ftppass",
				"rootPath": s.rootPath, "tlsMode": s.tlsMode, "timeout": "15s",
				"trustPASVIP": "true",
				// insecureSkipVerify deliberately omitted: it defaults to false.
			}
			b := binding.New(logger.NewLogger("integration"), func(c *config.Config) (ftpclient.Dialer, error) {
				return ftpclient.NewDialer(ftpclient.DialerConfig{
					Address: c.Address, Username: c.Username, Password: c.Password,
					TLSMode: ftpclient.TLSMode(c.TLSMode), TLSConfig: c.TLSConfig,
					Timeout: c.Timeout, TrustPASVIP: c.TrustPASVIP, TargetName: c.Target(),
				}), nil
			})
			err := b.Init(context.Background(), contribbindings.Metadata{Base: contribmd.Base{Properties: props}})
			if err == nil {
				_ = b.Close()
				t.Fatal("a self-signed certificate was accepted; verification is not enforced by default")
			}
			// Assert it failed for the RIGHT reason. Any connection problem would
			// otherwise make this test pass while proving nothing.
			if !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("expected a certificate verification failure, got: %v", err)
			}
		})
	}
}

// The data connection must resume the control connection's TLS session when the
// server demands it. Go only does that when tls.Config carries a
// ClientSessionCache, which the adapter sets per session.
func TestTLSSessionReuseRequired(t *testing.T) {
	requireServer(t, serverReuse.address)
	b := newBinding(t, serverReuse, nil)

	name := fmt.Sprintf("reuse-%d.bin", time.Now().UnixNano())
	if _, err := invoke(t, b, contribbindings.CreateOperation, payload, map[string]string{"fileName": name}); err != nil {
		t.Fatalf("create against a server requiring TLS session reuse: %v", err)
	}
	got, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
	if err != nil {
		t.Fatalf("get against a server requiring TLS session reuse: %v", err)
	}
	if !bytes.Equal(got.Data, payload) {
		t.Error("content mismatch")
	}
	_, _ = invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name})
}

// Parallel work through the pool must not corrupt sessions or exceed the cap.
func TestConcurrentOperations(t *testing.T) {
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) {
			b := newBinding(t, s, map[string]string{"maxConnections": "4"})

			const workers = 12
			var wg sync.WaitGroup
			errs := make(chan error, workers)
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					name := fmt.Sprintf("concurrent-%d-%d.bin", time.Now().UnixNano(), i)
					body := []byte(strings.Repeat(fmt.Sprintf("%d", i), 256))

					if _, err := invoke(t, b, contribbindings.CreateOperation, body, map[string]string{"fileName": name}); err != nil {
						errs <- fmt.Errorf("worker %d create: %w", i, err)
						return
					}
					got, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
					if err != nil {
						errs <- fmt.Errorf("worker %d get: %w", i, err)
						return
					}
					if !bytes.Equal(got.Data, body) {
						errs <- fmt.Errorf("worker %d: content mismatch, sessions may have crossed", i)
						return
					}
					if _, err := invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name}); err != nil {
						errs <- fmt.Errorf("worker %d delete: %w", i, err)
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

// A session idle longer than the server's timeout must be detected and replaced
// rather than handed to a caller.
func TestStaleConnectionRecovery(t *testing.T) {
	requireServer(t, serverIdle.address)
	b := newBinding(t, serverIdle, nil)
	name := fmt.Sprintf("stale-%d.bin", time.Now().UnixNano())

	if _, err := invoke(t, b, contribbindings.CreateOperation, payload, map[string]string{"fileName": name}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// This server has idle_session_timeout=5, so by now it has dropped the pooled
	// session. There is no health probe any more: the next operation fails at the
	// connection level and withConn retries once on a fresh session, which is
	// exactly what this asserts.
	t.Log("idling for 20s so the server drops the pooled session...")
	time.Sleep(20 * time.Second)

	got, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
	if err != nil {
		t.Fatalf("get after idling should have recovered: %v", err)
	}
	if !bytes.Equal(got.Data, payload) {
		t.Error("content mismatch after reconnect")
	}
	_, _ = invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name})
}

// The temp-name write against every real server: the temp name must be gone,
// the final name must hold the exact payload, and nothing may be left behind.
func TestTempFileNameWriteAllServers(t *testing.T) {
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) {
			b := newBinding(t, s, map[string]string{"tempFileNamePattern": "{name}.tmp"})
			name := fmt.Sprintf("temp-%d.bin", time.Now().UnixNano())

			if _, err := invoke(t, b, contribbindings.CreateOperation, payload, map[string]string{"fileName": name}); err != nil {
				t.Fatalf("create: %v", err)
			}
			t.Cleanup(func() {
				_, _ = invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name})
			})

			got, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
			if err != nil {
				t.Fatalf("get %q after the rename: %v", name, err)
			}
			if !bytes.Equal(got.Data, payload) {
				t.Fatalf("payload corrupted:\n got %q\nwant %q", got.Data, payload)
			}
			if _, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name + ".tmp"}); err == nil {
				t.Errorf("the temp file %q.tmp is still on the server after the rename", name)
			}
		})
	}
}

// Whether RNTO replaces an existing target is server-defined: RFC 959 does not
// say. This test does not assert one behaviour, it records which one each
// server we support actually has, so the README's claim is measured rather
// than assumed. Both outcomes pass; a partially applied one does not.
func TestRenameOntoExistingIsServerDefined(t *testing.T) {
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) {
			b := newBinding(t, s, map[string]string{"tempFileNamePattern": "{name}.tmp"})
			name := fmt.Sprintf("clobber-%d.bin", time.Now().UnixNano())

			if _, err := invoke(t, b, contribbindings.CreateOperation, []byte("first"), map[string]string{"fileName": name}); err != nil {
				t.Fatalf("create the first file: %v", err)
			}
			t.Cleanup(func() {
				_, _ = invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name})
				_, _ = invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name + ".tmp"})
			})

			_, err := invoke(t, b, contribbindings.CreateOperation, []byte("second"), map[string]string{"fileName": name})

			got, getErr := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
			if getErr != nil {
				t.Fatalf("the file is gone after the second create: %v", getErr)
			}
			switch {
			case err == nil && string(got.Data) == "second":
				t.Logf("%s: RNTO replaces an existing target", s.name)
			case err != nil && string(got.Data) == "first":
				t.Logf("%s: RNTO refuses an existing target, reported as: %v", s.name, err)
				// A refusal must not strand the temp file.
				if _, e := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name + ".tmp"}); e == nil {
					t.Errorf("the refused rename left %q.tmp behind", name)
				}
			default:
				t.Fatalf("neither replaced nor refused cleanly: create err = %v, content = %q", err, got.Data)
			}
		})
	}
}

// Creating the directory tree on the way, against every real server.
//
// This is the half the fake cannot prove: whether a server refuses a store
// into a missing directory with 550 or 553, and whether MKD behaves as the
// walk assumes. The test leaves the directories behind -- the binding has no
// rmdir and the containers are disposable -- so the names are unique per run.
func TestCreateDirsAllServers(t *testing.T) {
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) {
			name := fmt.Sprintf("mkdir-%d/nested/deeper/a.bin", time.Now().UnixNano())

			// Without the flag the same write fails. That failure is the whole
			// reason the flag exists, so it is worth asserting rather than
			// assuming, and its wording differs per server.
			plain := newBinding(t, s, nil)
			if _, err := invoke(t, plain, contribbindings.CreateOperation, payload, map[string]string{"fileName": name}); err == nil {
				t.Fatal("a create into a missing directory succeeded without createDirs")
			} else {
				t.Logf("%s refuses a store into a missing directory with: %v", s.name, err)
			}

			b := newBinding(t, s, map[string]string{"createDirs": "true"})
			if _, err := invoke(t, b, contribbindings.CreateOperation, payload, map[string]string{"fileName": name}); err != nil {
				t.Fatalf("create with createDirs: %v", err)
			}
			t.Cleanup(func() {
				_, _ = invoke(t, b, contribbindings.DeleteOperation, nil, map[string]string{"fileName": name})
			})

			got, err := invoke(t, b, contribbindings.GetOperation, nil, map[string]string{"fileName": name})
			if err != nil {
				t.Fatalf("get %q: %v", name, err)
			}
			if !bytes.Equal(got.Data, payload) {
				t.Fatalf("payload corrupted:\n got %q\nwant %q", got.Data, payload)
			}

			// Again, into the tree that now exists: every MKD along the chain
			// is refused this time, and none of that may reach the caller.
			if _, err := invoke(t, b, contribbindings.CreateOperation, payload, map[string]string{"fileName": name}); err != nil {
				t.Fatalf("second create into the existing tree: %v", err)
			}
		})
	}
}
