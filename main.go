// Entry point: the entry <-> env lifecycle, as env/src/wire.ts settles it:
// bind an ephemeral port, tell the env the port it bound, then poll until
// the env says the trial is over and exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial
//      container). Without it there is nothing to talk to, so the program
//      exits 1 at once.
//   2. Listen on :0: the kernel picks a free port. Never bind 8080 — the env
//      owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the
//      address the request came from plus N as your base URL and dials it
//      from then on. The first register wins; a 409 means another entry
//      already registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is a
//      poll, not a long-held request. A failed or slow poll is retried with
//      a short exponential backoff; the process never exits before the env
//      says done.
//   5. Close the listener and exit 0. The harness then stops the env, which
//      writes the export the scorer reads.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

func sleep(d time.Duration) { time.Sleep(d) }

func retryDelay(attempt int) time.Duration {
	delay := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		delay *= 2
	}
	if delay > retryMax {
		delay = retryMax
	}
	return delay
}

var httpClient = &http.Client{Timeout: requestTimeout}

// register POSTs the bound port until the env accepts it. 200 is the normal
// answer; 409 means a first register already won, which is not this
// process's to fix. 400 and 405 are contract errors and end the program; a
// network error or another status is retried, since the env may still be
// starting.
func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		resp, err := httpClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}; it never
// returns before that.
func waitForDone(envURL string) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("GET", envURL+"/done", nil)
		if err == nil {
			resp, err := httpClient.Do(req)
			if err == nil {
				var data struct {
					Done bool `json:"done"`
				}
				decErr := json.NewDecoder(resp.Body).Decode(&data)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if decErr == nil && data.Done {
					return
				}
			}
		}
		sleep(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "[entry] the server did not bind an ephemeral TCP port")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "[entry] listening on port %d\n", addr.Port)
	go serve(listener)

	if err := register(envURL, addr.Port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	listener.Close()
	fmt.Fprintln(os.Stdout, "[entry] done")
}
