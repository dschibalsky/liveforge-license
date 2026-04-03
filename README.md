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
cd license-backend-go
go run .
```

Server defaults to `:8085`.

Optional env vars:

- `ADDR` (example: `:8085`)
- `DATA_FILE` (example: `/data/license-db.json`)

## Docker example (service only)

```yaml
license_backend:
  image: golang:1.22-alpine
  container_name: liveforge-license-backend
  working_dir: /app
  command: sh -c "go run ."
  environment:
    ADDR: ":8085"
    DATA_FILE: "/data/license-db.json"
  volumes:
    - ./license-backend-go:/app
    - ./license-data:/data
  restart: unless-stopped
  networks:
    - internal
```

Then route it via your reverse proxy to `liveforge.hideandpass.com` and keep BasicAuth at proxy level.

## Security note

This is intentionally minimal for trusted testers. For public production, add:

- server-side auth (admin sessions/API tokens),
- key hashing at rest,
- audit logs and revocation endpoints,
- rate limiting and abuse protection.
