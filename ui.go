package main

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"time"
)

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	return fmtDur(d.Truncate(time.Minute)) + " ago"
}

func until(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Until(t)
	if d <= 0 {
		return "overdue"
	}
	return "in " + fmtDur(d.Truncate(time.Second))
}

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"ago": ago, "until": until, "dur": fmtDur,
	"ts": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") },
}).Parse(templates))

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

type row struct {
	Check
	State   string
	PingURL string
}

func (s *Server) uiLoginForm(w http.ResponseWriter, r *http.Request) {
	render(w, "login", map[string]any{"Login": true})
}

func (s *Server) uiDashboard(w http.ResponseWriter, r *http.Request) {
	checks, err := s.store.ListChecks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := s.now()
	counts := map[string]int{}
	rows := make([]row, 0, len(checks))
	for _, c := range checks {
		st := c.State(now)
		counts[st]++
		rows = append(rows, row{Check: c, State: st, PingURL: s.pingURL(c.ID)})
	}
	render(w, "dashboard", map[string]any{"Rows": rows, "Counts": counts, "Error": r.URL.Query().Get("err")})
}

func (s *Server) uiCheck(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCheck(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pings, err := s.store.Pings(c.ID, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "check", map[string]any{
		"C": c, "State": c.State(s.now()), "PingURL": s.pingURL(c.ID), "Pings": pings,
		"BaseURL": s.baseURL, "Error": r.URL.Query().Get("err"),
	})
}

func (s *Server) uiCreate(w http.ResponseWriter, r *http.Request) {
	name, period, grace, alert := r.FormValue("name"), r.FormValue("period"), r.FormValue("grace"), r.FormValue("alert_url")
	schedule, tz := r.FormValue("schedule"), r.FormValue("tz")
	c := Check{Period: 24 * time.Hour, Grace: time.Hour}
	in := checkInput{Name: &name, Schedule: &schedule, TZ: &tz, AlertURL: &alert}
	if period != "" {
		in.Period = period
	}
	if grace != "" {
		in.Grace = grace
	}
	if err := in.apply(&c); err != nil {
		http.Redirect(w, r, "/?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	c, err := s.store.CreateCheck(c, s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/checks/"+c.ID, http.StatusSeeOther)
}

func (s *Server) uiAction(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCheck(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back := "/checks/" + c.ID
	switch r.PathValue("op") {
	case "pause":
		c.Paused = true
		err = s.store.UpdateCheck(c)
	case "resume":
		c.Paused = false
		err = s.store.UpdateCheck(c)
	case "edit":
		name, period, grace, alert := r.FormValue("name"), r.FormValue("period"), r.FormValue("grace"), r.FormValue("alert_url")
		schedule, tz := r.FormValue("schedule"), r.FormValue("tz")
		in := checkInput{Name: &name, Period: period, Schedule: &schedule, TZ: &tz, Grace: grace, AlertURL: &alert}
		if schedule != "" && period == "" {
			in.Period = nil
		}
		if err := in.apply(&c); err != nil {
			http.Redirect(w, r, back+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		err = s.store.UpdateCheck(c)
	case "delete":
		err = s.store.DeleteCheck(c.ID)
		back = "/"
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

const templates = `
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>cronbeat</title>
<style>
:root{--bg:#f7f7f5;--card:#fff;--fg:#1d1d1b;--muted:#6b6b66;--line:#e4e4df;--up:#1f9d55;--down:#d64545;--late:#d98e04;--idle:#9a9a94;--accent:#2b59c3}
@media (prefers-color-scheme:dark){:root{--bg:#121211;--card:#1b1b1a;--fg:#ececea;--muted:#9a9a94;--line:#2c2c2a;--accent:#7aa2ff}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1000px;margin:0 auto;padding:24px 16px}
header{display:flex;align-items:center;justify-content:space-between;margin-bottom:20px}
h1{font-size:18px;margin:0;letter-spacing:-.01em}h1 a{color:inherit;text-decoration:none}h2{font-size:15px;margin:28px 0 10px}
a{color:var(--accent)}code,pre{font:12.5px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
pre{background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:10px;overflow-x:auto;white-space:pre-wrap;word-break:break-all;margin:6px 0}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;overflow-x:auto}
table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--line);vertical-align:top}
th{font-weight:500;color:var(--muted);font-size:12px}tr:last-child td{border-bottom:0}
.pill{display:inline-flex;align-items:center;gap:6px;font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:.03em}
.pill::before{content:"";width:8px;height:8px;border-radius:50%;background:var(--idle)}
.up::before,.running::before{background:var(--up)}.down::before{background:var(--down)}.late::before{background:var(--late)}
.muted{color:var(--muted)}.err{color:var(--down);margin:0 0 12px}
.stats{display:flex;gap:18px;color:var(--muted)}.stats b{color:var(--fg)}
form.inline{display:inline}button{font:inherit;padding:6px 12px;border-radius:6px;border:1px solid var(--line);background:var(--card);color:var(--fg);cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}button.danger{color:var(--down)}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:10px;padding:14px;align-items:end}
label{display:block;font-size:12px;color:var(--muted)}input{width:100%;font:inherit;padding:6px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg);margin-top:3px}
.kv{display:grid;grid-template-columns:140px 1fr;gap:6px 12px;padding:14px}
</style></head><body><main>
<header><h1><a href="/">⏱ cronbeat</a></h1>{{if not .Login}}<form class="inline" method="post" action="/logout"><button>Log out</button></form>{{end}}</header>
{{end}}

{{define "foot"}}<datalist id="tzs"><option>UTC</option><option>America/New_York</option><option>America/Chicago</option><option>America/Los_Angeles</option><option>Europe/London</option><option>Europe/Berlin</option><option>Africa/Nairobi</option><option>Asia/Kolkata</option><option>Asia/Singapore</option><option>Asia/Tokyo</option><option>Australia/Sydney</option></datalist></main></body></html>{{end}}

{{define "login"}}{{template "head" .}}
<div class="card" style="max-width:380px;margin:60px auto;padding:20px">
<form method="post" action="/login">
{{with .Error}}<p class="err">{{.}}</p>{{end}}
<label>API key<input type="password" name="key" autofocus autocomplete="current-password"></label>
<p class="muted">Printed in the server log on first start, or set via <code>CRONBEAT_API_KEY</code>.</p>
<button class="primary">Sign in</button></form></div>
{{template "foot"}}{{end}}

{{define "dashboard"}}{{template "head" .}}
<meta http-equiv="refresh" content="30">
{{with .Error}}<p class="err">{{.}}</p>{{end}}
<div class="stats" style="margin-bottom:12px">
<span><b>{{index .Counts "up"}}</b> up</span><span><b>{{index .Counts "running"}}</b> running</span><span><b>{{index .Counts "late"}}</b> late</span>
<span><b>{{index .Counts "down"}}</b> down</span><span><b>{{index .Counts "new"}}</b> new</span><span><b>{{index .Counts "paused"}}</b> paused</span></div>
<div class="card"><table>
<tr><th>Status</th><th>Name</th><th>Schedule</th><th>Last ping</th><th>Next due</th><th>Last run</th></tr>
{{range .Rows}}<tr>
<td><span class="pill {{.State}}">{{.State}}</span></td>
<td><a href="/checks/{{.ID}}">{{.Name}}</a></td>
<td class="muted">{{if .Schedule}}<code>{{.Describe}}</code>{{else}}{{.Describe}}{{end}} · grace {{dur .Grace}}</td>
<td>{{ago .LastPing}}</td><td>{{until .NextDue}}</td>
<td class="muted">{{if .LastDuration}}{{dur .LastDuration}}{{else}}—{{end}}</td>
</tr>{{else}}<tr><td colspan="6" class="muted">No checks yet. Create one below.</td></tr>{{end}}
</table></div>
<h2>New check</h2>
<div class="card"><form method="post" action="/ui/checks" class="grid">
<label>Name<input name="name" placeholder="nightly-db-backup" required></label>
<label>Period<input name="period" placeholder="1d" value="1d"></label>
<label>…or cron schedule<input name="schedule" placeholder="0 3 * * *"></label>
<label>Timezone<input name="tz" placeholder="UTC" list="tzs"></label>
<label>Grace<input name="grace" placeholder="1h" value="1h"></label>
<label>Alert URL (optional)<input name="alert_url" placeholder="default from server"></label>
<button class="primary">Create</button></form></div>
{{template "foot"}}{{end}}

{{define "check"}}{{template "head" .}}
{{with .Error}}<p class="err">{{.}}</p>{{end}}
<div style="display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap">
<h2 style="margin:0">{{.C.Name}} <span class="pill {{.State}}" style="margin-left:8px">{{.State}}</span>
<span class="muted" style="font-weight:400;margin-left:8px">{{.C.Describe}} · grace {{dur .C.Grace}} · due {{until .C.NextDue}}</span></h2>
<div>{{if .C.Paused}}<form class="inline" method="post" action="/ui/checks/{{.C.ID}}/resume"><button>Resume</button></form>
{{else}}<form class="inline" method="post" action="/ui/checks/{{.C.ID}}/pause"><button>Pause</button></form>{{end}}
<form class="inline" method="post" action="/ui/checks/{{.C.ID}}/delete" onsubmit="return confirm('Delete this check and its history?')"><button class="danger">Delete</button></form></div></div>

<h2>Wire it up</h2>
<div class="card" style="padding:14px">
<span class="muted">Plain curl at the end of your job:</span>
<pre>{{if .C.Schedule}}{{if .C.TZ}}CRON_TZ={{.C.TZ}}
{{end}}{{.C.Schedule}}{{else}}0 3 * * *{{end}} /usr/local/bin/backup.sh &amp;&amp; curl -fsS -m 10 --retry 3 {{.PingURL}}</pre>
<span class="muted">Report the exit code (non-zero alerts immediately):</span>
<pre>/usr/local/bin/backup.sh; curl -fsS -m 10 --retry 3 {{.PingURL}}/$?</pre>
<span class="muted">Or wrap it — tracks run time, catches hangs, ships the last 10 KB of output:</span>
<pre>cronbeat run --url {{.PingURL}} -- /usr/local/bin/backup.sh</pre>
</div>

<h2>Settings</h2>
<div class="card"><form method="post" action="/ui/checks/{{.C.ID}}/edit" class="grid">
<label>Name<input name="name" value="{{.C.Name}}" required></label>
<label>Period<input name="period" value="{{dur .C.Period}}"></label>
<label>…or cron schedule<input name="schedule" value="{{.C.Schedule}}" placeholder="0 3 * * *"></label>
<label>Timezone<input name="tz" value="{{.C.TZ}}" placeholder="UTC" list="tzs"></label>
<label>Grace<input name="grace" value="{{dur .C.Grace}}"></label>
<label>Alert URL<input name="alert_url" value="{{.C.AlertURL}}" placeholder="default from server"></label>
<button>Save</button></form></div>

<h2>Recent pings</h2>
<div class="card"><table>
<tr><th>Event</th><th>Time (UTC)</th><th>Exit</th><th>From</th><th>Output</th></tr>
{{range .Pings}}<tr>
<td><span class="pill {{if eq .Kind "success"}}up{{else if eq .Kind "fail"}}down{{else}}late{{end}}">{{.Kind}}</span></td>
<td>{{ts .At}} <span class="muted">({{ago .At}})</span></td>
<td>{{with .ExitCode}}{{.}}{{else}}—{{end}}</td><td class="muted">{{.Remote}}</td>
<td>{{if .Body}}<details><summary class="muted">{{len .Body}} bytes</summary><pre>{{.Body}}</pre></details>{{else}}<span class="muted">—</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="5" class="muted">Waiting for the first ping…</td></tr>{{end}}
</table></div>
{{template "foot"}}{{end}}
`
