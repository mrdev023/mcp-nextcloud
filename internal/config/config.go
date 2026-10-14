// Package config loads the news-mcp settings from the environment.
package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/mrdev023/mcp-nextcloud/internal/httpx"
)

// Authentication modes (MCP_AUTH_MODE).
const (
	AuthModeStandalone = "standalone"
	AuthModeOIDC       = "oidc"
)

// Config is the validated server configuration.
type Config struct {
	AuthMode      string // AuthModeStandalone or AuthModeOIDC
	NextcloudURL  string
	NextcloudUser string
	AppPassword   string
	Port          int
	BaseURL       *url.URL
	AuthPassword  string
	ClientID      string
	ClientSecret  string
	RedirectURIs  []string
	AllowDCR      bool
	SigningKey    []byte
	LogLevel      slog.Level
	// ReadOnly exposes only the tools that do not change News data.
	ReadOnly bool

	// OIDC mode.
	OIDCIssuer           string
	OIDCAudience         string
	OIDCRequiredScopes   []string
	OIDCClientID         string
	OIDCClientSecret     string
	OIDCExchangeAudience string
	OIDCExchangeScope    string
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// Load reads and validates the configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		NextcloudURL:  strings.TrimRight(env("NEXTCLOUD_URL", ""), "/"),
		NextcloudUser: env("NEXTCLOUD_USER", ""),
		AppPassword:   os.Getenv("NEXTCLOUD_APP_PASSWORD"),
		AuthPassword:  os.Getenv("MCP_AUTH_PASSWORD"),
		ClientID:      env("MCP_OAUTH_CLIENT_ID", ""),
		ClientSecret:  os.Getenv("MCP_OAUTH_CLIENT_SECRET"),
	}
	switch c.AuthMode = strings.ToLower(env("MCP_AUTH_MODE", AuthModeStandalone)); c.AuthMode {
	case AuthModeStandalone, AuthModeOIDC:
	case "oauth": // former name of standalone
		c.AuthMode = AuthModeStandalone
	case "gateway", "passthrough":
		return nil, errors.New("MCP_AUTH_MODE=" + c.AuthMode + " was removed: it forwarded tokens not issued for this server; use MCP_AUTH_MODE=oidc")
	default:
		return nil, errors.New("MCP_AUTH_MODE must be standalone or oidc")
	}
	isOIDC := c.AuthMode == AuthModeOIDC
	if isOIDC {
		c.OIDCIssuer = env("MCP_OIDC_ISSUER", "")
		c.OIDCAudience = env("MCP_OIDC_AUDIENCE", "")
		c.OIDCRequiredScopes = strings.FieldsFunc(env("MCP_OIDC_REQUIRED_SCOPES", ""), func(r rune) bool { return r == ' ' || r == ',' })
		c.OIDCClientID = env("MCP_OIDC_CLIENT_ID", "")
		c.OIDCClientSecret = os.Getenv("MCP_OIDC_CLIENT_SECRET")
		c.OIDCExchangeAudience = env("MCP_OIDC_EXCHANGE_AUDIENCE", "")
		c.OIDCExchangeScope = env("MCP_OIDC_EXCHANGE_SCOPE", "")
	}

	required := map[string]string{"NEXTCLOUD_URL": c.NextcloudURL}
	if isOIDC {
		required["MCP_OIDC_ISSUER"] = c.OIDCIssuer
		required["MCP_OIDC_CLIENT_ID"] = c.OIDCClientID
		required["MCP_OIDC_CLIENT_SECRET"] = c.OIDCClientSecret
		required["MCP_OIDC_EXCHANGE_AUDIENCE"] = c.OIDCExchangeAudience
	} else {
		required["NEXTCLOUD_USER"] = c.NextcloudUser
		required["NEXTCLOUD_APP_PASSWORD"] = c.AppPassword
		required["MCP_AUTH_PASSWORD"] = c.AuthPassword
	}
	var missing []string
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if u, err := url.Parse(c.NextcloudURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("NEXTCLOUD_URL must be an absolute http(s) URL")
	}
	if !isOIDC && len(c.AuthPassword) < 12 {
		return nil, errors.New("MCP_AUTH_PASSWORD must be at least 12 characters")
	}

	port, err := strconv.Atoi(env("MCP_PORT", "8080"))
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("MCP_PORT must be a valid port number")
	}
	c.Port = port

	c.ReadOnly, err = strconv.ParseBool(env("MCP_READ_ONLY", "false"))
	if err != nil {
		return nil, errors.New("MCP_READ_ONLY must be true or false")
	}

	base := strings.TrimRight(env("MCP_BASE_URL", fmt.Sprintf("http://localhost:%d", port)), "/")
	c.BaseURL, err = url.Parse(base)
	if err != nil || (c.BaseURL.Scheme != "https" && c.BaseURL.Scheme != "http") || c.BaseURL.Host == "" ||
		c.BaseURL.RawQuery != "" || c.BaseURL.Fragment != "" {
		return nil, errors.New("MCP_BASE_URL must be an absolute http(s) URL without query or fragment")
	}
	if isOIDC {
		u, err := url.Parse(c.OIDCIssuer)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "https" && !(u.Scheme == "http" && httpx.IsLoopbackHost(u.Hostname()))) {
			return nil, errors.New("MCP_OIDC_ISSUER must be an https URL (http is only allowed on loopback)")
		}
		if c.OIDCAudience == "" {
			// Canonical resource URI, as RFC 8707 audience binding expects.
			c.OIDCAudience = base + "/mcp"
		}
		return c, c.parseLogLevel()
	}

	for _, u := range strings.Split(env("MCP_OAUTH_REDIRECT_URIS", ""), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.RedirectURIs = append(c.RedirectURIs, u)
		}
	}
	c.AllowDCR, err = strconv.ParseBool(env("MCP_OAUTH_DCR", "true"))
	if err != nil {
		return nil, errors.New("MCP_OAUTH_DCR must be true or false")
	}
	if !c.AllowDCR && c.ClientID == "" {
		return nil, errors.New("MCP_OAUTH_DCR=false requires MCP_OAUTH_CLIENT_ID")
	}

	if k := os.Getenv("MCP_OAUTH_SIGNING_KEY"); k != "" {
		if len(k) < 32 {
			return nil, errors.New("MCP_OAUTH_SIGNING_KEY must be at least 32 characters")
		}
		c.SigningKey = []byte(k)
	} else {
		// Deterministic key derived from the server secrets: tokens survive
		// restarts, and changing any secret revokes all issued tokens.
		m := hmac.New(sha256.New, []byte("news-mcp signing key v1"))
		for _, s := range []string{c.AppPassword, c.AuthPassword, c.ClientSecret, base} {
			m.Write([]byte(s))
			m.Write([]byte{0})
		}
		c.SigningKey = m.Sum(nil)
	}

	return c, c.parseLogLevel()
}

func (c *Config) parseLogLevel() error {
	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return errors.New("LOG_LEVEL must be debug, info, warn or error")
	}
	return nil
}
