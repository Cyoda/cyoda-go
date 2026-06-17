package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func encodeSeg(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func buildTestJWT(t *testing.T, kid, alg string, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"kid": kid, "alg": alg, "typ": "JWT"}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("buildTestJWT: marshal header: %v", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("buildTestJWT: marshal claims: %v", err)
	}
	return encodeSeg(hb) + "." + encodeSeg(cb) + ".c2ln"
}
