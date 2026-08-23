package audit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/audit"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// versionMetadataSpyStore wraps a real spi.EntityStore and records the
// spi.VersionMetadataOptions passed to the most recent GetVersionMetadata
// call, so a test can assert the audit handler pushes its request's time
// window down to the store instead of relying purely on the in-memory
// filter that runs after the merge with StateMachine events.
type versionMetadataSpyStore struct {
	spi.EntityStore
	called   bool
	lastOpts spi.VersionMetadataOptions
}

func (s *versionMetadataSpyStore) GetVersionMetadata(ctx context.Context, entityID string, opts spi.VersionMetadataOptions) ([]spi.EntityVersionMeta, error) {
	s.called = true
	s.lastOpts = opts
	return s.EntityStore.GetVersionMetadata(ctx, entityID, opts)
}

// versionMetadataSpyFactory hands out a fresh versionMetadataSpyStore
// wrapping the real per-call EntityStore, and remembers the last one handed
// out so a test can inspect it after the request completes.
type versionMetadataSpyFactory struct {
	spi.StoreFactory
	last *versionMetadataSpyStore
}

func (f *versionMetadataSpyFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	real, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	f.last = &versionMetadataSpyStore{EntityStore: real}
	return f.last, nil
}

func pushdownTestCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "pushdown-user",
		Tenant: spi.Tenant{ID: "tenant-pushdown", Name: "tenant-pushdown"},
		Roles:  []string{"USER"},
	})
}

// TestSearchEntityAuditEvents_PushesTimeWindowToStore pins that
// SearchEntityAuditEvents forwards the request's fromUtcTime/toUtcTime
// straight into spi.VersionMetadataOptions.From/.Until on the
// GetVersionMetadata call (task E6: history read rewires), rather than
// leaving the store call unbounded and relying solely on the post-merge
// in-memory filter.
func TestSearchEntityAuditEvents_PushesTimeWindowToStore(t *testing.T) {
	ctx := pushdownTestCtx()
	base := memory.NewStoreFactory()

	ref := spi.ModelRef{EntityName: "AuditPushdown", ModelVersion: "1"}
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	modelStore, err := base.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked, Schema: raw}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}

	txMgr, err := base.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	spyFactory := &versionMetadataSpyFactory{StoreFactory: base}
	engine := wfengine.NewEngine(spyFactory, common.NewDefaultUUIDGenerator(), txMgr)
	eh := entity.New(spyFactory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())

	res, err := eh.CreateEntity(ctx, entity.CreateEntityInput{
		EntityName:   ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Format:       "JSON",
		Data:         json.RawMessage(`{"name":"Bob"}`),
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	entityID := res.EntityIDs[0]

	if _, err := eh.UpdateEntity(ctx, entity.UpdateEntityInput{
		EntityID: entityID,
		Format:   "JSON",
		Data:     json.RawMessage(`{"name":"Carol"}`),
	}); err != nil {
		t.Fatalf("UpdateEntity: %v", err)
	}

	from := time.Now().Add(-time.Hour).UTC()
	to := time.Now().Add(time.Hour).UTC()

	ah := audit.New(spyFactory)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/audit/entity/"+entityID, nil).WithContext(ctx)
	ah.SearchEntityAuditEvents(w, r, uuid.MustParse(entityID), genapi.SearchEntityAuditEventsParams{
		FromUtcTime: &from,
		ToUtcTime:   &to,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if spyFactory.last == nil || !spyFactory.last.called {
		t.Fatal("expected GetVersionMetadata to be called on the spy store")
	}
	got := spyFactory.last.lastOpts
	if got.From == nil || !got.From.Equal(from) {
		t.Errorf("opts.From = %v, want %v", got.From, from)
	}
	if got.Until == nil || !got.Until.Equal(to) {
		t.Errorf("opts.Until = %v, want %v", got.Until, to)
	}

	// Regression: the merge with SM events, entityId-from-request-param, and
	// actor.legalId-from-ctx-tenant must all still hold after the pushdown.
	var body []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &struct {
		Items *[]map[string]any `json:"items"`
	}{Items: &body}); err != nil {
		t.Fatalf("decode response: %v: %s", err, w.Body.String())
	}
	if len(body) == 0 {
		t.Fatal("expected at least one audit event in range")
	}
	for _, ev := range body {
		if ev["entityId"] != entityID {
			t.Errorf("event entityId = %v, want %q", ev["entityId"], entityID)
		}
		actor, ok := ev["actor"].(map[string]any)
		if ok {
			if legalID := actor["legalId"]; legalID != "tenant-pushdown" {
				t.Errorf("actor.legalId = %v, want %q", legalID, "tenant-pushdown")
			}
		}
	}
}
