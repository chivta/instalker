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
- `internal/storage` — SQLite state, migrations embedded and applied on startup
- `internal/notifier` — Telegram delivery
- `internal/health`, `internal/metrics`, `internal/logging`, `internal/config`

Targets come from `TARGETS`; when it is empty, the accounts the logged-in user
**follows** are watched instead.

The first cycle for a target only records a baseline — existing posts and live
stories are marked as seen without being sent, so starting the bot does not dump
history into the chat. Everything after that is forwarded.

## Commands

`/ping` scrapes every watched account once and reports what came back, so you can
tell whether scraping works right now instead of waiting for the next tick to
show up in the logs:

```
🏓 Instagram scraping is working
checked in 1.2s

✅ locroise — 12 posts, 2 stories, latest 2h0m0s ago
✅ lem1rol — 11 posts, 0 stories, latest 18h0m0s ago
```

When it fails, the reason is spelled out — throttling, a rejected session, or a
pending challenge — because those need different responses. The probe delivers
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

## Running

```sh
go run ./cmd/instalker          # local
docker compose up               # local, hot reload via air
docker build --target production -t instalker .
```

`GET /health` returns 204, `GET /metrics` exposes counters in Prometheus format.

## Deploying

Manifests live in `k8s/` and are the ground truth for what runs in the cluster.
Flux reconciles them — nothing is applied by hand. The cluster side is registered
in the `homelab` repo under `clusters/main/apps/instalker/`.

### Secrets

The bot's credentials live in `k8s/secrets.yaml`, which is **gitignored**, and are
committed only in SOPS-encrypted form as `k8s/secrets.enc.yaml`. Encryption uses
the same age recipient as `ruscan`, so Flux decrypts it in-cluster with the
existing `ruscan-sops-age` secret referenced by the Kustomization.

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

The registry pull secret is the one thing SOPS does not cover, since it is a
cluster-level docker config rather than app config:

```sh
cp .env.secrets.example .env.secrets   # GHCR credentials only
./create-secrets.sh                    # creates the namespace and ghcr-secret
```

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

### 2. Instagram requires a login challenge

Instagram answers password logins for this account with `checkpoint_required`.
The challenge code goes to the account's email or phone, so it cannot be cleared
from the service.

Clear it once in a browser, then hand the session to the bot:

1. Log in to <https://www.instagram.com> as the account in `USERNAME` and
   complete whatever verification is asked for.
2. Open DevTools → **Application** → **Cookies** → `https://www.instagram.com`.
3. Copy the value of the **`sessionid`** cookie.
4. Put it in `.env` as `IG_SESSIONID=...` and restart.

`IG_SESSIONID` takes priority over password login, and the bot falls back to a
password login only when it is empty or rejected. Sessions last weeks; when one
expires the bot reports it in the chat and the four steps above are repeated.

## Notes

- SQLite (pure-Go `modernc.org/sqlite`) is used instead of Postgres: the state is
  a single dedupe table, and a one-binary deploy with no database server is worth
  more here than the shared convention.
- Stories expire after 24 hours — keep `POLL_INTERVAL` well below that.
- Instagram reports throttling as a **401 with `"Please wait a few minutes"`**,
  not only as a 429. The client classifies on that message rather than the status
  code, because reading it as a dead session leads to a password login and a
  challenge that cannot be cleared automatically.
- Throttling is applied per source address. A session that works from a home
  connection can be refused from a datacenter, which is what makes the egress
  network the thing to change when polling stalls for good.
- Polling too aggressively is what gets Instagram accounts flagged. 5 minutes is
  a reasonable floor.
