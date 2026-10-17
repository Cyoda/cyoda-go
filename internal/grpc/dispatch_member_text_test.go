package grpc

import (
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestLocalDispatch_MemberText(t *testing.T) {
	long := strings.Repeat("é", maxMemberMessageRunes+88)
	tests := []struct {
		name        string
		memberErr   string
		wantText    string
		wantMessage string
	}{
		{"the member's text", "boom", "boom", "boom"},
		{"no text", "", "", "processor returned failure"},
		{"a long text is bounded as boundMemberText bounds it", long, boundMemberText(long), boundMemberText(long)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, registry, memberID, sentCh := setupTestDispatcher(t)
			member := registry.Get(memberID)
			go func() {
				<-sentCh
				member.CompleteRequest("r1", &ProcessingResponse{Success: false, Error: tt.memberErr})
			}()
			failure, ctxErr := kindOf(d, testContext(), member, rawCall(5*time.Second))
			assertKindAndCode(t, failure, ctxErr, contract.MemberFailed, "")
			if failure.MemberText != tt.wantText || failure.Message != tt.wantMessage {
				t.Errorf("MemberText/Message = %q/%q, want %q/%q", failure.MemberText, failure.Message, tt.wantText, tt.wantMessage)
			}
		})
	}
}
