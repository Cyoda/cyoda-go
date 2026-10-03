package authctx

import (
	"reflect"
	"testing"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

func ceWith(attrs map[string]string) *cepb.CloudEvent {
	ce := &cepb.CloudEvent{Attributes: make(map[string]*cepb.CloudEvent_CloudEventAttributeValue)}
	for k, v := range attrs {
		ce.Attributes[k] = &cepb.CloudEvent_CloudEventAttributeValue{
			Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: v},
		}
	}
	return ce
}

func TestType(t *testing.T) {
	if got := Type(nil); got != "" {
		t.Errorf("Type(nil) = %q, want empty", got)
	}
	ce := ceWith(map[string]string{"authtype": "user"})
	if got := Type(ce); got != "user" {
		t.Errorf("Type() = %q, want %q", got, "user")
	}
	if got := Type(ceWith(nil)); got != "" {
		t.Errorf("Type() with absent attr = %q, want empty", got)
	}
}

func TestID(t *testing.T) {
	if got := ID(nil); got != "" {
		t.Errorf("ID(nil) = %q, want empty", got)
	}
	ce := ceWith(map[string]string{"authid": "alice"})
	if got := ID(ce); got != "alice" {
		t.Errorf("ID() = %q, want %q", got, "alice")
	}
}

func TestRoles(t *testing.T) {
	if got := Roles(nil); got != nil {
		t.Errorf("Roles(nil) = %v, want nil", got)
	}
	if got := Roles(ceWith(nil)); got != nil {
		t.Errorf("Roles() with absent claims = %v, want nil", got)
	}
	if got := Roles(ceWith(map[string]string{"authclaims": ""})); got != nil {
		t.Errorf("Roles() with empty claims = %v, want nil", got)
	}
	ce := ceWith(map[string]string{"authclaims": "admin,editor,viewer"})
	want := []string{"admin", "editor", "viewer"}
	if got := Roles(ce); !reflect.DeepEqual(got, want) {
		t.Errorf("Roles() = %v, want %v", got, want)
	}
}

func TestExecutorType(t *testing.T) {
	if got := ExecutorType(nil); got != "" {
		t.Errorf("ExecutorType(nil) = %q, want empty", got)
	}
	ce := ceWith(map[string]string{"authtype": "user", "authexectype": "service"})
	if got := ExecutorType(ce); got != "service" {
		t.Errorf("ExecutorType() = %q, want %q", got, "service")
	}
	if got := ExecutorType(ceWith(nil)); got != "" {
		t.Errorf("ExecutorType() with absent attr = %q, want empty", got)
	}
}

func TestExecutorID(t *testing.T) {
	if got := ExecutorID(nil); got != "" {
		t.Errorf("ExecutorID(nil) = %q, want empty", got)
	}
	ce := ceWith(map[string]string{"authid": "alice", "authexecid": "obo-client"})
	if got := ExecutorID(ce); got != "obo-client" {
		t.Errorf("ExecutorID() = %q, want %q", got, "obo-client")
	}
}

// TestRequire pins the gate: it admits only a service executor holding role.
// The attributed principal (authtype/authid) plays no part — an on-behalf-of
// callout attributed to a user passes on its client's roles, and a scheduled
// fire, executed by the system, never does.
func TestRequire(t *testing.T) {
	tests := []struct {
		name string
		ce   *cepb.CloudEvent
		role string
		want bool
	}{
		{
			name: "nil event fails closed",
			ce:   nil,
			role: "ROLE_M2M",
			want: false,
		},
		{
			name: "service executor for a user holding the role",
			ce:   ceWith(map[string]string{"authtype": "user", "authid": "alice", "authexectype": "service", "authexecid": "obo", "authclaims": "ROLE_M2M"}),
			role: "ROLE_M2M",
			want: true,
		},
		{
			name: "service executor for itself holding the role",
			ce:   ceWith(map[string]string{"authtype": "service", "authid": "c1", "authexectype": "service", "authexecid": "c1", "authclaims": "ROLE_ADMIN,ROLE_M2M"}),
			role: "ROLE_M2M",
			want: true,
		},
		{
			name: "system executor fails closed even when the role is listed",
			ce:   ceWith(map[string]string{"authtype": "user", "authid": "bob", "authexectype": "system", "authexecid": "system", "authclaims": "ROLE_M2M"}),
			role: "ROLE_M2M",
			want: false,
		},
		{
			name: "user executor fails closed even when the role is listed",
			ce:   ceWith(map[string]string{"authtype": "user", "authid": "alice", "authexectype": "user", "authexecid": "alice", "authclaims": "ROLE_M2M"}),
			role: "ROLE_M2M",
			want: false,
		},
		{
			name: "absent executor type fails closed even when authtype is service",
			ce:   ceWith(map[string]string{"authtype": "service", "authid": "c1", "authclaims": "ROLE_M2M"}),
			role: "ROLE_M2M",
			want: false,
		},
		{
			name: "unrecognized executor type fails closed",
			ce:   ceWith(map[string]string{"authtype": "service", "authid": "c1", "authexectype": "root", "authexecid": "c1", "authclaims": "ROLE_M2M"}),
			role: "ROLE_M2M",
			want: false,
		},
		{
			name: "absent claims fails closed",
			ce:   ceWith(map[string]string{"authtype": "service", "authid": "c1", "authexectype": "service", "authexecid": "c1"}),
			role: "ROLE_M2M",
			want: false,
		},
		{
			name: "empty claims fails closed",
			ce:   ceWith(map[string]string{"authtype": "service", "authid": "c1", "authexectype": "service", "authexecid": "c1", "authclaims": ""}),
			role: "ROLE_M2M",
			want: false,
		},
		{
			name: "role absent from claims",
			ce:   ceWith(map[string]string{"authtype": "service", "authid": "c1", "authexectype": "service", "authexecid": "c1", "authclaims": "ROLE_ADMIN"}),
			role: "ROLE_M2M",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Require(tt.ce, tt.role); got != tt.want {
				t.Errorf("Require() = %v, want %v", got, tt.want)
			}
		})
	}
}
