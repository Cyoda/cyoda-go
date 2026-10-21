package events_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/strictjson"

	"github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// A CloudEvent's data is a client's typed JSON. Every generated type decodes
// itself, through decodeWithUseNumber, so the rule lives there: a member name
// spells a schema property exactly, once. encoding/json (v1) read "Success"
// as success — and the default check, which looks the key up exactly, then
// forced it back to true — and let a duplicate id replace the first.

const createData = `"dataFormat":"JSON","payload":{"model":{"name":"m","version":1},"data":{}}`

func TestEventData_MemberNamesAreExact(t *testing.T) {
	for name, tc := range map[string]struct{ data, want string }{
		"case variant": {
			`{"id":"e1",` + createData + `,"Success":false}`,
			`invalid JSON at "": unknown member name "Success"`},
		"duplicate": {
			`{"id":"e1","id":"e2",` + createData + `}`,
			`invalid JSON at "": duplicate member name "id"`},
		"unknown": {
			`{"id":"e1",` + createData + `,"extra":1}`,
			`invalid JSON at "": unknown member name "extra"`},
		// The required check runs first, and "Name" is not "name".
		"nested case variant": {
			`{"id":"e1","dataFormat":"JSON","payload":{"model":{"Name":"m","version":1},"data":{}}}`,
			`invalid JSON at "/payload/model": field name: required`},
	} {
		t.Run(name, func(t *testing.T) {
			var req events.EntityCreateRequestJson
			err := strictjson.Decode([]byte(tc.data), &req)
			if err == nil {
				t.Fatalf("accepted %s as %+v", tc.data, req)
			}
			if err.Error() != tc.want {
				t.Errorf("error\n got: %s\nwant: %s", err, tc.want)
			}
		})
	}
}

// TestEventData_NullIsNotAnEvent: a JSON null holds none of the required
// members, so it is refused as missing them. Before, the required checks were
// skipped for null and a zero event, success forced to true, went on.
func TestEventData_NullIsNotAnEvent(t *testing.T) {
	var req events.EntityCreateRequestJson
	err := strictjson.Decode([]byte(`null`), &req)
	if err == nil || !strings.Contains(err.Error(), ": required") {
		t.Fatalf("err = %v, want a required-member refusal", err)
	}
}

// TestEventData_ErrorsNameNoGoType: the decode error reaches the client in
// the response envelope, so it names the member and where, never a Go type.
func TestEventData_ErrorsNameNoGoType(t *testing.T) {
	for data, want := range map[string]string{
		`{"dataFormat":"JSON","payload":{"model":{"name":"m","version":1},"data":{}}}`:                          `invalid JSON at "": field id: required`,
		`{"id":"e1","dataFormat":"JSON","payload":{"model":{"version":1},"data":{}}}`:                           `invalid JSON at "/payload/model": field name: required`,
		`{"id":"e1","dataFormat":"JSON","payload":{"model":{"name":"m","version":1},"data":{}},"success":null}`: `invalid JSON at "": field success: must not be null`,
	} {
		var req events.EntityCreateRequestJson
		err := strictjson.Decode([]byte(data), &req)
		if err == nil || err.Error() != want {
			t.Errorf("%s:\n got: %v\nwant: %s", data, err, want)
		}
	}
}

// TestEventData_CloudClientWireFormat: the messages Cyoda Cloud's own client
// sends (copied from its wire-format fixtures, client/src/test/resources/
// wire-format) decode here, so the strict rule refuses nothing a Cloud
// client writes.
func TestEventData_CloudClientWireFormat(t *testing.T) {
	targets := map[string]func() any{
		"CalculationMemberJoinEvent.json":         func() any { return &events.CalculationMemberJoinEventJson{} },
		"EntityCreateRequest.json":                func() any { return &events.EntityCreateRequestJson{} },
		"EntityGetAllRequest.json":                func() any { return &events.EntityGetAllRequestJson{} },
		"EntityModelImportRequest.json":           func() any { return &events.EntityModelImportRequestJson{} },
		"EntityProcessorCalculationResponse.json": func() any { return &events.EntityProcessorCalculationResponseJson{} },
		"EntityCriteriaCalculationResponse.json":  func() any { return &events.EntityCriteriaCalculationResponseJson{} },
	}
	files, err := filepath.Glob("testdata/cloud-client-wire-format/*.json")
	if err != nil || len(files) != len(targets) {
		t.Fatalf("fixtures: %v, %d files, want %d", err, len(files), len(targets))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dst := targets[filepath.Base(f)]()
		if err := strictjson.Decode(data, dst); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

// TestJoinEvent_JoinedLegalEntityID: `cyoda help grpc` documents an optional
// joinedLegalEntityId on the join event, which the server checks against the
// token's tenant; the event's schema declares it, so the strict decode reads
// it rather than refusing it.
func TestJoinEvent_JoinedLegalEntityID(t *testing.T) {
	var join events.CalculationMemberJoinEventJson
	if err := strictjson.Decode([]byte(`{"id":"j1","tags":["a"],"joinedLegalEntityId":"acme"}`), &join); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if join.JoinedLegalEntityID == nil || *join.JoinedLegalEntityID != "acme" {
		t.Errorf("JoinedLegalEntityID = %v", join.JoinedLegalEntityID)
	}
}
