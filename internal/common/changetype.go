package common

// CanonicalChangeType maps the internal storage spelling of a change type
// (CREATED/UPDATED/DELETED) to the canonical wire spelling (CREATE/UPDATE/DELETE)
// shared by HTTP, gRPC, and the audit endpoint (design §6.3 / E8).
// Unknown values pass through unchanged for forward compatibility.
func CanonicalChangeType(ct string) string {
	switch ct {
	case "CREATED":
		return "CREATE"
	case "UPDATED":
		return "UPDATE"
	case "DELETED":
		return "DELETE"
	default:
		return ct
	}
}
