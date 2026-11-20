package api_test

import (
	"testing"

	genapi "github.com/cyoda/cyoda-go/api"
	internalapi "github.com/cyoda/cyoda-go/internal/api"
)

var _ genapi.ServerInterface = (*internalapi.Server)(nil)

func TestNewServer(t *testing.T) {
	s := internalapi.NewServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}
