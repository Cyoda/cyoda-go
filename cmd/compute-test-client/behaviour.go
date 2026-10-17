package main

import (
	"fmt"
	"strings"
)

// behaviour is what this client does with every calculation request it
// receives, in place of serving it from the catalog. It exists so a parity
// scenario can attach a compute node that misbehaves in one known way.
type behaviour string

const (
	behaviourCatalog       behaviour = ""               // serve the catalog
	behaviourStall         behaviour = "stall"          // take the work, never answer
	behaviourFail          behaviour = "fail"           // answer success=false, no verdict
	behaviourFailRetryable behaviour = "fail-retryable" // answer success=false, retryable=true
	behaviourFailSilent    behaviour = "fail-silent"    // answer success=false with no message and no verdict
	behaviourLateCallback  behaviour = "late-callback"  // never answer; call back with the pass when told to
	behaviourDrop          behaviour = "drop"           // close the stream on receiving work
	behaviourHold          behaviour = "hold"           // take the work; answer it from the catalog on release

	// behaviourCriterionReasonNUL answers a criterion as a refusal whose reason
	// holds U+0000, and serves every other request from the catalog.
	behaviourCriterionReasonNUL behaviour = "criterion-reason-nul"
)

func parseBehaviour(s string) (behaviour, error) {
	switch b := behaviour(strings.TrimSpace(s)); b {
	case behaviourCatalog, behaviourStall, behaviourFail, behaviourFailRetryable, behaviourFailSilent, behaviourLateCallback, behaviourDrop, behaviourHold, behaviourCriterionReasonNUL:
		return b, nil
	default:
		return "", fmt.Errorf("unknown behaviour %q (want stall, fail, fail-retryable, fail-silent, late-callback, drop, hold, criterion-reason-nul, or unset)", s)
	}
}

// defaultTag is the tag the client joins with when none is configured.
const defaultTag = "compute-test-client"

// parseTags splits a comma-separated tag list; empty means the default tag.
func parseTags(csv string) []string {
	var tags []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			tags = append(tags, p)
		}
	}
	if len(tags) == 0 {
		return []string{defaultTag}
	}
	return tags
}
