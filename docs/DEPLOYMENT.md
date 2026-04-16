# Deployment

## Version Rule

**Every deploy must bump the patch version.** Never deploy the same version twice. The version lives in `.github/workflows/ci.yml` under `env.VERSION`. Bump it before pushing to main.

---

FleetCom runs as a Docker container. Two production instances exist:

| Instance | Domain | Host | Secrets |
|----------|--------|------|---------|
| Personal | fleet.barta.cm | csb1 (NixOS, Hetzner) | agenix (`/run/agenix/csb1-fleetcom-env`) |
| BYTEPOETS | fleet.bytepoets.com | 5.75.130.206 (Ubuntu, Hetzner) | `/home/service-user/.fleetcom.env` |

## Docker Image

Built and pushed to GHCR by CI on every push to main:

```
ghcr.io/markus-barta/fleetcom:latest
ghcr.io/markus-barta/fleetcom:v{VERSION}
ghcr.io/markus-barta/fleetcom:sha-{COMMIT}
```

The image is public. Source repo is private.

## Environment Variables

### Required (first run)

| Variable | Purpose |
|----------|---------|
| `FLEETCOM_ADMIN_EMAIL` | Seed admin user email (only when users table is empty) |
| `FLEETCOM_ADMIN_PASSWORD` | Seed admin user password (only when users table is empty) |

### Optional

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `8090` | HTTP listen port |
| `DB_PATH` | `fleetcom.db` | SQLite database path |
| `FLEETCOM_INSTANCE_LABEL` | (none) | Header label (also settable via admin UI) |
| `APP_BASE_URL` | `http://localhost:8090` | Base URL for password reset links |
| `SMTP_HOST` | (none) | SMTP server for password reset emails |
| `SMTP_PORT` | `587` | SMTP port |
| `SMTP_USER` | (none) | SMTP username |
| `SMTP_PASS` | (none) | SMTP password |
| `SMTP_FROM` | `noreply@fleetcom.local` | From address for emails |

If `SMTP_HOST` is not set, password reset links are logged to stdout (dev mode).

## Initial Setup (New Instance)

### 1. Prepare environment file

Generate a strong password and write the env file:

```bash
PW=$(openssl rand -base64 24)
cat > .fleetcom.env <<EOF
FLEETCOM_ADMIN_EMAIL=you@example.com
FLEETCOM_ADMIN_PASSWORD=$PW
EOF
chmod 600 .fleetcom.env
echo "Save this password: $PW"
```

### 2. Start the container

```yaml
# docker-compose.yml
services:
  fleetcom:
    image: ghcr.io/markus-barta/fleetcom:latest
    ports:
      - "127.0.0.1:8090:8090"
    volumes:
      - ./data:/app/data
    env_file:
      - .fleetcom.env
    environment:
      - PORT=8090
      - DB_PATH=/app/data/fleetcom.db
    restart: unless-stopped
```

```bash
docker compose up -d
docker logs fleetcom  # should show "seeded admin user: you@example.com"
```

### 3. Log in and set up TOTP

Navigate to your domain. Log in with the seeded email + password. You'll be forced through TOTP setup — scan the QR code with your authenticator app.

### 4. Create users and assign hosts

Settings > Users > create additional users. Then click "Hosts" per user to grant host access.

## Personal Instance (fleet.barta.cm)

### NixOS / agenix secrets

Secrets are managed via agenix in `~/Code/nixcfg`:

```bash
# Edit the secret
cd ~/Code/nixcfg
agenix -e secrets/csb1-fleetcom-env.age

# Commit and push
git add secrets/csb1-fleetcom-env.age && git commit -m "update fleetcom env" && git push

# Deploy on csb1
ssh -p 2222 mba@cs1.barta.cm 'cd ~/Code/nixcfg && git pull && sudo nixos-rebuild switch --flake .#csb1'

# Recreate container to pick up new env
ssh -p 2222 mba@cs1.barta.cm 'cd ~/docker/fleetcom && docker compose up -d --force-recreate fleetcom'
```

### CI auto-deploy

Pushing to main triggers CI → build → auto-deploy to csb1 via SSH + `/etc/fleetcom-deploy.sh`.

## BYTEPOETS Instance (fleet.bytepoets.com)

### Infrastructure

Managed via `BYTEPOETS/infracore` repo:
- `docker-compose.yml` — all services (bp-pm, fleetcom, grafana, etc.)
- `nginx/fleet.bytepoets.com.conf` — reverse proxy with SSE support
- `scripts/fleetcom-init-admin.sh` — admin credential generator

### Secrets

Env file on server: `/home/service-user/.fleetcom.env` (mode 600).

To rotate or update:
```bash
ssh -i ~/.ssh/BP_OPS_Server_SSH_Key service-user@5.75.130.206
nano ~/.fleetcom.env
cd ~/docker && docker compose up -d --force-recreate fleetcom
```

### Deploy

Manual trigger via GitHub Actions:

```bash
gh workflow run deploy-bytepoets.yml -f image_tag=latest
```

Or via the GitHub UI: Actions > "Deploy to BYTEPOETS" > Run workflow.

### Nginx

SSE-aware reverse proxy at `/etc/nginx/sites-available/fleet.bytepoets.com`:
- `/api/events` has buffering off + 24h read timeout
- TLS via Let's Encrypt (certbot auto-renewal)

## Bosun Agent Deployment

Each monitored host runs a bosun agent that reports to FleetCom.

### 1. Add host in FleetCom

Settings > Hosts > enter hostname > "Add Host". Copy the bearer token (shown only once).

### 2. Deploy the agent

```yaml
# docker-compose.yml on the target host
services:
  fleetcom-bosun:
    image: ghcr.io/markus-barta/fleetcom-bosun:latest
    container_name: fleetcom-bosun
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
      - /etc/os-release:/host/etc/os-release:ro
    environment:
      - FLEETCOM_URL=https://fleet.barta.cm
      - FLEETCOM_TOKEN=<your-token>
      - FLEETCOM_HOSTNAME=<hostname>
```

### 3. Grant user access

Settings > Users > click "Hosts" > grant the new host to relevant users.

## Backups

```bash
# Personal
ssh -p 2222 mba@cs1.barta.cm 'cp /home/mba/docker/fleetcom/data/fleetcom.db ~/backups/'

# BYTEPOETS
ssh -i ~/.ssh/BP_OPS_Server_SSH_Key service-user@5.75.130.206 \
  'cp ~/docker/mounts/fleetcom/data/fleetcom.db ~/backups/'
```
