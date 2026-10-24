package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// tailBuffer keeps only the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.buf)
}

// sendPing never fails the wrapped job: monitoring outages must not break backups.
func sendPing(client *http.Client, url string, body []byte) {
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		resp, err := client.Post(url, "text/plain; charset=utf-8", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				if resp.StatusCode >= 400 {
					fmt.Fprintf(os.Stderr, "cronbeat: ping %s returned HTTP %d\n", url, resp.StatusCode)
				}
				return
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if attempt == 2 {
			fmt.Fprintf(os.Stderr, "cronbeat: ping failed: %v\n", err)
		}
	}
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	pingURL := fs.String("url", os.Getenv("CRONBEAT_URL"), "check ping URL (env CRONBEAT_URL)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cronbeat run --url https://host/ping/<id> -- command [args...]")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	argv := fs.Args()
	if *pingURL == "" || len(argv) == 0 {
		fs.Usage()
		return 2
	}
	base := strings.TrimRight(*pingURL, "/")
	client := &http.Client{Timeout: 10 * time.Second}

	sendPing(client, base+"/start", nil)

	tail := &tailBuffer{max: maxBodyBytes}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)

	code := 0
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(tail, "cronbeat: failed to start %q: %v\n", argv[0], err)
		fmt.Fprintf(os.Stderr, "cronbeat: failed to start %q: %v\n", argv[0], err)
		code = 127
	} else {
		// Forward termination signals so `systemctl stop` or Ctrl-C reach the job.
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		go func() {
			for s := range sigs {
				cmd.Process.Signal(s)
			}
		}()
		err := cmd.Wait()
		signal.Stop(sigs)
		close(sigs)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
				fmt.Fprintf(tail, "\ncronbeat: killed by signal %s\n", ws.Signal())
			}
		} else if err != nil {
			code = 1
		}
	}

	sendPing(client, base+"/"+strconv.Itoa(code), tail.Bytes())
	return code
}
