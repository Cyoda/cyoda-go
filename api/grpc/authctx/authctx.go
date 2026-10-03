// Package authctx lets compute-node authors read the CloudEvents AuthContext
// extension that cyoda-go attaches to callout events (see
// internal/grpc/cloudevent.go AttachAuthContext) and apply a fail-closed role
// gate.
//
// A callout names two principals. The attributed principal (Type, ID) is who
// the work is for: the user of an on-behalf-of request, a transaction's origin
// for a processor write-back, the arming principal for a scheduled fire. The
// executor (ExecutorType, ExecutorID) is who does the work: the M2M client
// that made the request, or the system for a scheduled fire. The roles
// (Roles) are the executor's.
//
// Trust basis (spec §10.1): a compute node may rely on the AuthContext only
// if it authenticates the cyoda server endpoint over TLS (server
// verification) — an unauthenticated channel makes the attributes
// unattributable. Application authorization built on authclaims must fail
// closed when claims are absent or empty; this includes the system
// principal, which never carries claims.
package authctx

import (
	"slices"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// Type returns the authtype extension attribute — the attributed principal's
// kind (one of user/service/system) — or "" if ce is nil or the attribute is
// absent.
func Type(ce *cepb.CloudEvent) string {
	return attr(ce, "authtype")
}

// ID returns the authid extension attribute — the attributed principal's id —
// or "" if ce is nil or the attribute is absent.
func ID(ce *cepb.CloudEvent) string {
	return attr(ce, "authid")
}

// ExecutorType returns the authexectype extension attribute — the executor's
// kind (one of user/service/system) — or "" if ce is nil or the attribute is
// absent.
func ExecutorType(ce *cepb.CloudEvent) string {
	return attr(ce, "authexectype")
}

// ExecutorID returns the authexecid extension attribute — the executor's id —
// or "" if ce is nil or the attribute is absent.
func ExecutorID(ce *cepb.CloudEvent) string {
	return attr(ce, "authexecid")
}

// Roles returns the authclaims extension attribute — the executor's roles —
// split on ",", or nil if ce is nil or the attribute is absent or empty.
func Roles(ce *cepb.CloudEvent) []string {
	claims := attr(ce, "authclaims")
	if claims == "" {
		return nil
	}
	return strings.Split(claims, ",")
}

// Require reports whether the AuthContext on ce authorizes role. It is
// fail-closed: it returns true ONLY when the executor is explicitly a service
// AND role is present in authclaims. The attributed principal plays no part.
// Every other case returns false — a nil event, absent/empty claims, a system
// executor (a scheduled fire), a user executor, and any unset or unrecognized
// executor type (even if claims happen to be present). The executor-type
// allowlist is deliberate: it does not rely on the server invariant that
// authclaims is only ever set alongside a valid executor type, so a compute
// node stays fail-closed against any producer that violates it.
func Require(ce *cepb.CloudEvent, role string) bool {
	if ce == nil {
		return false
	}
	if ExecutorType(ce) != string(spi.PrincipalService) {
		return false
	}
	rs := Roles(ce)
	if len(rs) == 0 {
		return false
	}
	return slices.Contains(rs, role)
}

func attr(ce *cepb.CloudEvent, key string) string {
	if ce == nil || ce.Attributes == nil {
		return ""
	}
	return ce.Attributes[key].GetCeString()
}
