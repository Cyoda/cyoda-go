package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// healthServer is the standard grpc.health.v1.Health service, Check only.
// Check answers for the server as a whole (the empty service name): SERVING
// while readiness is nil (healthy and not draining), NOT_SERVING otherwise.
// The answer carries the status alone; it does not state why the node is not
// ready, because Check is the one method a caller reaches without a token.
// Watch and List are left unimplemented and keep full authentication.
type healthServer struct {
	grpc_health_v1.UnimplementedHealthServer
	readiness func() error
}

func (h *healthServer) Check(_ context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	if req.GetService() != "" {
		return nil, status.Error(codes.NotFound, "unknown service")
	}
	if h.readiness() != nil {
		return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING}, nil
	}
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}
