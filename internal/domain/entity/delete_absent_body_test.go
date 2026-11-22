package entity

import (
	"errors"
	"testing"
)

// A conditional delete with no condition deletes the whole model, so "no
// condition" means an empty body or JSON whitespace only. A body of other
// white space — a vertical tab, a no-break space — is not JSON: it is an
// invalid condition and nothing is deleted.
func TestDeleteEntitiesConditional_NonJSONWhitespaceIsNotAbsent(t *testing.T) {
	factory, ctx, ref := newDeleteStreamFixture(t)
	h := buildDeleteStreamHandler(t, factory, mustTxMgr(t, factory))
	seedKind(t, h, ctx, ref, 3, "keep")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"\v", "\u00a0", "\f"} {
		if _, err := h.DeleteEntitiesConditional(ctx, ref.EntityName, 1, []byte(body), nil, false, 0); !errors.Is(err, ErrInvalidCondition) {
			t.Errorf("%q: err = %v, want ErrInvalidCondition", body, err)
		}
		if n, err := store.Count(ctx, ref, nil); err != nil || n != 3 {
			t.Fatalf("%q: %d entities left (err %v), want all 3", body, n, err)
		}
	}
}
