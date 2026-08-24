package entity_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// noWholeModelEntityStore wraps a real spi.EntityStore and fails any call to
// GetAll/GetAllAsAt — the whole-model-materialization methods ListEntities
// used before it was rewired onto GetPage (task E5). Every other method,
// including GetPage, is forwarded unchanged via the embedded interface.
// A test built on this store can only pass if ListEntities genuinely never
// reaches GetAll/GetAllAsAt anymore, not merely if GetPage happens to be
// called too.
type noWholeModelEntityStore struct {
	spi.EntityStore
	calledGetAll     bool
	calledGetAllAsAt bool
}

func (s *noWholeModelEntityStore) GetAll(_ context.Context, _ spi.ModelRef) ([]*spi.Entity, error) {
	s.calledGetAll = true
	return nil, fmt.Errorf("GetAll must not be called by ListEntities anymore")
}

func (s *noWholeModelEntityStore) GetAllAsAt(_ context.Context, _ spi.ModelRef, _ time.Time) ([]*spi.Entity, error) {
	s.calledGetAllAsAt = true
	return nil, fmt.Errorf("GetAllAsAt must not be called by ListEntities anymore")
}

// noWholeModelStoreFactory wraps a real spi.StoreFactory, substituting a
// noWholeModelEntityStore for EntityStore() while forwarding every other
// accessor (ModelStore, etc.) via the embedded interface.
type noWholeModelStoreFactory struct {
	spi.StoreFactory
	store *noWholeModelEntityStore
}

func (f *noWholeModelStoreFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

// newListEntitiesFixture registers modelRef and saves n entities with IDs
// "id-00".."id-{n-1}" (zero-padded so byte-wise sort matches numeric order),
// via a noWholeModelStoreFactory so any accidental GetAll/GetAllAsAt call is
// caught immediately as a hard error instead of silently passing.
func newListEntitiesFixture(t *testing.T, tenantID spi.TenantID, ref spi.ModelRef, n int) (context.Context, *entity.Handler, *noWholeModelEntityStore) {
	t.Helper()
	base := memory.NewStoreFactory()
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "list-user",
		Tenant: spi.Tenant{ID: tenantID, Name: string(tenantID)},
		Roles:  []string{"USER"},
	})

	mstore, err := base.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := mstore.Save(ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}

	realStore, err := base.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%02d", i)
		if _, err := realStore.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{
				ID:       id,
				TenantID: tenantID,
				ModelRef: ref,
				State:    "NEW",
			},
			Data: []byte(`{}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	wrapped := &noWholeModelEntityStore{EntityStore: realStore}
	factory := &noWholeModelStoreFactory{StoreFactory: base, store: wrapped}
	h := entity.New(factory, nil, common.NewDefaultUUIDGenerator(), nil, txgate.New())
	return ctx, h, wrapped
}

// TestListEntities_PagesViaGetPage asserts ListEntities pages at the store
// via GetPage rather than materialising the whole model: page number 1 with
// page size 3 over 10 entities returns exactly ids "id-03","id-04","id-05"
// (byte-wise ID order), and GetAll/GetAllAsAt are never reached.
func TestListEntities_PagesViaGetPage(t *testing.T) {
	ref := spi.ModelRef{EntityName: "list-getpage-model", ModelVersion: "1"}
	ctx, h, wrapped := newListEntitiesFixture(t, "tenant-list-page", ref, 10)

	envs, err := h.ListEntities(ctx, ref.EntityName, ref.ModelVersion, entity.PaginationParams{PageSize: 3, PageNumber: 1}, nil)
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if wrapped.calledGetAll {
		t.Error("ListEntities called GetAll — whole-model materialisation not removed")
	}
	want := []string{"id-03", "id-04", "id-05"}
	if len(envs) != len(want) {
		t.Fatalf("got %d envelopes, want %d (%v)", len(envs), len(want), envs)
	}
	for i, env := range envs {
		got, _ := env.Meta["id"].(string)
		if got != want[i] {
			t.Errorf("envelope[%d].id = %q, want %q", i, got, want[i])
		}
	}
}

// TestListEntities_PagePastEnd_ReturnsEmpty asserts a page entirely past the
// end of the result set is an empty page, not an error.
func TestListEntities_PagePastEnd_ReturnsEmpty(t *testing.T) {
	ref := spi.ModelRef{EntityName: "list-getpage-pastend-model", ModelVersion: "1"}
	ctx, h, wrapped := newListEntitiesFixture(t, "tenant-list-pastend", ref, 10)

	envs, err := h.ListEntities(ctx, ref.EntityName, ref.ModelVersion, entity.PaginationParams{PageSize: 3, PageNumber: 10}, nil)
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if wrapped.calledGetAll {
		t.Error("ListEntities called GetAll — whole-model materialisation not removed")
	}
	if len(envs) != 0 {
		t.Errorf("got %d envelopes for a page past the end, want 0 (%v)", len(envs), envs)
	}
}

// TestListEntities_ZeroPageSize_ReturnsEmptyWithoutStoreCall asserts that a
// pageSize of 0 — a value pagination.ValidateOffset accepts (it rejects only
// negative sizes) — short-circuits to an empty page without reaching
// GetPage, whose contract requires limit >= 1.
func TestListEntities_ZeroPageSize_ReturnsEmptyWithoutStoreCall(t *testing.T) {
	ref := spi.ModelRef{EntityName: "list-getpage-zerosize-model", ModelVersion: "1"}
	ctx, h, _ := newListEntitiesFixture(t, "tenant-list-zerosize", ref, 5)

	envs, err := h.ListEntities(ctx, ref.EntityName, ref.ModelVersion, entity.PaginationParams{PageSize: 0, PageNumber: 0}, nil)
	if err != nil {
		t.Fatalf("ListEntities(pageSize=0): %v", err)
	}
	if len(envs) != 0 {
		t.Errorf("got %d envelopes for pageSize=0, want 0 (%v)", len(envs), envs)
	}
}

// TestListEntities_PointInTime_UsesGetPageAsAt asserts the pointInTime
// variant pages via GetPage's asAt branch (never GetAllAsAt) and still
// stamps meta.pointInTime on every envelope.
func TestListEntities_PointInTime_UsesGetPageAsAt(t *testing.T) {
	ref := spi.ModelRef{EntityName: "list-getpage-asat-model", ModelVersion: "1"}
	ctx, h, wrapped := newListEntitiesFixture(t, "tenant-list-asat", ref, 4)

	asAt := time.Now().UTC()

	envs, err := h.ListEntities(ctx, ref.EntityName, ref.ModelVersion, entity.PaginationParams{PageSize: 10, PageNumber: 0}, &asAt)
	if err != nil {
		t.Fatalf("ListEntities(pointInTime): %v", err)
	}
	if wrapped.calledGetAllAsAt {
		t.Error("ListEntities called GetAllAsAt — whole-model materialisation not removed")
	}
	if len(envs) != 4 {
		t.Fatalf("got %d envelopes, want 4 (%v)", len(envs), envs)
	}
	for _, env := range envs {
		if env.Meta["pointInTime"] == nil {
			t.Errorf("envelope %v missing meta.pointInTime", env)
		}
	}
}
