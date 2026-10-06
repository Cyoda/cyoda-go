package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
)

// consistencyTimeTM answers ConsistencyTime with the instant or error the test
// chose; nothing else on it is used.
type consistencyTimeTM struct {
	spi.TransactionManager
	at  time.Time
	err error
}

func (m consistencyTimeTM) ConsistencyTime(context.Context) (time.Time, error) { return m.at, m.err }

func consistencyTimeCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "ct-user",
		Kind:   spi.PrincipalUser,
		Tenant: spi.Tenant{ID: "ct-tenant", Name: "ct"},
		Roles:  []string{"ROLE_M2M"},
	})
}

func TestEntityConsistencyTimeGet_ReturnsC(t *testing.T) {
	c := time.Date(2026, 10, 5, 14, 3, 7, 123456789, time.UTC)
	svc := &CloudEventsServiceImpl{cons: consistency.New(consistencyTimeTM{at: c})}

	ce, err := svc.EntitySearch(consistencyTimeCtx(), makeCE(EntityConsistencyTimeGetRequest, map[string]any{"id": "ct-1"}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if ce.Type != EntityConsistencyTimeResponse {
		t.Fatalf("type = %q, want %q", ce.Type, EntityConsistencyTimeResponse)
	}
	var env events.EntityConsistencyTimeResponseJson
	validateResponse(t, ce, &env)
	if !env.Success || env.Error != nil {
		t.Fatalf("want success, got %+v", env)
	}
	if env.ConsistencyTime == nil || !env.ConsistencyTime.Equal(c) {
		t.Fatalf("consistencyTime = %v, want %v", env.ConsistencyTime, c)
	}
}

func TestEntityConsistencyTimeGet_Unavailable(t *testing.T) {
	svc := &CloudEventsServiceImpl{cons: consistency.New(consistencyTimeTM{err: spi.ErrConsistencyTimeUnavailable})}

	ce, err := svc.EntitySearch(consistencyTimeCtx(), makeCE(EntityConsistencyTimeGetRequest, map[string]any{"id": "ct-2"}))
	if err != nil {
		t.Fatalf("failures belong in the envelope, got transport error: %v", err)
	}
	var env events.EntityConsistencyTimeResponseJson
	validateResponse(t, ce, &env)
	if env.Success || env.Error == nil {
		t.Fatalf("want a failure envelope, got %+v", env)
	}
	if env.ConsistencyTime != nil {
		t.Errorf("consistencyTime = %v on a failure, want unset", env.ConsistencyTime)
	}
	if env.Error.Code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q, want CLIENT_ERROR", env.Error.Code)
	}
	if !strings.HasPrefix(env.Error.Message, common.ErrCodeConsistencyTimeUnavailable+":") {
		t.Errorf("Error.Message = %q, want the %s prefix", env.Error.Message, common.ErrCodeConsistencyTimeUnavailable)
	}
	if env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Errorf("Error.Retryable = %v, want true", env.Error.Retryable)
	}
}
