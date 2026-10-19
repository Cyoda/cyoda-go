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
	// An operation that does not declare the parameter ignores it, as it
	// ignores any query parameter it does not declare.
	resp, err := http.Get(srv.URL + "/model/?pointInTime=2024-01-02")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if body := readBody(t, resp); resp.StatusCode == http.StatusBadRequest {
		t.Errorf("an undeclared pointInTime was checked: 400 %s", body)
	}
}
