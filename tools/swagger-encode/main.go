// Command swagger-encode reads api/openapi.yaml, gzip-compresses it, and
// base64-encodes the result. It then writes the swaggerSpec []string block
// into api/generated.go, replacing the existing block.
//
// This is necessary because oapi-codegen v2.6.0 is incompatible with Go 1.26
// so we cannot regenerate the full generated.go; instead we manually refresh
// the embedded spec blob after each spec edit.
//
// Usage (from repo root):
//
//	go run ./tools/swagger-encode/main.go
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	specPath      = "api/openapi.yaml"
	generatedPath = "api/generated.go"
	chunkSize     = 76 // characters per swaggerSpec string element
)

func main() {
	spec, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", specPath, err)
		os.Exit(1)
	}

	// Gzip-compress.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(spec); err != nil {
		fmt.Fprintf(os.Stderr, "gzip write: %v\n", err)
		os.Exit(1)
	}
	if err := gz.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "gzip close: %v\n", err)
		os.Exit(1)
	}

	// Base64-encode.
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Split into chunks.
	var chunks []string
	for i := 0; i < len(encoded); i += chunkSize {
		end := i + chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		chunks = append(chunks, "\t\""+encoded[i:end]+"\"")
	}

	newBlock := "// Base64 encoded, gzipped, json marshaled Swagger object\nvar swaggerSpec = []string{\n\n" +
		strings.Join(chunks, ",\n") + ",\n\n}"

	// Read existing generated.go.
	gen, err := os.ReadFile(generatedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", generatedPath, err)
		os.Exit(1)
	}

	// Replace the swaggerSpec block using a regexp that matches from the comment
	// through the closing brace.
	re := regexp.MustCompile(`(?s)// Base64 encoded, gzipped, json marshaled Swagger object\nvar swaggerSpec = \[\]string\{.*?\n\}`)
	updated := re.ReplaceAllString(string(gen), newBlock)
	if updated == string(gen) {
		fmt.Fprintf(os.Stderr, "warning: swaggerSpec block not found or unchanged in %s\n", generatedPath)
		os.Exit(1)
	}

	if err := os.WriteFile(generatedPath, []byte(updated), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", generatedPath, err)
		os.Exit(1)
	}

	fmt.Printf("swaggerSpec updated: %d bytes → %d gzipped → %d base64 (%d chunks)\n",
		len(spec), buf.Len(), len(encoded), len(chunks))
}
