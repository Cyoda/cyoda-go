package main

import "sync"

// calloutRecord is one calculation request as this client received it. The
// pass itself is never recorded — only whether one came with the work.
type calloutRecord struct {
	Seq         int              `json:"seq"`  // 1-based arrival order at this client
	Kind        string           `json:"kind"` // processor | criterion | function
	Name        string           `json:"name"`
	RequestID   string           `json:"requestId"`
	EventID     string           `json:"eventId"` // the CloudEvent id
	EntityID    string           `json:"entityId"`
	PassPresent bool             `json:"passPresent"`
	Callback    *callbackOutcome `json:"callback,omitempty"`
}

// callbackOutcome is what the two doors answered to a late callback.
type callbackOutcome struct {
	HTTPStatus       int    `json:"httpStatus"`
	HTTPErrorCode    string `json:"httpErrorCode,omitempty"`
	GRPCAttempted    bool   `json:"grpcAttempted"`
	GRPCSuccess      bool   `json:"grpcSuccess"`
	GRPCErrorCode    string `json:"grpcErrorCode,omitempty"`
	GRPCErrorMessage string `json:"grpcErrorMessage,omitempty"`
	Error            string `json:"error,omitempty"` // the callback could not be made at all
}

// recordDoc is what the control endpoint serves.
type recordDoc struct {
	MemberID string          `json:"memberId"`
	Received []calloutRecord `json:"received"`
}

type recorder struct {
	mu       sync.Mutex
	memberID string
	records  []calloutRecord
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) setMemberID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memberID = id
}

// add appends rec and returns the sequence number it was given.
func (r *recorder) add(rec calloutRecord) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.Seq = len(r.records) + 1
	r.records = append(r.records, rec)
	return rec.Seq
}

func (r *recorder) setCallback(seq int, out callbackOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq >= 1 && seq <= len(r.records) {
		r.records[seq-1].Callback = &out
	}
}

func (r *recorder) snapshot() recordDoc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recordDoc{MemberID: r.memberID, Received: append([]calloutRecord{}, r.records...)}
}
