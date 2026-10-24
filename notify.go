package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type Event struct {
	Kind   string // down, up
	Check  Check
	Reason string
	At     time.Time
}

// Notifier fans events out to webhook targets. The payload shape is picked
// from the target URL so Slack, Discord, Telegram and ntfy work with no config.
type Notifier struct {
	client   *http.Client
	defaults []string
	baseURL  string
}

func NewNotifier(defaults []string, baseURL string) *Notifier {
	return &Notifier{client: &http.Client{Timeout: 10 * time.Second}, defaults: defaults, baseURL: baseURL}
}

func splitURLs(s string) []string {
	var out []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func (n *Notifier) Send(ev Event) {
	targets := n.defaults
	if ev.Check.AlertURL != "" {
		targets = splitURLs(ev.Check.AlertURL)
	}
	if len(targets) == 0 {
		log.Printf("alert %s for %q dropped: no alert URL configured", ev.Kind, ev.Check.Name)
		return
	}
	for _, t := range targets {
		go n.deliver(t, ev)
	}
}

func (n *Notifier) deliver(target string, ev Event) {
	body, ctype, headers := n.format(target, ev)
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			cancel()
			log.Printf("alert: bad target URL: %v", err)
			return
		}
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("User-Agent", "cronbeat")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := n.client.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		lastErr = err
	}
	log.Printf("alert %s for %q to %s failed: %v", ev.Kind, ev.Check.Name, redactURL(target), lastErr)
}

// redactURL keeps webhook secrets (which usually live in the path) out of logs.
func redactURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.Index(u[i+3:], "/"); j >= 0 {
			return u[:i+3+j] + "/…"
		}
	}
	return u
}

func (n *Notifier) message(ev Event) (title, text string) {
	if ev.Kind == "down" {
		title = fmt.Sprintf("🔴 %s is DOWN", ev.Check.Name)
	} else {
		title = fmt.Sprintf("🟢 %s is back UP", ev.Check.Name)
	}
	text = title
	if ev.Reason != "" {
		text += ": " + ev.Reason
	}
	text += "\n" + n.baseURL + "/checks/" + ev.Check.ID
	return title, text
}

func (n *Notifier) format(target string, ev Event) (body []byte, ctype string, headers map[string]string) {
	title, text := n.message(ev)
	switch {
	case strings.Contains(target, "hooks.slack.com"), strings.Contains(target, "api.telegram.org"):
		body, _ = json.Marshal(map[string]string{"text": text})
	case strings.Contains(target, "discord.com/api/webhooks"), strings.Contains(target, "discordapp.com/api/webhooks"):
		body, _ = json.Marshal(map[string]string{"content": text})
	case strings.Contains(target, "ntfy"):
		prio := "default"
		if ev.Kind == "down" {
			prio = "high"
		}
		return []byte(text), "text/plain; charset=utf-8", map[string]string{"Title": title, "Priority": prio}
	default:
		c := ev.Check
		payload := map[string]any{
			"event":   ev.Kind,
			"reason":  ev.Reason,
			"message": text,
			"at":      ev.At.UTC().Format(time.RFC3339),
			"check": map[string]any{
				"id": c.ID, "name": c.Name, "status": c.Status,
				"url": n.baseURL + "/checks/" + c.ID,
			},
		}
		if !c.LastPing.IsZero() {
			payload["check"].(map[string]any)["last_ping"] = c.LastPing.UTC().Format(time.RFC3339)
		}
		body, _ = json.Marshal(payload)
	}
	return body, "application/json", nil
}

// fmtDur renders durations the way people write them: 1d, 6h, 90s → 1m30s.
func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	var b strings.Builder
	if days := d / (24 * time.Hour); days > 0 {
		fmt.Fprintf(&b, "%dd", days)
		d -= days * 24 * time.Hour
	}
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= h * time.Hour
	}
	if m := d / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		d -= m * time.Minute
	}
	if d > 0 {
		fmt.Fprintf(&b, "%ds", d/time.Second)
	}
	return b.String()
}

// parseDur accepts Go durations plus a "d" (day) unit, e.g. "1d12h", "15m".
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var total time.Duration
	if i := strings.Index(s, "d"); i > 0 {
		var days int
		if _, err := fmt.Sscanf(s[:i], "%d", &days); err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total = time.Duration(days) * 24 * time.Hour
		s = s[i+1:]
	}
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (use e.g. 30m, 6h, 1d)", s)
		}
		total += d
	}
	return total, nil
}
