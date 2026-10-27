package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// undecodableDataG is stored entity data that is not JSON. EntityGetRequest
// fails on it with SERVER_ERROR; a search stream must give the same answer,
// not leave the entity out.
var undecodableDataG = []byte(`{"name":`)

// goodAndUndecodable is a result of two entities, the second one unreadable.
func goodAndUndecodable(ref spi.ModelRef) []*spi.Entity {
	now := time.Now()
	meta := func(id string) spi.EntityMeta {
		return spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW", CreationDate: now, LastModifiedDate: now}
	}
	return []*spi.Entity{
		{Meta: meta("good-id"), Data: []byte(`{"name":"Alice"}`)},
		{Meta: meta("bad-id"), Data: undecodableDataG},
	}
}

// expectOnlyServerError asserts the stream carries one envelope, a
// SERVER_ERROR refusal, and no entity of the result.
func expectOnlyServerError(t *testing.T, stream *mockEntityStream) {
	t.Helper()
	if len(stream.sent) != 1 {
		t.Fatalf("stream carried %d envelopes, want exactly one refusal", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatalf("got Success=true with undecodable stored data; want a refusal")
	}
	if typed.Error == nil || typed.Error.Code != "SERVER_ERROR" {
		t.Fatalf("Error = %+v, want Code=SERVER_ERROR", typed.Error)
	}
	if strings.Contains(string(stream.sent[0].GetTextData()), "Alice") {
		t.Errorf("part of the result was sent: %s", stream.sent[0].GetTextData())
	}
}

func TestDirectSearch_UndecodableStoredData_ServerErrorEnvelope(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()
	ctx := grpcTenantCtx()
	ref := spi.ModelRef{EntityName: "undecodable", ModelVersion: "1"}
	saveMinimalModelGRPC(t, ctx, base, ref)
	realStore, _ := base.EntityStore(ctx)
	ses := &searcherEntityStoreG{EntityStore: realStore,
		searchFn: func(_ context.Context, _ spi.Filter, _ spi.SearchOptions) ([]*spi.Entity, error) {
			return goodAndUndecodable(ref), nil
		}}
	factory := &searcherFactoryG{StoreFactory: base, entityStore: ses}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore, newTestConsistency(t, factory))}

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(makeCE(EntitySearchRequest, map[string]any{
		"id":        "s-undecodable-1",
		"model":     map[string]any{"name": "undecodable", "version": 1},
		"condition": map[string]any{"type": "simple", "jsonPath": "$.name", "operatorType": "EQUALS", "value": "Alice"},
	}), stream); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	expectOnlyServerError(t, stream)
}

// snapshotResultsStoreG serves a SUCCESSFUL job whose result ids are those of
// the given entities.
type snapshotResultsStoreG struct {
	spi.AsyncSearchStore
	ids []string
}

func (s *snapshotResultsStoreG) GetJob(_ context.Context, jobID string) (*spi.SearchJob, error) {
	return &spi.SearchJob{ID: jobID, Status: "SUCCESSFUL", ResultCount: len(s.ids)}, nil
}

func (s *snapshotResultsStoreG) GetResultIDs(_ context.Context, _ string, _, _ int) ([]string, int, error) {
	return s.ids, len(s.ids), nil
}

// snapshotEntityStoreG answers GetAsAt from a fixed set of entities.
type snapshotEntityStoreG struct {
	spi.EntityStore
	byID map[string]*spi.Entity
}

func (s *snapshotEntityStoreG) GetAsAt(_ context.Context, id string, _ time.Time) (*spi.Entity, error) {
	return s.byID[id], nil
}

type snapshotFactoryG struct {
	spi.StoreFactory
	store spi.EntityStore
}

func (f *snapshotFactoryG) EntityStore(context.Context) (spi.EntityStore, error) {
	return f.store, nil
}

func TestSnapshotGet_UndecodableStoredData_ServerErrorEnvelope(t *testing.T) {
	ctx := grpcTenantCtx()
	results := goodAndUndecodable(spi.ModelRef{EntityName: "undecodable", ModelVersion: "1"})
	byID := map[string]*spi.Entity{}
	ids := make([]string, 0, len(results))
	for _, e := range results {
		byID[e.Meta.ID] = e
		ids = append(ids, e.Meta.ID)
	}
	svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(
		&snapshotFactoryG{store: &snapshotEntityStoreG{byID: byID}}, nil, &snapshotResultsStoreG{ids: ids}, newFixedConsistency())}

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(makeCE(SnapshotGetRequest, map[string]any{
		"id": "s-undecodable-2", "snapshotId": uuid.NewString(), "pageNumber": 0, "pageSize": 100,
	}), stream); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	expectOnlyServerError(t, stream)
}
