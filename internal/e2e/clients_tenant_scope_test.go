package e2e_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// clients_tenant_scope_test.go covers caller-chosen client ids and the
// per-tenant scope of a client id: the same id in two tenants is two clients.

// createChosen runs POST /api/clients?clientId=id against baseURL as bearer.
func createChosen(t *testing.T, baseURL, bearer, id string) (int, []byte) {
	t.Helper()
	resp := doAuthAgainst(t, baseURL, bearer, http.MethodPost, "/api/clients?clientId="+url.QueryEscape(id), "")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// clientCall runs method path against baseURL as bearer and returns the status.
func clientCall(t *testing.T, baseURL, bearer, method, path string) int {
	t.Helper()
	resp := doAuthAgainst(t, baseURL, bearer, method, path, "")
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestClients_ChosenID(t *testing.T) {
	id := "order-service-" + uuid.NewString()[:8]
	bearer := suiteToken(t)
	code, raw := createChosen(t, serverURL, bearer, id)
	if code != http.StatusOK {
		t.Fatalf("create with a chosen id: %d %s, want 200", code, withheld(code, raw))
	}
	cred := decodeCredential(t, "create with a chosen id", raw)
	deleteClientAtCleanup(t, serverURL, id, func() string { return suiteToken(t) })
	if cred.id != id {
		t.Fatalf("client_id = %q, want %q", cred.id, id)
	}
	if got := tokenStatusOn(t, serverURL, suiteTenant, id, cred.secret); got != http.StatusOK {
		t.Fatalf("token for the chosen id: %d, want 200", got)
	}
	resp := doAuthAgainst(t, serverURL, bearer, http.MethodPost, "/api/clients?clientId="+url.QueryEscape(id), "")
	assertProblemJSON(t, resp, http.StatusConflict, "M2M_CLIENT_EXISTS")
}

// TestClients_ChosenIDGrammar runs on a harness stack: the shared server's
// OpenAPI validator would refuse these before the handler sees them.
func TestClients_ChosenIDGrammar(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := newCalloutHarness(t, nil)
	bearer := h.token(t)
	for name, id := range map[string]string{
		"empty":          "",
		"leading dash":   "-x",
		"colon":          "a:b",
		"system":         "system",
		"SYSTEM":         "SYSTEM",
		"101 characters": strings.Repeat("a", 101),
	} {
		t.Run(name, func(t *testing.T) {
			resp := h.doAuthBearer(t, bearer, http.MethodPost, "/api/clients?clientId="+url.QueryEscape(id), "", "")
			assertProblemJSON(t, resp, http.StatusBadRequest, "BAD_REQUEST")
		})
	}
}

func TestClientsStore_TakenIDBeforeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := capStack(t, 1)
	bearer := h.token(t)
	if code, raw := h.postClientWithID(t, bearer, "only"); code != http.StatusOK {
		t.Fatalf("first create: %d %s, want 200", code, withheld(code, raw))
	}
	resp := h.doAuthBearer(t, bearer, http.MethodPost, "/api/clients?clientId=only", "", "")
	assertProblemJSON(t, resp, http.StatusConflict, "M2M_CLIENT_EXISTS")
}

func TestClients_SameIDInTwoTenants(t *testing.T) {
	ta, tb := "ta-"+uuid.NewString(), "tb-"+uuid.NewString()
	adminA, adminB := adminTokenForTenant(t, ta, "admin-a"), adminTokenForTenant(t, tb, "admin-b")
	const id = "backend"
	create := func(tenant, bearer string) m2mCredential {
		code, raw := createChosen(t, serverURL, bearer, id)
		if code != http.StatusOK {
			t.Fatalf("create %s in %s: %d %s, want 200", id, tenant, code, withheld(code, raw))
		}
		cred := decodeCredential(t, "create in "+tenant, raw)
		deleteClientAtCleanup(t, serverURL, id, func() string { return bearer })
		return cred
	}
	a, b := create(ta, adminA), create(tb, adminB)

	for tenant, c := range map[string]m2mCredential{ta: a, tb: b} {
		tok := getTokenIn(t, tenant, id, c.secret)
		parsed, err := auth.Parse(tok)
		if err != nil {
			t.Fatalf("parse token of %s: %v", tenant, err)
		}
		if got := parsed.Claims["caas_org_id"]; got != tenant {
			t.Fatalf("token of %s carries caas_org_id %v", tenant, got)
		}
	}
	if got := tokenStatusOn(t, serverURL, tb, id, a.secret); got != http.StatusUnauthorized {
		t.Fatalf("A's secret at B's token URL: %d, want 401", got)
	}
	if got := tokenStatusOn(t, serverURL, ta, id, b.secret); got != http.StatusUnauthorized {
		t.Fatalf("B's secret at A's token URL: %d, want 401", got)
	}

	// Reset in A leaves B alone.
	if code := clientCall(t, serverURL, adminA, http.MethodPut, "/api/clients/"+id+"/secret"); code != http.StatusOK {
		t.Fatalf("reset in A: %d, want 200", code)
	}
	if got := tokenStatusOn(t, serverURL, tb, id, b.secret); got != http.StatusOK {
		t.Fatalf("B's secret after A's reset: %d, want 200", got)
	}
	if got := tokenStatusOn(t, serverURL, ta, id, a.secret); got != http.StatusUnauthorized {
		t.Fatalf("A's replaced secret: %d, want 401", got)
	}

	// Delete in A leaves B alone; A can create the id again.
	if code := clientCall(t, serverURL, adminA, http.MethodDelete, "/api/clients/"+id); code != http.StatusOK {
		t.Fatalf("delete in A: %d, want 200", code)
	}
	if got := tokenStatusOn(t, serverURL, tb, id, b.secret); got != http.StatusOK {
		t.Fatalf("B after A's delete: %d, want 200", got)
	}
	if code, raw := createChosen(t, serverURL, adminA, id); code != http.StatusOK {
		t.Fatalf("create %s in A again: %d %s, want 200", id, code, withheld(code, raw))
	}
}

func TestClients_RateLimitIsPerTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := newCalloutHarness(t, func(cfg *app.Config) { cfg.IAM.TokenRequestsPerMinute = 1 })
	const id = "backend"
	var secrets [2]string
	tenants := [2]string{"rl-a", "rl-b"}
	for i, tenant := range tenants {
		code, raw := h.postClientWithID(t, h.adminTokenFor(t, tenant, "admin-"+tenant), id)
		if code != http.StatusOK {
			t.Fatalf("create %s in %s: %d %s", id, tenant, code, withheld(code, raw))
		}
		secrets[i] = decodeCredential(t, "create in "+tenant, raw).secret
	}
	if got := tokenStatusOn(t, h.baseURL, tenants[0], id, secrets[0]); got != http.StatusOK {
		t.Fatalf("A's first token request: %d, want 200", got)
	}
	if got := tokenStatusOn(t, h.baseURL, tenants[0], id, secrets[0]); got != http.StatusTooManyRequests {
		t.Fatalf("A's second token request in a minute: %d, want 429", got)
	}
	if got := tokenStatusOn(t, h.baseURL, tenants[1], id, secrets[1]); got != http.StatusOK {
		t.Fatalf("B's first token request after A was limited: %d, want 200", got)
	}
}

// raceCall runs one request from a goroutine: it never touches *testing.T.
func raceCall(method, target, bearer string) (int, []byte, error) {
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

type raceResult struct {
	code int
	raw  []byte
	err  error
}

// raceAll runs every call at once and returns the results in call order.
func raceAll(calls []func() (int, []byte, error)) []raceResult {
	out := make([]raceResult, len(calls))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i].code, out[i].raw, out[i].err = call()
		}()
	}
	close(start)
	wg.Wait()
	return out
}

func TestClientsStore_ConcurrentCreatesOfOneIDOnTwoNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	a, b := twoNodes(t)
	bearer := a.platformToken(t) // both stacks share the signing key
	for round := range 10 {
		id := "race" + string(rune('A'+round))
		call := func(ks *keyStack) func() (int, []byte, error) {
			return func() (int, []byte, error) {
				return raceCall(http.MethodPost, ks.baseURL+"/api/clients?clientId="+id, bearer)
			}
		}
		results := raceAll([]func() (int, []byte, error){call(a), call(b)})
		var winner []byte
		wins, conflicts := 0, 0
		for i, r := range results {
			switch {
			case r.err != nil:
				t.Fatalf("round %d call %d: %v", round, i, r.err)
			case r.code == http.StatusOK:
				wins++
				winner = r.raw
			case r.code == http.StatusConflict && problemErrorCode(string(r.raw)) == "M2M_CLIENT_EXISTS":
				conflicts++
			default:
				t.Fatalf("round %d call %d: %d %s", round, i, r.code, withheld(r.code, r.raw))
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d created, %d conflicts; want 1 and 1", round, wins, conflicts)
		}
		cred := decodeCredential(t, "winning create", winner)
		for _, ks := range []*keyStack{a, b} {
			if got := tokenStatusOn(t, ks.baseURL, string(auth.PlatformTenantID), id, cred.secret); got != http.StatusOK {
				t.Fatalf("round %d: the winner's secret at a node: %d, want 200", round, got)
			}
		}
	}
}

func TestClientsStore_ConcurrentResetsAreConsistent(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	a, b := twoNodes(t)
	bearer := a.platformToken(t)
	code, raw := a.postClientWithID(t, bearer, "resetme")
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, withheld(code, raw))
	}
	const attempts = 8
	calls := make([]func() (int, []byte, error), attempts)
	for i := range calls {
		ks := []*keyStack{a, b}[i%2]
		calls[i] = func() (int, []byte, error) {
			return raceCall(http.MethodPut, ks.baseURL+"/api/clients/resetme/secret", bearer)
		}
	}
	var secrets []string
	for i, r := range raceAll(calls) {
		switch {
		case r.err != nil:
			t.Fatalf("reset %d: %v", i, r.err)
		case r.code == http.StatusOK:
			secrets = append(secrets, decodeCredential(t, "reset", r.raw).secret)
		case r.code == http.StatusConflict && problemErrorCode(string(r.raw)) == "CONFLICT":
		default:
			t.Fatalf("reset %d: %d %s", i, r.code, withheld(r.code, r.raw))
		}
	}
	if len(secrets) == 0 {
		t.Fatal("no reset succeeded")
	}
	valid := 0
	for _, s := range secrets {
		if tokenStatusOn(t, a.baseURL, string(auth.PlatformTenantID), "resetme", s) == http.StatusOK {
			valid++
		}
	}
	if valid != 1 {
		t.Fatalf("%d of %d returned secrets get a token, want exactly 1", valid, len(secrets))
	}
}
