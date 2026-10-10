package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// runHealth implements 'cyoda health': GET /readyz on the admin listener,
// exit 0 on 200, 1 otherwise. Primary consumer is the compose-level
// healthcheck; the Helm chart's readinessProbe hits the same endpoint over
// httpGet rather than through this subcommand.
//
// The 2-second client timeout is load-bearing. A deadlocked readiness
// handler looks exactly like "server accepts connection then hangs" to
// this client; without the timeout, Docker's HEALTHCHECK inherits the
// deadlock and never marks the container unhealthy.
func runHealth() int {
	// The env files the server reads, so an admin port set in one of them is
	// the port probed.
	app.LoadEnvFiles()
	port := 9091
	if v := os.Getenv("CYODA_ADMIN_PORT"); v != "" {
		// A port number and nothing else: the value can come from ./.env in
		// the working directory, and any other text could move the probe's
		// host off 127.0.0.1 (e.g. "9091@host").
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			fmt.Fprintf(os.Stderr, "cyoda health: CYODA_ADMIN_PORT %q is not a port number\n", v)
			return 1
		}
		port = p
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
		// /readyz never redirects; following one would carry the probe off
		// 127.0.0.1. A 3xx is a non-200 answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/readyz"
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cyoda health: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "cyoda health: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
