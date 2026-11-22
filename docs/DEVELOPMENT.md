# Developing cronbeat

## Prerequisites
Go 1.26 (from `go.mod`). Nothing else: SQLite is compiled in (`modernc.org/sqlite`, pure Go).

## Build, run, test
```sh
go build -o cronbeat .
CRONBEAT_ALERT_URL=https://ntfy.sh/<your-test-topic> ./cronbeat serve     # logs a generated API key; http://localhost:8080
gofmt -l . && go vet ./... && go test -count=1 ./...
```
A quick end-to-end check by hand:
```sh
KEY=<the logged key>
ID=$(curl -s -H "Authorization: Bearer $KEY" -d '{"name":"demo","period":"1m","grace":"30s"}' localhost:8080/api/checks | sed -n 's/.*"id": "\([^"]*\)".*/\1/p')
curl localhost:8080/ping/$ID                          # up
./cronbeat run --url http://localhost:8080/ping/$ID -- sh -c 'echo hello; exit 3'   # down, alert, output on the check page
```

## Tests (`cronbeat_test.go`)
| Test | Covers |
|---|---|
| `TestLifecycle` | new → up → late → down → up, alerts on transitions only, pause and un-pause |
| `TestHungJobAndDuration` | `/start` without a finish within grace is reported hung; run time is measured |
| `TestValidation` | bad durations, cron expressions, time zones, missing names |
| `TestMetricsAndDashboard` | Prometheus output (with label escaping), the dashboard login and its session cookie |
| `TestNotifierFormats` | the payload for Slack, Discord, Telegram, ntfy and generic webhooks |
| `TestParseDur` | `5m`, `6h`, `1d`, seconds as numbers |
| `TestRunWrapper` | the wrapper builds and runs a job: exit codes passed through (7 stays 7, a missing command is 127), stdout and stderr captured |
| `TestCronSchedule` | next fire times with time zones and daylight saving |
| `TestMigrateOldSchema` | an old database is upgraded in place |

Tests use a controllable clock and a recording notifier (`newTestServer`), so time-based rules run instantly.

CI (`.github/workflows/ci.yml`) on every pull request and push to `main`: gofmt, `go vet`, tests, cross-compiling the four
release targets, and a multi-arch Docker build (not pushed).

## Releasing
Push a tag like `v0.2.0`. `.github/workflows/release.yml` runs the tests, builds static binaries (linux and macOS,
amd64 and arm64) with `checksums.txt` into a GitHub release, and pushes a multi-arch image to
`ghcr.io/ekiprop/cronbeat` (`:0.2.0`, `:0.2` and, for releases without a `-` suffix, `:latest`). Write the release notes from the merged pull requests.

## How to add…
- **An alert format:** a case in `notify.go` matched on the URL, a payload builder, a test in `TestNotifierFormats`, and a
  line in the README's Alerts list.
- **A check field:** a column in the schema plus a migration step in `store.go` (`ALTER TABLE … ADD COLUMN` when
  missing), the field in `checkView` and the update request in `server.go`, validation, the dashboard (`ui.go`), and
  `TestMigrateOldSchema`.
- **A ping action:** `handlePing` in `server.go`, the state rules, the README's ping table and `docs/API.md`.

## Conventions
- **One binary, no external services.** A new dependency needs a strong reason (today: SQLite and the cron parser).
- **State changes go through one guarded `UPDATE`**, so a ping can't be lost between reading and writing.
- **Alerts on transitions only.**
- Docs change in the same pull request as the behaviour they describe (README for users, `docs/` for operators and
  contributors).
