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
			role: "admin",
			want: false,
		},
		{
			name: "absent claims fails closed",
			ce:   ceWith(map[string]string{"authtype": "user", "authid": "alice"}),
			role: "admin",
			want: false,
		},
		{
			name: "empty claims fails closed",
			ce:   ceWith(map[string]string{"authtype": "user", "authid": "alice", "authclaims": ""}),
			role: "admin",
			want: false,
		},
		{
			name: "system authtype fails closed even when role listed",
			ce:   ceWith(map[string]string{"authtype": "system", "authid": "sys", "authclaims": "admin,editor"}),
			role: "admin",
			want: false,
		},
		{
			name: "role absent from claims",
			ce:   ceWith(map[string]string{"authtype": "user", "authid": "alice", "authclaims": "editor,viewer"}),
			role: "admin",
			want: false,
		},
		{
			name: "happy path user",
			ce:   ceWith(map[string]string{"authtype": "user", "authid": "alice", "authclaims": "admin,editor"}),
			role: "admin",
			want: true,
		},
		{
			name: "happy path service",
			ce:   ceWith(map[string]string{"authtype": "service", "authid": "svc-1", "authclaims": "admin,editor"}),
			role: "admin",
			want: true,
		},
		{
			name: "unset authtype fails closed even with claims (allowlist, not denylist)",
			ce:   ceWith(map[string]string{"authid": "x", "authclaims": "admin,editor"}),
			role: "admin",
			want: false,
		},
		{
			name: "unrecognized authtype fails closed even with claims",
			ce:   ceWith(map[string]string{"authtype": "superuser", "authid": "x", "authclaims": "admin,editor"}),
			role: "admin",
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
