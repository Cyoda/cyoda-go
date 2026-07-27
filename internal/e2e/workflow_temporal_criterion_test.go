package e2e_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Workflow-criterion temporal coverage (issue #423, task 16): proves that a
// transition gated by a LifecycleCondition on creationDate evaluates
// chronologically at fire time through the full HTTP stack — the same
// match.Match -> matchLifecycle path already made temporal-correct for
// ad-hoc search (routed through spi.CompareTemporal). This is coverage for
// an already-correct evaluation path, not a fix: a GREATER_THAN threshold
// safely in the past must fire an automated transition, and a threshold in
// the future must not.
// ---------------------------------------------------------------------------

// temporalCriterionWorkflow builds a workflow whose CREATED state has one
// automated (manual:false) transition to ADVANCED, gated by a
// creationDate GREATER_THAN threshold LifecycleCondition. NONE -> CREATED is
// an unconditioned automated transition, so entity creation cascades through
// CREATED and evaluates the guarded transition in the same request.
func temporalCriterionWorkflow(t *testing.T, wfName, threshold string) string {
	t.Helper()
	criterion, err := json.Marshal(map[string]any{
		"type":         "lifecycle",
		"field":        "creationDate",
		"operatorType": "GREATER_THAN",
		"value":        threshold,
	})
	if err != nil {
		t.Fatalf("marshal lifecycle criterion: %v", err)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, wfName, string(criterion))
}

// TestTemporalCriterion_PastThreshold_FiresChronologically verifies that a
// creationDate GREATER_THAN <safely-past timestamp> criterion evaluates TRUE
// for a freshly-created entity (whose creationDate is ~now, well after the
// threshold) and the automated transition fires, advancing the entity past
// CREATED to ADVANCED within the create call.
func TestTemporalCriterion_PastThreshold_FiresChronologically(t *testing.T) {
	const model = "e2e-temporal-crit-past"
	pastThreshold := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	setupModelWithWorkflow(t, model, temporalCriterionWorkflow(t, "temporal-crit-past-wf", pastThreshold))
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":1}`)

	state := getEntityState(t, entityID)
	if state != "ADVANCED" {
		t.Errorf("creationDate GREATER_THAN past threshold %q: expected state ADVANCED (criterion true, transition fires), got %q",
			pastThreshold, state)
	}
}

// TestTemporalCriterion_FutureThreshold_DoesNotFire verifies that a
// creationDate GREATER_THAN <safely-future timestamp> criterion evaluates
// FALSE for a freshly-created entity (whose creationDate is ~now, well
// before the threshold), so the automated transition does not fire and the
// entity remains at CREATED.
func TestTemporalCriterion_FutureThreshold_DoesNotFire(t *testing.T) {
	const model = "e2e-temporal-crit-future"
	futureThreshold := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	setupModelWithWorkflow(t, model, temporalCriterionWorkflow(t, "temporal-crit-future-wf", futureThreshold))
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":1}`)

	state := getEntityState(t, entityID)
	if state != "CREATED" {
		t.Errorf("creationDate GREATER_THAN future threshold %q: expected state CREATED (criterion false, transition does not fire), got %q",
			futureThreshold, state)
	}
}
