# LiveForge Minimal License Backend (Go)

Small standalone admin backend to create users, generate license keys, and verify keys.

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

## Run locally

```bash
cd license-backend
go run .
```

Server defaults to `:8085`.

Optional env vars:

- `ADDR` (example: `:8085`)
- `DATA_FILE` (example: `/data/license-db.json`)

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
- Admin UI (`/`) and all other admin API routes require BasicAuth.

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
