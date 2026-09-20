package grpc

import (
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The keep-alive config reaches the service: with a 20ms/60ms configuration
// a silent member is evicted in well under a second (it would take 30s at
// the old hard-wired defaults).
func TestNewServer_KeepAliveConfigReachesService(t *testing.T) {
	srv := NewServer(&fixedAuthService{uc: m2mUser()}, NewMemberRegistry(), nil, nil, nil, nil, nil, noCalloutJoiner(nil, nil), nil,
		"n", false, 0, true, nil, KeepAliveConfig{Interval: 20 * time.Millisecond, Timeout: 60 * time.Millisecond})
	if srv.service.keepAliveInterval != 20*time.Millisecond || srv.service.keepAliveTimeout != 60*time.Millisecond {
		t.Fatalf("service keep-alive = %v/%v, want 20ms/60ms", srv.service.keepAliveInterval, srv.service.keepAliveTimeout)
	}
}

func m2mUser() *spi.UserContext {
	return &spi.UserContext{UserID: "m", UserName: "m", Tenant: spi.Tenant{ID: "t", Name: "t"}, Roles: []string{"ROLE_M2M"}}
}
