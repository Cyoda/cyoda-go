package model_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
)

func TestNew_NilClockPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("model.New with a nil clock did not panic")
		}
	}()
	model.New(nil, nil)
}
