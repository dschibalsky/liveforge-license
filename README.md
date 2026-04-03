# LiveForge Minimal License Backend (Go)

Small standalone admin backend to create users, generate license keys, verify keys, and (for test phase) host Tauri updater assets.

## What it includes

- Go HTTP server (`main.go`)
- JSON file persistence (`DATA_FILE`, default `./data/license-db.json`)
- Small admin web UI at `/`
- API endpoints:
  - `GET /api/users`
  - `POST /api/users`
  - `GET /api/keys`
  - `POST /api/keys`
  - `POST /api/keys/verify`
- Public updater static hosting under `/updates/` (e.g. `/updates/latest.json`)

## Run locally

```bash
cd license-backend
go run .
```

Server defaults to `:8085`.

Optional env vars:

- `ADDR` (example: `:8085`)
- `DATA_FILE` (example: `/data/license-db.json`)
- `UPDATES_DIR` (example: `/app/updates`)

## Docker / Compose

```bash
cd license-backend
docker compose up -d --build
```

Included files:

- `Dockerfile`
- `docker-compose.yml` (Traefik labels for `liveforge.hideandpass.com`)
- `.env.example`

The compose setup routes through the `proxy` network with split access:

- `POST /api/keys/verify` is public (no BasicAuth) for app-side license checks.
- `/updates/*` is public (no BasicAuth) for Tauri updater manifest + binaries during test phase.
- Admin UI (`/`) and all other admin API routes require BasicAuth.

## Updater assets for private repo test phase

When your source repo is private, desktop clients cannot pull updater artifacts directly from private GitHub Releases without auth.  
For test phase, host updater artifacts in this service:

1. Put updater files into `license-backend/updates/`:
   - `latest.json`
   - platform files referenced by `latest.json` (e.g. `.exe`, `.dmg`, signatures)
2. Deploy/restart compose.
3. Verify URLs are reachable publicly (without BasicAuth), e.g.:
   - `https://liveforge.hideandpass.com/updates/latest.json`
4. Point Tauri updater endpoint to that URL.

Setup:

1. Copy `.env.example` to `.env`
2. Set `BASIC_AUTH_USERS` in htpasswd format (`user:hash`) and replace every `$` with `$$`
3. Run `docker compose up -d --build`

Hash generation (Python/bcrypt example):

```bash
python -c "import bcrypt; print(bcrypt.hashpw(b'MY_PASSWORD', bcrypt.gensalt()).decode())"
```

## Security note

This is intentionally minimal for trusted testers. For public production, add:

- server-side auth (admin sessions/API tokens),
- key hashing at rest,
- audit logs and revocation endpoints,
- rate limiting and abuse protection.
