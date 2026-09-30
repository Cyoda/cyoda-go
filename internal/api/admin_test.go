package api_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/logging"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// operatorContext returns a request with a platform-operator UserContext
// attached: ROLE_ADMIN in the PLATFORM tenant.
func operatorContext(req *http.Request) *http.Request {
	uc := &spi.UserContext{
		UserID:   "test-operator",
		UserName: "operator",
		Tenant:   spi.Tenant{ID: auth.PlatformTenantID, Name: "Platform"},
		Roles:    []string{"ROLE_ADMIN"},
	}
	return req.WithContext(spi.WithUserContext(req.Context(), uc))
}

// tenantAdminContext returns a request with a ROLE_ADMIN UserContext in a
// non-PLATFORM tenant: a tenant admin, not a platform operator.
func tenantAdminContext(req *http.Request) *http.Request {
	uc := &spi.UserContext{
		UserID:   "test-admin",
		UserName: "admin",
		Tenant:   spi.Tenant{ID: "acme", Name: "Acme"},
		Roles:    []string{"ROLE_ADMIN"},
	}
	return req.WithContext(spi.WithUserContext(req.Context(), uc))
}

// mockTenantAdminContext returns a request with a ROLE_ADMIN UserContext in
// the mock-mode tenant, as the mock IAM's single fixed principal would carry.
func mockTenantAdminContext(req *http.Request) *http.Request {
	uc := &spi.UserContext{
		UserID:   "mock-admin",
		UserName: "admin",
		Tenant:   spi.Tenant{ID: "mock-tenant", Name: "Mock"},
		Roles:    []string{"ROLE_ADMIN"},
	}
	return req.WithContext(spi.WithUserContext(req.Context(), uc))
}

// errorCode decodes the RFC 9457 ProblemDetail body and returns
// properties.errorCode.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode ProblemDetail: %v; body=%s", err, rec.Body.String())
	}
	return fmt.Sprintf("%v", pd.Properties["errorCode"])
}

func TestHandleGetLogLevel(t *testing.T) {
	// Set a known level
	logging.Level.Set(slog.LevelInfo)

	req := operatorContext(httptest.NewRequest(http.MethodGet, "/admin/log-level", nil))
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).GetLogLevel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["level"] != "info" {
		t.Fatalf("expected level info, got %q", body["level"])
	}
}

func TestHandleGetLogLevel_NoUserContext_401(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/log-level", nil)
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).GetLogLevel(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "UNAUTHORIZED" {
		t.Fatalf("expected errorCode UNAUTHORIZED, got %q", code)
	}
}

func TestHandleGetLogLevel_TenantAdmin_403(t *testing.T) {
	req := tenantAdminContext(httptest.NewRequest(http.MethodGet, "/admin/log-level", nil))
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).GetLogLevel(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("expected errorCode FORBIDDEN, got %q", code)
	}
}

func TestHandleSetLogLevel(t *testing.T) {
	// Start at INFO
	logging.Level.Set(slog.LevelInfo)

	payload := `{"level":"debug"}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["previous"] != "info" {
		t.Fatalf("expected previous info, got %q", body["previous"])
	}
	if body["level"] != "debug" {
		t.Fatalf("expected level debug, got %q", body["level"])
	}

	// Verify level actually changed
	if logging.LevelString(logging.Level.Level()) != "debug" {
		t.Fatal("level was not changed")
	}

	// Reset for other tests
	logging.Level.Set(slog.LevelInfo)
}

func TestHandleSetLogLevel_NoUserContext_401(t *testing.T) {
	payload := `{"level":"debug"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "UNAUTHORIZED" {
		t.Fatalf("expected errorCode UNAUTHORIZED, got %q", code)
	}
}

func TestHandleSetLogLevel_TenantAdmin_403(t *testing.T) {
	logging.Level.Set(slog.LevelInfo)
	before := logging.LevelString(logging.Level.Level())

	payload := `{"level":"debug"}`
	req := tenantAdminContext(httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("expected errorCode FORBIDDEN, got %q", code)
	}
	if after := logging.LevelString(logging.Level.Level()); after != before {
		t.Fatalf("level changed on a refused request: before=%q after=%q", before, after)
	}
}

func TestHandleGetLogLevel_NoUserContext_401_RFC9457(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/log-level", nil)
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).GetLogLevel(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Should be RFC 9457 problem+json, not raw JSON
	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected Content-Type application/problem+json, got %q", ct)
	}

	var pd map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&pd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pd["status"] != float64(401) {
		t.Errorf("expected status 401 in body, got %v", pd["status"])
	}
}

func TestHandleSetLogLevel_NoUserContext_401_RFC9457(t *testing.T) {
	payload := `{"level":"debug"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected Content-Type application/problem+json, got %q", ct)
	}
}

func TestHandleSetLogLevel_BadBody_RFC9457(t *testing.T) {
	payload := `not json`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected Content-Type application/problem+json, got %q", ct)
	}
}

func TestHandleSetLogLevel_EmptyLevel_RFC9457(t *testing.T) {
	payload := `{"level":""}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected Content-Type application/problem+json, got %q", ct)
	}
}

func TestHandleSetLogLevel_EmptyLevel(t *testing.T) {
	payload := `{"level":""}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestHandleSetLogLevel_UnknownLevel_400 asserts that an unrecognised level
// is refused with 400 rather than silently substituted with info, and that
// the level itself is left unchanged.
func TestHandleSetLogLevel_UnknownLevel_400(t *testing.T) {
	logging.Level.Set(slog.LevelWarn)

	payload := `{"level":"verbose"}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "BAD_REQUEST" {
		t.Fatalf("expected errorCode BAD_REQUEST, got %q", code)
	}
	if got := logging.LevelString(logging.Level.Level()); got != "warn" {
		t.Fatalf("level substituted on an unknown value: got %q, want unchanged \"warn\"", got)
	}

	logging.Level.Set(slog.LevelInfo)
}

// TestHandleSetLogLevel_OversizeBody_400 asserts the request body is bounded
// (1 MiB), matching the pattern internal/domain/account uses for its POST
// bodies, rather than an admin endpoint accepting an unbounded body. The
// padding sits in a field the request struct doesn't declare — an otherwise
// well-formed, accepted request except for size — so the 400 can only come
// from the size bound, not from an unrelated validation error.
func TestHandleSetLogLevel_OversizeBody_400(t *testing.T) {
	logging.Level.Set(slog.LevelInfo)

	oversized := `{"level":"debug","padding":"` + strings.Repeat("x", 2<<20) + `"}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/log-level", strings.NewReader(oversized)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetLogLevel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversize body, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := logging.LevelString(logging.Level.Level()); got != "info" {
		t.Fatalf("level changed on a refused oversize request: got %q, want unchanged \"info\"", got)
	}
}

// TestHandleSetTraceSampler_OversizeBody_400 mirrors
// TestHandleSetLogLevel_OversizeBody_400 for the sibling admin endpoint.
func TestHandleSetTraceSampler_OversizeBody_400(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	oversized := `{"sampler":"always","padding":"` + strings.Repeat("x", 2<<20) + `"}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(oversized)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversize body, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := observability.Sampler.Config(); got != prev {
		t.Fatalf("sampler changed on a refused oversize request: got %+v, want unchanged %+v", got, prev)
	}
}

func TestHandleGetTraceSampler(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	if err := observability.Sampler.SetSampler(observability.SamplerConfig{
		Sampler: "ratio", Ratio: 0.1, ParentBased: true,
	}); err != nil {
		t.Fatalf("SetSampler: %v", err)
	}

	req := operatorContext(httptest.NewRequest(http.MethodGet, "/admin/trace-sampler", nil))
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).GetTraceSampler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var got observability.SamplerConfig
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := observability.SamplerConfig{Sampler: "ratio", Ratio: 0.1, ParentBased: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestHandleGetTraceSampler_NoUserContext_401(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/trace-sampler", nil)
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).GetTraceSampler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected Content-Type application/problem+json, got %q", ct)
	}
	if code := errorCode(t, rec); code != "UNAUTHORIZED" {
		t.Fatalf("expected errorCode UNAUTHORIZED, got %q", code)
	}
}

func TestHandleGetTraceSampler_TenantAdmin_403(t *testing.T) {
	req := tenantAdminContext(httptest.NewRequest(http.MethodGet, "/admin/trace-sampler", nil))
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).GetTraceSampler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("expected errorCode FORBIDDEN, got %q", code)
	}
}

func TestHandleSetTraceSampler_Always(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `{"sampler":"always"}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := observability.Sampler.Config()
	want := observability.SamplerConfig{Sampler: "always", ParentBased: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestHandleSetTraceSampler_Ratio(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `{"sampler":"ratio","ratio":0.1}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := observability.Sampler.Config()
	want := observability.SamplerConfig{Sampler: "ratio", Ratio: 0.1, ParentBased: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestHandleSetTraceSampler_ParentBasedFalse(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `{"sampler":"always","parent_based":false}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := observability.Sampler.Config()
	if got.ParentBased {
		t.Errorf("ParentBased = true, want false (explicitly set)")
	}
}

func TestHandleSetTraceSampler_ParentBasedDefault(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	// Omit parent_based — should default to true.
	payload := `{"sampler":"always"}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := observability.Sampler.Config()
	if !got.ParentBased {
		t.Errorf("ParentBased = false, want true (default)")
	}
}

func TestHandleSetTraceSampler_InvalidSamplerType(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `{"sampler":"foo"}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("expected Content-Type application/problem+json, got %q", ct)
	}
}

func TestHandleSetTraceSampler_RatioOnNonRatio(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `{"sampler":"always","ratio":0.1}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleSetTraceSampler_RatioOutOfRange(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `{"sampler":"ratio","ratio":1.5}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleSetTraceSampler_RatioZero(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `{"sampler":"ratio","ratio":0}`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (ratio=0 should be rejected; use sampler=never for zero sampling), got %d", rec.Code)
	}
}

func TestHandleSetTraceSampler_NoUserContext_401(t *testing.T) {
	payload := `{"sampler":"always"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "UNAUTHORIZED" {
		t.Fatalf("expected errorCode UNAUTHORIZED, got %q", code)
	}
}

func TestHandleSetTraceSampler_TenantAdmin_403(t *testing.T) {
	before := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(before) })

	payload := `{"sampler":"always"}`
	req := tenantAdminContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if code := errorCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("expected errorCode FORBIDDEN, got %q", code)
	}
	if after := observability.Sampler.Config(); after != before {
		t.Fatalf("sampler changed on a refused request: before=%+v after=%+v", before, after)
	}
}

func TestHandleSetTraceSampler_BadBody(t *testing.T) {
	prev := observability.Sampler.Config()
	t.Cleanup(func() { _ = observability.Sampler.SetSampler(prev) })

	payload := `not-json`
	req := operatorContext(httptest.NewRequest(http.MethodPost, "/admin/trace-sampler", strings.NewReader(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.OperatorGuard{}).SetTraceSampler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestAdminHandlers_MockGuard_AdmitsAdminInAnyTenant asserts that, when the
// handlers are built with the mock-mode guard, a ROLE_ADMIN principal in the
// mock tenant (not PLATFORM) is admitted — mock mode has no PLATFORM tenant.
func TestAdminHandlers_MockGuard_AdmitsAdminInAnyTenant(t *testing.T) {
	logging.Level.Set(slog.LevelInfo)

	req := mockTenantAdminContext(httptest.NewRequest(http.MethodGet, "/admin/log-level", nil))
	rec := httptest.NewRecorder()

	api.NewAdminHandlers(auth.MockOperatorGuard()).GetLogLevel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
}
