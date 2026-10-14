package config

import (
	"maps"
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"NEXTCLOUD_URL", "NEXTCLOUD_USER", "NEXTCLOUD_APP_PASSWORD", "MCP_AUTH_PASSWORD",
		"MCP_AUTH_MODE", "MCP_BASE_URL", "MCP_PORT", "MCP_OAUTH_CLIENT_ID", "MCP_OAUTH_DCR", "LOG_LEVEL",
		"MCP_OIDC_ISSUER", "MCP_OIDC_AUDIENCE", "MCP_OIDC_REQUIRED_SCOPES", "MCP_OIDC_CLIENT_ID",
		"MCP_OIDC_CLIENT_SECRET", "MCP_OIDC_EXCHANGE_AUDIENCE", "MCP_OIDC_EXCHANGE_SCOPE", "MCP_READ_ONLY"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// ---------- tests ----------

func TestLoadConfigModes(t *testing.T) {
	oidcEnv := map[string]string{
		"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "oidc",
		"MCP_OIDC_ISSUER": "https://id.example/realms/r", "MCP_OIDC_CLIENT_ID": "mcp-nextcloud",
		"MCP_OIDC_CLIENT_SECRET": "x", "MCP_OIDC_EXCHANGE_AUDIENCE": "nextcloud",
		"MCP_BASE_URL": "https://news.example",
	}
	with := func(kv map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range oidcEnv {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name     string
		env      map[string]string
		wantErr  string
		wantMode string
		wantAud  string
	}{
		{"oidc defaults audience to resource URI", oidcEnv, "", AuthModeOIDC, "https://news.example/mcp"},
		{"oidc explicit audience", with(map[string]string{"MCP_OIDC_AUDIENCE": "mcp-nextcloud"}), "", AuthModeOIDC, "mcp-nextcloud"},
		{"oidc missing settings", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "oidc"},
			"MCP_OIDC_CLIENT_ID, MCP_OIDC_CLIENT_SECRET, MCP_OIDC_EXCHANGE_AUDIENCE, MCP_OIDC_ISSUER", "", ""},
		{"oidc plain http issuer refused", with(map[string]string{"MCP_OIDC_ISSUER": "http://id.example"}), "https", "", ""},
		{"oidc http loopback issuer allowed", with(map[string]string{"MCP_OIDC_ISSUER": "http://127.0.0.1:9000/realms/r"}), "", AuthModeOIDC, ""},
		{"standalone still needs secrets", map[string]string{"NEXTCLOUD_URL": "https://cloud.example"}, "MCP_AUTH_PASSWORD", "", ""},
		{"oauth is an alias of standalone", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "oauth",
			"NEXTCLOUD_USER": "u", "NEXTCLOUD_APP_PASSWORD": "p", "MCP_AUTH_PASSWORD": "correct horse battery"}, "", AuthModeStandalone, ""},
		{"gateway mode removed", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "gateway"}, "use MCP_AUTH_MODE=oidc", "", ""},
		{"passthrough mode removed", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "passthrough"}, "use MCP_AUTH_MODE=oidc", "", ""},
		{"unknown mode", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "magic"}, "MCP_AUTH_MODE", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			cfg, err := Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.AuthMode != tc.wantMode {
				t.Errorf("mode = %q, want %q", cfg.AuthMode, tc.wantMode)
			}
			if tc.wantAud != "" && cfg.OIDCAudience != tc.wantAud {
				t.Errorf("audience = %q, want %q", cfg.OIDCAudience, tc.wantAud)
			}
		})
	}
}

func TestReadOnly(t *testing.T) {
	base := map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "NEXTCLOUD_USER": "u",
		"NEXTCLOUD_APP_PASSWORD": "p", "MCP_AUTH_PASSWORD": "correct horse battery"}
	for value, want := range map[string]bool{"": false, "false": false, "true": true, "1": true} {
		env := maps.Clone(base)
		env["MCP_READ_ONLY"] = value
		setEnv(t, env)
		cfg, err := Load()
		if err != nil || cfg.ReadOnly != want {
			t.Errorf("MCP_READ_ONLY=%q: ReadOnly = %v, err = %v; want %v", value, cfg != nil && cfg.ReadOnly, err, want)
		}
	}
	// Also honoured in OIDC mode, which returns early from Load.
	setEnv(t, map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "oidc",
		"MCP_OIDC_ISSUER": "https://id.example/realms/r", "MCP_OIDC_CLIENT_ID": "mcp-nextcloud",
		"MCP_OIDC_CLIENT_SECRET": "x", "MCP_OIDC_EXCHANGE_AUDIENCE": "nextcloud", "MCP_READ_ONLY": "true"})
	if cfg, err := Load(); err != nil || !cfg.ReadOnly {
		t.Errorf("oidc mode: cfg = %+v, err = %v", cfg, err)
	}
	env := maps.Clone(base)
	env["MCP_READ_ONLY"] = "maybe"
	setEnv(t, env)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MCP_READ_ONLY must be true or false") {
		t.Errorf("invalid value: err = %v", err)
	}
}
