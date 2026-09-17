package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// entityMeta is the JSON-serializable representation of the _meta block
// stored alongside domain data in the JSONB document.
type entityMeta struct {
	ID           string `json:"id"`
	TenantID     string `json:"tenant_id"`
	ModelName    string `json:"model_name"`
	ModelVersion string `json:"model_version"`
	Version      int64  `json:"version"`
	State        string `json:"state"`
	ChangeType   string `json:"change_type"`
	ChangeUser   string `json:"change_user"`
	// ChangeUserKind is the PrincipalKind of the attributed ChangeUser above;
	// empty ("") on legacy docs written before attribution existed — reads
	// back as spi.PrincipalKind("") (the zero value), never synthesized.
	ChangeUserKind string `json:"change_user_kind,omitempty"`
	// ChangeExecutorID/ChangeExecutorKind carry the actual caller that
	// performed the change (spi.EntityMeta.ChangeExecutor), independent of
	// the attributed ChangeUser/ChangeUserKind above. Both empty on legacy
	// docs.
	ChangeExecutorID   string `json:"change_executor_id,omitempty"`
	ChangeExecutorKind string `json:"change_executor_kind,omitempty"`
	TransactionID      string `json:"transaction_id"`
	Transition         string `json:"transition"`
	Deleted            bool   `json:"deleted"`
}

// marshalEntityDoc produces a merged JSONB document containing a _meta block
// and the entity's domain data as top-level keys.
//
// It takes no temporal arguments: valid_time/transaction_time/wall_clock_time
// and creation_date/last_modified_date all live in columns on entities and
// entity_versions (Task 5's migration), not in this document. Rewriting JSONB
// at commit time to keep in-document copies honest would roughly double every
// transaction's write volume and end entity_versions' append-only property —
// the columns are the sole source of truth, and reads project them back
// (unmarshalEntityDoc below).
func marshalEntityDoc(entity *spi.Entity, deleted bool) ([]byte, error) {
	meta := entityMeta{
		ID:                 entity.Meta.ID,
		TenantID:           string(entity.Meta.TenantID),
		ModelName:          entity.Meta.ModelRef.EntityName,
		ModelVersion:       entity.Meta.ModelRef.ModelVersion,
		Version:            entity.Meta.Version,
		State:              entity.Meta.State,
		ChangeType:         entity.Meta.ChangeType,
		ChangeUser:         entity.Meta.ChangeUser,
		ChangeUserKind:     string(entity.Meta.ChangeUserKind),
		ChangeExecutorID:   entity.Meta.ChangeExecutor.ID,
		ChangeExecutorKind: string(entity.Meta.ChangeExecutor.Kind),
		TransactionID:      entity.Meta.TransactionID,
		Transition:         entity.Meta.TransitionForLatestSave,
		Deleted:            deleted,
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal entity meta: %w", err)
	}

	if len(entity.Data) == 0 {
		// No domain data — doc is just {"_meta": {...}}
		doc := map[string]json.RawMessage{
			"_meta": metaJSON,
		}
		return json.Marshal(doc)
	}

	// Merge _meta into the domain data
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(entity.Data, &doc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal entity data: %w", err)
	}
	// The JSON literal `null` unmarshals into a NIL map without error, and
	// assigning into a nil map panics. That is reachable — a processor
	// returning {"data":null} produces exactly these bytes — and the panic is
	// only recovered several packages up, unwinding past the caller's
	// non-deferred rollback, so the transaction is neither committed nor rolled
	// back and its pooled connection is never returned. Fail cleanly instead.
	if doc == nil {
		return nil, fmt.Errorf("entity data must be a JSON object, got null")
	}
	doc["_meta"] = metaJSON
	return json.Marshal(doc)
}

// unmarshalEntityDoc rebuilds an Entity from its stored document plus the
// temporal columns. The dates are NOT in the document: the columns are the
// source of truth, so a commit-phase stamp updates two narrow columns rather
// than rewriting every document it wrote. The _meta block is parsed into
// EntityMeta and removed; the remaining keys become entity.Data.
func unmarshalEntityDoc(raw []byte, creationDate, lastModified time.Time) (*spi.Entity, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal entity doc: %w", err)
	}

	metaRaw, ok := doc["_meta"]
	if !ok {
		return nil, fmt.Errorf("entity doc missing _meta block")
	}

	var meta entityMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal _meta: %w", err)
	}

	delete(doc, "_meta")

	// A document with no domain keys left after removing _meta is either a
	// legitimate empty payload or a DELETED version that carries none. Those
	// are different things and the difference is observable, so report the
	// empty payload as `{}` and only a deleted version as no data at all.
	//
	// Reporting `{}` as no data was a defect: the consumer decodes Data
	// directly, decoding a zero-length slice is io.EOF, and the entity — plus
	// every list of the model containing it — then failed permanently with a
	// 500. memory and sqlite always round-tripped `{}` as `{}`.
	var data []byte
	if len(doc) > 0 || !meta.Deleted {
		var err error
		data, err = json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("failed to re-marshal domain data: %w", err)
		}
	}

	return &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       meta.ID,
			TenantID: spi.TenantID(meta.TenantID),
			ModelRef: spi.ModelRef{
				EntityName:   meta.ModelName,
				ModelVersion: meta.ModelVersion,
			},
			State:                   meta.State,
			Version:                 meta.Version,
			CreationDate:            creationDate,
			LastModifiedDate:        lastModified,
			TransactionID:           meta.TransactionID,
			ChangeType:              meta.ChangeType,
			ChangeUser:              meta.ChangeUser,
			ChangeUserKind:          spi.PrincipalKind(meta.ChangeUserKind),
			ChangeExecutor:          spi.Principal{ID: meta.ChangeExecutorID, Kind: spi.PrincipalKind(meta.ChangeExecutorKind)},
			TransitionForLatestSave: meta.Transition,
		},
		Data: data,
	}, nil
}

// unmarshalEntityVersion extracts an EntityVersion from a JSONB document,
// supplementing with the version number, valid time, transaction time and
// creation date from the query context.
//
// Timestamp stays validTime — the instant THIS revision became effective,
// unrelated to the definitional question below and uncontroversial. The
// embedded Entity's LastModifiedDate is a DIFFERENT field with a fixed SPI
// definition ("the instant the transaction that wrote this revision
// committed", cyoda-go-spi/types.go): that is transaction_time, matching the
// same choice GetAsAt and the PIT base make for the same field
// (entity_store.go's GetAsAt, search_base.go's pitBaseQueryTemplate) — not a
// second, independent decision. entity_versions has no last_modified column
// of its own, which is why this takes transactionTime as a separate argument
// rather than reusing validTime the way earlier code here (wrongly) did: the
// two are equal for every write today (no backdating support yet), so a
// caller cannot observe the earlier mistake until they diverge — exactly how
// it stayed latent until reads started projecting columns instead of
// re-serializing whatever the caller happened to pass at save time.
func unmarshalEntityVersion(raw []byte, version int64, validTime, transactionTime, creationDate time.Time) (*spi.EntityVersion, error) {
	entity, err := unmarshalEntityDoc(raw, creationDate, transactionTime)
	if err != nil {
		return nil, err
	}

	// Extract deleted flag from _meta
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("failed to re-unmarshal for deleted flag: %w", err)
	}
	var meta entityMeta
	if err := json.Unmarshal(doc["_meta"], &meta); err != nil {
		return nil, fmt.Errorf("failed to re-unmarshal _meta for deleted flag: %w", err)
	}

	return &spi.EntityVersion{
		Entity:     entity,
		ChangeType: meta.ChangeType,
		User:       meta.ChangeUser,
		Timestamp:  validTime,
		Version:    version,
		Deleted:    meta.Deleted,
		// AttributedKind/Executor are populated independently of Entity —
		// meta is parsed directly above, so this holds even for a DELETED
		// version whose Entity carries no domain data.
		AttributedKind: spi.PrincipalKind(meta.ChangeUserKind),
		Executor:       spi.Principal{ID: meta.ChangeExecutorID, Kind: spi.PrincipalKind(meta.ChangeExecutorKind)},
	}, nil
}
