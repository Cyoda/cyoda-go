package schema

import "strings"

// kindNames renders the set of kinds a node declares, for diagnostics that used
// to print a single label. A node observed only as null declares none, and
// saying so is more useful than naming the kind it merely resembles.
func kindNames(n *ModelNode) string {
	kinds := n.Kinds()
	if len(kinds) == 0 {
		return "no kind"
	}
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.String())
	}
	return strings.Join(names, "+")
}
