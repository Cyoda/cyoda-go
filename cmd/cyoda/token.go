package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// defaultTokenTTL is the lifetime without --ttl, capped by
// CYODA_JWT_EXPIRY_SECONDS when that is shorter.
const defaultTokenTTL = 15 * time.Minute

// runToken is `cyoda token`: sign a short-lived admin token offline with
// CYODA_JWT_SIGNING_KEY and print it, and nothing else, on stdout. It opens no
// store and makes no network call. Exit codes: 0 success (or -h/--help); 1
// key or configuration error; 2 flag error. It never calls logging.Init (which
// writes to stdout) and never writes the token or key material to stderr.
func runToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "tenant id the token acts in (required)")
	user := fs.String("user", "operator", "user id recorded for calls made with the token")
	roles := fs.String("roles", "ROLE_ADMIN", "comma-separated roles")
	ttl := fs.Duration("ttl", defaultTokenTTL, "token lifetime; at most CYODA_JWT_EXPIRY_SECONDS, which also caps the default")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h/--help: the flag package printed the usage to stderr
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "cyoda token: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *tenant == "" {
		fmt.Fprintln(stderr, "cyoda token: --tenant is required")
		return 2
	}
	roleList := strings.Split(*roles, ",")
	for i := range roleList {
		roleList[i] = strings.TrimSpace(roleList[i])
	}

	app.LoadEnvFiles()
	settings, err := app.LoadJWTSettings()
	if err != nil {
		fmt.Fprintf(stderr, "cyoda token: %v\n", err)
		return 1
	}
	maxTTL := time.Duration(settings.ExpirySeconds) * time.Second
	ttlSet := false
	fs.Visit(func(f *flag.Flag) { ttlSet = ttlSet || f.Name == "ttl" })
	if !ttlSet && *ttl > maxTTL {
		*ttl = maxTTL
	}
	if *ttl <= 0 || *ttl > maxTTL {
		fmt.Fprintf(stderr, "cyoda token: --ttl must be greater than 0 and at most %ds (CYODA_JWT_EXPIRY_SECONDS)\n", settings.ExpirySeconds)
		return 2
	}
	req := auth.OperatorTokenRequest{
		Tenant: spi.TenantID(*tenant), UserID: *user, Roles: roleList, TTL: *ttl,
		Issuer: settings.Issuer, Audience: settings.Audience,
	}
	if err := auth.ValidateOperatorTokenRequest(req); err != nil {
		fmt.Fprintf(stderr, "cyoda token: %v\n", err)
		return 2
	}
	if settings.SigningKeyPEM == "" {
		fmt.Fprintln(stderr, "cyoda token: CYODA_JWT_SIGNING_KEY (or CYODA_JWT_SIGNING_KEY_FILE) is not set")
		return 1
	}
	key, err := auth.ParseRSAPrivateKeyFromPEM([]byte(settings.SigningKeyPEM))
	if err != nil {
		fmt.Fprintln(stderr, "cyoda token: CYODA_JWT_SIGNING_KEY is not a usable RSA private key")
		return 1
	}
	tok, err := auth.MintOperatorToken(context.Background(), key, req)
	if err != nil {
		fmt.Fprintf(stderr, "cyoda token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	return 0
}
