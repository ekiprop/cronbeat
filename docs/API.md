# cronbeat API

Two kinds of endpoint: **pings** (no authentication: the check's UUID is the secret) and the **JSON API** (a bearer
token). Replace `https://beat.example.com` with your `CRONBEAT_BASE_URL`.

## Pings
| Endpoint | Records | State after |
|---|---|---|
| `GET` or `POST /ping/<id>` | a success | `up` (an alert if it was `down`) |
| `/ping/<id>/start` | the job started | `running`: run time is measured, and a job that doesn't finish within `grace` is reported as hung |
| `/ping/<id>/fail` | a failure | `down`, alert at once |
| `/ping/<id>/<0-255>` | an exit code: 0 = success, anything else = failure | `up` or `down` |

- **Output:** send the job's output as the `POST` body; the last 10 KB are kept and shown on the check's page and in alerts.
- **Replies:** `200 OK`; `404 no such check` (unknown id); `404 unknown ping action` (an action that isn't `start`, `fail`
  or 0–255); `500 internal error`.
- **Any ping un-pauses a check.** Each check keeps its last 100 pings.

```sh
backup.sh && curl -fsS -m 10 --retry 3 https://beat.example.com/ping/<id>          # success only
backup.sh; curl -fsS -m 10 --retry 3 https://beat.example.com/ping/<id>/$?          # the exit code
cronbeat run --url https://beat.example.com/ping/<id> -- backup.sh                  # start, exit code, output, run time
```

## JSON API
Every request needs `Authorization: Bearer <key>`. The key is generated on first start (and logged once) or set with
`CRONBEAT_API_KEY`. Without it: `401 {"error": "missing or invalid bearer token"}`.

### The check object
```json
{
  "id": "6f1c…",                     "name": "db-backup",
  "state": "up",                     "period": "1d",  "schedule": "30 2 * * 1-5",  "tz": "Africa/Nairobi",  "grace": "1h",
  "alert_url": "https://ntfy.sh/…",  "last_ping": "2026-11-22T02:31:05Z",  "next_due": "2026-11-23T02:30:00Z",
  "last_duration_ms": 41230,         "ping_url": "https://beat.example.com/ping/6f1c…",
  "created_at": "2026-11-01T09:00:00Z",
  "pings": [{ "kind": "success", "exit_code": 0, "at": "…", "remote": "203.0.113.7", "body": "…" }]
}
```
- `state`: `new` (no ping yet, never alerts), `up`, `running`, `late` (past due, inside grace), `down`, `paused`.
- `period` / `grace`: durations like `5m`, `6h`, `1d` (or seconds as a number). `schedule`: a cron expression
  (`0 3 * * *`, `*/15 * * * 1-5`, `@daily`), which overrides `period`; `tz` is its IANA time zone (default `UTC`).
- `alert_url`: this check's targets (comma-separated), overriding `CRONBEAT_ALERT_URL`.
- `pings` appear only in `GET /api/checks/<id>` (the most recent first).

### Endpoints
| Request | Body | Reply |
|---|---|---|
| `GET /api/checks` | | `200` an array of checks |
| `POST /api/checks` | `{"name", "period" or "schedule", "tz"?, "grace", "alert_url"?}` | `201` the new check (with its `ping_url`) |
| `GET /api/checks/<id>` | | `200` the check with recent pings |
| `PATCH /api/checks/<id>` | any of `name`, `period`, `schedule`, `tz`, `grace`, `alert_url`, `paused` | `200` the updated check |
| `DELETE /api/checks/<id>` | | `204` (its pings go too) |
| `GET /metrics` | | Prometheus text (below) |
| `GET /healthz` | (no token) | `200 ok` |

Errors are `{"error": "<message>"}`: `400` for invalid input (a bad duration, cron expression or time zone, a missing
name), `404 not found`, `500 internal error`.

```sh
curl -H "Authorization: Bearer $KEY" -d '{"name":"db-backup","period":"1d","grace":"1h"}' https://beat.example.com/api/checks
curl -H "Authorization: Bearer $KEY" -X PATCH -d '{"paused":true}' https://beat.example.com/api/checks/<id>
```

### Metrics (`GET /metrics`, bearer token)
| Metric | Labels | Meaning |
|---|---|---|
| `cronbeat_check_up` | `id`, `name` | 1 up, 0 down, −1 new or paused |
| `cronbeat_check_last_ping_timestamp_seconds` | `id`, `name` | the last success or fail ping's Unix time |
| `cronbeat_check_last_duration_seconds` | `id`, `name` | the last measured run time (`/start` → finish) |

## Alerts
Sent on **transitions only**: once going down (missed deadline, failure, hung job) and once coming back up. The format is
picked from the URL:

| URL contains | Sent as |
|---|---|
| `hooks.slack.com` | Slack `{"text"}` |
| `discord.com/api/webhooks` | Discord `{"content"}` |
| `api.telegram.org/bot<token>/sendMessage?chat_id=<id>` | Telegram message |
| `ntfy` | plain text with `Title` and `Priority` headers |
| anything else | `{"event", "reason", "message", "at", "check": {…}}` (for PagerDuty or Opsgenie via a relay, n8n, Zapier, your code) |

Failed deliveries are retried 4 times with back-off; each attempt times out after 10 s.
