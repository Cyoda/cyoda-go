package sqlite_test

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestGetVersionMetadata_EmptyWindow_ReturnsEmptyNotNotFound verifies that
// an entity which exists but has no versions inside the requested
// From/Until window returns an empty slice with a nil error — matching the
// memory plugin (plugins/memory/entity_store.go's GetVersionMetadata checks
// existence BEFORE applying the window filter). Before this fix, sqlite
// applied the window in SQL and treated ANY empty result as ErrNotFound,
// which wrongly made an existing entity look missing whenever the caller's
// window happened not to cover it.
func TestGetVersionMetadata_EmptyWindow_ReturnsEmptyNotNotFound(t *testing.T) {
	factory, _ := newAttrFactory(t)
	ctx := attrCtx("tenant-pgdel", "u1", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	id := "e-window-1"
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: id, TenantID: "tenant-pgdel",
			ModelRef: spi.ModelRef{EntityName: "m-window", ModelVersion: "1"}},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	future := time.Now().Add(24 * time.Hour)
	metas, err := store.GetVersionMetadata(ctx, id, spi.VersionMetadataOptions{From: &future})
	if err != nil {
		t.Fatalf("GetVersionMetadata with a future From must not error, got: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("GetVersionMetadata with a future From = %v, want empty", metas)
	}
}

// TestGetVersionMetadata_NeverExisted_StillNotFound is the companion
// regression check: an entity that never had ANY version row must still
// return ErrNotFound, distinguishing "genuinely missing" from "exists but
// outside the window" (the case the fix above addresses).
func TestGetVersionMetadata_NeverExisted_StillNotFound(t *testing.T) {
	factory, _ := newAttrFactory(t)
	ctx := attrCtx("tenant-pgdel", "u1", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	_, err = store.GetVersionMetadata(ctx, "never-existed", spi.VersionMetadataOptions{})
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("GetVersionMetadata for a never-existed entity = %v, want ErrNotFound", err)
	}
}
