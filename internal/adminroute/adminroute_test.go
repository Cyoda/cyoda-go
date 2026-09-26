package adminroute

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsAdministration(t *testing.T) {
	for _, tc := range []struct {
		method, path, contextPath string
		want                      bool
	}{
		// every administration route, below and above the context path
		{http.MethodPost, "/model/import/JSON/SAMPLE_DATA/order/1", "", true},
		{http.MethodDelete, "/model/order/1", "", true},
		{http.MethodPost, "/model/order/1/changeLevel/STRUCTURAL", "", true},
		{http.MethodPut, "/model/order/1/lock", "", true},
		{http.MethodPut, "/model/order/1/unlock", "", true},
		{http.MethodPut, "/model/order/1/unique-keys", "", true},
		{http.MethodPost, "/model/order/1/workflow/import", "", true},
		{http.MethodPost, "/api/model/import/JSON/SAMPLE_DATA/order/1", "/api", true},
		{http.MethodDelete, "/api/model/order/1", "/api", true},
		{http.MethodPost, "/api/model/order/1/changeLevel/STRUCTURAL", "/api", true},
		{http.MethodPut, "/api/model/order/1/lock", "/api", true},
		{http.MethodPut, "/api/model/order/1/unlock", "/api", true},
		{http.MethodPut, "/api/model/order/1/unique-keys", "/api", true},
		{http.MethodPost, "/api/model/order/1/workflow/import", "/api", true},
		// the context path is applied, not optional, at the layer that carries it
		{http.MethodPut, "/model/order/1/lock", "/api", false},
		{http.MethodPut, "/other/model/order/1/lock", "/api", false},
		{http.MethodPut, "/apix/model/order/1/lock", "/api", false},
		{http.MethodPut, "/api/model/order/1/lock", "", false},
		// read-only model and workflow routes
		{http.MethodGet, "/model/", "", false},
		{http.MethodGet, "/model/export/SIMPLE_VIEW/order/1", "", false},
		{http.MethodPost, "/model/validate/order/1", "", false},
		{http.MethodGet, "/model/order/1/workflow/export", "", false},
		// a method no route registers, and entity routes
		{http.MethodGet, "/model/order/1/lock", "", false},
		{http.MethodPost, "/entity/JSON/order/1", "", false},
		// a non-canonical path is redirected above the router, never routed
		{http.MethodPut, "/model//1/lock", "", false},
		{http.MethodPut, "/api/model//1/lock", "/api", false},
		{http.MethodPut, "/model/order/1/../1/lock", "", false},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if got := IsAdministration(r, tc.contextPath); got != tc.want {
			t.Errorf("IsAdministration(%s %s, %q) = %v, want %v", tc.method, tc.path, tc.contextPath, got, tc.want)
		}
	}
}
