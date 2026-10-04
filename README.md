# instalker

A Telegram bot that polls Instagram for new **posts** and **stories** from a fixed
set of accounts and forwards them to one chat.

## How it works

```
poller ──▶ instagram (web private API)
   │
   ├──▶ storage  (SQLite: which media was already delivered)
   └──▶ notifier (telebot: photos, videos, albums)
```

- `cmd/instalker` — wiring and graceful shutdown
- `internal/domain` — types and sentinel errors, no dependencies
- `internal/instagram` — Instagram web private API client
- `internal/poller` — business logic: resolve targets, diff, deliver
- `internal/schedule` — when each account is checked, embedded as TOML
- `internal/storage` — SQLite state, migrations embedded and applied on startup
- `internal/notifier` — Telegram delivery
- `internal/health`, `internal/metrics`, `internal/logging`, `internal/config`

Who is watched, and how often, comes from `internal/schedule/schedule.toml`.
When it lists no accounts, `TARGETS` is used, and failing that the accounts the
logged-in user **follows**.

Resolved accounts are remembered in the database, and startup uses them without
asking Instagram again. An account the database does not know yet is resolved
from its profile page.

The first cycle for a target only records a baseline — existing posts and live
stories are marked as seen without being sent, so starting the bot does not dump
history into the chat. Everything after that is forwarded.

## Commands

`/ping` scrapes every watched account once and reports what came back, so you can
tell whether scraping works right now instead of waiting for the next tick to
show up in the logs:

```
🟡 Instagram scraping is partly working
checked in 1.2s

✅ locroise — 12 posts, 2 stories, latest 2h0m0s ago
🟡 lem1rol — posts: rate limited by Instagram, 0 stories
```

Each feed is reported separately, so one failing feed does not hide a working
one. A failure names its reason (throttling, a logged-out session, a pending
challenge), because each needs a different response. The probe delivers
nothing and does not touch the seen-state, so running it never causes a missed
or duplicated notification.

`/session <sessionid>` replaces the Instagram session cookie at runtime. The new
cookie is applied immediately and saved to the database, so rotation needs no
redeploy and no restart. The message carrying the cookie is deleted from the chat
once processed.

Commands are only accepted from `CHAT_ID`; the bot's username is public, so
anything else is ignored.

## Testing

`test/offline` drives the whole bot as a real process against a **fake Bot API
server** (`internal/tgfake`) — no network and nothing to configure. It runs in CI
with `go test ./...` and covers what unit tests cannot: whether commands are
reachable, routed, authorised, and answered, whether side effects reach the
database, and whether the bot survives the API failing under it.

The bot reaches the fake through `TELEGRAM_API_URL`, which is empty in
production and also serves a self-hosted Bot API or Telegram's test environment.

```sh
go test ./test/offline/ -v
```

## Configuration

Copy `.example.env` to `.env` and fill it in. Every variable is documented there.
The process exits immediately if anything required is missing or invalid.

### Schedule

When each account is checked lives in `internal/schedule/schedule.toml`, which is
compiled into the binary:

```toml
timezone = "Europe/Kyiv"

[window]
from = "10:00"
to = "01:00"

[defaults]
posts = "1h"
stories = "1h"

[[accounts]]
username = "locroise"
stories = "30m"
```

Nothing is polled outside the window, which may cross midnight. Each account
takes the defaults unless it overrides them, and `"0"` switches a feed off.
The accounts listed here are also *who* gets watched; `TARGETS` and the
following list are only fallbacks when the file names none.

Changing the schedule means editing that file and rebuilding — no secret to
re-encrypt, no manifest to touch. `SCHEDULE_PATH` loads a file from disk instead,
which is how a ConfigMap or a local experiment overrides it without a rebuild.

The timezone database is compiled in (`time/tzdata`), so the minimal container
image resolves `Europe/Kyiv` without needing a `tzdata` package.

## Deploying

Manifests live in `k8s/` and are the ground truth for what runs in the cluster.
Flux reconciles them — nothing is applied by hand. The cluster side is registered
in the `homelab` repo under `clusters/main/apps/instalker/`.

### Secrets

The bot's credentials live in `k8s/secrets.yaml`, which is **gitignored**, and are
committed only in SOPS-encrypted form as `k8s/secrets.enc.yaml`. Encryption uses
the shared app age recipient, so Flux decrypts it in-cluster with the
existing `apps-sops-age` secret referenced by the Kustomization.

To change a credential:

```sh
sops -d k8s/secrets.enc.yaml > k8s/secrets.yaml   # needs the age private key
$EDITOR k8s/secrets.yaml
sops -e k8s/secrets.yaml > k8s/secrets.enc.yaml
```

**`IG_SESSIONID` is not rotated this way.** It expires on its own schedule, far
more often than anything else here, so the database on the PVC is its source of
truth and `/session` is how it is replaced. The environment variable is only a
bootstrap: it seeds the database the first time there is nothing stored, and is
ignored from then on. Once seeded it can be emptied.

Image pulls from GHCR authenticate through the k3s node's `registries.yaml`, so the namespace needs no pull secret.
The SQLite file sits on a 1Gi `ReadWriteOnce` PVC mounted at `/app/data`. Because
that volume cannot be attached twice, the Deployment uses the `Recreate` strategy
and stays at one replica — a rolling update would deadlock on the mount. The
container runs as UID 10001 with a read-only root filesystem and all capabilities
dropped; `/tmp` is an `emptyDir`.

There is no Ingress: the service exposes only `/health` and `/metrics`, which are
cluster-internal. Add one only if you want to scrape metrics from outside.

### Pipelines

- **CI** (`.github/workflows/ci.yaml`) — gofmt check, vet, test, build.
- **CD** (`.github/workflows/cd.yaml`) — runs only after CI succeeds on `main`.
  Builds the `production` stage, pushes it to GHCR tagged with the full commit
  SHA (and `latest`), then rewrites the image tag in `k8s/bot/deployment.yaml`
  and commits it as `deploy: instalker <sha>`.

The image name follows `${{ github.repository }}`, so the placeholder
`ghcr.io/chivta/instalker` in the manifest is replaced on the first CD run.

## Two things must be done by hand

### 1. The Telegram chat must message the bot first

Telegram forbids bots from opening a conversation. Open
[@talinstalerbot](https://t.me/talinstalerbot) from the account behind
`CHAT_ID` and press **Start**. Until then every send fails with
`chat not found`.

### 2. Instagram logins

The bot logs in with `USERNAME` and `PASSWORD` when it has no session, and again
whenever Instagram ends the session it has, at most once every 2 hours so a
failing account is not pushed into a checkpoint.

A checkpoint (Instagram asking for a code sent to the account's email or phone)
needs a person. Log in at <https://www.instagram.com> as `USERNAME`, complete the
verification, copy the `sessionid` cookie from DevTools, and send it to the bot
as `/session <value>`.

## Notes

- SQLite (pure-Go `modernc.org/sqlite`) is used instead of Postgres: the state is
  a single dedupe table, and a one-binary deploy with no database server is worth
  more here than the shared convention.
- Stories expire after 24 hours, so keep story intervals in `schedule.toml` well
  below that.
- Posts come from the GraphQL query Instagram's own profile page runs,
  `PolarisProfilePostsTabContentQuery_connection`. In September 2026 Instagram
  stopped serving `/api/v1/feed/user/` to web sessions and redirects it to the
  homepage. The query's `doc_id` rotates; when it stops working the bot reads the
  current one out of the page's scripts. gallery-dl's Instagram extractor
  (codeberg.org/mikf/gallery-dl) follows these changes closely and is the place
  to look when this breaks again.
- Instagram answers a logged-out session the way it answers an anonymous
  visitor: a 401 with `"require_login":true` and a "Please wait a few minutes"
  message. That message is not throttling, and the client treats it as a dead
  session. Throttling of a live session arrives as a 429.
- Each feed backs off on its own when throttled, so a throttled feed does not
  pause a working one.
- Polling too aggressively is what gets Instagram accounts flagged. 5 minutes is
  a reasonable floor.
