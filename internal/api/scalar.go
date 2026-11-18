package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"

	genapi "github.com/cyoda/cyoda-go/api"
)

func RegisterDiscoveryRoutes(mux *http.ServeMux, contextPath string) {
	mux.HandleFunc("GET /docs", handleScalarUI(contextPath))
	mux.HandleFunc("GET /openapi.json", handleOpenAPISpec(contextPath))
}

func handleScalarUI(contextPath string) http.HandlerFunc {
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <title>Cyoda-Go API Reference</title>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
</head>
<body>
  <script id="api-reference" data-url="/openapi.json"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>`)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	}
}

func handleOpenAPISpec(contextPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		swagger, err := genapi.GetSwagger()
		if err != nil {
			http.Error(w, "failed to load spec", http.StatusInternalServerError)
			return
		}

		// Override servers to match the actual runtime host and context path
		// so Scalar sends test requests to the right address.
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		swagger.Servers = openapi3.Servers{
			{URL: scheme + "://" + r.Host + contextPath},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(swagger)
	}
}
