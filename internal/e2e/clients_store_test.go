package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// clients_store_test.go proves the cluster-shared M2M client store end to end
// against a real PostgreSQL. Two stacks on one database are two nodes, and a
// new stack on a database is a restart. The store keeps no node copy, so a
// change made through one stack is visible through the other when the call
// that made it returns: no assertion here waits or polls for it.
//
// The clients a test creates on a stack of its own live in that test's
// database, which is dropped when the test ends. Tests on the shared server
// use createClient / createM2MClient, which delete their clients.

// twoNodes opens two stacks on one new database, with one bootstrap key.
func twoNodes(t *testing.T) (a, b *keyStack) {
	t.Helper()
	s, key := newSchedDB(t), genKey(t)
	return newKeyStackOn(t, s, key), newKeyStackOn(t, s, key)
}

// resetSecret resets id's secret through ks and returns the new one.
func (ks *keyStack) resetSecret(t *testing.T, id string) string {
	t.Helper()
	code, raw := ks.keyCall(t, http.MethodPut, "/clients/"+id+"/secret", "")
	if code != http.StatusOK {
		t.Fatalf("reset secret of %s: %d %s", id, code, raw)
	}
	return decodeCredential(t, "reset secret", raw).secret
}

// deleteClient runs DELETE /clients/{id} on h as bearer and returns the
// status and body.
func (h *callbackHarness) deleteClient(t *testing.T, bearer, id string) (int, string) {
	t.Helper()
	resp := h.doAuthBearer(t, bearer, http.MethodDelete, "/api/clients/"+id, "", "")
	return resp.StatusCode, h.readBody(t, resp)
}

// postClient runs POST /clients on h as bearer and returns the status and body.
func (h *callbackHarness) postClient(t *testing.T, bearer string) (int, []byte) {
	t.Helper()
	resp := h.doAuthBearer(t, bearer, http.MethodPost, "/api/clients", "", "")
	return resp.StatusCode, []byte(h.readBody(t, resp))
}

// putRawKV writes a SYSTEM-tenant KV row straight into s's database.
func (s *schedDB) putRawKV(t *testing.T, namespace, key, value string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO kv_store (tenant_id, namespace, key, value) VALUES ('SYSTEM', $1, $2, $3)
		 ON CONFLICT (tenant_id, namespace, key) DO UPDATE SET value = EXCLUDED.value`,
		namespace, key, []byte(value)); err != nil {
		t.Fatalf("raw KV write %s/%s: %v", namespace, key, err)
	}
}

// deleteRawKV removes a SYSTEM-tenant KV row straight from s's database.
func (s *schedDB) deleteRawKV(t *testing.T, namespace, key string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`DELETE FROM kv_store WHERE tenant_id = 'SYSTEM' AND namespace = $1 AND key = $2`, namespace, key); err != nil {
		t.Fatalf("raw KV delete %s/%s: %v", namespace, key, err)
	}
}

// hasRawKV reports whether s's database holds the SYSTEM-tenant KV row.
func (s *schedDB) hasRawKV(t *testing.T, namespace, key string) bool {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM kv_store WHERE tenant_id = 'SYSTEM' AND namespace = $1 AND key = $2`, namespace, key).Scan(&n); err != nil {
		t.Fatalf("raw KV read %s/%s: %v", namespace, key, err)
	}
	return n > 0
}

func TestClientsStore_CrossNodeAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	a, b := twoNodes(t)
	c := createKeyStackClient(t, a.callbackHarness)
	if code := tokenStatusOn(t, b.baseURL, c.id, c.secret); code != http.StatusOK {
		t.Fatalf("token on B right after the create on A: %d, want 200", code)
	}
	newSecret := a.resetSecret(t, c.id)
	if code := tokenStatusOn(t, b.baseURL, c.id, c.secret); code != http.StatusUnauthorized {
		t.Fatalf("old secret on B right after the reset on A: %d, want 401", code)
	}
	if code := tokenStatusOn(t, b.baseURL, c.id, newSecret); code != http.StatusOK {
		t.Fatalf("new secret on B right after the reset on A: %d, want 200", code)
	}
	if code, raw := a.deleteClient(t, a.oauthToken(t), c.id); code != http.StatusOK {
		t.Fatalf("delete on A: %d %s", code, raw)
	}
	if code := tokenStatusOn(t, b.baseURL, c.id, newSecret); code != http.StatusUnauthorized {
		t.Fatalf("deleted client on B right after the delete on A: %d, want 401", code)
	}
}

func TestClientsStore_SurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s, key := newSchedDB(t), genKey(t)
	a := newKeyStackOn(t, s, key)
	created := createKeyStackClient(t, a.callbackHarness)
	reset := createKeyStackClient(t, a.callbackHarness)
	gone := createKeyStackClient(t, a.callbackHarness)
	newSecret := a.resetSecret(t, reset.id)
	if code, raw := a.deleteClient(t, a.oauthToken(t), gone.id); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, raw)
	}

	r := newKeyStackOn(t, s, key) // the restart
	if code := tokenStatusOn(t, r.baseURL, created.id, created.secret); code != http.StatusOK {
		t.Fatalf("created client after restart: %d, want 200", code)
	}
	if code := tokenStatusOn(t, r.baseURL, reset.id, newSecret); code != http.StatusOK {
		t.Fatalf("reset secret after restart: %d, want 200", code)
	}
	if code := tokenStatusOn(t, r.baseURL, reset.id, reset.secret); code != http.StatusUnauthorized {
		t.Fatalf("secret replaced by the reset, after restart: %d, want 401", code)
	}
	if code := tokenStatusOn(t, r.baseURL, gone.id, gone.secret); code != http.StatusUnauthorized {
		t.Fatalf("deleted client after restart: %d, want 401", code)
	}
	ids := clientIDsOn(t, r.baseURL, r.oauthToken(t))
	if !ids[created.id] || !ids[reset.id] || ids[gone.id] {
		t.Fatalf("GET /clients after restart: created listed %v, reset listed %v, deleted listed %v; want true, true, false",
			ids[created.id], ids[reset.id], ids[gone.id])
	}
}

// capStack opens a stack on a new database with the given per-tenant cap.
// test-tenant holds the keyStack's client; cap-tenant starts empty.
func capStack(t *testing.T, maxPerTenant int) *callbackHarness {
	t.Helper()
	return newKeyStackWith(t, newSchedDB(t), genKey(t), func(cfg *app.Config) {
		cfg.IAM.M2MClientMaxPerTenant = maxPerTenant
	}).callbackHarness
}

func TestClientsStore_Cap(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := capStack(t, 2)
	bearer := h.adminTokenFor(t, "cap-tenant", "cap-admin")
	var first m2mCredential
	for i := 1; i <= 2; i++ {
		code, raw := h.postClient(t, bearer)
		if code != http.StatusOK {
			t.Fatalf("create %d of 2: %d %s", i, code, raw)
		}
		if i == 1 {
			first = decodeCredential(t, "create", raw)
		}
	}
	code, raw := h.postClient(t, bearer)
	if code != http.StatusBadRequest || problemErrorCode(string(raw)) != "M2M_CLIENT_CAP_REACHED" {
		t.Fatalf("create at the cap: %d %s, want 400 M2M_CLIENT_CAP_REACHED", code, withheld(code, raw))
	}
	if n := len(clientIDsOn(t, h.baseURL, bearer)); n != 2 {
		t.Fatalf("clients after the refused create: %d, want 2", n)
	}
	// The cap is per tenant: test-tenant, holding one client, still creates.
	if code, raw := h.postClient(t, h.token(t)); code != http.StatusOK {
		t.Fatalf("create in another tenant while cap-tenant is at the cap: %d %s, want 200", code, raw)
	}
	// A delete frees a slot.
	if code, body := h.deleteClient(t, bearer, first.id); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, raw := h.postClient(t, bearer); code != http.StatusOK {
		t.Fatalf("create after a delete freed a slot: %d %s, want 200", code, raw)
	}
}

func TestClientsStore_CapZeroIsUnbounded(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	h := capStack(t, 0)
	bearer := h.adminTokenFor(t, "cap-tenant", "cap-admin")
	for i := 1; i <= 3; i++ {
		if code, raw := h.postClient(t, bearer); code != http.StatusOK {
			t.Fatalf("create %d with no cap: %d %s, want 200", i, code, raw)
		}
	}
}

// TestClientsStore_ConcurrentCreatesStopAtCap fires more creates at one node
// at once than the cap allows: exactly the cap's worth succeed.
func TestClientsStore_ConcurrentCreatesStopAtCap(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const limit, attempts = 5, 30
	h := capStack(t, limit)
	bearer := h.adminTokenFor(t, "cap-tenant", "cap-admin")
	type result struct {
		code int
		body string
		err  error
	}
	results := make([]result, attempts)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/clients", nil)
			if err != nil {
				results[i].err = err
				return
			}
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i].err = err
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			results[i].code = resp.StatusCode
			if resp.StatusCode != http.StatusOK { // a 200 body carries a secret
				results[i].body = string(raw)
			}
		}()
	}
	close(start)
	wg.Wait()
	created, refused := 0, 0
	for i, r := range results {
		switch {
		case r.err != nil:
			t.Fatalf("create %d: %v", i, r.err)
		case r.code == http.StatusOK:
			created++
		case r.code == http.StatusBadRequest && problemErrorCode(r.body) == "M2M_CLIENT_CAP_REACHED":
			refused++
		default:
			t.Fatalf("create %d: %d %s", i, r.code, r.body)
		}
	}
	if created != limit || refused != attempts-limit {
		t.Fatalf("concurrent creates: %d created, %d refused; want %d and %d", created, refused, limit, attempts-limit)
	}
	if n := len(clientIDsOn(t, h.baseURL, bearer)); n != limit {
		t.Fatalf("clients stored: %d, want %d", n, limit)
	}
}

// TestClientsStore_TokenEndpointRefusesMalformedIDs sends client ids outside
// the client-id grammar, form-urlencoded in Basic credentials as RFC 6749
// §2.3.1 has it: each is refused 401 invalid_client, not handed to the store.
func TestClientsStore_TokenEndpointRefusesMalformedIDs(t *testing.T) {
	for name, id := range map[string]string{
		"NUL":            "a%00b",
		"invalid UTF-8":  "%FF",
		"101 characters": strings.Repeat("A", 101),
		"encoded colon":  "a%3Ab",
	} {
		t.Run(name, func(t *testing.T) {
			resp := postToken(t, url.Values{"grant_type": {"client_credentials"}}, id, "some-secret")
			assertOAuthError(t, resp, http.StatusUnauthorized, "invalid_client")
		})
	}
}

// TestClientsStore_RawRecords drives the store through rows written straight
// into the database: undecodable data fails closed, and a delete removes what
// its tenant owns and nothing else.
func TestClientsStore_RawRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s := newSchedDB(t)
	h := newKeyStackOn(t, s, genKey(t))
	const tenantNS, indexNS = "m2m-clients:test-tenant", "m2m-client-ids"
	admin := h.token(t)

	t.Run("undecodable record", func(t *testing.T) {
		s.putRawKV(t, tenantNS, "BADREC1", "{")
		s.putRawKV(t, indexNS, "BADREC1", `{"tenantId":"test-tenant"}`)
		assertOAuthError(t, postTokenTo(t, h.baseURL, url.Values{"grant_type": {"client_credentials"}}, "BADREC1", "some-secret"),
			http.StatusInternalServerError, "server_error")
		ids := clientIDsOn(t, h.baseURL, admin)
		if ids["BADREC1"] || !ids[h.clientID] {
			t.Fatalf("GET /clients: undecodable listed %v, the keyStack's client listed %v; want false, true", ids["BADREC1"], ids[h.clientID])
		}
		resp := h.doAuthBearer(t, admin, http.MethodPut, "/api/clients/BADREC1/secret", "", "")
		if code, body := resp.StatusCode, h.readBody(t, resp); code != http.StatusInternalServerError {
			t.Fatalf("reset of an undecodable record: %d %s, want 500", code, withheld(code, []byte(body)))
		}
		if code, body := h.deleteClient(t, admin, "BADREC1"); code != http.StatusOK {
			t.Fatalf("delete of an undecodable record: %d %s, want 200", code, body)
		}
		if s.hasRawKV(t, tenantNS, "BADREC1") || s.hasRawKV(t, indexNS, "BADREC1") {
			t.Fatal("delete left the undecodable record or its index entry behind")
		}
		if code, body := h.deleteClient(t, admin, "BADREC1"); code != http.StatusNotFound {
			t.Fatalf("second delete: %d %s, want 404", code, body)
		}
	})

	t.Run("undecodable index entry", func(t *testing.T) {
		c := createKeyStackClient(t, h.callbackHarness)
		s.putRawKV(t, indexNS, c.id, "{")
		assertOAuthError(t, postTokenTo(t, h.baseURL, url.Values{"grant_type": {"client_credentials"}}, c.id, c.secret),
			http.StatusInternalServerError, "server_error")
	})

	t.Run("record without its index entry", func(t *testing.T) {
		c := createKeyStackClient(t, h.callbackHarness)
		s.deleteRawKV(t, indexNS, c.id)
		if code, body := h.deleteClient(t, admin, c.id); code != http.StatusOK {
			t.Fatalf("delete of a record without its index entry: %d %s, want 200", code, body)
		}
		if s.hasRawKV(t, tenantNS, c.id) {
			t.Fatal("delete left the record behind")
		}
	})

	t.Run("index entry naming another tenant", func(t *testing.T) {
		otherID, otherSecret := h.provisionTenant(t, "other-tenant", "other-admin")
		// A stray record under test-tenant with other-tenant's client id.
		s.putRawKV(t, tenantNS, otherID, "{")
		if code, body := h.deleteClient(t, admin, otherID); code != http.StatusOK {
			t.Fatalf("test-tenant's delete of its stray record: %d %s, want 200", code, body)
		}
		if s.hasRawKV(t, tenantNS, otherID) {
			t.Fatal("delete left test-tenant's stray record behind")
		}
		if !s.hasRawKV(t, indexNS, otherID) {
			t.Fatal("test-tenant's delete removed the index entry of other-tenant's client")
		}
		if code := tokenStatusOn(t, h.baseURL, otherID, otherSecret); code != http.StatusOK {
			t.Fatalf("other-tenant's client after test-tenant's delete: %d, want 200", code)
		}
	})
}

// TestClientsStore_NoPlaintextSecretStored reads every stored M2M value and
// looks for each secret the stack handed out: create, reset, and the
// keyStack's own client.
func TestClientsStore_NoPlaintextSecretStored(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	s := newSchedDB(t)
	h := newKeyStackOn(t, s, genKey(t))
	c := createKeyStackClient(t, h.callbackHarness)
	reset := createKeyStackClient(t, h.callbackHarness)
	secrets := []string{h.clientSecret, c.secret, reset.secret, h.resetSecret(t, reset.id)}

	rows, err := s.pool.Query(context.Background(), `SELECT namespace, key, value FROM kv_store WHERE namespace LIKE 'm2m-%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ns, key string
		var value []byte
		if err := rows.Scan(&ns, &key, &value); err != nil {
			t.Fatal(err)
		}
		n++
		for i, sec := range secrets {
			if bytes.Contains(value, []byte(sec)) {
				t.Errorf("stored value %s/%s holds secret #%d in plaintext (value withheld)", ns, key, i)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 6 { // three clients: a record and an index entry each
		t.Fatalf("read %d stored m2m values, want at least 6", n)
	}
}

// TestClientsStore_ClientChangeNotInCallersTransaction creates a client
// through POST /clients joined to an open entity transaction T: the client is
// stored at once, outside T, so another node authenticates it before T
// commits.
func TestClientsStore_ClientChangeNotInCallersTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	a, b := twoNodes(t)
	a.member = a.AttachCnode(t, cnodeSpec{name: "default", tags: []string{"sched-fn"}, script: a.registeredScript}).m

	tokenCh := make(chan string, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseT := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseT) // never leave T, and its processor, hanging
	a.RegisterProc("m2m-hold", func(rc *reqCtx) (map[string]any, error) {
		tokenCh <- rc.token // T's pass; never logged
		<-release
		return nil, nil
	})
	const model = "m2m-txhold"
	a.SetupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "m2m-txhold-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "m2m-hold", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`)

	type createRes struct {
		status int
		body   string
	}
	done := make(chan createRes, 1)
	go func() {
		res, err := a.callback(http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), `{"name":"parent","amount":1,"status":"new"}`, "")
		if err != nil {
			done <- createRes{status: -1, body: err.Error()}
			return
		}
		done <- createRes{status: res.StatusCode, body: res.Body}
	}()
	var pass string
	select {
	case pass = <-tokenCh:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: the hold processor did not run")
	}

	// T is open. Create a client in it, then authenticate it on B.
	resp := a.DoAuth(t, http.MethodPost, "/api/clients", "", pass)
	raw := a.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /clients joined to T: %d %s", resp.StatusCode, raw)
	}
	c := decodeCredential(t, "POST /clients joined to T", []byte(raw))
	if code := tokenStatusOn(t, b.baseURL, c.id, c.secret); code != http.StatusOK {
		t.Fatalf("token on B for a client created in the still-open T: %d, want 200", code)
	}

	releaseT()
	select {
	case cr := <-done:
		if cr.status != http.StatusOK {
			t.Fatalf("entity create after release: %d %s", cr.status, cr.body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: the entity create did not complete after release")
	}
}
