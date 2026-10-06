package entity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
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
	f.save(t, "e1", "NEW")

	call := func(h *entity.Handler, query string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "/entity/e1/transitions?"+query, nil).WithContext(f.ctx)
		r.SetPathValue("entityId", "e1")
		w := httptest.NewRecorder()
		h.HandleGetTransitions(w, r)
		var pd struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &pd)
		code, _ := pd.Properties["errorCode"].(string)
		return w.Code, code
	}

	later := f.c.Add(time.Millisecond).Format(time.RFC3339Nano)
	if status, code := call(f.h, "pointInTime="+later); status != http.StatusBadRequest || code != common.ErrCodePointInTimeAfterConsistencyTime {
		t.Errorf("pointInTime after C: %d %s", status, code)
	}

	txh := entity.New(f.factory, submitTimeTM{TransactionManager: f.tm, submit: f.c.Add(time.Millisecond)},
		common.NewDefaultUUIDGenerator(), nil, txgate.New(), consistency.New(f.tm))
	if status, code := call(txh, "transactionId=some-tx"); status != http.StatusBadRequest || code != common.ErrCodePointInTimeAfterConsistencyTime {
		t.Errorf("transactionId submit time after C: %d %s", status, code)
	}
}
