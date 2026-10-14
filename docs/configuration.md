# Configuration reference

news-mcp is configured with environment variables only.

## Common settings

| Variable | Required | Description |
|---|---|---|
| `MCP_AUTH_MODE` | no | `standalone` (default) or `oidc` |
| `NEXTCLOUD_URL` | yes | Nextcloud base URL, e.g. `https://cloud.example.com` |
| `MCP_BASE_URL` | no | Public URL of this server; `$MCP_BASE_URL/mcp` is the resource URI. Default `http://localhost:$MCP_PORT` |
| `MCP_PORT` | no | Listen port, default `8080` |
| `MCP_READ_ONLY` | no | `true` exposes only the read-only tools (`news_list_feeds`, `news_list_items`, `news_get_item`, `news_get_updated`); default `false` |
| `LOG_LEVEL` | no | `debug`, `info` (default), `warn`, `error` |
| `GOMEMLIMIT` | no | Go runtime soft memory limit; 32 MiB by default |

## Standalone mode

| Variable | Required | Description |
|---|---|---|
| `NEXTCLOUD_USER` | yes | Nextcloud user name |
| `NEXTCLOUD_APP_PASSWORD` | yes | Nextcloud app-password |
| `MCP_AUTH_PASSWORD` | yes | Password (≥ 12 characters) typed on the authorization page. This is what protects access. |
| `MCP_OAUTH_CLIENT_ID` / `MCP_OAUTH_CLIENT_SECRET` | no | Pre-registered client. Without a secret it is a public client. |
| `MCP_OAUTH_REDIRECT_URIS` | no | Comma-separated redirect URIs allowed for the pre-registered client. Default: any loopback URI plus the claude.ai / claude.com callbacks. |
| `MCP_OAUTH_DCR` | no | Enable dynamic client registration at `/register`, default `true` |
| `MCP_OAUTH_SIGNING_KEY` | no | HMAC key for tokens (≥ 32 characters). Derived from the secrets above by default. |

## OIDC mode

| Variable | Required | Description |
|---|---|---|
| `MCP_OIDC_ISSUER` | yes | Issuer URL, e.g. `https://id.example.com/realms/example`. Must be https (http only on loopback). Discovery uses `$MCP_OIDC_ISSUER/.well-known/openid-configuration`. |
| `MCP_OIDC_AUDIENCE` | no | Audience (`aud`) that incoming tokens must contain. Default: the resource URI `$MCP_BASE_URL/mcp`. |
| `MCP_OIDC_REQUIRED_SCOPES` | no | Space- or comma-separated scopes every token must carry; also advertised in the metadata. Default: none. |
| `MCP_OIDC_CLIENT_ID` / `MCP_OIDC_CLIENT_SECRET` | yes | news-mcp's own confidential client at the provider, used for token exchange (`client_secret_basic`). |
| `MCP_OIDC_EXCHANGE_AUDIENCE` | yes | Audience requested for Nextcloud tokens, e.g. the client id Nextcloud validates tokens against. |
| `MCP_OIDC_EXCHANGE_SCOPE` | no | Scope requested in the token exchange |

Secrets and tokens are never logged; the access log only records method, path and status.

## Endpoints

| Path | Mode | Purpose |
|---|---|---|
| `/mcp` | both | MCP Streamable HTTP endpoint (bearer token required) |
| `/healthz` | both | Liveness probe, returns `ok` |
| `/.well-known/oauth-protected-resource/mcp` (and the root fallback `/.well-known/oauth-protected-resource`) | both | Protected Resource Metadata (RFC 9728) |
| `/.well-known/oauth-authorization-server` | standalone | Authorization Server Metadata (RFC 8414) |
| `/authorize`, `/token` | standalone | OAuth authorization and token endpoints |
| `/register` | standalone, if `MCP_OAUTH_DCR=true` | Dynamic client registration (RFC 7591) |

When `MCP_BASE_URL` has a path (e.g. `/news`), every route is mounted under it. The well-known suffix is inserted before that path instead: `/.well-known/oauth-protected-resource/news/mcp`, `/.well-known/oauth-authorization-server/news`.
