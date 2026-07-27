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
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id"`
	ModelName        string `json:"model_name"`
	ModelVersion     string `json:"model_version"`
	Version          int64  `json:"version"`
	State            string `json:"state"`
	ValidTime        string `json:"valid_time"`
	TransactionTime  string `json:"transaction_time"`
	WallClockTime    string `json:"wall_clock_time"`
	CreationDate     string `json:"creation_date"`
	LastModifiedDate string `json:"last_modified_date"`
	ChangeType       string `json:"change_type"`
	ChangeUser       string `json:"change_user"`
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
func marshalEntityDoc(entity *spi.Entity, validTime, txTime, wallClockTime time.Time, deleted bool) ([]byte, error) {
	meta := entityMeta{
		ID:                 entity.Meta.ID,
		TenantID:           string(entity.Meta.TenantID),
		ModelName:          entity.Meta.ModelRef.EntityName,
		ModelVersion:       entity.Meta.ModelRef.ModelVersion,
		Version:            entity.Meta.Version,
		State:              entity.Meta.State,
		ValidTime:          validTime.UTC().Format(time.RFC3339Nano),
		TransactionTime:    txTime.UTC().Format(time.RFC3339Nano),
		WallClockTime:      wallClockTime.UTC().Format(time.RFC3339Nano),
		CreationDate:       entity.Meta.CreationDate.UTC().Format(time.RFC3339Nano),
		LastModifiedDate:   entity.Meta.LastModifiedDate.UTC().Format(time.RFC3339Nano),
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
	doc["_meta"] = metaJSON
	return json.Marshal(doc)
}

// unmarshalEntityDoc extracts an Entity from a merged JSONB document.
// The _meta block is parsed into EntityMeta and removed; the remaining
// keys become entity.Data.
func unmarshalEntityDoc(raw []byte) (*spi.Entity, error) {
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

	var data []byte
	if len(doc) > 0 {
		var err error
		data, err = json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("failed to re-marshal domain data: %w", err)
		}
	}

	creationDate, err := time.Parse(time.RFC3339Nano, meta.CreationDate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse creation_date: %w", err)
	}
	lastModified, err := time.Parse(time.RFC3339Nano, meta.LastModifiedDate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse last_modified_date: %w", err)
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
// supplementing with the version number and valid time from the query context.
func unmarshalEntityVersion(raw []byte, version int64, validTime time.Time) (*spi.EntityVersion, error) {
	entity, err := unmarshalEntityDoc(raw)
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
