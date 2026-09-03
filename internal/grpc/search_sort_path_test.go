package grpc

import (
	"strings"
	"testing"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// A sort path is held to the scalar path grammar on EVERY entry point, not
// just the one that parses a query string.
//
// The HTTP surface builds its OrderKeys through a parser that refuses "["
// (search.ParseSortParam); gRPC builds one from the client's path verbatim, so
// only the shared resolver can make the two agree. "$.items[*].name" is the
// spelling that made the disagreement matter: it IS a recorded field — a
// scalar leaf inside an array of objects, keyed under the wildcard with
// IsArray false — so schema membership and the array guard both admitted it.
// What followed depended on which branch served the request: the pushdown
// branch was refused by the plugin's own path validator, while the in-memory
// branch handed the path to gjson, which has no bracket syntax, so every
// entity missed, all compared equal, and the caller got 200 with results that
// were simply not sorted.
var grpcNonScalarSortPaths = []struct {
	name string
	path string
}{
	{"array projection", "items[*].name"},
	{"array projection with leader", "$.items[*].name"},
	{"positional subscript", "items[0].name"},
	{"disallowed character", "sur name"},
	{"sql tail", "surname';DROP"},
}

// sortPathModel is an array of objects, so "$.items[*].name" resolves to a
// recorded FieldDescriptor — without it the request would be refused as an
// unknown field and prove nothing about the grammar.
var sortPathModel = map[string]any{
	"surname": "Smith",
	"items":   []any{map[string]any{"name": "widget"}},
}

// searchConditions are two condition shapes a direct search can carry,
// probing sort-path validation against both a trivial and a
// subscript-carrying condition.
//
// A well-formed array-subscripted condition ("$.items[*].name EQUALS
// "widget"") used to be the one shape spi.ConditionToFilter declined to
// translate, routing the WHOLE request — sort included — through an
// in-process evaluator where an unchecked sort path stopped being an error
// and became a wrong answer: gjson has no bracket syntax, so every entity
// missed, all compared equal, and the caller received a 200 whose order was
// not the one they asked for.
//
// The kernel resolves a subscripted path directly now (see spi.ResolvePath),
// so this condition translates and pushes down exactly like the trivial one,
// and a condition that cannot be translated is refused rather than served
// another way (see TestValidateCondition_ClearingImpliesTranslates in
// internal/domain/search). Both rows below therefore exercise the same code
// path; they are kept as two rows because sort-path validation must hold for
// either condition shape, not because they still probe different mechanisms.
var searchConditions = []struct {
	name string
	cond map[string]any
}{
	{"trivial condition", map[string]any{"type": "group", "operator": "AND", "conditions": []any{}}},
	{"subscript-carrying condition", map[string]any{
		"type": "simple", "jsonPath": "$.items[*].name",
		"operatorType": "EQUALS", "value": "widget",
	}},
}

// TestEntitySearch_DirectSearch_NonScalarSortPath_InvalidFieldPath pins the
// envelope on the streaming (direct) search door, on both branches.
func TestEntitySearch_DirectSearch_NonScalarSortPath_InvalidFieldPath(t *testing.T) {
	for _, sc := range searchConditions {
		t.Run(sc.name, func(t *testing.T) {
			for _, tc := range grpcNonScalarSortPaths {
				t.Run(tc.name, func(t *testing.T) {
					svc, ctx := newTestEnv(t)
					importAndLockModel(t, svc, ctx, "sortitem", "1", sortPathModel)

					ce := makeCE(EntitySearchRequest, map[string]any{
						"id":        "sort-badpath",
						"model":     map[string]any{"name": "sortitem", "version": 1},
						"condition": sc.cond,
						"orderBy":   []any{map[string]any{"path": tc.path}},
					})
					stream := &mockEntityStream{ctx: ctx}
					if err := svc.EntitySearchCollection(ce, stream); err != nil {
						t.Fatalf("unexpected stream error (errors should be envelope responses): %v", err)
					}
					if len(stream.sent) == 0 {
						t.Fatal("expected an error response on the stream")
					}
					var typed events.EntityResponseJson
					validateResponse(t, stream.sent[0], &typed)
					if typed.Success {
						t.Fatalf("sort path %q was accepted; it cannot denote the single scalar an ordering needs, and this branch would answer an unsorted page rather than refuse", tc.path)
					}
					if typed.Error == nil {
						t.Fatal("expected error block in response")
					}
					if typed.Error.Code != "CLIENT_ERROR" {
						t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
					}
					if !strings.Contains(typed.Error.Message, "INVALID_FIELD_PATH") {
						t.Errorf("expected INVALID_FIELD_PATH in message, got %q", typed.Error.Message)
					}
				})
			}
		})
	}
}

// TestEntitySearch_SnapshotSearch_NonScalarSortPath_InvalidFieldPath is the
// same check on the async submit door. It matters more here than on the direct
// one: an async job that starts and then fails reports through the job record,
// where a client's own malformed input would read as a server-side failure.
// Refusing at submit keeps it a 400.
func TestEntitySearch_SnapshotSearch_NonScalarSortPath_InvalidFieldPath(t *testing.T) {
	for _, tc := range grpcNonScalarSortPaths {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctx := newTestEnv(t)
			importAndLockModel(t, svc, ctx, "sortitem", "1", sortPathModel)

			resp, err := svc.EntitySearch(ctx, makeCE(EntitySnapshotSearchRequest, map[string]any{
				"id":    "sort-badpath-async",
				"model": map[string]any{"name": "sortitem", "version": 1},
				"condition": map[string]any{
					"type": "group", "operator": "AND", "conditions": []any{},
				},
				"orderBy": []any{map[string]any{"path": tc.path}},
			}))
			if err != nil {
				t.Fatalf("unexpected transport error (errors should be envelope responses): %v", err)
			}
			var typed events.EntitySnapshotSearchResponseJson
			validateResponse(t, resp, &typed)
			if typed.Success {
				t.Fatalf("sort path %q was accepted at submit", tc.path)
			}
			if typed.Error == nil {
				t.Fatal("expected error block in response")
			}
			if typed.Error.Code != "CLIENT_ERROR" {
				t.Errorf("expected code=CLIENT_ERROR, got %q", typed.Error.Code)
			}
			if !strings.Contains(typed.Error.Message, "INVALID_FIELD_PATH") {
				t.Errorf("expected INVALID_FIELD_PATH in message, got %q", typed.Error.Message)
			}
		})
	}
}
