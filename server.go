package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const minPeriod = 30 * time.Second

type Server struct {
	store   *Store
	notify  func(Event)
	apiKey  string
	baseURL string
	now     func() time.Time
	mux     *http.ServeMux
}

func NewServer(store *Store, notify func(Event), apiKey, baseURL string) *Server {
	s := &Server{store: store, notify: notify, apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"), now: time.Now, mux: http.NewServeMux()}

	// Pings are unauthenticated: the check UUID is the secret.
	s.mux.HandleFunc("/ping/{id}", s.handlePing)
	s.mux.HandleFunc("/ping/{id}/{action}", s.handlePing)

	s.mux.HandleFunc("GET /api/checks", s.api(s.apiList))
	s.mux.HandleFunc("POST /api/checks", s.api(s.apiCreate))
	s.mux.HandleFunc("GET /api/checks/{id}", s.api(s.apiGet))
	s.mux.HandleFunc("PATCH /api/checks/{id}", s.api(s.apiUpdate))
	s.mux.HandleFunc("DELETE /api/checks/{id}", s.api(s.apiDelete))
	s.mux.HandleFunc("GET /metrics", s.api(s.handleMetrics))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	s.mux.HandleFunc("GET /{$}", s.ui(s.uiDashboard))
	s.mux.HandleFunc("GET /checks/{id}", s.ui(s.uiCheck))
	s.mux.HandleFunc("POST /ui/checks", s.ui(s.uiCreate))
	s.mux.HandleFunc("POST /ui/checks/{id}/{op}", s.ui(s.uiAction))
	s.mux.HandleFunc("GET /login", s.uiLoginForm)
	s.mux.HandleFunc("POST /login", s.uiLogin)
	s.mux.HandleFunc("POST /logout", s.uiLogout)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) pingURL(id string) string { return s.baseURL + "/ping/" + id }

// ---- pings ----

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	kind := "success"
	var exitCode *int
	switch action := r.PathValue("action"); action {
	case "":
	case "start":
		kind = "start"
	case "fail":
		kind = "fail"
	default:
		code, err := strconv.Atoi(action)
		if err != nil || code < 0 || code > 255 {
			http.Error(w, "unknown ping action", http.StatusNotFound)
			return
		}
		exitCode = &code
		if code != 0 {
			kind = "fail"
		}
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	now := s.now()
	prev, c, err := s.store.RecordPing(r.PathValue("id"), kind, exitCode, clientIP(r), string(body), now)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "no such check", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("ping: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	switch {
	case prev != "down" && c.Status == "down":
		reason := "job reported failure"
		if exitCode != nil {
			reason = fmt.Sprintf("job exited with code %d", *exitCode)
		}
		s.notify(Event{Kind: "down", Check: c, Reason: reason, At: now})
	case prev == "down" && c.Status == "up":
		s.notify(Event{Kind: "up", Check: c, Reason: "received a success ping", At: now})
	}
	io.WriteString(w, "OK\n")
}

// ---- JSON API ----

type apiFunc func(w http.ResponseWriter, r *http.Request) (any, int, error)

func (s *Server) authorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(tok), []byte(s.apiKey)) == 1
}

func (s *Server) api(fn apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
			return
		}
		v, status, err := fn(w, r)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		case err != nil && status >= 400 && status < 500:
			writeJSON(w, status, map[string]string{"error": err.Error()})
		case err != nil:
			log.Printf("api %s %s: %v", r.Method, r.URL.Path, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		case v != nil:
			writeJSON(w, status, v)
		case status != 0:
			w.WriteHeader(status)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

type checkView struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	State          string     `json:"state"`
	Period         string     `json:"period"`
	Schedule       string     `json:"schedule,omitempty"`
	TZ             string     `json:"tz,omitempty"`
	Grace          string     `json:"grace"`
	AlertURL       string     `json:"alert_url,omitempty"`
	LastPing       *time.Time `json:"last_ping,omitempty"`
	NextDue        *time.Time `json:"next_due,omitempty"`
	LastDurationMs int64      `json:"last_duration_ms,omitempty"`
	PingURL        string     `json:"ping_url"`
	CreatedAt      time.Time  `json:"created_at"`
	Pings          []pingView `json:"pings,omitempty"`
}

type pingView struct {
	Kind     string    `json:"kind"`
	ExitCode *int      `json:"exit_code,omitempty"`
	At       time.Time `json:"at"`
	Remote   string    `json:"remote"`
	Body     string    `json:"body,omitempty"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func (s *Server) view(c Check) checkView {
	return checkView{
		ID: c.ID, Name: c.Name, State: c.State(s.now()),
		Period: fmtDur(c.Period), Schedule: c.Schedule, TZ: c.TZ, Grace: fmtDur(c.Grace), AlertURL: c.AlertURL,
		LastPing: optTime(c.LastPing), NextDue: optTime(c.NextDue()),
		LastDurationMs: c.LastDuration.Milliseconds(), PingURL: s.pingURL(c.ID),
		CreatedAt: c.CreatedAt.UTC(),
	}
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) (any, int, error) {
	checks, err := s.store.ListChecks()
	if err != nil {
		return nil, 0, err
	}
	out := make([]checkView, 0, len(checks))
	for _, c := range checks {
		out = append(out, s.view(c))
	}
	return out, http.StatusOK, nil
}

func (s *Server) apiGet(w http.ResponseWriter, r *http.Request) (any, int, error) {
	c, err := s.store.GetCheck(r.PathValue("id"))
	if err != nil {
		return nil, 0, err
	}
	pings, err := s.store.Pings(c.ID, 20)
	if err != nil {
		return nil, 0, err
	}
	v := s.view(c)
	for _, p := range pings {
		v.Pings = append(v.Pings, pingView{Kind: p.Kind, ExitCode: p.ExitCode, At: p.At.UTC(), Remote: p.Remote, Body: p.Body})
	}
	return v, http.StatusOK, nil
}

// checkInput is shared by create and update; nil fields are left unchanged.
// Durations may be strings ("6h", "1d") or numbers of seconds. A non-empty
// schedule (cron expression) takes precedence over period; "" clears it.
type checkInput struct {
	Name     *string `json:"name"`
	Period   any     `json:"period"`
	Schedule *string `json:"schedule"`
	TZ       *string `json:"tz"`
	Grace    any     `json:"grace"`
	AlertURL *string `json:"alert_url"`
	Paused   *bool   `json:"paused"`
}

func durFrom(v any) (time.Duration, error) {
	switch v := v.(type) {
	case float64:
		return time.Duration(v * float64(time.Second)), nil
	case string:
		return parseDur(v)
	}
	return 0, fmt.Errorf("duration must be a string like \"1h\" or a number of seconds")
}

func (in checkInput) apply(c *Check) error {
	if in.Name != nil {
		c.Name = strings.TrimSpace(*in.Name)
	}
	if in.Period != nil {
		d, err := durFrom(in.Period)
		if err != nil {
			return fmt.Errorf("period: %w", err)
		}
		c.Period = d
	}
	if in.Schedule != nil {
		c.Schedule = strings.TrimSpace(*in.Schedule)
	}
	if in.TZ != nil {
		c.TZ = strings.TrimSpace(*in.TZ)
	}
	if in.Grace != nil {
		d, err := durFrom(in.Grace)
		if err != nil {
			return fmt.Errorf("grace: %w", err)
		}
		c.Grace = d
	}
	if in.AlertURL != nil {
		c.AlertURL = strings.TrimSpace(*in.AlertURL)
	}
	if in.Paused != nil {
		c.Paused = *in.Paused
	}
	if c.Schedule != "" {
		if _, _, err := parseSchedule(c.Schedule, c.TZ); err != nil {
			return err
		}
	}
	switch {
	case c.Name == "":
		return errors.New("name is required")
	case c.Schedule == "" && c.Period < minPeriod:
		return fmt.Errorf("period must be at least %s", fmtDur(minPeriod))
	case c.Grace < 0:
		return errors.New("grace must not be negative")
	}
	for _, u := range splitURLs(c.AlertURL) {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("alert_url %q must be http(s)", u)
		}
	}
	return nil
}

func decodeInput(r *http.Request) (checkInput, error) {
	var in checkInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		return in, fmt.Errorf("invalid JSON: %w", err)
	}
	return in, nil
}

func (s *Server) apiCreate(w http.ResponseWriter, r *http.Request) (any, int, error) {
	in, err := decodeInput(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	c := Check{Period: 24 * time.Hour, Grace: time.Hour}
	if err := in.apply(&c); err != nil {
		return nil, http.StatusBadRequest, err
	}
	c, err = s.store.CreateCheck(c, s.now())
	if err != nil {
		return nil, 0, err
	}
	return s.view(c), http.StatusCreated, nil
}

func (s *Server) apiUpdate(w http.ResponseWriter, r *http.Request) (any, int, error) {
	c, err := s.store.GetCheck(r.PathValue("id"))
	if err != nil {
		return nil, 0, err
	}
	in, err := decodeInput(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := in.apply(&c); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := s.store.UpdateCheck(c); err != nil {
		return nil, 0, err
	}
	return s.view(c), http.StatusOK, nil
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) (any, int, error) {
	return nil, http.StatusNoContent, s.store.DeleteCheck(r.PathValue("id"))
}

// ---- Prometheus metrics ----

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) (any, int, error) {
	checks, err := s.store.ListChecks()
	if err != nil {
		return nil, 0, err
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	b.WriteString("# HELP cronbeat_check_up 1 if the check is up, 0 if down, -1 if new or paused.\n# TYPE cronbeat_check_up gauge\n")
	for _, c := range checks {
		up := -1
		switch {
		case c.Paused:
		case c.Status == "up":
			up = 1
		case c.Status == "down":
			up = 0
		}
		fmt.Fprintf(&b, "cronbeat_check_up{id=%q,name=\"%s\"} %d\n", c.ID, labelEscaper.Replace(c.Name), up)
	}
	b.WriteString("# HELP cronbeat_check_last_ping_timestamp_seconds Unix time of the last success or fail ping.\n# TYPE cronbeat_check_last_ping_timestamp_seconds gauge\n")
	for _, c := range checks {
		if !c.LastPing.IsZero() {
			fmt.Fprintf(&b, "cronbeat_check_last_ping_timestamp_seconds{id=%q,name=\"%s\"} %d\n", c.ID, labelEscaper.Replace(c.Name), c.LastPing.Unix())
		}
	}
	b.WriteString("# HELP cronbeat_check_last_duration_seconds Run time of the last start→finish cycle.\n# TYPE cronbeat_check_last_duration_seconds gauge\n")
	for _, c := range checks {
		if c.LastDuration > 0 {
			fmt.Fprintf(&b, "cronbeat_check_last_duration_seconds{id=%q,name=\"%s\"} %.3f\n", c.ID, labelEscaper.Replace(c.Name), c.LastDuration.Seconds())
		}
	}
	io.WriteString(w, b.String())
	return nil, 0, nil
}

// ---- dashboard session ----

const sessionCookie = "cronbeat_session"

func (s *Server) sessionToken() string {
	m := hmac.New(sha256.New, []byte(s.apiKey))
	m.Write([]byte("cronbeat-dashboard-session"))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) ui(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(sessionCookie)
		if err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(s.sessionToken())) != 1 {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}
		fn(w, r)
	}
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("key")
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		render(w, "login", map[string]any{"Login": true, "Error": "Wrong key."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sessionToken(), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: strings.HasPrefix(s.baseURL, "https://"), MaxAge: 30 * 24 * 3600,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
