# AGENTS.md

Guidance for AI coding agents (Claude Code, Codex, Copilot, etc.) working in this repo.
Human contributors should find it useful too.

## What this is

`sal-pub` is an early-stage Go service that gives users of the `sal` Keycloak realm an
ActivityPub identity (actor, WebFinger, inbox/outbox). Users sign in through Keycloak
(which brokers Microsoft, GitHub, Google and GitLab logins), and each Keycloak subject
maps to one ActivityPub actor stored in Postgres.

The project is a **prototype**. Two approaches are being explored side by side:

1. **Custom implementation** in `internal/` and `cmd/server/`: hand-rolled ActivityPub
   handlers backed by Postgres, with Keycloak OIDC login.
2. **FedBOX** (`docker-compose-fedbox.yml`): the off-the-shelf go-ap stack
   (`fedbox`, `point`, `auth`) for comparison.

Don't assume one is canonical. Ask before deleting or consolidating either one.

## Layout

| Path | Purpose |
|---|---|
| `cmd/server/main.go` | Runnable OIDC login demo (Keycloak → session cookie). Listens on `:8000`. |
| `main.go` | Older AP entrypoint on `:8080`. **Broken:** it imports `internal/handlers/*`, which doesn't exist. |
| `internal/activitypub/` | Actor, WebFinger, inbox (stub, 501) and outbox HTTP handlers. Not wired into any `main` yet. |
| `internal/actor/` | RSA-4096 keypair generation for actor HTTP signatures. |
| `internal/auth/` | Bearer-token OIDC middleware (`Claims` in request context). |
| `internal/store/` | Postgres access for the `actors` table (`database/sql` + `lib/pq`). |
| `pkg/util/` | Small shared helpers (`GetEnv`). |
| `migrations/` | Plain SQL migrations, numbered `NNN_name.sql`. No migration tool is wired up yet. |
| `docker/caddy/` | Reverse proxy: `/auth/*` → Keycloak. |
| `docker/keycloak/` | Keycloak 26 image plus the `sal` realm import (`sal.json`). |
| `docker/fedbox/` | FedBOX config (`fedbox.env`, `Caddyfile`). `storage/` holds runtime data, not source. |

## Commands

```sh
# Build / check (scope to the packages that compile; the root package is broken)
go build ./cmd/... ./internal/... ./pkg/...
go vet   ./cmd/... ./internal/... ./pkg/...
gofmt -l .                      # should print nothing
go test  ./...                  # no tests exist yet

# Local infra: Caddy + Keycloak + Postgres
cp .env.default .env            # first time only, then fill in KEYCLOAK_CLIENT_SECRET etc.
docker compose up -d --build
docker compose logs -f keycloak

# Run the OIDC demo app against that stack
set -a && source .env && set +a && go run ./cmd/server   # http://localhost:8000

# FedBOX experiment (separate stack)
docker compose -f docker-compose-fedbox.yml up -d
```

Keycloak admin is at `$SALPUB_HOSTNAME/auth`. Default bootstrap credentials are in `.env.default`.

## Configuration

All runtime config comes from environment variables, read through `util.GetEnv(key, fallback)`.
`.env.default` is the committed template and `.env` is the local, git-ignored copy. When you add
a variable, add it to `.env.default` with a safe placeholder value.

Variables the Go code reads: `KC_HOSTNAME`, `KC_HTTP_RELATIVE_PATH`, `KEYCLOAK_REALM`,
`KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`, `REDIRECT_URL`, `SESSION_SECRET`.

`docker/keycloak/sal.json` uses `${VAR}` placeholders, which Keycloak fills from its own
environment when it imports the realm. Identity-provider credentials come from `IDP_<ALIAS>_CLIENT_ID`
and `IDP_<ALIAS>_CLIENT_SECRET` (plus `IDP_MICROSOFT_TENANT_ID`). Don't put literal secrets in that
file, and don't name new variables with a `KC_` prefix, because Keycloak reserves it for its own options.
`--import-realm` skips a realm that already exists, so changes only take effect when Keycloak starts
with a fresh database.

## Conventions

- Go 1.25. Use only the standard library plus what's already in `go.mod`. Ask before adding a dependency.
- Run `gofmt` on every Go file you touch. Some existing files use spaces; fixing that while you're there is fine.
- Wrap errors with context: `fmt.Errorf("verb noun: %w", err)`. Return `store.ErrNotFound` for missing rows.
- HTTP handlers are constructors that close over their dependencies (`func XHandler(s *store.Store) http.HandlerFunc`).
- ActivityPub responses use `Content-Type: application/activity+json`. WebFinger responses use `application/jrd+json`.
- Schema changes go in a new numbered file in `migrations/`. Never edit a migration that has already been applied.
- Prefer the standard library over hand-written helpers. For example, `internal/activitypub` has a
  hand-rolled `stringsSplit` that should just be `strings.Split`.

## Known issues and gotchas

- **Secrets.** Real IdP client secrets live only in `.env` and in `t.json` (a full realm export).
  Both files are git-ignored. Don't copy secret values into code, docs or commit messages, and don't
  print them in output.
- `main.go` (root) doesn't compile (see Layout). `go build ./...` fails because of it.
- `go.mod` marks directly used modules (`go-oidc`, `oauth2`, `gorilla/sessions`, `lib/pq`) as
  `// indirect`. Run `go mod tidy` once the root package builds.
- **Development only:** `InsecureSkipVerify: true` is set in both `cmd/server` and `internal/auth`
  so the stack works with Caddy's local CA. The OAuth `state` is hard-coded, and `SESSION_SECRET`
  has an insecure fallback. Fix all three before anything is deployed, and don't copy these
  patterns into new code.
- The inbox doesn't verify HTTP Signatures yet and deliberately returns 501. Don't accept inbox
  activities without signature verification.
- `actors.private_key_pem` is stored in plaintext.

## Working agreements for agents

- Keep changes small and scoped to the task. Don't reformat or restructure unrelated code.
- Check that your change builds with the scoped `go build` / `go vet` commands above, and report
  the results honestly.
- Don't run `docker compose down -v`, delete `docker/fedbox/storage/`, or drop database tables
  without asking first.
- Commit only when asked. Branch off `main` first.
