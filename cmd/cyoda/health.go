package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
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
func runHealth(args []string) int {
	port := os.Getenv("CYODA_ADMIN_PORT")
	if port == "" {
		port = "9091"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%s/readyz", port)
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
