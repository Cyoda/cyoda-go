package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	internalapi "github.com/cyoda-platform/cyoda-go/internal/api"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/scheduledtask"
)

type routeStore struct {
	spi.ScheduledTaskStore
	calls int
	query spi.ScheduledTaskQuery
}

func (s *routeStore) Query(_ context.Context, _ spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	s.calls++
	s.query = q
	return spi.ScheduledTaskPage{}, nil
}

type routeFactory struct {
	spi.StoreFactory
	store spi.ScheduledTaskStore
}

func (f routeFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return f.store, nil
}

// TestListScheduledTasks_RoutedAndBound: the generated router reaches the
// handler through Server, binds repeated and typed parameters, and answers the
// binder's own refusals with 400 BAD_REQUEST without echoing the value.
func TestListScheduledTasks_RoutedAndBound(t *testing.T) {
	st := &routeStore{}
	s := internalapi.NewServer()
	s.ScheduledTasks = scheduledtask.NewHandler(routeFactory{store: st})
	h := genapi.HandlerWithOptions(s, genapi.StdHTTPServerOptions{
		BaseRouter:       internalapi.NewChiMux(),
		ErrorHandlerFunc: internalapi.BindingErrorHandler,
	})
	serve := func(target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r = r.WithContext(spi.WithUserContext(r.Context(),
			&spi.UserContext{UserID: "u1", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: "tenant-a"}, Roles: []string{"ROLE_M2M"}}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := serve("/scheduled-tasks?status=WAITING&status=FAILED&modelName=orders&modelVersion=2&limit=5")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	want := spi.ScheduledTaskQuery{
		Statuses:     []spi.ScheduledTaskStatus{spi.ScheduledTaskWaiting, spi.ScheduledTaskFailed},
		ModelName:    "orders",
		ModelVersion: 2,
		Limit:        5,
	}
	if !reflect.DeepEqual(st.query, want) {
		t.Errorf("query = %+v, want %+v", st.query, want)
	}

	for name, tc := range map[string]struct{ target, value string }{
		"limit not an integer":        {"/scheduled-tasks?limit=abc", "abc"},
		"limit empty":                 {"/scheduled-tasks?limit=", ""},
		"limit decimal":               {"/scheduled-tasks?limit=2.5", "2.5"},
		"limit repeated":              {"/scheduled-tasks?limit=1&limit=2", ""},
		"modelVersion not an integer": {"/scheduled-tasks?modelName=m&modelVersion=xyz", "xyz"},
		"entityId not a UUID":         {"/scheduled-tasks?entityId=not-a-uuid", "not-a-uuid"},
	} {
		t.Run(name, func(t *testing.T) {
			before := st.calls
			w := serve(tc.target)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), "BAD_REQUEST")
			if tc.value != "" && strings.Contains(w.Body.String(), tc.value) {
				t.Errorf("the 400 echoes %q: %s", tc.value, w.Body.String())
			}
			if st.calls != before {
				t.Errorf("the store was queried for a request the binder refused")
			}
		})
	}
}
