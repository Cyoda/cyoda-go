package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestSchemaExtension_ConcurrentEveryDoor_No409 fires N concurrent writes on
// each write door, every write adding a field of its own to one STRUCTURAL
// model. A schema extension commits in its own short transaction and never
// conflicts with another, so every write answers 2xx and the model gains every
// field. The requests are sent once each: doAuthRaw retries a retryable 409,
// which would hide the conflict this test is about.
func TestSchemaExtension_ConcurrentEveryDoor_No409(t *testing.T) {
	const N = 8
	const procName = "se-door-proc-adds"
	// The processor door's processor adds proc_<amount> to the entity.
	procSvc.RegisterProcessor(procName, func(ctx context.Context, e *spi.Entity, p spi.ProcessorDefinition) (*spi.Entity, error) {
		var data map[string]any
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return nil, fmt.Errorf("decode entity data: %w", err)
		}
		data[fmt.Sprintf("proc_%v", data["amount"])] = "v"
		out, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("encode entity data: %w", err)
		}
		return &spi.Entity{Meta: e.Meta, Data: out}, nil
	})
	defer procSvc.Reset()

	base := func(i int) string { return fmt.Sprintf(`"name":"w%d","amount":%d,"status":"new"`, i, i) }
	doors := []struct {
		name string
		// workflow is imported on the model when set.
		workflow string
		// preCreate creates N entities before the concurrent phase.
		preCreate bool
		// write is writer i's request on its entity (ids[i] when preCreate).
		write func(model string, i int, id string) extWrite
		field func(i int) string
	}{
		{name: "Create", write: func(model string, i int, _ string) extWrite {
			return extWrite{http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), fmt.Sprintf(`{%s,"create_%d":"v"}`, base(i), i)}
		}, field: func(i int) string { return fmt.Sprintf("create_%d", i) }},
		{name: "Update", preCreate: true, write: func(_ string, i int, id string) extWrite {
			return extWrite{http.MethodPut, "/api/entity/JSON/" + id, fmt.Sprintf(`{%s,"put_%d":"v"}`, base(i), i)}
		}, field: func(i int) string { return fmt.Sprintf("put_%d", i) }},
		{name: "CollectionCreate", write: func(model string, i int, _ string) extWrite {
			return extWrite{http.MethodPost, "/api/entity/JSON", collectionCreateItems(model, fmt.Sprintf(`{%s,"ccreate_%d":"v"}`, base(i), i))}
		}, field: func(i int) string { return fmt.Sprintf("ccreate_%d", i) }},
		{name: "CollectionUpdate", preCreate: true, write: func(_ string, i int, id string) extWrite {
			return extWrite{http.MethodPut, "/api/entity/JSON", collectionUpdateBody(id, "", fmt.Sprintf(`{%s,"cupdate_%d":"v"}`, base(i), i))}
		}, field: func(i int) string { return fmt.Sprintf("cupdate_%d", i) }},
		{name: "ProcessorOutput", workflow: openThenGoWorkflowJSON("se-door-proc-wf", procName), preCreate: true,
			write: func(_ string, i int, id string) extWrite {
				return extWrite{http.MethodPut, "/api/entity/JSON/" + id + "/go", fmt.Sprintf(`{%s}`, base(i))}
			}, field: func(i int) string { return fmt.Sprintf("proc_%d", i) }},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			model := uniq("se-door-" + strings.ToLower(door.name))
			importModelE2E(t, model, 1)
			lockModelE2E(t, model, 1)
			setChangeLevelE2E(t, model, 1, "STRUCTURAL")
			if door.workflow != "" {
				if status, body := importWorkflowE2E(t, model, 1, door.workflow); status != http.StatusOK {
					t.Fatalf("workflow import: %d %s", status, body)
				}
			}
			ids := make([]string, N)
			if door.preCreate {
				for i := range ids {
					ids[i] = createEntityE2E(t, model, 1, fmt.Sprintf(`{%s}`, base(i)))
				}
			}
			writes := make([]extWrite, N)
			fields := make([]string, N)
			for i := range writes {
				writes[i] = door.write(model, i, ids[i])
				fields[i] = door.field(i)
			}

			requireAllWritesSucceeded(t, fireWritesConcurrently(t, writes))
			requireFieldsInExport(t, model, fields...)
		})
	}
}

// TestModelSchemaExtensions_SequentialFoldAcrossRequests asserts that
// the Postgres Get-fold correctly replays the extension log across
// multiple HTTP requests. Each POST appends a delta; the final export
// must reflect every accumulated field.
//
// This is the single-node read-side correctness check. The multi-node
// side (an extension on one node reaching another through the model
// cache's invalidation broadcast) is tested in internal/cluster/modelcache
// (integration_test.go).
func TestModelSchemaExtensions_SequentialFoldAcrossRequests(t *testing.T) {
	const modelName = "e2e-schema-ext-sequential"
	const version = 1

	importModelE2E(t, modelName, version)
	lockModelE2E(t, modelName, version)
	setChangeLevelE2E(t, modelName, version, "STRUCTURAL")

	// Six sequential writes, each adding a new field.
	for i := 0; i < 6; i++ {
		payload := fmt.Sprintf(
			`{"name":"Sequential-%d","amount":%d,"status":"new","seq_field_%d":"val_%d"}`,
			i, i, i, i,
		)
		path := fmt.Sprintf("/api/entity/JSON/%s/%d", modelName, version)
		resp := doAuth(t, http.MethodPost, path, payload)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create #%d failed: status=%d body=%s", i, resp.StatusCode, body)
		}
	}

	schema := exportModelE2E(t, modelName, version)
	raw := fmt.Sprintf("%v", schema)
	for i := 0; i < 6; i++ {
		want := fmt.Sprintf("seq_field_%d", i)
		if !strings.Contains(raw, want) {
			t.Errorf("folded schema missing %q after sequential writes: %s", want, raw)
		}
	}
}
