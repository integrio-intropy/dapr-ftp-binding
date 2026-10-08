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
