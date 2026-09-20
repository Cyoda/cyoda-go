package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// replyBody is the part of a calculation response these tests read.
type replyBody struct {
	RequestID string `json:"requestId"`
	Success   bool   `json:"success"`
	Error     *struct {
		Message   string `json:"message"`
		Retryable *bool  `json:"retryable"`
	} `json:"error"`
}

func decodeReply(t *testing.T, ce *cepb.CloudEvent) replyBody {
	t.Helper()
	if ce == nil {
		t.Fatal("no reply was produced")
	}
	var body replyBody
	if err := json.Unmarshal([]byte(ce.GetTextData()), &body); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	return body
}

func assertVerdict(t *testing.T, body replyBody, want *bool) {
	t.Helper()
	if body.Success {
		t.Fatal("reply says success=true; want a failure")
	}
	if body.Error == nil {
		t.Fatal("failure reply carries no error node")
	}
	switch {
	case want == nil && body.Error.Retryable != nil:
		t.Errorf("error.retryable = %t; want it absent", *body.Error.Retryable)
	case want != nil && body.Error.Retryable == nil:
		t.Errorf("error.retryable absent; want %t", *want)
	case want != nil && *body.Error.Retryable != *want:
		t.Errorf("error.retryable = %t; want %t", *body.Error.Retryable, *want)
	}
}

func TestCatalogFailure_CarriesVerdict(t *testing.T) {
	yes, no := true, false
	d := &dispatcher{cat: newCatalog(nil, nil)}
	ctx := context.Background()

	processors := []struct {
		name string
		want *bool
	}{
		{"inject-error", nil},
		{"inject-error-retryable", &yes},
		{"inject-error-not-retryable", &no},
	}
	for _, tc := range processors {
		t.Run("processor/"+tc.name, func(t *testing.T) {
			payload := json.RawMessage(fmt.Sprintf(
				`{"requestId":"r-1","entityId":"e-1","processorName":%q,"payload":{"data":{}}}`, tc.name))
			ce, err := d.handleProcessorRequest(ctx, payload, "", "")
			if err != nil {
				t.Fatalf("handleProcessorRequest: %v", err)
			}
			assertVerdict(t, decodeReply(t, ce), tc.want)
		})
	}

	t.Run("criterion", func(t *testing.T) {
		ce, err := d.handleCriteriaRequest(ctx, json.RawMessage(
			`{"requestId":"r-2","entityId":"e-1","criteriaName":"inject-criterion-error-retryable"}`), "")
		if err != nil {
			t.Fatalf("handleCriteriaRequest: %v", err)
		}
		assertVerdict(t, decodeReply(t, ce), &yes)
	})

	t.Run("function", func(t *testing.T) {
		ce, err := d.handleFunctionRequest(ctx, json.RawMessage(
			`{"requestId":"r-3","entityId":"e-1","functionName":"inject-fn-error-retryable"}`), "")
		if err != nil {
			t.Fatalf("handleFunctionRequest: %v", err)
		}
		assertVerdict(t, decodeReply(t, ce), &yes)
	})
}
