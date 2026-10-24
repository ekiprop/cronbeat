// Command cronbeat is a dead man's switch for cron jobs and scheduled tasks.
// Jobs ping it when they run; when a ping doesn't arrive on time, it alerts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

const usage = `cronbeat — a dead man's switch for cron jobs

usage:
  cronbeat serve [flags]                    run the server
  cronbeat run --url <ping-url> -- cmd ...  run a command and report to cronbeat
  cronbeat version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serveCmd(os.Args[2:]))
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func serveCmd(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("CRONBEAT_ADDR", ":8080"), "listen address (env CRONBEAT_ADDR)")
	dbPath := fs.String("db", envOr("CRONBEAT_DB", "cronbeat.db"), "SQLite database path (env CRONBEAT_DB)")
	baseURL := fs.String("base-url", envOr("CRONBEAT_BASE_URL", "http://localhost:8080"), "public URL, used in ping URLs and alerts (env CRONBEAT_BASE_URL)")
	alertURL := fs.String("alert-url", os.Getenv("CRONBEAT_ALERT_URL"), "default alert webhook(s), comma-separated (env CRONBEAT_ALERT_URL)")
	interval := fs.Duration("check-interval", 15*time.Second, "how often to look for overdue checks")
	fs.Parse(args)

	store, err := OpenStore(*dbPath)
	if err != nil {
		log.Printf("open db: %v", err)
		return 1
	}
	defer store.Close()

	apiKey := os.Getenv("CRONBEAT_API_KEY")
	if apiKey == "" {
		key, created, err := store.APIKey()
		if err != nil {
			log.Printf("api key: %v", err)
			return 1
		}
		apiKey = key
		if created {
			log.Printf("generated API key (stored in %s): %s", *dbPath, apiKey)
		}
	}

	notifier := NewNotifier(splitURLs(*alertURL), *baseURL)
	if len(notifier.defaults) == 0 {
		log.Printf("warning: no default alert URL set; only checks with their own alert_url will alert")
	}
	srv := NewServer(store, notifier.Send, apiKey, *baseURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(*interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				overdue, err := store.MarkOverdue(now)
				if err != nil {
					log.Printf("sweep: %v", err)
				}
				for _, o := range overdue {
					log.Printf("check %q is down: %s", o.Check.Name, o.Reason)
					notifier.Send(Event{Kind: "down", Check: o.Check, Reason: o.Reason, At: now})
				}
			}
		}
	}()

	hs := &http.Server{
		Addr: *addr, Handler: srv,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutCtx)
	}()

	log.Printf("cronbeat %s listening on %s (public URL %s)", version, *addr, *baseURL)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("listen: %v", err)
		return 1
	}
	return 0
}
