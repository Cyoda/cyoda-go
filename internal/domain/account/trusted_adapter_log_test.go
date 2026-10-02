package account_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

// Register, invalidate, reactivate and delete each write one INFO line
// naming the tenant, the kid, the attributed principal and the executor. The
// request here is an on-behalf-of one: alice, executed by the client obo-1,
// so the two must differ in the line.
func TestTrustedKeyChanges_WriteInfoLines(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	ts := newTestTrustedStore(t)
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	h := account.New(newTestKeyStore(t), ts, nil, feats, auth.OperatorGuard{})
	uc := &spi.UserContext{
		UserID: "alice", UserName: "alice", Kind: spi.PrincipalUser,
		Executor: &spi.Principal{ID: "obo-1", Kind: spi.PrincipalService},
		Tenant:   spi.Tenant{ID: "t1", Name: "t1"},
		Roles:    []string{"ROLE_ADMIN"},
	}
	req := func(method string, body []byte) *http.Request {
		var r *http.Request
		if body == nil {
			r = httptest.NewRequest(method, "/", nil)
		} else {
			r = httptest.NewRequest(method, "/", bytes.NewReader(body))
		}
		return r.WithContext(spi.WithUserContext(context.Background(), uc))
	}

	regBody, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k", Jwk: rsaJWK(t, "k")})
	reactBody, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
	steps := []struct {
		msg  string
		call func(w http.ResponseWriter)
	}{
		{"trusted key registered", func(w http.ResponseWriter) { h.RegisterTrustedKey(w, req("POST", regBody)) }},
		{"trusted key invalidated", func(w http.ResponseWriter) { h.InvalidateTrustedKey(w, req("POST", nil), "k") }},
		{"trusted key reactivated", func(w http.ResponseWriter) { h.ReactivateTrustedKey(w, req("POST", reactBody), "k") }},
		{"trusted key deleted", func(w http.ResponseWriter) { h.DeleteTrustedKey(w, req("DELETE", nil), "k") }},
	}
	for _, step := range steps {
		buf.Reset()
		w := httptest.NewRecorder()
		step.call(w)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", step.msg, w.Code, w.Body.String())
		}
		var line map[string]any
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == step.msg {
				line = m
			}
		}
		if line == nil {
			t.Fatalf("no INFO line %q; log: %s", step.msg, buf.String())
		}
		want := map[string]string{
			"level": "INFO", "pkg": "account", "tenant": "t1", "kid": "k",
			"attributedId": "alice", "attributedKind": "user",
			"executorId": "obo-1", "executorKind": "service",
		}
		for k, v := range want {
			if line[k] != v {
				t.Errorf("%s: %s = %v, want %s", step.msg, k, line[k], v)
			}
		}
	}
}
