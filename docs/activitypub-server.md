# sal-pub ActivityPub server

`cmd/server` is a minimal ActivityPub server. Each person who signs in through
Keycloak gets an actor that other fediverse servers (Mastodon, etc.) can find
and follow. From the web page they can publish public notes, and those notes are
delivered to their followers.

## Running it with Docker

```sh
cp .env.default .env          # first time; fill in KEYCLOAK_CLIENT_SECRET etc.
docker compose up -d --build  # Caddy + sal-pub + Keycloak + Postgres
docker compose logs -f salpub
```

Caddy serves everything at `SALPUB_HOSTNAME`. Requests under `/auth/*` go to Keycloak, and
everything else goes to the `salpub` container (built from the root `Dockerfile`). Open
`SALPUB_HOSTNAME` and choose **Log in with Keycloak**. The first login creates your actor, and
the page then shows your handle (e.g. `@jane_doe@localhost`).

`salpub` exits if Postgres or Keycloak isn't ready yet, and Compose restarts it until they are.
After changing Go code, run `docker compose up -d --build salpub`.

Inside the container the public hostname points back at the container itself, so the server
fetches Keycloak's configuration from `OIDC_DISCOVERY_URL` (`http://keycloak:8080/...`, set in
`docker-compose.yml`). It still checks tokens against the public issuer. For this to work,
Keycloak needs `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true` and a `KC_HOSTNAME` that is a full URL
including `/auth` (both are in `.env.default`). Those settings make Keycloak give internal
callers internal token and key URLs, while browsers keep getting the public ones.

If `SALPUB_HOSTNAME` is HTTPS with Caddy's local CA, also set `OIDC_INSECURE_SKIP_VERIFY=true`
(development only).

**Existing realms.** `--import-realm` skips a realm that already exists. If yours predates
these settings, open the `sal-pub` client in the Keycloak admin console and set:
- **Valid redirect URIs** to `REDIRECT_URL` (e.g. `https://localhost/oauth/callback`)
- **Valid post logout redirect URIs** to `SALPUB_HOSTNAME/` (e.g. `https://localhost/`)

If Postgres was already running before `docker/postgres/01-salpub.sql` existed, create the
database once:

```sh
docker compose exec postgres createdb -U keycloak salpub
```

### Without Docker

`set -a && source .env && set +a && go run ./cmd/server` runs the server on `LISTEN_ADDR` against
the same Postgres and Keycloak. Set `BASE_URL` and `REDIRECT_URL` to `http://localhost:8000`
URLs, and add that redirect URI to the Keycloak client.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `SALPUB_HOSTNAME` | `http://localhost:8000` (`.env.default`: `${SALPUB_HOSTNAME}`) | Public URL. Actor IDs are built from it, so don't change it once real users exist. A bare host gets `https://`. |
| `LISTEN_ADDR` | `:8000` | Address to listen on. |
| `DATABASE_URL` | `postgres://keycloak:keycloak@localhost:5432/salpub?sslmode=disable` | Postgres DSN (Compose points it at the `postgres` service). Migrations in `migrations/` are applied at startup. |
| `OIDC_DISCOVERY_URL` | unset | Internal URL to fetch Keycloak's OIDC discovery document from (set by Compose). |
| `KC_HOSTNAME`, `KC_HTTP_RELATIVE_PATH`, `KEYCLOAK_REALM` | `http://localhost`, `/auth`, `sal` | Used to build the OIDC issuer URL. `KC_HOSTNAME` may be a bare host or a full URL that already ends in the relative path. |
| `KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET` | `sal-pub` | OIDC client. |
| `REDIRECT_URL` | `$SALPUB_HOSTNAME/oauth/callback` | Must match the client's redirect URI in Keycloak, and must not be under `/auth` (Caddy routes that to Keycloak). |
| `SESSION_SECRET` | random per start | Cookie signing key (`openssl rand -hex 32`). |
| `OIDC_INSECURE_SKIP_VERIFY` | `false` | Development only. Skips TLS verification for Keycloak calls. |

## Endpoints

| Route | What it does |
|---|---|
| `GET /` | Home page: log in, see your handle and followers, publish notes. |
| `GET /login`, `GET /oauth/callback` | Keycloak authorization-code flow with PKCE. |
| `POST /logout` | Clears the session, then ends the Keycloak SSO session through its end-session endpoint and returns to `SALPUB_HOSTNAME`. |
| `POST /notes` | Session-authenticated and protected against cross-origin posts. |
| `GET /.well-known/webfinger?resource=acct:user@host` | WebFinger lookup. |
| `GET /users/{username}` | Actor (`Person`) with its public key. |
| `POST /users/{username}/inbox` | Accepts `Follow` and `Undo{Follow}`. Requires a valid HTTP Signature. |
| `GET /users/{username}/outbox` | Latest 50 `Create{Note}` activities. |
| `GET /users/{username}/followers`, `/following` | Collections (`following` is always empty). |
| `GET /notes/{id}` | A single note. |

## How it works

- **Identity.** On first login the Keycloak `sub` is mapped to a new actor. The username comes
  from `preferred_username`, lowercased and reduced to `[a-z0-9_]`. If that name is taken, a
  `_2`, `_3`, … suffix is added. The username never changes afterwards, but the display name
  follows Keycloak's `name` claim.
- **Keys.** Each actor gets a 2048-bit RSA key. It is used to sign outgoing requests, and remote
  servers verify those signatures with the public key published in the actor document.
- **Inbox.** The server verifies each incoming request with draft-cavage HTTP Signatures
  ([go-fed/httpsig](https://github.com/go-fed/httpsig)), which is what Mastodon uses. It
  requires `(request-target)`, `host`, `date` and `digest` to be signed, a `Date` within one
  hour of now, a `Digest` that matches the body, and a signing actor that matches the activity's
  `actor`. A `Follow` is accepted automatically, and the server replies with a signed `Accept`.
- **Vocabulary.** ActivityStreams documents are built and parsed with
  [go-ap/activitypub](https://github.com/go-ap/activitypub).

## Federating with real servers

Remote servers need to reach `SALPUB_HOSTNAME` over public HTTPS, and most refuse plain-HTTP actors.
For a quick test, run a tunnel (for example `cloudflared` or `ngrok`) in front of port 8000.
Set `SALPUB_HOSTNAME` and `REDIRECT_URL` to the tunnel URL, and add that redirect URI to the client in
Keycloak. Then search for `@you@your-tunnel-host` from a Mastodon account.

## Limitations

- Deliveries are sent once, with no retry queue.
- Remote keys aren't cached, so every inbox request fetches the sender's actor.
- Local users can't follow anyone, and the inbox ignores everything except `Follow` and `Undo{Follow}`.
- Private keys are stored in plaintext in Postgres.
