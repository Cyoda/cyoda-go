package entity_test

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
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// submitTimeTM answers GetSubmitTime with an instant the test chose.
type submitTimeTM struct {
	spi.TransactionManager
	submit time.Time
}

func (s submitTimeTM) GetSubmitTime(context.Context, string) (time.Time, error) {
	return s.submit, nil
}

func TestTransitions_FenceOnPointInTimeAndTransactionID(t *testing.T) {
	f := newFenceFixture(t, "fence-transitions", true)
	// The entity is deliberately missing: without the fence both calls answer
	// 404, so the refusal is proven to come first.

	call := func(h *entity.Handler, params genapi.GetEntityTransitionsParams) (int, string) {
		id := uuid.New()
		r := httptest.NewRequest(http.MethodGet, "/entity/"+id.String()+"/transitions", nil).WithContext(f.ctx)
		w := httptest.NewRecorder()
		h.GetEntityTransitions(w, r, id, params)
		var pd struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &pd)
		code, _ := pd.Properties["errorCode"].(string)
		return w.Code, code
	}

	later := f.c.Add(time.Millisecond)
	if status, code := call(f.h, genapi.GetEntityTransitionsParams{PointInTime: &later}); status != http.StatusBadRequest || code != common.ErrCodePointInTimeAfterConsistencyTime {
		t.Errorf("pointInTime after C: %d %s", status, code)
	}

	txh := entity.New(f.factory, submitTimeTM{TransactionManager: f.tm, submit: f.c.Add(time.Millisecond)},
		common.NewDefaultUUIDGenerator(), nil, txgate.New(), consistency.New(f.tm))
	txID := uuid.New()
	if status, code := call(txh, genapi.GetEntityTransitionsParams{TransactionId: &txID}); status != http.StatusBadRequest || code != common.ErrCodePointInTimeAfterConsistencyTime {
		t.Errorf("transactionId submit time after C: %d %s", status, code)
	}
}
