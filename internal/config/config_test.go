package config

import (
	"strings"
	"testing"
	"time"
)

func base() map[string]string {
	return map[string]string{
		"address":  "ftp.example.com:21",
		"username": "user",
		"password": "pass",
		"rootPath": "/upload",
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, warns, err := Parse(base())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Address != "ftp.example.com:21" {
		t.Errorf("Address = %q", cfg.Address)
	}
	if cfg.RootPath != "/upload" {
		t.Errorf("RootPath = %q", cfg.RootPath)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, DefaultTimeout)
	}
	if cfg.MaxConnections != DefaultMaxConnections {
		t.Errorf("MaxConnections = %d", cfg.MaxConnections)
	}
	if cfg.TLSMode != TLSNone {
		t.Errorf("TLSMode = %v, want none", cfg.TLSMode)
	}
	if cfg.TLSConfig != nil {
		t.Error("TLSConfig should be nil when tlsMode is none")
	}
	// Cleartext must always be called out.
	if !hasWarning(warns, "cleartext") {
		t.Errorf("expected a cleartext warning, got %v", warns)
	}
}

// Dapr users write metadata keys with inconsistent casing and every built-in
// component tolerates it, so we must too.
func TestParseIsCaseInsensitive(t *testing.T) {
	props := map[string]string{
		"ADDRESS":  "ftp.example.com:21",
		"rootpath": "/upload",
		"UserName": "user",
	}
	cfg, _, err := Parse(props)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RootPath != "/upload" || cfg.Username != "user" {
		t.Errorf("case-insensitive decode failed: root=%q user=%q", cfg.RootPath, cfg.Username)
	}
}

func TestParseTLSModes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want TLSMode
	}{
		{"", TLSNone}, {"none", TLSNone}, {"NONE", TLSNone},
		{"explicit", TLSExplicit}, {"Explicit", TLSExplicit},
		{"implicit", TLSImplicit},
	} {
		props := base()
		props["tlsMode"] = tc.in
		cfg, _, err := Parse(props)
		if err != nil {
			t.Fatalf("tlsMode %q: unexpected error: %v", tc.in, err)
		}
		if cfg.TLSMode != tc.want {
			t.Errorf("tlsMode %q = %v, want %v", tc.in, cfg.TLSMode, tc.want)
		}
	}
}

// A typo in tlsMode must never silently fall back to cleartext.
func TestParseRejectsUnknownTLSMode(t *testing.T) {
	props := base()
	props["tlsMode"] = "tls"
	_, _, err := Parse(props)
	if err == nil {
		t.Fatal("expected an error for an unknown tlsMode")
	}
	if !strings.Contains(err.Error(), "explicit") {
		t.Errorf("error should list the valid values, got: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantSub string
	}{
		{"missing rootPath", func(m map[string]string) { delete(m, "rootPath") }, "rootPath is required"},
		{"missing address", func(m map[string]string) { delete(m, "address") }, "address is required"},
		{"address without port", func(m map[string]string) { m["address"] = "ftp.example.com" }, "host:port"},
		{"port out of range", func(m map[string]string) { m["address"] = "h:70000" }, "out of range"},
		{"timeout without unit", func(m map[string]string) { m["timeout"] = "30" }, "needs a unit"},
		{"bad timeout", func(m map[string]string) { m["timeout"] = "soon" }, "not a valid duration"},
		{"zero maxConnections is a default, negative is not",
			func(m map[string]string) { m["maxConnections"] = "-1" }, "at least"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props := base()
			tt.mutate(props)
			_, _, err := Parse(props)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestParseTimeout(t *testing.T) {
	props := base()
	props["timeout"] = "5s"
	cfg, _, err := Parse(props)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v", cfg.Timeout)
	}
}

func TestMaxConnectionsClamped(t *testing.T) {
	props := base()
	props["maxConnections"] = "500"
	cfg, warns, err := Parse(props)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxConnections != maxConnections {
		t.Errorf("MaxConnections = %d, want %d", cfg.MaxConnections, maxConnections)
	}
	if !hasWarning(warns, "clamped") {
		t.Errorf("expected a clamp warning, got %v", warns)
	}
}

func TestInsecureSkipVerifyWarns(t *testing.T) {
	props := base()
	props["tlsMode"] = "explicit"
	props["insecureSkipVerify"] = "true"
	cfg, warns, err := Parse(props)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be set on the tls.Config")
	}
	if !hasWarning(warns, "not verified") {
		t.Errorf("expected a verification warning, got %v", warns)
	}
}

func TestTargetOmitsCredentials(t *testing.T) {
	props := base()
	props["tlsMode"] = "explicit"
	cfg, _, err := Parse(props)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	target := cfg.Target()
	if strings.Contains(target, "pass") || strings.Contains(target, "user") {
		t.Fatalf("Target() leaked credentials: %q", target)
	}
	if target != "ftps-explicit://ftp.example.com:21" {
		t.Errorf("Target() = %q", target)
	}
}

func hasWarning(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
