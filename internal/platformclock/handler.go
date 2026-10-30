package platformclock

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// maxBodyBytes bounds a POST /platform/clock body.
const maxBodyBytes = 1 << 20

// maxRateLiteral bounds the length of a rate literal. What bounds the cost of
// parsing one is Go's exponent cap: big.Rat refuses exponents above about 1e6.
const maxRateLiteral = 64

// Controller is the clock service the handler drives; *Service is one.
type Controller interface {
	State() State
	Change(ctx context.Context, ch Change) (State, error)
}

// Handler serves GET and POST /platform/clock.
type Handler struct {
	svc     Controller
	guard   auth.OperatorGuard
	enabled bool
}

// NewHandler returns the handler. With enabled false, svc is not used: GET
// answers real time at rate 1 and POST refuses every change.
func NewHandler(svc Controller, guard auth.OperatorGuard, enabled bool) *Handler {
	return &Handler{svc: svc, guard: guard, enabled: enabled}
}

// clockBody is the PlatformClockDto response; rate is a float64 so that a
// six-digit rate is rendered as written.
type clockBody struct {
	Now          time.Time `json:"now"`
	Rate         float64   `json:"rate"`
	Controllable bool      `json:"controllable"`
}

func bodyOf(st State) clockBody {
	return clockBody{Now: st.Now.UTC(), Rate: st.Rate(), Controllable: st.Controllable}
}

// StateOf is the state both doors answer: svc's state when clock control is
// enabled, else real time at rate 1, not controllable (svc is not used).
func StateOf(svc interface{ State() State }, enabled bool) State {
	if !enabled {
		return State{Now: Real().Now(), RatePPM: 1_000_000, Controllable: false}
	}
	return svc.State()
}

// GetPlatformClock implements GET /platform/clock.
func (h *Handler) GetPlatformClock(w http.ResponseWriter, r *http.Request) {
	common.WriteJSON(w, http.StatusOK, bodyOf(StateOf(h.svc, h.enabled)))
}

// member is a body member that records it was present and refuses JSON null:
// a member given as null is refused, not read as absent.
type member[T any] struct {
	v       T
	present bool
}

func (m *member[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("a member must not be null")
	}
	if err := json.Unmarshal(data, &m.v); err != nil {
		return err
	}
	m.present = true
	return nil
}

// rateLiteral keeps a JSON number as written.
type rateLiteral string

func (l *rateLiteral) UnmarshalJSON(data []byte) error {
	if data[0] == '"' || data[0] == 't' || data[0] == 'f' ||
		data[0] == '{' || data[0] == '[' {
		return errors.New("rate must be a number")
	}
	*l = rateLiteral(data)
	return nil
}

type changeRequest struct {
	Time        member[time.Time]   `json:"time"`
	AdvanceByMs member[int64]       `json:"advanceByMs"`
	Rate        member[rateLiteral] `json:"rate"`
}

// maxAdvanceMs is the largest advance whose duration fits time.Duration.
const maxAdvanceMs = int64(1<<63-1) / int64(time.Millisecond)

func badRequest(msg string) *common.AppError {
	return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, msg)
}

// ratePPM converts the decimal text of a rate to parts per million exactly.
func ratePPM(lit string) (int64, *common.AppError) {
	if len(lit) > maxRateLiteral {
		return 0, badRequest("rate is not a number from 0.1 to 100 with at most six fractional digits")
	}
	// lit is a JSON number; one big.Rat refuses has an exponent past its cap.
	r, ok := new(big.Rat).SetString(lit)
	if !ok {
		return 0, badRequest("rate is not a number from 0.1 to 100 with at most six fractional digits")
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	if !r.IsInt() {
		return 0, badRequest("rate has more than six fractional digits")
	}
	if r.Cmp(big.NewRat(minRatePPM, 1)) < 0 || r.Cmp(big.NewRat(maxRatePPM, 1)) > 0 {
		return 0, badRequest("rate must be from 0.1 to 100")
	}
	return r.Num().Int64(), nil
}

// parseChange validates the body and returns the change. The service
// resolves an advance against the clock it moves and checks its range.
func parseChange(req changeRequest) (Change, *common.AppError) {
	var ch Change
	if !req.Time.present && !req.AdvanceByMs.present && !req.Rate.present {
		return ch, badRequest("the body names no change: give time, advanceByMs or rate")
	}
	if req.Time.present && req.AdvanceByMs.present {
		return ch, badRequest("give time or advanceByMs, not both")
	}
	if req.Time.present {
		t := req.Time.v.UTC()
		if t.Before(minTime) || t.After(maxSetTime) {
			return ch, badRequest("time must lie in the years 1970 to 9000")
		}
		ch.At = &t
	}
	if req.AdvanceByMs.present {
		ms := req.AdvanceByMs.v
		if ms < 1 || ms > maxAdvanceMs {
			return ch, badRequest("advanceByMs must be a positive number of milliseconds that fits a duration")
		}
		d := time.Duration(ms) * time.Millisecond
		ch.AdvanceBy = &d
	}
	if req.Rate.present {
		ppm, appErr := ratePPM(string(req.Rate.v))
		if appErr != nil {
			return ch, appErr
		}
		ch.RatePPM = &ppm
	}
	return ch, nil
}

// ChangePlatformClock implements POST /platform/clock. The checks run in the
// order: operator, feature enabled, body, then the service's own refusals.
func (h *Handler) ChangePlatformClock(w http.ResponseWriter, r *http.Request) {
	if !h.guard.Require(w, r) {
		return
	}
	if !h.enabled {
		common.WriteError(w, r, common.Operational(http.StatusForbidden, common.ErrCodeClockControlDisabled,
			"this node was not started with clock control enabled"))
		return
	}
	var req changeRequest
	if err := common.DecodeBoundedJSON(w, r, maxBodyBytes, &req); err != nil {
		common.WriteError(w, r, common.InvalidBody(err))
		return
	}
	ch, appErr := parseChange(req)
	if appErr != nil {
		common.WriteError(w, r, appErr)
		return
	}
	st, err := h.svc.Change(r.Context(), ch)
	if err != nil {
		common.WriteError(w, r, classify(err))
		return
	}
	common.WriteJSON(w, http.StatusOK, bodyOf(st))
}

func classify(err error) *common.AppError {
	var before *BeforeRecordedTimeError
	if errors.As(err, &before) {
		e := common.Operational(http.StatusConflict, common.ErrCodeClockBeforeRecordedTime,
			fmt.Sprintf("the store has recorded a time later than the requested time; latest recorded time %s",
				stamp(before.Latest)))
		e.Props = map[string]any{"latestRecordedTime": stamp(before.Latest)}
		return e
	}
	if errors.As(err, new(*OutOfRangeError)) {
		return badRequest(err.Error())
	}
	if errors.Is(err, spi.ErrConsistencyTimeUnavailable) {
		return common.Operational(http.StatusServiceUnavailable, common.ErrCodeConsistencyTimeUnavailable,
			"the consistency time could not be certified in time — retry").AsRetryable().WithCause(err)
	}
	return common.Internal("failed to change the platform clock", err)
}
