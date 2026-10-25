# Running cronbeat in production

cronbeat is one binary with SQLite inside. A small VPS is plenty.

## Docker Compose + Caddy
`/srv/cronbeat/compose.yml`
```yaml
services:
  cronbeat:
    image: ghcr.io/ekiprop/cronbeat:0.1.0     # pin a version; roll back by changing it
    environment:
      CRONBEAT_BASE_URL: https://beat.example
      CRONBEAT_ALERT_URL: https://ntfy.sh/your-topic   # or Slack/Discord/Telegram/webhook URLs, comma-separated
    volumes: ["./data:/data"]
    ports: ["127.0.0.1:8090:8080"]
    restart: unless-stopped
```
Caddy:
```
beat.example {
    reverse_proxy 127.0.0.1:8090
}
```
The first start logs a generated API key (stored in the database); keep it in a password manager, or set `CRONBEAT_API_KEY`.

## Backups
```sh
sqlite3 /srv/cronbeat/data/cronbeat.db ".backup '/srv/backups/cronbeat-$(date -u +%Y%m%d).db'"
```
(The image is distroless, so run `sqlite3` on the host; the online backup is safe while cronbeat runs.) Copy backups off the server.

## Watching the watcher
cronbeat can't alert you if its own server is down, so point a free external uptime check at `https://beat.example/healthz`.

## Upgrading
Change the image tag, then `docker compose pull && docker compose up -d`. Schema changes are applied in place at startup.

## Hardening checklist
- Only Caddy listens publicly (cronbeat binds to 127.0.0.1 above)
- Rate-limit `/ping/*` at the proxy if the instance is shared
- `CRONBEAT_BASE_URL` must be the public HTTPS URL (it's used in ping URLs and alert links)
