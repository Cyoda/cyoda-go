package grpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// mockAuthService is a test double for contract.AuthenticationService.
type mockAuthService struct {
	user *spi.UserContext
	err  error
}

func (m *mockAuthService) Authenticate(_ context.Context, _ *http.Request) (*spi.UserContext, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.user, nil
}

// mockServerStream is a minimal test double for grpc.ServerStream.
type mockServerStream struct {
	googlegrpc.ServerStream
	ctx context.Context
}

func (m *mockServerStream) Context() context.Context { return m.ctx }

func TestInterceptor_UnarySuccess(t *testing.T) {
	uc := &spi.UserContext{
		UserID:   "user-1",
		UserName: "alice",
		Tenant:   spi.Tenant{ID: "tenant-1", Name: "Tenant One"},
		Roles:    []string{"admin"},
	}
	authSvc := &mockAuthService{user: uc}
	interceptor := UnaryAuthInterceptor(authSvc)

	md := metadata.Pairs("authorization", "Bearer test-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	var handlerCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCtx = ctx
		return "ok", nil
	}

	resp, err := interceptor(ctx, "request", &googlegrpc.UnaryServerInfo{}, handler)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected resp 'ok', got %v", resp)
	}

	got := spi.GetUserContext(handlerCtx)
	if got == nil {
		t.Fatal("expected UserContext in handler context, got nil")
	}
	if got.UserID != "user-1" {
		t.Errorf("expected UserID 'user-1', got %q", got.UserID)
	}
	if got.UserName != "alice" {
		t.Errorf("expected UserName 'alice', got %q", got.UserName)
	}
	if got.Tenant.ID != "tenant-1" {
		t.Errorf("expected TenantID 'tenant-1', got %q", got.Tenant.ID)
	}
}

func TestInterceptor_UnaryAuthFailure(t *testing.T) {
	authSvc := &mockAuthService{err: errors.New("invalid token")}
	interceptor := UnaryAuthInterceptor(authSvc)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})

	handler := func(_ context.Context, _ any) (any, error) {
		t.Fatal("handler should not be called on auth failure")
		return nil, nil
	}

	_, err := interceptor(ctx, "request", &googlegrpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}, handler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("expected code Unauthenticated, got %v", st.Code())
	}
	if st.Message() != "authentication failed" {
		t.Errorf("expected generic message 'authentication failed', got %q", st.Message())
	}
	if strings.Contains(st.Message(), "invalid token") {
		t.Error("error message must not contain internal auth error details")
	}
}

func TestInterceptor_StreamSuccess(t *testing.T) {
	uc := &spi.UserContext{
		UserID:   "user-2",
		UserName: "bob",
		Tenant:   spi.Tenant{ID: "tenant-2", Name: "Tenant Two"},
		Roles:    []string{"reader"},
	}
	authSvc := &mockAuthService{user: uc}
	interceptor := StreamAuthInterceptor(authSvc)

	md := metadata.Pairs("authorization", "Bearer stream-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	stream := &mockServerStream{ctx: ctx}

	var handlerStream googlegrpc.ServerStream
	handler := func(_ any, ss googlegrpc.ServerStream) error {
		handlerStream = ss
		return nil
	}

	err := interceptor(nil, stream, &googlegrpc.StreamServerInfo{}, handler)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	got := spi.GetUserContext(handlerStream.Context())
	if got == nil {
		t.Fatal("expected UserContext in stream context, got nil")
	}
	if got.UserID != "user-2" {
		t.Errorf("expected UserID 'user-2', got %q", got.UserID)
	}
	if got.Tenant.ID != "tenant-2" {
		t.Errorf("expected TenantID 'tenant-2', got %q", got.Tenant.ID)
	}
}

func TestInterceptor_StreamAuthFailure(t *testing.T) {
	authSvc := &mockAuthService{err: errors.New("expired token")}
	interceptor := StreamAuthInterceptor(authSvc)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	stream := &mockServerStream{ctx: ctx}

	handler := func(_ any, _ googlegrpc.ServerStream) error {
		t.Fatal("handler should not be called on auth failure")
		return nil
	}

	err := interceptor(nil, stream, &googlegrpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}, handler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("expected code Unauthenticated, got %v", st.Code())
	}
	if st.Message() != "authentication failed" {
		t.Errorf("expected generic message 'authentication failed', got %q", st.Message())
	}
	if strings.Contains(st.Message(), "expired token") {
		t.Error("error message must not contain internal auth error details")
	}
}

// TestInterceptor_UnaryRejectsClaimOutsideCheck proves gRPC inherits the
// claim checks: the interceptor delegates to the same AuthenticationService
// the HTTP middleware uses, so a tenant claim outside the tenant grammar, or a
// user claim outside the user-id check, is Unauthenticated here too, with the
// same generic message.
func TestInterceptor_UnaryRejectsClaimOutsideCheck(t *testing.T) {
	for name, tc := range map[string]struct{ user, tenant string }{
		"tenant": {user: "user-1", tenant: "../victim"},
		"user":   {user: "user\nvictim", tenant: "tenant-1"},
	} {
		t.Run(name, func(t *testing.T) {
			assertUnaryRejectsClaims(t, tc.user, tc.tenant)
		})
	}
}

// assertUnaryRejectsClaims signs a first-party token carrying user and tenant,
// both chosen so that the rejected one contains "victim", and asserts the
// interceptor rejects it without the value reaching the error or a log record.
func assertUnaryRejectsClaims(t *testing.T, user, tenant string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const kid = "grpc-claim-test"
	const issuer = "cyoda-grpc-test"

	ks := auth.NewInMemoryKeyStore()
	if err := ks.Save(&auth.KeyPair{
		KID:        kid,
		Audience:   "client",
		Algorithm:  "RS256",
		PublicKey:  &priv.PublicKey,
		PrivateKey: priv,
		Active:     true,
		ValidFrom:  time.Now().Add(-time.Minute),
	}, auth.RotateOptions{}); err != nil {
		t.Fatalf("save key: %v", err)
	}

	validator := auth.NewValidatorFromSource(auth.NewLocalKeySource(ks), issuer)
	authSvc := auth.NewDelegatingAuthenticator(validator)
	interceptor := UnaryAuthInterceptor(authSvc)

	now := time.Now()
	tok, err := auth.Sign(map[string]any{
		"iss":          issuer,
		"sub":          user,
		"caas_user_id": user,
		"caas_org_id":  tenant,
		"iat":          float64(now.Unix()),
		"exp":          float64(now.Add(time.Hour).Unix()),
	}, priv, kid)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.MD{"authorization": []string{"Bearer " + tok}})

	handler := func(_ context.Context, _ any) (any, error) {
		t.Fatal("handler must not be called when a claim is rejected")
		return nil, nil
	}

	// Capture the default logger for the duration of the call: the rejected
	// claim is attacker-chosen, so it must reach neither the envelope nor a
	// log field.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	_, err = interceptor(ctx, "request",
		&googlegrpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}, handler)
	if err == nil {
		t.Fatal("expected an error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
	if st.Message() != "authentication failed" {
		t.Errorf("message = %q, want the generic %q", st.Message(), "authentication failed")
	}
	// The whole error, details included, and every log record written while
	// the claim was rejected: neither may carry the rejected value. The
	// emptiness guard keeps the log assertion from passing for the wrong
	// reason — the rejection path does write records, and if it stopped, a
	// silent buffer would look like a clean one.
	if logBuf.Len() == 0 {
		t.Fatal("no log record captured; the log assertion below would be vacuous")
	}
	if strings.Contains(err.Error(), "victim") {
		t.Errorf("gRPC error echoes the rejected claim: %v", err)
	}
	if strings.Contains(logBuf.String(), "victim") {
		t.Errorf("a log record echoes the rejected claim:\n%s", logBuf.String())
	}
}
