package entity_test

import (
	"net/http"
	"testing"
)

// A date-time query parameter is read as a body's date-time member is, on the
// generated routes and on the transitions route alike: a comma before the
// fractional seconds or a bare date is refused, as in a body.
func TestDateTimeQueryParams_ReadAsBodiesAre(t *testing.T) {
	_, srv := newTestAppAndServer(t)
	for _, route := range []string{
		"/entity/some-id/transitions?pointInTime=",
		"/entity/00000000-0000-0000-0000-000000000001?pointInTime=",
		"/entity/stats?pointInTime=",
		"/audit/entity/00000000-0000-0000-0000-000000000001?fromUtcTime=",
	} {
		for _, v := range []string{"2024-01-02T03:04:05,5Z", "2024-01-02"} {
			resp, err := http.Get(srv.URL + route + v)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest || !contains(string(body), "BAD_REQUEST") {
				t.Errorf("%s%s: status %d body %s, want 400 BAD_REQUEST", route, v, resp.StatusCode, body)
			}
		}
		resp, err := http.Get(srv.URL + route + "2024-01-02T03:04:05.5Z")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode == http.StatusBadRequest {
			t.Errorf("%s with a valid date-time: 400 %s", route, body)
		}
	}
	// On a generated route an empty value is refused too (the binding read it
	// as the year 0001), and so is a bad value repeated after a good one.
	for _, q := range []string{"pointInTime=", "pointInTime=2024-01-02T03:04:05Z&pointInTime=2024-01-02"} {
		resp, err := http.Get(srv.URL + "/entity/stats?" + q)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if body := readBody(t, resp); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s: status %d body %s, want 400", q, resp.StatusCode, body)
		}
	}
}

// Every operation refuses a query parameter it does not declare, on the
// generated and the hand-written routes alike, so a misspelt parameter is never
// silently dropped. On delete-by-model a dropped pointInTime turned "delete what
// existed then" into "delete everything now".
func TestUndeclaredQueryParams_Refused(t *testing.T) {
	_, srv := newTestAppAndServer(t)
	id := createEntityViaAPI(t, srv.URL, "QueryDel", 1, `{"n":1}`)

	do := func(method, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		return resp.StatusCode, string(readBody(t, resp))
	}
	// A query that does not parse is refused whole: url.Values would drop the
	// pair it cannot read, and a dropped pointInTime deletes everything now.
	for _, q := range []string{
		"transactionSize=100;pointInTime=2024-01-02T03:04:05Z",
		"pointInTime=2024-01-02T03:04:05%Z",
		"PointInTime%=2024",
	} {
		status, body := do(http.MethodDelete, "/entity/QueryDel/1?"+q)
		if status != http.StatusBadRequest || !contains(body, "malformed query string") {
			t.Errorf("DELETE ?%s: status %d body %s, want 400 malformed query string", q, status, body)
		}
	}
	// A declared parameter that is not an array takes one value: the binding
	// would read the first and drop the rest.
	if status, body := do(http.MethodDelete, "/entity/QueryDel/1?pointInTime=2024-01-02T03:04:05Z&pointInTime=2099-01-02T03:04:05Z"); status != http.StatusBadRequest || !contains(body, "given more than once") {
		t.Errorf("repeated pointInTime: status %d body %s, want 400 given more than once", status, body)
	}
	// An array parameter may repeat.
	if status, body := do(http.MethodGet, "/entity/stats/states?states=A&states=B"); contains(body, "given more than once") {
		t.Errorf("repeated array parameter: %d %s, refused", status, body)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, "/entity/QueryDel/1?PointInTime=2024-01-02T03:04:05Z"},
		{http.MethodGet, "/entity/stats?pointintime=2024-01-02T03:04:05Z"},
		{http.MethodGet, "/model/?pointInTime=2024-01-02"},
		{http.MethodGet, "/entity/" + id + "/transitions?pointintime=2024-01-02T03:04:05Z"},
		{http.MethodGet, "/platform-api/entity/fetch/transitions?entityClass=QueryDel.1&entityId=" + id + "&x=1"},
		{http.MethodPost, "/entity/stats/QueryDel/1/query?x=1"},
	} {
		status, body := do(c.method, c.path)
		if status != http.StatusBadRequest || !contains(body, "unknown query parameter") {
			t.Errorf("%s %s: status %d body %s, want 400 unknown query parameter", c.method, c.path, status, body)
		}
	}
	if status, body := do(http.MethodGet, "/entity/"+id); status != http.StatusOK {
		t.Fatalf("the entity is gone after a refused delete: %d %s", status, body)
	}
	// Declared parameters are read as before.
	for _, path := range []string{
		"/entity/stats?pointInTime=2024-01-02T03:04:05Z",
		"/entity/" + id + "/transitions?pointInTime=2099-01-02T03:04:05Z",
		"/platform-api/entity/fetch/transitions?entityClass=QueryDel.1&entityId=" + id,
	} {
		if status, body := do(http.MethodGet, path); contains(body, "unknown query parameter") {
			t.Errorf("GET %s: %d %s, a declared parameter was refused", path, status, body)
		}
	}
}
