package modelcache

import (
	"encoding/json"

	spi "github.com/cyoda/cyoda-go-spi"
)

// topicModelInvalidate is the gossip topic used for cache drop messages.
const topicModelInvalidate = "model.invalidate"

// invalidationPayload is the wire form for model-invalidate gossip.
// Keys are short to keep gossip packets small.
type invalidationPayload struct {
	TenantID     string `json:"t"`
	EntityName   string `json:"n"`
	ModelVersion string `json:"v"`
}

// EncodeInvalidation produces the payload sent on topicModelInvalidate.
func EncodeInvalidation(tenantID string, ref spi.ModelRef) ([]byte, error) {
	return json.Marshal(invalidationPayload{
		TenantID:     tenantID,
		EntityName:   ref.EntityName,
		ModelVersion: ref.ModelVersion,
	})
}

// DecodeInvalidation is the inverse. Returns ok=false on malformed
// input or any blank field so the gossip handler drops it silently.
func DecodeInvalidation(raw []byte) (tenantID string, ref spi.ModelRef, ok bool) {
	var p invalidationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", spi.ModelRef{}, false
	}
	if p.TenantID == "" || p.EntityName == "" || p.ModelVersion == "" {
		return "", spi.ModelRef{}, false
	}
	return p.TenantID, spi.ModelRef{EntityName: p.EntityName, ModelVersion: p.ModelVersion}, true
}
