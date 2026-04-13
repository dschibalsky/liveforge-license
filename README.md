# LiveForge Minimal License Backend (Go)

Small standalone admin backend: users, license keys, verify, **hosted prepaid wallet consume**, updater asset hosting.

## What it includes

- Go HTTP server (`main.go`)
- JSON file persistence (`DATA_FILE`, default `./data/license-db.json`)
- Admin web UI at `/` (BasicAuth behind Traefik in production)
- API endpoints:
  - `GET /api/users` — list users
  - `POST /api/users` — create user
  - `POST /api/users/delete` — delete user (body `{ "id" }`; fails if user still has keys)
  - `GET /api/keys` — list keys (optional `?user_id=`)
  - `POST /api/keys` — create key (optional `billing_mode`, `allowed_edition`, `model_tier`, `wallet_balance_usd` for hosted)
  - `POST /api/keys/verify` — public verify + activate device slot
  - `POST /api/keys/consume` — **public** wallet debit (idempotent by `trace_id`); protect with `CONSUME_SECRET` + header `x-liveforge-consume-secret`
  - `POST /api/keys/revoke` — set status `revoked`
  - `POST /api/keys/delete` — remove key record
  - `POST /api/keys/reset-activations` — clear device activations (does **not** clear consume dedupe list)
  - `POST /api/keys/patch` — partial update (`key` + optional `wallet_balance_usd`, `billing_mode`, `model_tier`, `max_devices`, `status`, `allowed_edition`)
  - `POST /api/updates/upload` — optional upload token
  - `GET /api/updates/files`
- Public updater static hosting under `/updates/`

### License fields (stored per key)

| Field | Meaning |
| --- | --- |
| `billing_mode` | `byok` (default) or `hosted` |
| `allowed_edition` | `full` or `cloud` (for client enforcement) |
| `wallet_balance_usd` | Prepaid balance when `hosted` |
| `model_tier` | Optional `t1` / `t2` / `t3` for hosted routing |
| `consumed_trace_ids` | Last N trace IDs used for idempotent `/api/keys/consume` |

Verify response `license` object includes `billing_mode`, `allowed_edition`, and for hosted `wallet_balance_usd` + `model_tier` when set — matches the LiveForge desktop `PublicLicenseMeta` shape.

### Next.js (GUI) env

Point the app at this service:

- `LIVEFORGE_LICENSE_BACKEND_VERIFY_URL` — e.g. `https://your-host/api/keys/verify`
- `LIVEFORGE_LICENSE_BACKEND_CONSUME_URL` — e.g. `https://your-host/api/keys/consume`
- `LIVEFORGE_LICENSE_BACKEND_CONSUME_SECRET` — same value as backend `CONSUME_SECRET` (server-side only in production)

## Run locally

```bash
cd backend
go run .
```

Server defaults to `:8085`.

Optional env vars:

- `ADDR` (example: `:8085`)
- `DATA_FILE` (example: `/data/license-db.json`)
- `UPDATES_DIR` (example: `/app/updates`)
- `UPDATES_UPLOAD_TOKEN` (recommended for update upload/list)
- `UPDATES_MAX_UPLOAD_MB` (default: `2048`)
- **`CONSUME_SECRET`** — when set, `POST /api/keys/consume` requires header `x-liveforge-consume-secret: <value>`

## Docker / Compose

```bash
cd backend
docker compose up -d --build
```

Traefik routes **`/api/keys/verify` and `/api/keys/consume`** without BasicAuth (app + Next server must reach them). Admin UI and other `/api/*` routes use BasicAuth — see compose labels.

## Hosting this backend vs building DMG / EXE

- **This service** on Ubuntu (or any Linux VM) is for **license API + optional updater static files**. That is orthogonal to desktop builds.
- **`.dmg`** normally needs **macOS** (signing / notarization) — typical pattern: **GitHub Actions `macos-latest`** or a Mac builder, not your Ubuntu license VM.
- **`.exe`** for Windows: **GitHub Actions `windows-latest`** or a Windows runner, or cross-compile where supported — again not required to live on the same host as this backend.
- You **can** still use the same repo’s CI to build Tauri artifacts and upload them **to** `/updates/` on this backend (see updater section below).

## Updater assets (private-repo test phase)

1. Upload `latest.json` and binaries to `/updates/` (or use `POST /api/updates/upload` with `x-updates-upload-token` when set).
2. Point Tauri `updater` endpoint at `https://<host>/updates/latest.json`.

## Security note

Minimal for trusted operators. For production hardening: rate limits on verify/consume, hash keys at rest, audit log, rotate `CONSUME_SECRET`, and never expose `CONSUME_SECRET` to the browser.

## Hash generation (BasicAuth)

```bash
python -c "import bcrypt; print(bcrypt.hashpw(b'MY_PASSWORD', bcrypt.gensalt()).decode())"
```

In `.env`, escape `$` as `$$` for Docker Compose.
