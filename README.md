# FFMUC Starlink Manager

A small Go web application for managing the several Starlink dishes and
routers on the FFMUC account(s) from one place.

It talks to the Starlink account API — the API that the Starlink app and
the starlink.com website use — so dishes that are not reachable on a local
network can still be monitored and controlled through the Starlink cloud.

## What it does

- **Dashboard** listing every dish and router on the account: state,
  software/hardware version, SNR, ping latency/drop rate, up/downlink
  throughput, obstruction status and alerts. Unreachable devices are shown
  as `unreachable` — the page never hangs on a dead device.
- **Per-dish history charts** (ping latency, ping drop rate, downlink and
  uplink throughput) over selectable windows. The cloud history buffer only
  holds a few minutes of samples, so the app polls regularly, merges the
  samples and persists its own history on disk for longer windows.
- **Admin actions** (behind Keycloak authentication and an admin group
  claim): reboot, stow / unstow, inhibit / allow RF, scheduled power save.
- **Router view**: connected WiFi clients.
- `/healthz` is unauthenticated for container health checks.

Everything is server-rendered Go templates plus one small dependency-free
JavaScript file for charts — no SPA framework, no build tooling for the UI.

## Configuration

All configuration is via environment variables (this makes them usable as
Komodo stack env vars in production). The app fails fast at startup with a
clear message for invalid or missing values.

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `STARLINK_SSO_COOKIE` | — | **Required.** Durable `Starlink.Com.Sso` cookie value (see [Credentials](#credentials)) |
| `STARLINK_SESSION_TOKEN` | — | Optional pre-minted access token override (usually not needed) |
| `STARLINK_REFRESH_COOKIES` | — | Optional seed cookie header (`Starlink.Com.Access.V1`, `.V1C1`, `.V1C2` trio from a browser session) enabling token renewal when the SSO cookie alone cannot mint tokens |
| `STARLINK_ACCOUNT_NUMBER` | — | Optional account number (`ACC-…`), used when running with a pre-minted token so telemetry enrichment works without a session probe |
| `STARLINK_API_BASE_URL` | `https://api.starlink.com` | Account API base URL |
| `STARLINK_WEB_BASE_URL` | `https://starlink.com` | Web host used for session renewal and the device gateway |
| `STARLINK_POLL_INTERVAL` | `30s` | Roster/status/telemetry polling cadence (min 2s; the cloud updates roughly every 2 min per device) |
| `STARLINK_HISTORY_WINDOW` | `24h` | How much per-device history to keep locally |
| `STARLINK_CALL_TIMEOUT` | `12s` | Timeout per device API call |
| `STARLINK_DATA_DIR` | `data` | Directory for persisted history (`data/store.json`) |
| `STARLINK_ROSTER_CACHE_TTL` | `5m` | Cache lifetime for the account device roster |
| `STARLINK_TELEMETRY_ONLINE_TTL` | `5m` | Telemetry age after which a device counts as offline in the UI |
| `AUTH_MODE` | `oidc` | `oidc`, `forwardauth` or `localdev` (see [Authentication](#authentication)) |
| `LOCAL_DEV_USER` | `false` | Dev mode: treat every request as an authenticated admin. Never enable in production. |
| `OIDC_ISSUER_URL` | — | Keycloak realm issuer URL (required in `oidc` mode) |
| `OIDC_CLIENT_ID` | — | OIDC client ID (required in `oidc` mode) |
| `OIDC_CLIENT_SECRET` | — | OIDC client secret (required in `oidc` mode) |
| `OIDC_REDIRECT_URL` | — | Redirect/callback URL of this app (required in `oidc` mode) |
| `OIDC_ADMIN_CLAIM` | `groups` | Token claim checked for admin groups |
| `OIDC_ADMIN_GROUPS` | — | Comma-separated claim values granting admin rights (required unless `localdev`) |
| `FORWARD_AUTH_USER_HEADER` | `X-Forwarded-User` | Forward-auth header names (forwardauth mode) |
| `FORWARD_AUTH_EMAIL_HEADER` | `X-Forwarded-Email` | |
| `FORWARD_AUTH_GROUPS_HEADER` | `X-Forwarded-Groups` | |
| `SESSION_COOKIE_SECRET` | — | Random secret (≥ 32 chars) signing the session cookie; required in `oidc`/`forwardauth` mode |
| `SESSION_COOKIE_SECURE` | auto | Force `Secure` on session cookies (auto-on when the OIDC redirect URL is HTTPS) |

## Credentials

There is exactly one Starlink credential the app needs:
the **`Starlink.Com.Sso` cookie** from a browser login at
[starlink.com](https://www.starlink.com).

To obtain it:

1. Log in at <https://www.starlink.com> in a normal browser session.
2. Open the browser dev tools → Application/Storage → Cookies →
   `starlink.com`.
3. Copy the value of the cookie named `Starlink.Com.Sso`.
4. Set it as `STARLINK_SSO_COOKIE`.

This cookie is long-lived; when Starlink invalidates it the UI shows a
"session invalid" banner and the secret must be refreshed by logging in
again and copying the new value.

### Where to put the credential

**Local development** — put it in a `.env` file that is git-ignored
(the repo ships `.env.example`; copy it to `.env` and fill in the value).
Never commit real values; `.env` is in `.gitignore` and gitleaks runs on
the repo before anything is pushed. Alternatively export it in the shell:

```sh
export STARLINK_SSO_COOKIE="<value from the browser>"
```

**Production (FFMUC, Komodo + Salt)** — credentials live as **Komodo
Periphery secrets**, never as literal values in the repo:

1. Store the value in the Periphery secret store and reference it from the
   `komodo-secrets/<host>.sls` Salt pillar.
2. In the Komodo stack (`stacks.toml`), interpolate it with
   `[[secrets.<name>]]` syntax into the compose environment.
3. The compose file passes it through as `${STARLINK_SSO_COOKIE}` to the
   container.

The env var names the app reads are exactly the ones in the table above, so
the Komodo mapping is one-to-one: `STARLINK_SSO_COOKIE`,
`SESSION_COOKIE_SECRET` and (if `oidc` mode is used) `OIDC_CLIENT_SECRET`
are the secrets involved.

## Running locally

With Go (1.27+):

```sh
cp .env.example .env
# edit .env: set STARLINK_SSO_COOKIE
set -a; source .env; set +a
LOCAL_DEV_USER=true go run ./cmd/starlink-manager
# → http://localhost:8080  (every request is treated as an admin)
```

With Docker:

```sh
cp .env.example .env   # fill in STARLINK_SSO_COOKIE
docker compose -f docker-compose.local.yaml up --build
# → http://localhost:8080
```

## Authentication

`AUTH_MODE` selects one of three modes:

- **`oidc`** (default) — full Keycloak login flow (authorization code +
  ID token verification) via `/login` and `/callback`. Any authenticated
  Keycloak user gets read-only access. A user is an **admin** if the claim
  named by `OIDC_ADMIN_CLAIM` (default `groups`) contains one of the values
  in `OIDC_ADMIN_GROUPS`. Admin actions return **403** for everyone else.
- **`forwardauth`** — production FFMUC mode: the app sits behind Traefik
  with the `noc-auth` forward-auth middleware; the proxy injects
  `X-Forwarded-User` / `X-Forwarded-Email` / `X-Forwarded-Groups` headers,
  which the app trusts. Group membership decides the admin flag the same
  way. The app must only be reachable through that proxy.
- **`localdev`** — development only (`LOCAL_DEV_USER=true`): every request
  is treated as an authenticated admin. Never expose this to a network.

## Deployment (ffmuc-salt-public)

The intended production contract, for the Komodo/Salt stack:

- Single static Go binary in a minimal distroless container,
  image `ghcr.io/awlx/starlink-manager`.
- Config **only** via environment variables (see table above); they become
  Komodo stack env vars. Secrets via Komodo Periphery secrets as described
  above.
- Listens on `LISTEN_ADDR` (default `:8080`), `/healthz` unauthenticated.
- Runs on the internal management Docker host, fronted by Traefik with
  Keycloak-based forward auth (`AUTH_MODE=forwardauth`).
- Persistent history lives in a volume mounted at `/data`.

## Security notes

- All admin actions require the configured admin group; read-only users
  can view dashboards and charts but get 403 on actions. Destructive
  actions (reboot, stow, power save, RF inhibit) show confirm dialogs.
- State-changing POSTs are same-origin checked (Origin header must match
  the request host when present) and session cookies are SameSite=Lax,
  HMAC-signed and expire after 12 hours.
- The Starlink SSO cookie grants full account access — treat it like a
  password. It is only ever stored in env vars/secrets, never in files
  committed to the repository.
- The app talks to Starlink over HTTPS only.

## Development

```sh
go test ./...   # unit tests incl. fake account API + auth middleware
go vet ./...
go build ./...
```

Device protocol definitions vendored from public sources under `protos/`;
the generated Go stubs live in `api/`. The app speaks the same wire
protocol the Starlink app/site uses, via HTTP/gRPC-Web through the
Starlink web host.