# Running cronbeat in production

cronbeat is one binary with SQLite inside. A small VPS is plenty.

## Option 1: Docker Compose + Caddy
`/srv/cronbeat/compose.yml`:
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
Caddy (TLS is automatic):
```
beat.example {
    reverse_proxy 127.0.0.1:8090
}
```

## Option 2: systemd
Install the binary from a release (check it against `checksums.txt`) to `/usr/local/bin/cronbeat`, copy
`cronbeat.service` to `/etc/systemd/system/`, set `CRONBEAT_BASE_URL` (and `CRONBEAT_ALERT_URL`) in it, then:
```sh
systemctl daemon-reload && systemctl enable --now cronbeat
journalctl -u cronbeat -n 50          # the first start logs the generated API key
```
The unit runs as a dynamic user with its database in `/var/lib/cronbeat`, `ProtectSystem=strict`, `ProtectHome`,
`PrivateTmp` and `NoNewPrivileges`, listening on `127.0.0.1:8080` behind your proxy.

## Configuration
| Env | Flag | Default | Notes |
|---|---|---|---|
| `CRONBEAT_ADDR` | `--addr` | `:8080` | bind to `127.0.0.1` behind a proxy |
| `CRONBEAT_DB` | `--db` | `cronbeat.db` | `/data/cronbeat.db` in the image |
| `CRONBEAT_BASE_URL` | `--base-url` | `http://localhost:8080` | **must be the public HTTPS URL**: it's in ping URLs and alert links |
| `CRONBEAT_ALERT_URL` | `--alert-url` | none | default targets, comma-separated; each check can override |
| `CRONBEAT_API_KEY` | | generated on first start, stored in the database | set it to manage the key yourself; changing it signs every dashboard out |
| | `--check-interval` | `15s` | how often overdue checks are looked for |

**The API key is a secret:** keep it in a password manager. Ping URLs are secrets too (anyone with one can report on that
check).

## Watching the watcher
cronbeat can't alert you if its own server is down. Point a free external uptime check at `https://beat.example/healthz`
(answers `ok`), and optionally scrape `/metrics` (bearer key) from Prometheus.

## Backups and restore
```sh
sqlite3 /srv/cronbeat/data/cronbeat.db ".backup '/srv/backups/cronbeat-$(date -u +%Y%m%d).db'"
```
The image is distroless, so run `sqlite3` on the host; the online backup is safe while cronbeat runs. Copy backups off the
server (or stream them with Litestream).

**Restore:** stop cronbeat, replace `cronbeat.db` with the backup (remove any `cronbeat.db-wal` and `-shm` beside it),
start cronbeat. Checks resume; any that missed their deadline while it was down alert on the first overdue pass.

## Upgrading and rolling back
Change the image tag (or replace the binary), then `docker compose pull && docker compose up -d` (or `systemctl restart
cronbeat`). Schema changes are applied in place at start-up and only add columns, so rolling back to the previous
version works with the same database.

## Troubleshooting
| Symptom | Cause | Fix |
|---|---|---|
| Ping URLs in the dashboard say `localhost` | `CRONBEAT_BASE_URL` not set | set it to the public URL, restart |
| No alerts arrive | wrong alert URL, or the target rejects the payload | the log shows each failed delivery and its retries; test the URL with curl |
| A check never alerts | it's `new` (no ping yet) or `paused` | send one ping; un-pause |
| Every check went down at once | cronbeat or its network was down longer than the checks' grace | check `/healthz` monitoring; alerts recover with the next pings |
| Jobs report "hung" | `/start` was sent but no finish within `grace` | raise `grace` to the job's longest normal run time |
| `401` from the API | missing or wrong `Authorization: Bearer <key>` | the key is in the first start's log, or `CRONBEAT_API_KEY` |

## Hardening checklist
- Only the proxy listens publicly; cronbeat binds to `127.0.0.1`.
- Rate-limit `/ping/*` at the proxy if the instance is shared with others.
- `CRONBEAT_BASE_URL` is the public HTTPS URL.
- Backups are copied off the server, and a restore has been tried once.
