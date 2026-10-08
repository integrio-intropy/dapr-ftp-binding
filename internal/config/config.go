// Package config parses and validates the component metadata.
package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/dapr/components-contrib/metadata"
)

// TLSMode selects how the control and data channels are protected.
type TLSMode uint8

const (
	// TLSNone sends credentials and file contents in cleartext.
	TLSNone TLSMode = iota
	// TLSExplicit connects in cleartext then issues AUTH TLS (RFC 4217).
	TLSExplicit
	// TLSImplicit negotiates TLS before the greeting, conventionally on port 990.
	TLSImplicit
)

func (m TLSMode) String() string {
	switch m {
	case TLSExplicit:
		return "ftps-explicit"
	case TLSImplicit:
		return "ftps-implicit"
	default:
		return "ftp"
	}
}

// Defaults applied when metadata omits a field.
const (
	DefaultTimeout        = 30 * time.Second
	DefaultMaxConnections = 5

	minConnections = 1
	maxConnections = 64
)

// raw mirrors the component metadata as written in YAML. Durations and sizes
// are strings so they can be parsed explicitly: contrib's duration hook has
// treated bare integers inconsistently across versions, and a silent 1000x
// error on a timeout is a miserable bug to chase.
type raw struct {
	Address            string `json:"address" mapstructure:"address"`
	Username           string `json:"username" mapstructure:"username"`
	Password           string `json:"password" mapstructure:"password"`
	RootPath           string `json:"rootPath" mapstructure:"rootPath"`
	TLSMode            string `json:"tlsMode" mapstructure:"tlsMode"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify" mapstructure:"insecureSkipVerify"`
	Timeout            string `json:"timeout" mapstructure:"timeout"`
	DisableEPSV        bool   `json:"disableEPSV" mapstructure:"disableEPSV"`
	TrustPASVIP        bool   `json:"trustPASVIP" mapstructure:"trustPASVIP"`
	MaxConnections     int    `json:"maxConnections" mapstructure:"maxConnections"`
}

// Config is validated and immutable.
type Config struct {
	Address        string
	Username       string
	Password       string
	RootPath       string
	TLSMode        TLSMode
	TLSConfig      *tls.Config // nil when TLSMode is TLSNone
	Timeout        time.Duration
	DisableEPSV    bool
	TrustPASVIP    bool
	MaxConnections int
}

// Target is a credential-free description for logs and error messages.
func (c *Config) Target() string {
	return c.TLSMode.String() + "://" + c.Address
}

// String redacts the password so that logging a Config -- with %v, %s, or by
// accident inside a wrapped error -- cannot leak it. Without this, the zero
// effort path (`log.Infof("%v", cfg)`) is the one that leaks, which is exactly
// backwards. GoString covers %#v for the same reason.
func (c *Config) String() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf(
		"ftp.Config{Target:%s Username:%q Password:%s RootPath:%q InsecureSkipVerify:%t Timeout:%s DisableEPSV:%t TrustPASVIP:%t MaxConnections:%d}",
		c.Target(), c.Username, redacted(c.Password), c.RootPath,
		c.TLSConfig != nil && c.TLSConfig.InsecureSkipVerify,
		c.Timeout, c.DisableEPSV, c.TrustPASVIP, c.MaxConnections)
}

// GoString implements fmt.GoStringer so %#v is redacted too.
func (c *Config) GoString() string { return c.String() }

func redacted(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<redacted>"
}

// RawType exposes the metadata struct type for GetComponentMetadata.
func RawType() any { return raw{} }

// Parse decodes, validates and defaults the component metadata. Warnings are
// returned separately from errors so Init can log them without failing.
func Parse(props map[string]string) (*Config, []string, error) {
	var r raw
	// DecodeMetadata gives the case-insensitive key matching that every
	// built-in Dapr component has; YAML authors and the HTTP API are
	// inconsistent about casing and users rely on it working either way.
	if err := metadata.DecodeMetadata(props, &r); err != nil {
		return nil, nil, fmt.Errorf("could not decode component metadata: %w", err)
	}

	var warns []string
	cfg := &Config{
		Username:    r.Username,
		Password:    r.Password,
		DisableEPSV: r.DisableEPSV,
		TrustPASVIP: r.TrustPASVIP,
	}

	mode, err := parseTLSMode(r.TLSMode)
	if err != nil {
		return nil, nil, err
	}
	cfg.TLSMode = mode

	if cfg.Address, err = parseAddress(r); err != nil {
		return nil, nil, err
	}

	if r.RootPath == "" {
		return nil, nil, errors.New("rootPath is required: set it to the working directory for this binding, for example \"/upload\"")
	}
	// Cleaned but not confined. Like the built-in SFTP binding, request paths are
	// joined to this and the FTP server decides what the account may reach.
	cfg.RootPath = path.Clean(r.RootPath)

	if cfg.Timeout, err = parseDuration("timeout", r.Timeout, DefaultTimeout); err != nil {
		return nil, nil, err
	}
	if cfg.Timeout <= 0 {
		return nil, nil, fmt.Errorf("timeout must be positive, got %q", r.Timeout)
	}

	cfg.MaxConnections = r.MaxConnections
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = DefaultMaxConnections
	}
	if cfg.MaxConnections < minConnections {
		return nil, nil, fmt.Errorf("maxConnections must be at least %d, got %d", minConnections, cfg.MaxConnections)
	}
	if cfg.MaxConnections > maxConnections {
		warns = append(warns, fmt.Sprintf(
			"maxConnections=%d is above the supported maximum of %d and has been clamped; most FTP servers cap concurrent logins per user well below this",
			cfg.MaxConnections, maxConnections))
		cfg.MaxConnections = maxConnections
	}

	if cfg.TLSConfig, warns, err = buildTLS(cfg, r, warns); err != nil {
		return nil, nil, err
	}
	if mode == TLSNone {
		warns = append(warns, fmt.Sprintf(
			"tlsMode is \"none\": credentials and file contents are sent to %s in cleartext", cfg.Address))
		if r.InsecureSkipVerify {
			warns = append(warns, "insecureSkipVerify has no effect when tlsMode is \"none\"")
		}
	}

	return cfg, warns, nil
}

func parseTLSMode(s string) (TLSMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return TLSNone, nil
	case "explicit":
		return TLSExplicit, nil
	case "implicit":
		return TLSImplicit, nil
	default:
		// Never fall back to cleartext on a typo.
		return 0, fmt.Errorf("tlsMode %q is not valid: use \"none\", \"explicit\" or \"implicit\"", s)
	}
}

func parseAddress(r raw) (string, error) {
	if r.Address == "" {
		return "", errors.New("address is required, for example \"ftp.example.com:21\"")
	}
	host, port, err := net.SplitHostPort(r.Address)
	if err != nil {
		return "", fmt.Errorf("address %q must be in host:port form: %w", r.Address, err)
	}
	if host == "" {
		return "", fmt.Errorf("address %q is missing a host", r.Address)
	}
	if err := validatePort(port); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}

func validatePort(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("port %q is not a number", s)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("port %d is out of range 1-65535", n)
	}
	return nil
}

func parseDuration(field, s string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		if _, numErr := strconv.Atoi(s); numErr == nil {
			return 0, fmt.Errorf("%s %q needs a unit, for example \"%ss\"", field, s, s)
		}
		return 0, fmt.Errorf("%s %q is not a valid duration: %w", field, s, err)
	}
	return d, nil
}

func buildTLS(cfg *Config, r raw, warns []string) (*tls.Config, []string, error) {
	if cfg.TLSMode == TLSNone {
		return nil, warns, nil
	}
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, warns, fmt.Errorf("could not derive a TLS server name from address %q: %w", cfg.Address, err)
	}
	t := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		//nolint:gosec // G402: opt-in, gated on explicit metadata and warned about.
		InsecureSkipVerify: r.InsecureSkipVerify,
		// Servers configured with require_ssl_reuse demand that the data
		// connection resume the control connection's TLS session, and Go only
		// resumes when a cache is set. The FTP library uses this one config for
		// both channels, so a single cache here covers it.
		ClientSessionCache: tls.NewLRUClientSessionCache(8),
	}
	if r.InsecureSkipVerify {
		warns = append(warns, fmt.Sprintf(
			"insecureSkipVerify is true: the certificate presented by %s is not verified, "+
				"so the connection is open to interception", cfg.Address))
	}
	return t, warns, nil
}
