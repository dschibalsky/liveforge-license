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
  - `POST /api/updates/upload` (admin/upload token protected)
  - `GET /api/updates/files` (admin/upload token protected)
- Public updater static hosting under `/updates/` (e.g. `/updates/latest.json`)

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
- `UPDATES_UPLOAD_TOKEN` (optional but recommended for update upload/list APIs)
- `UPDATES_MAX_UPLOAD_MB` (default: `2048`)

## Docker / Compose

```bash
cd backend
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

1. Put updater files into `backend/updates/`:
   - `latest.json`
   - platform files referenced by `latest.json` (e.g. `.exe`, `.dmg`, signatures)
   - alternatively upload via API:
     - `POST /api/updates/upload` with multipart field `file=@...`
     - optional header `x-updates-upload-token: <UPDATES_UPLOAD_TOKEN>`
     - example:
       - `curl -u "<admin-user>:<admin-password>" -H "x-updates-upload-token: <token>" -F "file=@latest.json" https://liveforge.hideandpass.com/api/updates/upload`
2. (optional) verify current uploaded files:
   - `GET /api/updates/files`
   - include `x-updates-upload-token` when configured
3. Deploy/restart compose.
4. Verify URLs are reachable publicly (without BasicAuth), e.g.:
   - `https://liveforge.hideandpass.com/updates/latest.json`
5. Point Tauri updater endpoint to that URL.

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
