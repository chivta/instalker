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

```sh
cp .env.secrets.example .env.secrets   # fill it in
./create-secrets.sh                    # creates the namespace, ghcr-secret, app-secrets
kubectl apply -k k8s/
```

`create-secrets.sh` reads `.env.secrets` (gitignored) and applies two secrets:
`ghcr-secret` for pulling the image and `app-secrets` for the bot's configuration,
which the Deployment consumes wholesale via `envFrom`.

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
- Polling too aggressively is what gets Instagram accounts flagged. 5 minutes is
  a reasonable floor.
