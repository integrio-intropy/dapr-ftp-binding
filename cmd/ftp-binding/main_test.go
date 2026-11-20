package main

import (
	"os"
	"testing"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/config"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient"
)

func TestParsePerm(t *testing.T) {
	tests := []struct {
		in      string
		want    os.FileMode
		wantErr bool
	}{
		{in: "0660", want: 0o660},
		// Read as octal with or without the prefix. This is the case flag.Uint
		// got wrong: it would have read 660 as decimal, giving 0o1224.
		{in: "660", want: 0o660},
		{in: "0o660", want: 0o660},
		{in: "600", want: 0o600},
		{in: "0777", want: 0o777},
		{in: "0", want: 0},

		{in: "", wantErr: true},
		{in: "0888", wantErr: true}, // 8 is not an octal digit
		{in: "rw-rw----", wantErr: true},
		{in: "1660", wantErr: true}, // wider than 0777: setgid
		{in: "-660", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parsePerm(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePerm(%q) = %#o, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePerm(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsePerm(%q) = %#o, want %#o", tc.in, got, tc.want)
			}
		})
	}
}

// The adapter mirrors config.TLSMode numerically but does not import it, so
// this is the only thing standing between a reordered constant and a connection
// quietly weaker than the one that was configured.
func TestTLSModeMappingIsFaithful(t *testing.T) {
	tests := []struct {
		in   config.TLSMode
		want ftpclient.TLSMode
	}{
		{config.TLSNone, ftpclient.TLSNone},
		{config.TLSExplicit, ftpclient.TLSExplicit},
		{config.TLSImplicit, ftpclient.TLSImplicit},
	}
	for _, tc := range tests {
		t.Run(tc.in.String(), func(t *testing.T) {
			got, err := tlsMode(tc.in)
			if err != nil {
				t.Fatalf("tlsMode(%v): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("tlsMode(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestTLSModeRejectsAnUnknownValue(t *testing.T) {
	// Never silently fall back to cleartext.
	if _, err := tlsMode(config.TLSMode(99)); err == nil {
		t.Fatal("an unknown TLS mode was accepted")
	}
}

func TestSocketFolderPrefersTheSidecarVariable(t *testing.T) {
	t.Setenv(socketFolderEnvDapr, "/from-daprd")
	t.Setenv(socketFolderEnvSDK, "/from-sdk")
	t.Setenv(socketFolderEnvLegacy, "/from-legacy")
	if got := socketFolder(); got != "/from-daprd" {
		t.Errorf("socketFolder() = %q, want the daprd variable to win", got)
	}
}

func TestSocketFolderFallsBackThroughTheSDKSpellings(t *testing.T) {
	t.Setenv(socketFolderEnvDapr, "")
	t.Setenv(socketFolderEnvSDK, "/from-sdk")
	t.Setenv(socketFolderEnvLegacy, "/from-legacy")
	if got := socketFolder(); got != "/from-sdk" {
		t.Errorf("socketFolder() = %q, want /from-sdk", got)
	}

	t.Setenv(socketFolderEnvSDK, "")
	if got := socketFolder(); got != "/from-legacy" {
		t.Errorf("socketFolder() = %q, want /from-legacy", got)
	}

	t.Setenv(socketFolderEnvLegacy, "")
	if got := socketFolder(); got != defaultSocketFolder {
		t.Errorf("socketFolder() = %q, want %q", got, defaultSocketFolder)
	}
}
