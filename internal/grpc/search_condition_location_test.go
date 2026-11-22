package grpc

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A refusal inside a search event's condition names its location from the
// top of the event, /condition included, as every other decode error does.
func TestSearch_ConditionRefusalNamesItsLocation(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})
	data := map[string]any{
		"id":        "test",
		"model":     map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{"type": "simple", "JsonPath": "$.name", "operatorType": "EQUALS", "value": "x"},
	}
	const want = `invalid JSON at "/condition": unknown member name "JsonPath"`

	_, err := svc.EntitySearch(ctx, makeCE(EntitySnapshotSearchRequest, data))
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.InvalidArgument || !strings.Contains(st.Message(), want) {
		t.Errorf("snapshot search: %v; want InvalidArgument containing %s", err, want)
	}
	err = svc.EntitySearchCollection(makeCE(EntitySearchRequest, data), &mockEntityStream{ctx: ctx})
	if st, _ := status.FromError(err); err == nil || st.Code() != codes.InvalidArgument || !strings.Contains(st.Message(), want) {
		t.Errorf("direct search: %v; want InvalidArgument containing %s", err, want)
	}
}
