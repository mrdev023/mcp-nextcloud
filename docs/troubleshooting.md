# Troubleshooting

Set `LOG_LEVEL=debug` to log every HTTP request (method, path, status) and the reason each token is rejected. Tokens and secrets are never logged.

## Startup

| Message | Cause |
|---|---|
| `missing required environment variables: …` | A required variable for the selected mode is unset. See the [configuration reference](configuration.md). |
| `MCP_AUTH_PASSWORD must be at least 12 characters` | Standalone mode requires a long server password. |
| `MCP_OIDC_ISSUER must be an https URL` | Plain HTTP is only accepted for a loopback issuer. |
| `MCP_AUTH_MODE=gateway was removed …` | The former gateway/passthrough mode forwarded tokens not issued for mcp-nextcloud. Use [OIDC mode](oidc.md). |

## MCP client cannot connect

| Symptom | Cause / fix |
|---|---|
| `401` with `WWW-Authenticate: Bearer resource_metadata="…"` | Normal first step: the client must authenticate. If it never opens a browser, check that it supports MCP OAuth. |
| `401 … error="invalid_token"` (OIDC) | The token is expired, signed by another issuer, or its `aud` lacks `MCP_OIDC_AUDIENCE`. Decode it and compare `iss` / `aud` with the configuration. A token meant for Nextcloud is refused on purpose. |
| `403 … error="insufficient_scope"` | The token lacks a scope listed in `MCP_OIDC_REQUIRED_SCOPES`. |
| Log line `OIDC discovery failed` | `MCP_OIDC_ISSUER` is unreachable, or differs from the `issuer` in its discovery document (watch for trailing slashes and http vs https behind a proxy). |
| `403 Forbidden` on `/mcp` from a local client | The request reached a loopback address with a non-local `Host` header in standalone mode. Set `MCP_BASE_URL` to the public URL. |
| Authorization page: *Wrong password* / *Too many failed attempts* | Standalone mode: the password is `MCP_AUTH_PASSWORD`. Lockout lasts up to 15 minutes. |

## Tool errors

| Tool error | Cause / fix |
|---|---|
| `could not obtain a Nextcloud token from the identity provider (token exchange failed: …)` | The provider refused the exchange; the reason follows. The provider's `error_description` is in mcp-nextcloud's logs. See the Keycloak errors below. |
| `Nextcloud authentication failed …` | Nextcloud refused the credentials. Standalone: the app-password was revoked. OIDC: see the Nextcloud errors below. |
| `item N not found` | The article id does not exist (or was purged by Nextcloud's retention). |

## Keycloak errors (token exchange)

| `error_description` | Fix |
|---|---|
| `Standard token exchange is not enabled for the requested client` | Enable *Standard Token Exchange* on the client that **authenticates** to the token endpoint (`mcp-nextcloud`, or the gateway's client), not on the audience. |
| `Requested audience not available` | The requesting client's scopes don't resolve that audience. Add an Audience mapper for it on the client's dedicated scope ([Keycloak guide](keycloak.md#2-create-the-mcp-nextcloud-client)). |
| An audience error such as `Client is not within the token audience` | The subject token's `aud` does not contain the requesting client. Add an Audience mapper on the client that issued the subject token. |
| `invalid_client` | Wrong `MCP_OIDC_CLIENT_ID` / `MCP_OIDC_CLIENT_SECRET`, or the client is not confidential. |

## Nextcloud errors

| Response | Fix |
|---|---|
| `401 "CORS requires basic auth"` | A bearer token was sent without `OCS-APIRequest: true`. mcp-nextcloud always sends it; this shows up in manual `curl` tests. |
| `401` with a valid token | Bearer checking is off (`occ user_oidc:provider <name> --check-bearer=1`), the token's `aud` is not the provider's client id, or the user never signed in to Nextcloud (and bearer provisioning is off). |
| Nextcloud cannot reach the provider | If the provider resolves to a private IP, set `'allow_local_remote_servers' => true` in `config.php`. |

## Gateway-specific

| Symptom | Fix |
|---|---|
| agentgateway: `oauth token exchange subject token missing` | The gateway removed the token after validating it. Use `jwtAuth` with `preserveToken: true`, and strip `Authorization` on other targets. |
| agentgateway: `token exchange response missing token_type` | Some Keycloak endpoints omit `token_type` ([keycloak#53954](https://github.com/keycloak/keycloak/issues/53954)). The standard token endpoint used here returns it. mcp-nextcloud itself tolerates a missing value. |
