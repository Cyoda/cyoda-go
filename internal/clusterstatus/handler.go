package clusterstatus

import (
	"context"
	"net/http"
	"time"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// The ranges of the GET /platform/cluster query parameters, and the wait's
// default bound.
const (
	maxWaitForNodes  = 1000
	maxTimeoutMs     = 600_000
	defaultTimeoutMs = 30_000
)

// Reader reads the cluster status; *Service is one.
type Reader interface {
	Read(ctx context.Context, waitFor int, timeout time.Duration) (Report, error)
}

// Handler serves GET /platform/cluster.
type Handler struct {
	svc   Reader
	guard auth.OperatorGuard
}

// NewHandler returns the handler.
func NewHandler(svc Reader, guard auth.OperatorGuard) *Handler {
	return &Handler{svc: svc, guard: guard}
}

// GetPlatformCluster implements GET /platform/cluster. The answer is 200 when
// the cluster is operational and 503 when it is not, with the same body: a
// status, not a problem.
func (h *Handler) GetPlatformCluster(w http.ResponseWriter, r *http.Request, params genapi.GetPlatformClusterParams) {
	if !h.guard.Require(w, r) {
		return
	}
	waitFor, timeout, appErr := readParams(params)
	if appErr != nil {
		common.WriteError(w, r, appErr)
		return
	}
	report, err := h.svc.Read(r.Context(), waitFor, timeout)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to read the cluster status", err))
		return
	}
	status := http.StatusServiceUnavailable
	if report.Operational {
		status = http.StatusOK
	}
	common.WriteJSON(w, status, dtoOf(report))
}

// readParams checks the ranges and answers the wait: waitFor 0 without
// waitForNodes, and the timeout, defaulted when timeoutMs is absent.
func readParams(p genapi.GetPlatformClusterParams) (int, time.Duration, *common.AppError) {
	if p.WaitForNodes != nil && (*p.WaitForNodes < 1 || *p.WaitForNodes > maxWaitForNodes) {
		return 0, 0, paramError("waitForNodes", "waitForNodes must be 1 to 1000")
	}
	if p.TimeoutMs != nil {
		if *p.TimeoutMs < 1 || *p.TimeoutMs > maxTimeoutMs {
			return 0, 0, paramError("timeoutMs", "timeoutMs must be 1 to 600000")
		}
		if p.WaitForNodes == nil {
			return 0, 0, paramError("timeoutMs", "timeoutMs is given only together with waitForNodes")
		}
	}
	if p.WaitForNodes == nil {
		return 0, 0, nil
	}
	ms := int32(defaultTimeoutMs)
	if p.TimeoutMs != nil {
		ms = *p.TimeoutMs
	}
	return int(*p.WaitForNodes), time.Duration(ms) * time.Millisecond, nil
}

func paramError(name, msg string) *common.AppError {
	appErr := common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, msg)
	appErr.Props = map[string]any{"parameter": name}
	return appErr
}

// dtoOf builds the body. Its arrays are never null: both are required.
func dtoOf(rep Report) genapi.ClusterStatusDto {
	dto := genapi.ClusterStatusDto{
		Operational: rep.Operational,
		Counts:      make([]genapi.ClusterStateCountDto, 0, len(rep.Counts)),
		Nodes:       make([]genapi.ClusterNodeStatusDto, 0, len(rep.Nodes)),
	}
	for _, c := range rep.Counts {
		item := genapi.ClusterStateCountDto{
			Reachability: genapi.ClusterStateCountDtoReachability(c.Reachability),
			Count:        int32(c.Count), // at most one per directory member
		}
		if c.State != "" {
			st := genapi.ClusterStateCountDtoState(c.State)
			item.State = &st
		}
		dto.Counts = append(dto.Counts, item)
	}
	for _, e := range rep.Nodes {
		item := genapi.ClusterNodeStatusDto{Id: e.ID, Reachability: genapi.ClusterNodeStatusDtoReachability(e.Reachability)}
		if e.State != "" {
			st := genapi.ClusterNodeStatusDtoState(e.State)
			item.State = &st
		}
		if s := e.Status; s != nil {
			db, version := s.Database, s.Version
			item.Database, item.Version = &db, &version
			item.Clock = &genapi.ClusterNodeClockDto{
				Controllable: s.Clock.Controllable,
				// A peer's answer above the int64 range fails its validation;
				// this node's own setting counts up from zero.
				Version: int64(s.Clock.Version),
				Writer:  s.Clock.Writer,
			}
		}
		dto.Nodes = append(dto.Nodes, item)
	}
	return dto
}
