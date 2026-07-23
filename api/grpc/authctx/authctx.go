// Package authctx lets compute-node authors read the CloudEvents AuthContext
// extension that cyoda-go attaches to callout events (see
// internal/grpc/cloudevent.go AttachAuthContext) and apply a fail-closed role
// gate.
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

// Type returns the authtype extension attribute (one of user/service/system),
// or "" if ce is nil or the attribute is absent.
func Type(ce *cepb.CloudEvent) string {
	return attr(ce, "authtype")
}

// ID returns the authid extension attribute, or "" if ce is nil or the
// attribute is absent.
func ID(ce *cepb.CloudEvent) string {
	return attr(ce, "authid")
}

// Roles returns the authclaims extension attribute split on ",", or nil if
// ce is nil or the attribute is absent or empty.
func Roles(ce *cepb.CloudEvent) []string {
	claims := attr(ce, "authclaims")
	if claims == "" {
		return nil
	}
	return strings.Split(claims, ",")
}

// Require reports whether the AuthContext on ce authorizes role. It is
// fail-closed: it returns false for a nil event, absent or empty claims, and
// for the system principal even if the role is listed (the system principal
// carries no meaningful claims to authorize against). It returns true only
// when authtype is user or service and role is present in authclaims.
func Require(ce *cepb.CloudEvent, role string) bool {
	if ce == nil || Type(ce) == string(spi.PrincipalSystem) {
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
