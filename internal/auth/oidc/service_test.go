package oidc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"
)

// newTestService builds a Service over an in-memory store with a fake (nop)
// discovery so tests stay offline.
func newTestService(t *testing.T) *Service {
	t.Helper()
	store := newTestStore(t)
	r := NewRegistry(store, &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}, nil, NopMetrics{}, nil)
	return NewService(store, r, nil)
}

// newTestServiceWithLogger returns a Service whose slog output is captured.
func newTestServiceWithLogger(t *testing.T) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newTestStore(t)
	r := NewRegistry(store, &fakeDiscovery{docs: map[string]*DiscoveryDoc{}}, nil, NopMetrics{}, logger)
	return NewService(store, r, logger), &buf
}

const (
	tenantA = spi.TenantID("00000000-0000-0000-0000-00000000000a")
	tenantB = spi.TenantID("00000000-0000-0000-0000-00000000000b")
)

const sampleURI = "https://idp.example/.well-known/openid-configuration"

func defaultInput(tenant spi.TenantID) RegisterInput {
	return RegisterInput{
		TenantID:           tenant,
		WellKnownConfigURI: sampleURI,
		Issuers:            []string{"https://idp.example"},
		ExpectedAudiences:  []string{"myapp"},
		OwnerLegalEntityID: uuid.MustParse(string(tenant)),
	}
}

// ---------- Register ----------

func TestService_Register_Success(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if p.ID == uuid.Nil {
		t.Error("Register did not assign ID")
	}
	if !p.CreatedAt.After(time.Now().Add(-time.Minute)) {
		t.Error("CreatedAt not set or too far in the past")
	}
	if p.Active() == false {
		t.Error("newly registered provider should be active")
	}
	if p.WellKnownConfigURI != sampleURI {
		t.Errorf("WellKnownConfigURI = %q, want %q", p.WellKnownConfigURI, sampleURI)
	}
}

func TestService_Register_Duplicate(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if _, err := s.Register(ctx, defaultInput(tenantA)); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	_, err := s.Register(ctx, defaultInput(tenantA))
	if !errors.Is(err, ErrProviderDuplicate) {
		t.Errorf("second Register = %v, want ErrProviderDuplicate", err)
	}
}

func TestService_Register_DifferentTenantsShareURI(t *testing.T) {
	// Two tenants may register the same URI independently — no conflict.
	s := newTestService(t)
	ctx := context.Background()

	inA := defaultInput(tenantA)
	inB := RegisterInput{
		TenantID:           tenantB,
		WellKnownConfigURI: sampleURI,
		Issuers:            []string{"https://idp.example"},
		OwnerLegalEntityID: uuid.MustParse(string(tenantB)),
	}

	if _, err := s.Register(ctx, inA); err != nil {
		t.Fatalf("Register tenantA: %v", err)
	}
	pB, err := s.Register(ctx, inB)
	if err != nil {
		t.Fatalf("Register tenantB: %v", err)
	}
	if pB == nil || pB.ID == uuid.Nil {
		t.Error("tenantB registration failed")
	}
}

func TestService_Register_EmitsOwnershipTransitionLog(t *testing.T) {
	// D25: register URI under tenantA, delete it, then register under tenantB.
	// The second Register must log oidc.cross_tenant_uri_registration.
	s, buf := newTestServiceWithLogger(t)
	ctx := context.Background()

	// Register and then delete under tenantA.
	pA, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register tenantA: %v", err)
	}
	if err := s.Delete(ctx, tenantA, pA.ID.String()); err != nil {
		t.Fatalf("Delete tenantA: %v", err)
	}

	// Register the same URI under tenantB.
	inB := RegisterInput{
		TenantID:           tenantB,
		WellKnownConfigURI: sampleURI,
		Issuers:            []string{"https://idp.example"},
		OwnerLegalEntityID: uuid.MustParse(string(tenantB)),
	}
	if _, err := s.Register(ctx, inB); err != nil {
		t.Fatalf("Register tenantB: %v", err)
	}

	// Verify the cross-tenant log was emitted.
	if !strings.Contains(buf.String(), "oidc.cross_tenant_uri_registration") {
		t.Errorf("expected cross_tenant_uri_registration log, got:\n%s", buf.String())
	}
}

func TestService_Register_SetsCreatedAt(t *testing.T) {
	s := newTestService(t)
	// Override clock to a known value.
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.clock = func() time.Time { return fixed }

	p, err := s.Register(context.Background(), defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !p.CreatedAt.Equal(fixed) {
		t.Errorf("CreatedAt = %v, want %v", p.CreatedAt, fixed)
	}
}

// ---------- Update ----------

func TestService_Update_Success(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	updated, err := s.Update(ctx, UpdateInput{
		TenantID:          tenantA,
		ProviderID:        p.ID.String(),
		UpdateIssuers:     true,
		Issuers:           []string{"https://updated-idp.example"},
		UpdateAudiences:   true,
		ExpectedAudiences: []string{"aud1", "aud2"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.Issuers) != 1 || updated.Issuers[0] != "https://updated-idp.example" {
		t.Errorf("Issuers not updated: %v", updated.Issuers)
	}
	if len(updated.ExpectedAudiences) != 2 {
		t.Errorf("ExpectedAudiences not updated: %v", updated.ExpectedAudiences)
	}
}

func TestService_Update_ClearIssuersWithEmpty(t *testing.T) {
	// UpdateIssuers=true with Issuers=nil (or []) must clear the field.
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	updated, err := s.Update(ctx, UpdateInput{
		TenantID:      tenantA,
		ProviderID:    p.ID.String(),
		UpdateIssuers: true,
		Issuers:       nil, // explicit clear
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.Issuers) != 0 {
		t.Errorf("expected Issuers cleared to nil/empty, got %v", updated.Issuers)
	}
}

func TestService_Update_AbsentFieldsUnchanged(t *testing.T) {
	// UpdateIssuers=false → field must not change.
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	origIssuers := p.Issuers

	claim := "roles"
	updated, err := s.Update(ctx, UpdateInput{
		TenantID:         tenantA,
		ProviderID:       p.ID.String(),
		UpdateRolesClaim: true,
		RolesClaim:       &claim,
		// UpdateIssuers and UpdateAudiences both false → leave existing values.
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.Issuers) != len(origIssuers) {
		t.Errorf("Issuers changed when UpdateIssuers=false: got %v, want %v", updated.Issuers, origIssuers)
	}
}

func TestService_Update_OnInvalidatedReturnsErrProviderInactive(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Invalidate(ctx, tenantA, p.ID.String()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	_, err = s.Update(ctx, UpdateInput{
		TenantID:      tenantA,
		ProviderID:    p.ID.String(),
		UpdateIssuers: true,
		Issuers:       []string{"https://new.example"},
	})
	if !errors.Is(err, ErrProviderInactive) {
		t.Errorf("Update on invalidated = %v, want ErrProviderInactive", err)
	}
}

func TestService_Update_NotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Update(context.Background(), UpdateInput{
		TenantID:   tenantA,
		ProviderID: uuid.New().String(),
	})
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("Update unknown = %v, want ErrProviderNotFound", err)
	}
}

// ---------- Invalidate ----------

func TestService_Invalidate_Success(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Invalidate(ctx, tenantA, p.ID.String()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	// Verify store reflects the invalidation.
	got, err := s.store.Get(ctx, tenantA, p.ID.String())
	if err != nil {
		t.Fatalf("store.Get after Invalidate: %v", err)
	}
	if got.InvalidatedAt == nil {
		t.Error("InvalidatedAt not set after Invalidate")
	}
}

func TestService_Invalidate_AlreadyInvalidatedIsIdempotent(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Invalidate(ctx, tenantA, p.ID.String()); err != nil {
		t.Fatalf("first Invalidate: %v", err)
	}
	// Second call must not error.
	if err := s.Invalidate(ctx, tenantA, p.ID.String()); err != nil {
		t.Errorf("second Invalidate (idempotent) = %v, want nil", err)
	}
}

func TestService_Invalidate_NotFound(t *testing.T) {
	s := newTestService(t)
	err := s.Invalidate(context.Background(), tenantA, uuid.New().String())
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("Invalidate unknown = %v, want ErrProviderNotFound", err)
	}
}

// ---------- Reactivate ----------

func TestService_Reactivate_Success(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Invalidate(ctx, tenantA, p.ID.String()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	back, err := s.Reactivate(ctx, ReactivateInput{TenantID: tenantA, ProviderID: p.ID.String()})
	if err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if back.InvalidatedAt != nil {
		t.Error("InvalidatedAt not cleared after Reactivate")
	}
	if !back.Active() {
		t.Error("provider not active after Reactivate")
	}
}

func TestService_Reactivate_AlreadyActiveIsIdempotent(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	back, err := s.Reactivate(ctx, ReactivateInput{TenantID: tenantA, ProviderID: p.ID.String()})
	if err != nil {
		t.Errorf("Reactivate on active = %v, want nil", err)
	}
	if back == nil || back.ID != p.ID {
		t.Error("Reactivate on active did not return current DTO")
	}
}

func TestService_Reactivate_NotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Reactivate(context.Background(), ReactivateInput{
		TenantID:   tenantA,
		ProviderID: uuid.New().String(),
	})
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("Reactivate unknown = %v, want ErrProviderNotFound", err)
	}
}

// ---------- Delete ----------

func TestService_Delete_RemovesBlobAndIndexAndMarksHistory(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	p, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := s.Delete(ctx, tenantA, p.ID.String()); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Blob gone.
	if _, err := s.store.Get(ctx, tenantA, p.ID.String()); !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("Get after Delete = %v, want ErrProviderNotFound", err)
	}
	// Index gone.
	if _, err := s.store.GetByURI(ctx, tenantA, sampleURI); !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("GetByURI after Delete = %v, want ErrProviderNotFound", err)
	}

	// D25 history: CurrentOwner should be nil, deleted owner in Past with DeletedAt set.
	h, err := s.store.GetURIHistory(ctx, sha256Hex(sampleURI))
	if err != nil {
		t.Fatalf("GetURIHistory after Delete: %v", err)
	}
	if h == nil {
		t.Fatal("expected history after Delete, got nil")
	}
	if h.CurrentOwner != nil {
		t.Errorf("CurrentOwner = %+v, want nil after Delete", h.CurrentOwner)
	}
	if len(h.Past) == 0 {
		t.Error("Past is empty after Delete; expected deleted owner entry")
	}
	if h.Past[0].DeletedAt == nil {
		t.Error("Past[0].DeletedAt is nil after Delete")
	}
}

func TestService_Delete_NotFound(t *testing.T) {
	s := newTestService(t)
	err := s.Delete(context.Background(), tenantA, uuid.New().String())
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("Delete unknown = %v, want ErrProviderNotFound", err)
	}
}

// ---------- ReloadAll ----------

func TestService_ReloadAll_RebuildsRegistry(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	// Plant a provider directly in the store (bypassing Service to avoid
	// reloadOne's discovery fetch altering registry state).
	tenantUUID := uuid.MustParse(string(tenantA))
	p := &OidcProvider{
		ID:                 uuid.New(),
		WellKnownConfigURI: sampleURI,
		Issuers:            []string{"https://idp.example"},
		CreatedAt:          time.Now().UTC(),
		OwnerLegalEntityID: tenantUUID,
	}
	if err := s.store.Register(ctx, p); err != nil {
		t.Fatalf("store.Register: %v", err)
	}

	if err := s.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}

	// Verify registry's in-memory map has the provider.
	s.registry.mu.RLock()
	byURI := s.registry.providers[tenantA]
	s.registry.mu.RUnlock()
	if byURI == nil {
		t.Fatal("registry provider map for tenantA is nil after ReloadAll")
	}
	if _, ok := byURI[sampleURI]; !ok {
		t.Error("provider not present in registry after ReloadAll")
	}
}

// ---------- ListByTenant ----------

func TestService_ListByTenant_ReturnsAll(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	uri2 := "https://other-idp.example/.well-known/openid-configuration"
	if _, err := s.Register(ctx, defaultInput(tenantA)); err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	if _, err := s.Register(ctx, RegisterInput{
		TenantID:           tenantA,
		WellKnownConfigURI: uri2,
		OwnerLegalEntityID: uuid.MustParse(string(tenantA)),
	}); err != nil {
		t.Fatalf("Register 2: %v", err)
	}

	all, err := s.ListByTenant(ctx, tenantA, false)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListByTenant = %d, want 2", len(all))
	}
}

func TestService_ListByTenant_FiltersActive(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	uri2 := "https://other-idp.example/.well-known/openid-configuration"
	p1, err := s.Register(ctx, defaultInput(tenantA))
	if err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	if _, err := s.Register(ctx, RegisterInput{
		TenantID:           tenantA,
		WellKnownConfigURI: uri2,
		OwnerLegalEntityID: uuid.MustParse(string(tenantA)),
	}); err != nil {
		t.Fatalf("Register 2: %v", err)
	}

	// Invalidate provider 1.
	if err := s.Invalidate(ctx, tenantA, p1.ID.String()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	active, err := s.ListByTenant(ctx, tenantA, true)
	if err != nil {
		t.Fatalf("ListByTenant(activeOnly): %v", err)
	}
	if len(active) != 1 {
		t.Errorf("ListByTenant(activeOnly) = %d, want 1", len(active))
	}
}
