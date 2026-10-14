# Documentation

mcp-nextcloud-news exposes a Nextcloud News account to MCP clients. Pick the authentication mode that matches your setup:

| You have… | Use | Guide |
|---|---|---|
| A Nextcloud account and nothing else | `MCP_AUTH_MODE=standalone` (default) | [Standalone mode](standalone.md) |
| An OpenID Connect provider (Keycloak, …) that Nextcloud already trusts | `MCP_AUTH_MODE=oidc` | [OIDC mode](oidc.md), then [Keycloak example](keycloak.md) |

- **Standalone** is the quickest start. One person uses one shared Nextcloud app-password, and news-mcp runs its own small OAuth server.
- **OIDC** suits multiple users with single sign-on. Each user reaches Nextcloud with their own identity, and news-mcp never stores credentials.

Reference:

- [Configuration reference](configuration.md): every environment variable and HTTP endpoint.
- [Architecture](architecture.md): code layout, request lifecycle, design choices, tests and build.
- [Troubleshooting](troubleshooting.md): common errors and what they mean.
