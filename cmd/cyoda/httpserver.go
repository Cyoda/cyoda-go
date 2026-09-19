package main

import (
	"net/http"

	"github.com/cyoda-platform/cyoda-go/app"
)

// newHTTPServer is the one place an http.Server is built for this binary, so
// the API server and the admin server carry the same receive-side timeouts.
// It carries no address: every server is served on a listener bound by the
// caller.
func newHTTPServer(handler http.Handler, t app.HTTPConfig) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: t.ReadHeaderTimeout,
		ReadTimeout:       t.ReadTimeout,
		WriteTimeout:      t.WriteTimeout,
		IdleTimeout:       t.IdleTimeout,
	}
}
