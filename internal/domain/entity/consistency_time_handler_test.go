package entity_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

func TestGetConsistencyTime_ReturnsFreshC(t *testing.T) {
	f := newFenceFixture(t, "ct-fresh", false)
	f.tm.at = time.Date(2026, 10, 5, 14, 3, 7, 123456789, time.UTC)

	w := httptest.NewRecorder()
	f.h.GetConsistencyTime(w, httptest.NewRequest(http.MethodGet, "/entity/consistency-time", nil).WithContext(f.ctx))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var body struct {
		ConsistencyTime string `json:"consistencyTime"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, err := time.Parse(time.RFC3339Nano, body.ConsistencyTime)
	if err != nil || !got.Equal(f.tm.at) {
		t.Fatalf("consistencyTime = %q (%v), want exactly %s", body.ConsistencyTime, err, f.tm.at.Format(time.RFC3339Nano))
	}
}

func TestGetConsistencyTime_Unavailable503(t *testing.T) {
	f := newFenceFixture(t, "ct-unavail", false)
	f.tm.err = spi.ErrConsistencyTimeUnavailable

	w := httptest.NewRecorder()
	f.h.GetConsistencyTime(w, httptest.NewRequest(http.MethodGet, "/entity/consistency-time", nil).WithContext(f.ctx))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", w.Code, w.Body.String())
	}
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &pd)
	if pd.Properties["errorCode"] != common.ErrCodeConsistencyTimeUnavailable {
		t.Fatalf("errorCode = %v, body %s", pd.Properties["errorCode"], w.Body.String())
	}
}
