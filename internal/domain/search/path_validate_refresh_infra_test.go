package search

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// failingRefreshStore is a minimal spi.ModelStore fake whose Get always
// serves a warm (but stale) cached descriptor and whose RefreshAndGet always
// fails with a caller-supplied error — modelling a genuine model-store
// outage discovered while trying to confirm a possibly-stale path miss.
type failingRefreshStore struct {
	warm    *spi.ModelDescriptor
	failErr error
}

func (s *failingRefreshStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return s.warm, nil
}

func (s *failingRefreshStore) RefreshAndGet(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return nil, s.failErr
}

func (s *failingRefreshStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *failingRefreshStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *failingRefreshStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *failingRefreshStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *failingRefreshStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *failingRefreshStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *failingRefreshStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *failingRefreshStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*failingRefreshStore)(nil)

// TestValidateKnownPaths_FailedRefreshIsInfraNotClientFault pins the fix for
// review round 1's Important-1 finding: a failed schema refresh cannot tell
// "this field is genuinely undeclared" from "the cache is merely stale", so
// per correctness-over-availability it must be reported as infrastructure —
// distinct from the genuine-unknown-path 400 INVALID_FIELD_PATH case — and
// with the underlying failure still reachable via errors.Is/errors.As (a
// context.DeadlineExceeded riding inside the refresh failure must still be
// detectable by a caller further up the chain).
func TestValidateKnownPaths_FailedRefreshIsInfraNotClientFault(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	node := schema.NewObjectNode()
	node.SetChild("a", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	warm := &spi.ModelDescriptor{Ref: ref, Schema: raw}

	wantCause := errors.New("connection refused")
	store := &failingRefreshStore{warm: warm, failErr: wantCause}

	fields, ffErr := loadFieldsMap(context.Background(), store, ref)
	if ffErr != nil {
		t.Fatalf("loadFieldsMap: %v", ffErr)
	}

	_, err = ValidateKnownPaths(context.Background(), store, ref, []string{"$.peer_field"}, fields)
	if err == nil {
		t.Fatal("expected an error: the refresh failed, so the path's status is unknown")
	}
	if !errors.Is(err, ErrPathRefreshInfra) {
		t.Errorf("expected errors.Is(err, ErrPathRefreshInfra), got: %v", err)
	}
	if !errors.Is(err, wantCause) {
		t.Errorf("expected the underlying refresh error to still be reachable via errors.Is, got: %v", err)
	}
	var appErr *common.AppError
	if errors.As(err, &appErr) {
		t.Errorf("expected a plain error (callers classify it themselves), got a pre-classified *common.AppError: %+v", appErr)
	}
}
