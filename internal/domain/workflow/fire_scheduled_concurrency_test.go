package workflow

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Isolated single-backend concurrency tests (never parity,
// .claude/rules/test-coverage.md). They assert consistency — one committing
// run, the other superseded or not claimed, no torn write — not an interleave.

func TestClaimRace_TwoOwnersOneDueTask_OneRunCommits(t *testing.T) {
	env := newRunEnv(t, nil)
	armed := env.setup(t, "race-e1", oneHopWF("CLOSED", nil, nil))

	owners := []uuid.UUID{testOwner, otherOwner}
	reports := make([]*RunReport, len(owners))
	var wg sync.WaitGroup
	for i, owner := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := env.sts.ClaimDue(env.ctx, claimRequest(owner, env.nowMs(), false))
			if err != nil {
				t.Errorf("ClaimDue(%d): %v", i, err)
				return
			}
			for _, task := range claimed {
				r, _ := env.run(task)
				reports[i] = &r
			}
		}()
	}
	wg.Wait()

	ran := 0
	for _, r := range reports {
		if r == nil {
			continue
		}
		ran++
		if r.Outcome != OutcomeFired || r.Err != nil {
			t.Errorf("report = %+v, want fired", *r)
		}
	}
	if ran != 1 {
		t.Fatalf("%d runs, want exactly 1 (one claim per life)", ran)
	}
	if got := env.state(t, "race-e1"); got != "CLOSED" {
		t.Errorf("entity state = %q, want CLOSED", got)
	}
	if _, found := env.task(t, armed.ID); found {
		t.Error("task must be removed once")
	}
	if n := countAuditEvents(t, env.factory, env.ctx, "race-e1", spi.SMEventScheduledTransitionFired); n != 1 {
		t.Errorf("SCHEDULED_TRANSITION_FIRE events = %d, want 1", n)
	}
}

func TestClaimRace_StaleOwnerRunVersusReclaim_OneRunCommits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int64 // 0: none
		lateMs  int64
	}{
		{"fire", 0, 0},
		// A's first attempt is late → expired; B's reclaim counts a lost owner → runs within RETRY_DELAY.
		{"expire_versus_fire", 1_000, 2_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newRunEnv(t, nil)
			armed := env.setup(t, "stale-e1", oneHopWF("CLOSED", nil, nil))
			if tc.timeout > 0 {
				armed.TimeoutMs = &tc.timeout
				armTask(t, env.factory, env.ctx, armed)
			}
			env.advance(tc.lateMs)
			claimedA := env.claimOne(t, testOwner, false)

			var rA, rB RunReport
			bRan := false
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); rA, _ = env.run(claimedA) }()
			go func() {
				defer wg.Done()
				claimed, err := env.sts.ClaimDue(env.ctx, claimRequest(otherOwner, env.nowMs(), true))
				if err != nil {
					t.Errorf("reclaim: %v", err)
					return
				}
				if len(claimed) == 1 {
					bRan = true
					rB, _ = env.run(claimed[0])
				}
			}()
			wg.Wait()

			reports := []RunReport{rA}
			if bRan {
				reports = append(reports, rB)
			}
			committed := 0
			for _, r := range reports {
				switch r.Outcome {
				case OutcomeFired, OutcomeExpired:
					committed++
				case OutcomeSuperseded:
				default:
					t.Errorf("report = %+v, want fired, expired or superseded", r)
				}
			}
			if committed != 1 {
				t.Fatalf("committing runs = %d, want 1 (A=%+v B=%+v ran=%v)", committed, rA, rB, bRan)
			}
			if _, found := env.task(t, armed.ID); found {
				t.Error("task must be removed by the committing run")
			}
			want, event, other := "CLOSED", spi.SMEventScheduledTransitionFired, spi.SMEventScheduledTransitionExpired
			if rA.Outcome == OutcomeExpired {
				want, event, other = "OPEN", spi.SMEventScheduledTransitionExpired, spi.SMEventScheduledTransitionFired
			}
			if got := env.state(t, "stale-e1"); got != want {
				t.Errorf("entity state = %q, want %q", got, want)
			}
			if n := countAuditEvents(t, env.factory, env.ctx, "stale-e1", event); n != 1 {
				t.Errorf("%s events = %d, want 1", event, n)
			}
			if n := countAuditEvents(t, env.factory, env.ctx, "stale-e1", other); n != 0 {
				t.Errorf("%s events = %d, want 0", other, n)
			}
		})
	}
}
