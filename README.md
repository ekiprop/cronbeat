# cronbeat

[![ci](https://github.com/ekiprop/cronbeat/actions/workflows/ci.yml/badge.svg)](https://github.com/ekiprop/cronbeat/actions/workflows/ci.yml)

A dead man's switch for cron jobs, backups, ETL pipelines, and anything else that runs on a schedule. It's one static binary with SQLite inside.

Your jobs ping cronbeat when they finish. If a ping is late, the job hangs, or it exits non-zero, you get an alert in Slack, Discord, Telegram, ntfy, or any webhook.

Cron fails silently. A backup script that stopped running three weeks ago looks exactly like one that's working. cronbeat catches that.

## Quick start

```sh
go build -o cronbeat .
CRONBEAT_ALERT_URL=https://hooks.slack.com/services/XXX ./cronbeat serve
# → logs a generated API key; open http://localhost:8080 and paste it
```

Or grab a prebuilt binary (linux/macOS, amd64/arm64) from [Releases](https://github.com/ekiprop/cronbeat/releases), or run the Docker image:

```sh
docker run -d -p 8080:8080 -v cronbeat:/data \
  -e CRONBEAT_BASE_URL=https://beat.example.com \
  -e CRONBEAT_ALERT_URL=https://ntfy.sh/my-alerts \
  ghcr.io/ekiprop/cronbeat:latest
```

## Wiring up a job

Create a check in the dashboard or through the API. You get back a ping URL. Then use any of these:

```sh
# 1. Simplest: ping on success
0 3 * * * backup.sh && curl -fsS -m 10 --retry 3 https://beat.example.com/ping/<id>

# 2. Report the exit code (non-zero → immediate alert)
0 3 * * * backup.sh; curl -fsS -m 10 --retry 3 https://beat.example.com/ping/<id>/$?

# 3. Wrapper: run time, hang detection, last 10 KB of output in the alert/dashboard
0 3 * * * cronbeat run --url https://beat.example.com/ping/<id> -- backup.sh
```

The wrapper passes the exit code through, forwards SIGINT, SIGTERM and SIGHUP to the job, and never fails your job when cronbeat itself is unreachable.

## How checks work

| Field    | Meaning |
|----------|---------|
| `period` | How often the job should ping, e.g. `5m`, `6h`, `1d`. |
| `schedule` | Or a cron expression, which overrides `period`: `0 3 * * *`, `*/15 * * * 1-5`, `@daily`. The next ping is expected at the next fire time after the last ping. |
| `tz` | IANA timezone for `schedule`, e.g. `Europe/Berlin` (default `UTC`). Daylight-saving changes are handled. Set it to whatever zone your crontab runs in. |
| `grace`  | Extra slack before alerting. It's also the **max run time** after a `/start` ping. A job that starts and then doesn't finish within `grace` is reported as hung. |

The states are `new` (no ping yet, never alerts), `up`, `running`, `late` (past the period but inside grace), `down`, and `paused`. Any ping un-pauses a check. Alerts fire on transitions only, once going down and once coming back up.

## Ping endpoints (no auth: the UUID is the secret)

| Endpoint | Effect |
|---|---|
| `GET/POST /ping/<id>` | success |
| `/ping/<id>/start` | job started (enables run-time tracking and hang detection) |
| `/ping/<id>/fail` | failure, alerts immediately |
| `/ping/<id>/<0-255>` | exit code; 0 is success, anything else is failure |

Send the job's output as the POST body (up to 10 KB) and it shows up on the check's page.

## API (`Authorization: Bearer <key>`)

```sh
curl -H "Authorization: Bearer $KEY" -d '{"name":"db-backup","period":"1d","grace":"1h"}' http://localhost:8080/api/checks
curl -H "Authorization: Bearer $KEY" -d '{"name":"etl","schedule":"30 2 * * 1-5","tz":"Africa/Nairobi","grace":"20m"}' http://localhost:8080/api/checks
curl -H "Authorization: Bearer $KEY" http://localhost:8080/api/checks
curl -H "Authorization: Bearer $KEY" http://localhost:8080/api/checks/<id>        # includes recent pings
curl -H "Authorization: Bearer $KEY" -X PATCH -d '{"paused":true}' http://localhost:8080/api/checks/<id>
curl -H "Authorization: Bearer $KEY" -X DELETE http://localhost:8080/api/checks/<id>
```

`GET /metrics` serves Prometheus metrics with the same bearer token: `cronbeat_check_up`, `cronbeat_check_last_ping_timestamp_seconds`, and `cronbeat_check_last_duration_seconds`. `GET /healthz` is the liveness probe.

## Alerts

`CRONBEAT_ALERT_URL` (comma-separated) sets the default targets, and each check can override it with `alert_url`. The payload format is picked from the URL:

- `hooks.slack.com` gets Slack `{"text"}`
- `discord.com/api/webhooks` gets Discord `{"content"}`
- `api.telegram.org/bot<token>/sendMessage?chat_id=<id>` gets Telegram
- anything containing `ntfy` gets a plain-text body with `Title`/`Priority` headers (push to your phone)
- anything else gets generic JSON `{event, reason, message, at, check{...}}`, which works for PagerDuty or Opsgenie through a relay, n8n, Zapier, or your own code

Failed deliveries are retried 4 times with backoff.

## Config

| Env | Flag | Default |
|---|---|---|
| `CRONBEAT_ADDR` | `--addr` | `:8080` |
| `CRONBEAT_DB` | `--db` | `cronbeat.db` |
| `CRONBEAT_BASE_URL` | `--base-url` | `http://localhost:8080` (used in ping URLs and alert links) |
| `CRONBEAT_ALERT_URL` | `--alert-url` | — |
| `CRONBEAT_API_KEY` | — | generated on first start and stored in the DB |

There's a hardened systemd unit in `cronbeat.service`. Put a TLS-terminating proxy in front, such as Caddy (`beat.example.com { reverse_proxy 127.0.0.1:8080 }`).

## Releasing

Push a tag like `v0.1.0`. CI then runs the tests, publishes binaries plus `checksums.txt` to a GitHub release, and pushes a multi-arch image to `ghcr.io/ekiprop/cronbeat`.

## License

[AGPL-3.0](LICENSE). You can self-host it freely. If you modify cronbeat and offer it to others as a network service, you must publish your changes under the same license.
