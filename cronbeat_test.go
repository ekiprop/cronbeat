package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) notify(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) kinds() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var k []string
	for _, e := range r.events {
		k = append(k, e.Kind)
	}
	return strings.Join(k, ",")
}

func newTestServer(t *testing.T) (*Server, *Store, *recorder, *time.Time) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rec := &recorder{}
	srv := NewServer(store, rec.notify, "secret", "http://cb.test")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	return srv, store, rec, &now
}

func do(t *testing.T, h http.Handler, method, path, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		req.Header.Set("Authorization", "Bearer secret")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestLifecycle(t *testing.T) {
	srv, store, rec, now := newTestServer(t)

	if w := do(t, srv, "GET", "/api/checks", "", false); w.Code != 401 {
		t.Fatalf("unauthenticated list: got %d", w.Code)
	}
	w := do(t, srv, "POST", "/api/checks", `{"name":"backup","period":"1h","grace":600}`, true)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var cv checkView
	json.Unmarshal(w.Body.Bytes(), &cv)
	if cv.State != "new" || cv.Period != "1h" || cv.Grace != "10m" || !strings.HasPrefix(cv.PingURL, "http://cb.test/ping/") {
		t.Fatalf("unexpected view: %+v", cv)
	}
	id := cv.ID

	// New checks never alert, even long after creation.
	if o, _ := store.MarkOverdue(now.Add(48 * time.Hour)); len(o) != 0 {
		t.Fatalf("new check went overdue")
	}

	if w := do(t, srv, "GET", "/ping/"+id, "", false); w.Code != 200 {
		t.Fatalf("ping: %d", w.Code)
	}
	if w := do(t, srv, "GET", "/ping/nope", "", false); w.Code != 404 {
		t.Fatalf("unknown ping: %d", w.Code)
	}

	c, _ := store.GetCheck(id)
	if s := c.State(now.Add(65 * time.Minute)); s != "late" {
		t.Fatalf("state inside grace = %s, want late", s)
	}
	if o, _ := store.MarkOverdue(now.Add(69 * time.Minute)); len(o) != 0 {
		t.Fatalf("down before grace expired")
	}
	o, _ := store.MarkOverdue(now.Add(71 * time.Minute))
	if len(o) != 1 || !strings.Contains(o[0].Reason, "no ping") {
		t.Fatalf("expected overdue, got %+v", o)
	}
	if o, _ := store.MarkOverdue(now.Add(72 * time.Minute)); len(o) != 0 {
		t.Fatalf("alerted twice for the same outage")
	}

	// A success ping recovers it.
	*now = now.Add(80 * time.Minute)
	do(t, srv, "POST", "/ping/"+id, "", false)
	if rec.kinds() != "up" {
		t.Fatalf("events = %q, want up", rec.kinds())
	}

	// Non-zero exit code alerts immediately, and stores the output.
	do(t, srv, "POST", "/ping/"+id+"/3", "disk full", false)
	if rec.kinds() != "up,down" {
		t.Fatalf("events = %q", rec.kinds())
	}
	pings, _ := store.Pings(id, 1)
	if pings[0].Kind != "fail" || *pings[0].ExitCode != 3 || pings[0].Body != "disk full" {
		t.Fatalf("bad ping row: %+v", pings[0])
	}

	// Pause, then a ping un-pauses.
	do(t, srv, "PATCH", "/api/checks/"+id, `{"paused":true}`, true)
	c, _ = store.GetCheck(id)
	if !c.Paused {
		t.Fatal("not paused")
	}
	do(t, srv, "GET", "/ping/"+id+"/0", "", false)
	c, _ = store.GetCheck(id)
	if c.Paused || c.Status != "up" {
		t.Fatalf("ping should unpause and recover: %+v", c)
	}

	if w := do(t, srv, "DELETE", "/api/checks/"+id, "", true); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := do(t, srv, "GET", "/api/checks/"+id, "", true); w.Code != 404 {
		t.Fatalf("get after delete: %d", w.Code)
	}
}

func TestHungJobAndDuration(t *testing.T) {
	srv, store, _, now := newTestServer(t)
	c, _ := store.CreateCheck(Check{Name: "etl", Period: time.Hour, Grace: 5 * time.Minute}, *now)

	do(t, srv, "GET", "/ping/"+c.ID+"/start", "", false)
	*now = now.Add(2 * time.Minute)
	do(t, srv, "GET", "/ping/"+c.ID, "", false)
	c, _ = store.GetCheck(c.ID)
	if c.LastDuration != 2*time.Minute || !c.LastStart.IsZero() {
		t.Fatalf("duration = %s, start = %v", c.LastDuration, c.LastStart)
	}

	// Started but never finished: down once grace passes, well before the period.
	do(t, srv, "GET", "/ping/"+c.ID+"/start", "", false)
	o, _ := store.MarkOverdue(now.Add(6 * time.Minute))
	if len(o) != 1 || !strings.Contains(o[0].Reason, "never finished") {
		t.Fatalf("expected hung-job alert, got %+v", o)
	}
}

func TestValidation(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	for _, body := range []string{
		`{"name":""}`,
		`{"name":"x","period":"5s"}`,
		`{"name":"x","period":"banana"}`,
		`{"name":"x","alert_url":"ftp://nope"}`,
		`{"name":"x","schedule":"not a cron"}`,
		`{"name":"x","schedule":"0 3 * * *","tz":"Mars/Olympus"}`,
		`{"name":"x","schedule":"0 0 30 2 *"}`,
		`not json`,
	} {
		if w := do(t, srv, "POST", "/api/checks", body, true); w.Code != 400 {
			t.Errorf("%s: got %d, want 400", body, w.Code)
		}
	}
}

func TestMetricsAndDashboard(t *testing.T) {
	srv, store, _, now := newTestServer(t)
	c, _ := store.CreateCheck(Check{Name: `we"ird`, Period: time.Hour, Grace: time.Minute}, *now)
	do(t, srv, "GET", "/ping/"+c.ID, "", false)

	w := do(t, srv, "GET", "/metrics", "", true)
	if !strings.Contains(w.Body.String(), `cronbeat_check_up{id="`+c.ID+`",name="we\"ird"} 1`) {
		t.Fatalf("metrics:\n%s", w.Body)
	}

	if w := do(t, srv, "GET", "/", "", false); w.Code != 303 {
		t.Fatalf("dashboard without session: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader("key=secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	lw := httptest.NewRecorder()
	srv.ServeHTTP(lw, req)
	cookies := lw.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	for _, path := range []string{"/", "/checks/" + c.ID} {
		req = httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookies[0])
		dw := httptest.NewRecorder()
		srv.ServeHTTP(dw, req)
		if dw.Code != 200 || !strings.Contains(dw.Body.String(), "we&#34;ird") {
			t.Fatalf("%s: %d\n%s", path, dw.Code, dw.Body)
		}
	}
}

func TestNotifierFormats(t *testing.T) {
	n := NewNotifier(nil, "http://cb.test")
	ev := Event{Kind: "down", Check: Check{ID: "abc", Name: "job"}, Reason: "late"}
	body, _, _ := n.format("https://hooks.slack.com/services/x", ev)
	if !strings.Contains(string(body), `"text":"🔴 job is DOWN: late\nhttp://cb.test/checks/abc"`) {
		t.Errorf("slack: %s", body)
	}
	body, _, _ = n.format("https://discord.com/api/webhooks/1/x", ev)
	if !strings.HasPrefix(string(body), `{"content":`) {
		t.Errorf("discord: %s", body)
	}
	_, ctype, h := n.format("https://ntfy.sh/topic", ev)
	if ctype != "text/plain; charset=utf-8" || h["Priority"] != "high" {
		t.Errorf("ntfy: %s %v", ctype, h)
	}

	got := make(chan string, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
	}))
	defer hook.Close()
	n.Send(Event{Kind: "up", Check: Check{ID: "abc", Name: "job", AlertURL: hook.URL}})
	select {
	case b := <-got:
		if !strings.Contains(b, `"event":"up"`) {
			t.Errorf("generic: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook never called")
	}
}

func TestParseDur(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"1d": 24 * time.Hour, "1d12h": 36 * time.Hour, "90s": 90 * time.Second, "15m": 15 * time.Minute,
	} {
		if got, err := parseDur(in); err != nil || got != want {
			t.Errorf("parseDur(%q) = %v, %v", in, got, err)
		}
		if got := fmtDur(want); got != in && in != "90s" {
			t.Errorf("fmtDur(%v) = %q, want %q", want, got, in)
		}
	}
	if fmtDur(90*time.Second) != "1m30s" {
		t.Error("fmtDur(90s)")
	}
}

// TestRunWrapper builds the binary and wraps a failing shell command.
func TestRunWrapper(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	srv, store, rec, _ := newTestServer(t)
	srv.now = time.Now
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c, _ := store.CreateCheck(Check{Name: "wrapped", Period: time.Hour, Grace: time.Minute}, time.Now())

	bin := filepath.Join(t.TempDir(), "cronbeat")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "run", "--url", ts.URL+"/ping/"+c.ID, "--", "sh", "-c", "echo hello; echo oops >&2; exit 7")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("wrapper should pass through exit 7, got %v", err)
	}
	pings, _ := store.Pings(c.ID, 2)
	if len(pings) != 2 || pings[1].Kind != "start" || pings[0].Kind != "fail" || *pings[0].ExitCode != 7 {
		t.Fatalf("pings: %+v", pings)
	}
	if !strings.Contains(pings[0].Body, "hello") || !strings.Contains(pings[0].Body, "oops") {
		t.Fatalf("output not captured: %q", pings[0].Body)
	}
	if rec.kinds() != "down" {
		t.Fatalf("events = %q", rec.kinds())
	}

	// Missing command → 127, still reported.
	cmd = exec.Command(bin, "run", "--url", ts.URL+"/ping/"+c.ID, "--", "/definitely/not/here")
	cmd.Stderr = io.Discard
	if ee, ok := cmd.Run().(*exec.ExitError); !ok || ee.ExitCode() != 127 {
		t.Fatalf("missing command should exit 127")
	}
}

func TestCronSchedule(t *testing.T) {
	srv, store, _, now := newTestServer(t) // now = 2026-01-01 00:00 UTC

	w := do(t, srv, "POST", "/api/checks", `{"name":"nightly","schedule":"0 3 * * *","tz":"Europe/Berlin","grace":"30m"}`, true)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var cv checkView
	json.Unmarshal(w.Body.Bytes(), &cv)
	if cv.Schedule != "0 3 * * *" || cv.TZ != "Europe/Berlin" {
		t.Fatalf("view: %+v", cv)
	}

	// Job runs at 03:00 Berlin (02:00 UTC) on Jan 1st.
	*now = time.Date(2026, 1, 1, 2, 0, 5, 0, time.UTC)
	do(t, srv, "GET", "/ping/"+cv.ID, "", false)
	c, _ := store.GetCheck(cv.ID)
	if want := time.Date(2026, 1, 2, 2, 30, 0, 0, time.UTC); !c.Deadline().Equal(want) {
		t.Fatalf("deadline = %v, want %v", c.Deadline().UTC(), want)
	}
	if s := c.State(time.Date(2026, 1, 2, 2, 10, 0, 0, time.UTC)); s != "late" {
		t.Fatalf("state after missed run = %s, want late", s)
	}
	if o, _ := store.MarkOverdue(time.Date(2026, 1, 2, 2, 29, 0, 0, time.UTC)); len(o) != 0 {
		t.Fatal("down inside grace")
	}
	o, _ := store.MarkOverdue(time.Date(2026, 1, 2, 2, 31, 0, 0, time.UTC))
	if len(o) != 1 || !strings.Contains(o[0].Reason, "0 3 * * * (Europe/Berlin)") {
		t.Fatalf("expected overdue, got %+v", o)
	}

	// Across the DST switch (Berlin goes UTC+2 on 2026-03-29), 03:00 local is 01:00 UTC.
	*now = time.Date(2026, 3, 29, 1, 0, 5, 0, time.UTC)
	do(t, srv, "GET", "/ping/"+cv.ID, "", false)
	c, _ = store.GetCheck(cv.ID)
	if want := time.Date(2026, 3, 30, 1, 30, 0, 0, time.UTC); !c.Deadline().Equal(want) {
		t.Fatalf("DST deadline = %v, want %v", c.Deadline().UTC(), want)
	}

	// Changing the schedule moves the stored deadline; clearing it falls back to period.
	do(t, srv, "PATCH", "/api/checks/"+cv.ID, `{"schedule":"@hourly"}`, true)
	if o, _ := store.MarkOverdue(time.Date(2026, 3, 29, 2, 31, 0, 0, time.UTC)); len(o) != 1 {
		t.Fatalf("@hourly should be overdue after 2h, got %+v", o)
	}
	w = do(t, srv, "PATCH", "/api/checks/"+cv.ID, `{"schedule":"","period":"6h"}`, true)
	var cleared checkView
	json.Unmarshal(w.Body.Bytes(), &cleared)
	if cleared.Schedule != "" || cleared.Period != "6h" {
		t.Fatalf("clear schedule: %+v", cleared)
	}
}

func TestMigrateOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// Schema as shipped before cron support.
	_, err = db.Exec(`CREATE TABLE checks (id TEXT PRIMARY KEY, name TEXT NOT NULL, period_s INTEGER NOT NULL,
		grace_s INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'new', paused INTEGER NOT NULL DEFAULT 0,
		alert_url TEXT NOT NULL DEFAULT '', last_ping_ms INTEGER NOT NULL DEFAULT 0, last_start_ms INTEGER NOT NULL DEFAULT 0,
		last_duration_ms INTEGER NOT NULL DEFAULT 0, created_ms INTEGER NOT NULL);
		INSERT INTO checks(id, name, period_s, grace_s, status, last_ping_ms, created_ms) VALUES('a', 'old', 3600, 60, 'up', 1000000, 1)`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	o, err := store.MarkOverdue(time.UnixMilli(1000000 + 3661*1000))
	if err != nil || len(o) != 1 {
		t.Fatalf("migrated check should go overdue: %v %+v", err, o)
	}
}
