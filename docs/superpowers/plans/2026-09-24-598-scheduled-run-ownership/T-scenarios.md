# Stream T — scenario tests (the E, P, M and 1 cells of spec §13)

These tasks are **tests only**. They drive the feature through its public doors
(HTTP, the compute-node stream, the process signals of a real pnode) and read
the store only where the doors do not show a fact (a mark, a claim token, a
liveness record). They cover every E, P, M and 1 cell of spec §13 except the
rows of "Entity writes and workflow import" (stream W) and of
"`GET /scheduled-tasks`" (stream Q). U and S cells belong to other streams.

**What RED means here.** T runs in wave 5, after R, W and Q
(`README.md`, "Order of work"); T-1 and T-3 run earlier, after Q-5 and R-10
and before R-11, and T-2 runs inside R-11 (README C-P5). By then the production code exists, so a
scenario cannot be red against the branch head. Each task therefore states:
- **Depends on:** the streams whose code the scenario exercises. Against the
  merge-base (`git merge-base HEAD origin/release/v0.9.0`) with only T-1
  applied, every scenario fails (most do not compile: they use
  `cfg.Scheduler.StaleAfter`, `spi.ScheduledTask.Status`, the query endpoint).
- **Teeth:** one named temporary revert of a production line that must turn
  the scenario red. The implementer applies it, runs the scenario, records the
  failure text in the commit body, and undoes it with `git checkout -- <file>`.
  It is never committed. A scenario that stays green under its revert is
  rewritten, not committed.

**Where the scenarios run.**
- **E** (`internal/e2e`): every scheduler scenario uses a stack of its own on
  a PostgreSQL **database of its own** in the shared container
  (`newSchedulerHarness`, T-1). A claim is cross-tenant
  (spec §6.1, §10.2 "Tenant scoping"), so a scheduler on the shared database
  would claim, and run through its own engine, tasks that another test's stack
  armed. Every other stack runs with the scheduler off (T-1 changes the
  default of `newCalloutHarness`).
- **P** (`e2e/parity/scheduledtransition`, `e2e/parity/scheduledfunction`):
  registered with `parity.Register`, run on memory, SQLite, PostgreSQL and the
  commercial backend. Each scenario uses a fresh tenant and a short tag of its
  own. No concurrency case is here (`.claude/rules/test-coverage.md`).
- **M** (`e2e/parity/postgres`): standalone tests with a cluster of their own,
  like `TestAsyncNodeCrash_PeerCompletes`
  (`e2e/parity/postgres/async_node_crash_test.go:43-61`). Not in the shared
  `multinode` registry: they kill, stop, signal and pause.
- **1** (`e2e/parity/sqlite`): a standalone test that kills and relaunches one
  SQLite pnode on the same database file.

**Timing.** The tuned settings (T-1) are: scan 50 ms, heartbeat 1 s,
`STALE_AFTER` 53 s, retry delay 1 s, retry delay max 4 s. `STALE_AFTER` cannot
be shorter: spec §11 requires `≥ 50 s + 3 × HEARTBEAT_INTERVAL`, because
`CommitBudget` is a fixed 30 s (`internal/common/rollback.go:36`). So every
lost-owner scenario waits at least one stale period. The M and 1 tests call
`t.Parallel()`; they have no shared fixture, and Go starts them together after
the package's sequential tests, so their stale periods overlap.

**Observing "sent once" and "not claimed elsewhere".**
- **Sent once.** In M and 1, every processor a scenario counts is served by a
  `compute-test-client` process. Its `/record` log (`parity.ComputeClient.Received`,
  `e2e/parity/compute_client.go:62-76`) lists every request it received, with
  the entity id. The process outlives its stream, so the log survives the
  death of the pnode it was attached to (`cmd/compute-test-client/main.go:110-121`).
  The clients run on a **host pnode** whose scheduler is off
  (`CYODA_SCHEDULER_ENABLED=false`), so no kill of an owner disconnects them.
  In E, the scripted cnode's record (`scriptedCnode.Received`,
  `internal/e2e/scripted_cnode_test.go:143-152`) does the same.
- **Not claimed elsewhere.** The task row's `claim_token` and `claim_owner`
  (spec §10.2) are read from PostgreSQL with the fixture's connection string
  (`e2e/parity/postgres/multinode_fixture.go:119-123`). An owner's
  incarnation is mapped to its pnode by the INFO line R's scheduler writes at
  start (`scheduler started … incarnation=<uuid>`), read from the pnode's
  captured log (`fixtureutil.ClusterLaunchResult.NodeLogs`,
  `e2e/parity/fixtureutil/fixtureutil.go:713-720`).

**Commands.**
- E: `go test ./internal/e2e/ -run '<TestName>'` (Docker required).
- P, one scenario on one backend. The fixture builds the server in a subprocess
  the test cache cannot see, so `-count=1` is right here (CLAUDE.md, "the
  parity suites are the one exception"; the Makefile does the same,
  `Makefile:99-107`):
  `go test -count=1 ./e2e/parity/memory/ -run 'TestParity/<Scenario>'`, then
  `./e2e/parity/sqlite/` and `./e2e/parity/postgres/`.
- M: `go test -count=1 -timeout 20m ./e2e/parity/postgres/ -run '<TestName>'`.
- 1: `go test -count=1 ./e2e/parity/sqlite/ -run '<TestName>'`.
- Never `-v`. Run `make preflight` first.

**Every commit message ends with**
`Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
Stage files explicitly; never `git add -A` (`go.work` must stay clean).

---

### Task T-1: harness — tuned timing, a hold client, cluster signals and connections, a restartable node, an isolated scheduler stack

**Spec:** §13 "Rules" (tuned timing); §6.2, §6.4 (what the fixtures must be
able to do to a pnode); §10.2 "Scheduler pool (C4)" (connections per pnode).

**Depends on:** nothing for Steps 1–6 (they are harness code with their own
unit tests). Step 7's harness compiles only once R has changed
`app.SchedulerConfig` (it sets `cfg.Scheduler.HeartbeatInterval` and the other
new fields, `interfaces.md` "App config").

**Files:**
- Modify: `e2e/parity/fixtureutil/tuned_env.go`, `e2e/parity/fixtureutil/tuned_env_test.go`
- Modify: `cmd/compute-test-client/behaviour.go`, `cmd/compute-test-client/dispatch.go`, `cmd/compute-test-client/behaviour_test.go`, `cmd/compute-test-client/main.go` (package comment :16-20)
- Modify: `e2e/parity/compute_client.go` (constants :14-21)
- Modify: `e2e/parity/fixtureutil/fixtureutil.go` (`LaunchOpts` :514-518; the single-node launch :550-697; the cluster launch :829-1096)
- Create: `e2e/parity/fixtureutil/incarnation.go`, `e2e/parity/fixtureutil/incarnation_test.go`
- Modify: `e2e/parity/postgres/multinode_fixture.go`
- Create: `internal/e2e/scheduler_harness_test.go`
- Modify: `internal/e2e/callback_harness_test.go` (`newCalloutHarness` :187-266, comments :245-250 and :268-274)
- Modify: `go.mod`, `go.sum` (staged in Step 8)

**Interfaces:**
- Consumes (R): `app.SchedulerConfig` fields `Enabled, ScanInterval, MaxRuns,
  MaxRunsPerTenant, HeartbeatInterval, StaleAfter, MaxLostOwners, RetryDelay,
  RetryDelayMax, ShutdownDrain`; the INFO log line `scheduler started` with the
  attribute `incarnation` (R-6, README C-R2).
- Consumes (BP): tables `scheduled_tasks` (columns of spec §10.2),
  `scheduled_task_marks (task_id, arm_token, claim_token)`,
  `scheduler_owners (owner, heartbeat_at)`; `CYODA_POSTGRES_SCHEDULER_CONNS`.
- Consumes (Q, Q-5): `(*client.Client).ListScheduledTasks(t, url.Values) (client.ScheduledTaskPage, error)` and `client.ScheduledTask` in `e2e/parity/client/scheduled_tasks.go` (Q-query.md "Stream interface summary"). T does not define them.
- Produces:
  - `fixtureutil.TunedHeartbeatInterval`, `TunedStaleAfter`, `TunedRetryDelay`, `TunedRetryDelayMax` (`time.Duration` constants)
  - `parity.ComputeBehaviourHold = "hold"`
  - `fixtureutil.LaunchOpts.NodeEnv func(i int) []string`
  - `fixtureutil.ClusterLaunchResult.SignalNode func(i int, sig syscall.Signal) error`, `.AwaitNodeExit func(i int, within time.Duration) error`
  - `fixtureutil.NodeProc`, `fixtureutil.LaunchCyodaNode(cyodaBin string, ks *JWTKeySet, extraEnv []string, readiness time.Duration) (*NodeProc, error)`
  - `fixtureutil.IncarnationFromLog(log string) (uuid.UUID, error)`
  - `(*pgMultiNode) SignalNode, AwaitNodeExit, PauseDatabase, UnpauseDatabase, Incarnation`; `MustSetupMultiNodeWithOpts(t, n, extraEnv, opts)`
  - e2e: `schedDB`, `newSchedDB`, `schedulerTuning`, `newSchedulerHarness`, `newSchedulerCallbackHarness`, `taskRow`, `(s) task`, `(s) awaitTask`, `(s) count`, `schedDoc`, `sProc`, `scriptHoldFirst`, `awaitDBCondition`

- [ ] **Step 1: Write the failing tuned-env test**

Replace the body of `e2e/parity/fixtureutil/tuned_env_test.go` after
`envValue` (:13-22) with:

```go
func mustDuration(t *testing.T, env []string, key string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(envValue(t, env, key))
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return d
}

func TestTunedEnv_PatienceIsShortAndValid(t *testing.T) {
	single := mustDuration(t, fixtureutil.TunedServerEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT")
	if single <= 0 || single > 500*time.Millisecond {
		t.Errorf("single-node patience = %v; want within (0, 500ms]", single)
	}
	cluster := mustDuration(t, fixtureutil.TunedClusterEnv(), "CYODA_DISPATCH_WAIT_TIMEOUT")
	if cluster < time.Second || cluster >= 5*time.Second {
		t.Errorf("cluster patience = %v; want within [1s, 5s): room for gossip, below the default", cluster)
	}
}

// TestTunedEnv_SchedulerTimingIsShortAndValid pins the scheduler timing every
// parity and multi-node fixture runs with. The server refuses to start with a
// STALE_AFTER below 50s + 3 x HEARTBEAT_INTERVAL (the watchdog margin: a
// 30s commit budget, 10s slack, one heartbeat budget, three intervals), so
// the tuned value is that floor and no lower.
func TestTunedEnv_SchedulerTimingIsShortAndValid(t *testing.T) {
	for name, env := range map[string][]string{
		"server":  fixtureutil.TunedServerEnv(),
		"cluster": fixtureutil.TunedClusterEnv(),
	} {
		t.Run(name, func(t *testing.T) {
			if scan := mustDuration(t, env, "CYODA_SCHEDULER_SCAN_INTERVAL"); scan != 50*time.Millisecond {
				t.Errorf("scan interval = %v; want 50ms", scan)
			}
			hb := mustDuration(t, env, "CYODA_SCHEDULER_HEARTBEAT_INTERVAL")
			stale := mustDuration(t, env, "CYODA_SCHEDULER_STALE_AFTER")
			retry := mustDuration(t, env, "CYODA_SCHEDULER_RETRY_DELAY")
			retryMax := mustDuration(t, env, "CYODA_SCHEDULER_RETRY_DELAY_MAX")
			if hb != fixtureutil.TunedHeartbeatInterval || stale != fixtureutil.TunedStaleAfter ||
				retry != fixtureutil.TunedRetryDelay || retryMax != fixtureutil.TunedRetryDelayMax {
				t.Errorf("env and exported constants disagree: hb=%v stale=%v retry=%v retryMax=%v", hb, stale, retry, retryMax)
			}
			if hb <= 0 || hb > time.Second {
				t.Errorf("heartbeat interval = %v; want within (0, 1s]", hb)
			}
			if floor := 50*time.Second + 3*hb; stale < floor || stale > floor+5*time.Second {
				t.Errorf("stale after = %v; want within [%v, %v]: the server's floor, and no slower", stale, floor, floor+5*time.Second)
			}
			if retry <= 0 || retry > time.Second || retryMax < retry || retryMax > 5*time.Second {
				t.Errorf("retry delay %v / max %v; want a delay within (0, 1s] and a max within [delay, 5s]", retry, retryMax)
			}
		})
	}
}
```

In `TestFixturesTakeTheirTuningFromOnePlace` (:40-63) extend the literal list
(:57) to:

```go
		for _, literal := range []string{
			"CYODA_SCHEDULER_SCAN_INTERVAL", "CYODA_DISPATCH_WAIT_TIMEOUT",
			"CYODA_SCHEDULER_HEARTBEAT_INTERVAL", "CYODA_SCHEDULER_STALE_AFTER",
			"CYODA_SCHEDULER_RETRY_DELAY",
		} {
```

(`CYODA_SCHEDULER_RETRY_DELAY` is a prefix of `…_RETRY_DELAY_MAX`, so one
literal covers both.)

- [ ] **Step 2: Run it and see it fail**

Run: `go test -count=1 ./e2e/parity/fixtureutil/ -run 'TestTunedEnv|TestFixturesTake'`
Expected: FAIL — `undefined: fixtureutil.TunedHeartbeatInterval` (compile error).

- [ ] **Step 3: Make it pass**

`e2e/parity/fixtureutil/tuned_env.go`:

```go
package fixtureutil

import "time"

// The scheduler timing every parity and multi-node fixture runs with. The
// scenarios read these to size their waits; the fixtures pass them to the
// server through TunedServerEnv / TunedClusterEnv.
//
//   - TunedHeartbeatInterval 1s (default 15s): a pnode's liveness record is
//     refreshed every second.
//   - TunedStaleAfter 53s (default 2m): the lowest value the server accepts
//     with a 1s heartbeat (50s + 3 x heartbeat). A lost-owner scenario waits
//     at least this long; nothing shorter is valid.
//   - TunedRetryDelay 1s (default 30s) and TunedRetryDelayMax 4s (default
//     15m): a safe failure is retried after 1s, 2s, 4s, 4s, …. One second is
//     long enough that a scenario polling every 50ms sees "attempts 1"
//     before the second attempt.
const (
	TunedHeartbeatInterval = time.Second
	TunedStaleAfter        = 53 * time.Second
	TunedRetryDelay        = time.Second
	TunedRetryDelayMax     = 4 * time.Second
)

// TunedServerEnv returns the server settings every single-node parity fixture
// runs with, so the shared scenario set stays fast. Every fixture — in-tree
// and out-of-tree — appends it to its backend env; the values live here only.
//
//   - CYODA_SCHEDULER_SCAN_INTERVAL=50ms (default 1s): the scheduled-transition
//     scenarios observe fires within a small poll window. Harmless to every
//     other scenario — an empty claim is a cheap no-op query.
//   - CYODA_DISPATCH_WAIT_TIMEOUT=200ms (default 5s): how long a callout waits
//     for a compute node to exist. On a single node nothing needs waiting for
//     — a compute client is registered before it is told so — and the
//     scenarios that end with "no compute node" would otherwise each cost the
//     full default on every backend. Scenarios must not depend on a compute
//     client appearing within this window.
//   - the scheduler timing above.
func TunedServerEnv() []string {
	return append([]string{
		"CYODA_SCHEDULER_SCAN_INTERVAL=50ms",
		"CYODA_DISPATCH_WAIT_TIMEOUT=200ms",
	}, schedulerTimingEnv()...)
}

// TunedClusterEnv returns the settings every cluster parity fixture adds per
// node. The wait is longer than on a single node because in a cluster it also
// covers the gossip delay between a compute client joining one node and the
// others learning its tags. The scan interval and the scheduler timing are
// the single-node values.
func TunedClusterEnv() []string {
	return append([]string{
		"CYODA_DISPATCH_WAIT_TIMEOUT=2s",
		"CYODA_SCHEDULER_SCAN_INTERVAL=50ms",
	}, schedulerTimingEnv()...)
}

func schedulerTimingEnv() []string {
	return []string{
		"CYODA_SCHEDULER_HEARTBEAT_INTERVAL=" + TunedHeartbeatInterval.String(),
		"CYODA_SCHEDULER_STALE_AFTER=" + TunedStaleAfter.String(),
		"CYODA_SCHEDULER_RETRY_DELAY=" + TunedRetryDelay.String(),
		"CYODA_SCHEDULER_RETRY_DELAY_MAX=" + TunedRetryDelayMax.String(),
	}
}
```

`time.Duration.String` renders `1s`, `53s`, `4s` — the forms
`app/config.go`'s `envDuration` parses.

No fixture spells a removed variable (`CYODA_SCHEDULER_DISTRIBUTION`,
`…_COORDINATOR`, `…_REDISPATCH_BACKOFF`, `…_BATCH_SIZE`, `…_EXPIRY_GRACE`,
`CYODA_DISPATCH_FORWARD_TIMEOUT`):
`git grep -n -e SCHEDULER_DISTRIBUTION -e SCHEDULER_COORDINATOR -e REDISPATCH_BACKOFF -e SCHEDULER_BATCH_SIZE -e EXPIRY_GRACE -e DISPATCH_FORWARD_TIMEOUT -- e2e internal/e2e cmd/compute-test-client`
prints nothing today. Nothing to remove.

- [ ] **Step 4: Run it and see it pass**

Run: `go test -count=1 ./e2e/parity/fixtureutil/ -run 'TestTunedEnv|TestFixturesTake'`
Expected: PASS.

- [ ] **Step 5: The hold client — failing test, then code**

A scenario needs a compute node that takes the work and answers only when told
to (a run that is in flight until the test releases it). The existing
behaviours stall for ever or call back late (`cmd/compute-test-client/behaviour.go:13-20`).

Add to `TestParseBehaviour` (`behaviour_test.go:29-38`) the value `"hold"` in
the accepted list, add the row
`{"hold", behaviourHold, false, false, false, nil, 0},` to
`TestHandleCallout_Behaviours` (:71-126), and add:

```go
// TestHandleCallout_HoldAnswersFromCatalogOnRelease: a hold client records the
// work, stays silent, and on release answers it as the catalog would.
func TestHandleCallout_HoldAnswersFromCatalogOnRelease(t *testing.T) {
	d := newDispatcher("", "", newCatalog(nil, nil), nil, []string{"x"}, behaviourHold, newRecorder())
	ce, payload := processorRequest(t, "r-1", "noop", "pass-value", "")

	reply, drop, err := d.handleCallout(context.Background(), ce, payload)
	if err != nil || reply != nil || drop {
		t.Fatalf("handleCallout = (%v, %t, %v); want a silent, open stream", reply, drop, err)
	}
	work := d.takeHeldWork()
	if len(work) != 1 {
		t.Fatalf("held work = %d; want 1", len(work))
	}
	answer, err := d.answer(context.Background(), work[0].msg, work[0].payload, work[0].pass)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if body := decodeReply(t, answer); body.RequestID != "r-1" || !body.Success {
		t.Errorf("released answer = %+v; want requestId r-1, success", body)
	}
	if again := d.takeHeldWork(); len(again) != 0 {
		t.Errorf("held work taken twice: %d left", len(again))
	}
}
```

Run: `go test ./cmd/compute-test-client/ -run 'TestParseBehaviour|TestHandleCallout'`
Expected: FAIL — `undefined: behaviourHold`.

Code. `behaviour.go`: add the constant and accept it.

```go
	behaviourHold          behaviour = "hold"           // take the work; answer it from the catalog on release
```

```go
	case behaviourCatalog, behaviourStall, behaviourFail, behaviourFailRetryable, behaviourLateCallback, behaviourDrop, behaviourHold:
		return b, nil
	default:
		return "", fmt.Errorf("unknown behaviour %q (want stall, fail, fail-retryable, late-callback, drop, hold, or unset)", s)
```

`dispatch.go`: in `dispatcher` (:35-54) add

```go
	// heldWork is the work a hold client took and answers on release;
	// stream is where those answers go. Both are guarded by heldMu.
	heldWork []heldWork
	stream   grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent]
```

and the type

```go
// heldWork is one request a hold client took and has not answered yet.
type heldWork struct {
	msg     *cepb.CloudEvent
	payload json.RawMessage
	pass    string
}

// takeHeldWork returns the held work and forgets it.
func (d *dispatcher) takeHeldWork() []heldWork {
	d.heldMu.Lock()
	defer d.heldMu.Unlock()
	work := d.heldWork
	d.heldWork = nil
	return work
}
```

At the top of `run` (:163) record the stream:

```go
	d.heldMu.Lock()
	d.stream = stream
	d.heldMu.Unlock()
```

In `handleCallout`, add the case before `case behaviourFail, behaviourFailRetryable:` (:271):

```go
	case behaviourHold:
		d.heldMu.Lock()
		d.heldWork = append(d.heldWork, heldWork{msg: msg, payload: payload, pass: pass})
		d.heldMu.Unlock()
		return nil, false, nil
```

and replace the tail of `handleCallout` (:287-303, from `var reply *cepb.CloudEvent`)
with a call to a new method that both paths share:

```go
	reply, err := d.answer(ctx, msg, payload, pass)
	return reply, false, err
}

// answer serves one request from the catalog.
func (d *dispatcher) answer(ctx context.Context, msg *cepb.CloudEvent, payload json.RawMessage, pass string) (*cepb.CloudEvent, error) {
	switch msg.Type {
	case ceTypeCriteriaRequest:
		return d.handleCriteriaRequest(ctx, payload, pass)
	case ceTypeFunctionRequest:
		return d.handleFunctionRequest(ctx, payload, pass)
	default:
		// authtype carries the executor's principal kind; processors see it as
		// Entity.AuthType.
		return d.handleProcessorRequest(ctx, payload, pass, authTypeFromCloudEvent(msg))
	}
}
```

`release` (:306-312) answers the held work after the late callbacks:

```go
// release makes the late callback for every held callout, records what each
// door answered, and answers every request a hold client took.
func (d *dispatcher) release(ctx context.Context) {
	for _, hc := range d.takeHeld() {
		d.rec.setCallback(hc.seq, d.lateCallback(ctx, hc))
	}
	d.heldMu.Lock()
	stream := d.stream
	d.heldMu.Unlock()
	for _, w := range d.takeHeldWork() {
		reply, err := d.answer(ctx, w.msg, w.payload, w.pass)
		if err != nil {
			slog.Error("held request could not be answered", "pkg", "compute-test-client", "error", err)
			continue
		}
		if stream == nil {
			continue
		}
		if err := d.send(stream, reply); err != nil {
			// The pnode this client was attached to may be gone; the record
			// of what was received is what a scenario reads.
			slog.Warn("held answer not delivered", "pkg", "compute-test-client", "error", err)
		}
	}
}
```

`main.go` package comment (:16-20): add `hold` to the list of behaviours.
`e2e/parity/compute_client.go` (:14-21):

```go
	ComputeBehaviourHold          = "hold"           // take the work; answer it from the catalog on Release
```

and in the `Release` doc (:69-70): "Release tells a late-callback client to make
its callbacks now, and a hold client to answer what it holds; it waits for
them and returns the record."

Run: `go test ./cmd/compute-test-client/`
Expected: PASS.

- [ ] **Step 6: Cluster signals, per-node env, a restartable node, incarnations — failing tests, then code**

`e2e/parity/fixtureutil/incarnation_test.go`:

```go
package fixtureutil_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

func TestIncarnationFromLog(t *testing.T) {
	const id = "0b6f4a2e-3c1d-4e5f-8a9b-0c1d2e3f4a5b"
	line := `time=2026-09-24T10:00:00.000Z level=INFO msg="scheduler started" pkg=scheduler incarnation=` + id + "\n"
	got, err := fixtureutil.IncarnationFromLog("noise\n" + line + "more noise\n")
	if err != nil || got != uuid.MustParse(id) {
		t.Fatalf("IncarnationFromLog = %v, %v; want %s", got, err, id)
	}
	if _, err := fixtureutil.IncarnationFromLog("no scheduler here\n"); err == nil {
		t.Error("a log without the line gave an incarnation")
	}
	if _, err := fixtureutil.IncarnationFromLog(line + line); err == nil {
		t.Error("a log with two start lines gave an incarnation; one process starts one scheduler")
	}
}
```

Run: `go test -count=1 ./e2e/parity/fixtureutil/ -run TestIncarnationFromLog`
Expected: FAIL — `undefined: fixtureutil.IncarnationFromLog`.

`e2e/parity/fixtureutil/incarnation.go`:

```go
package fixtureutil

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
)

// incarnationLine matches the INFO line a pnode's scheduler writes when it
// starts (slog text handler, internal/logging/logging.go).
var incarnationLine = regexp.MustCompile(`msg="scheduler started"[^\n]*\bincarnation=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// IncarnationFromLog returns the scheduler incarnation a pnode's captured
// log announces. A scenario uses it to map a task's claim_owner to the pnode
// that holds the claim. Exactly one start line is expected per process.
func IncarnationFromLog(log string) (uuid.UUID, error) {
	m := incarnationLine.FindAllStringSubmatch(log, -1)
	switch len(m) {
	case 0:
		return uuid.Nil, errors.New(`no "scheduler started" line with an incarnation`)
	case 1:
		return uuid.Parse(m[0][1])
	default:
		return uuid.Nil, fmt.Errorf(`%d "scheduler started" lines; want one per process`, len(m))
	}
}
```

`fixtureutil.go`, `LaunchOpts` (:514-518):

```go
type LaunchOpts struct {
	// ReadinessTimeout overrides the default health-check timeout for
	// cyoda-go. Defaults to 30s if zero.
	ReadinessTimeout time.Duration
	// NodeEnv, when set, returns extra environment for cluster node i. It is
	// appended after every other variable, so it overrides them (os/exec uses
	// the last value of a duplicated key). A scenario uses it to turn one
	// node's scheduler off, or to isolate one node's gossip.
	NodeEnv func(i int) []string
}
```

In the cluster launch loop, directly after the `env = append(env, …)` block
that ends with `"CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING=true",` (:931-949):

```go
			if opt.NodeEnv != nil {
				env = append(env, opt.NodeEnv(i)...)
			}
```

`ClusterLaunchResult` (after `KillNode`, :721-729):

```go
	// SignalNode sends sig to node i's process group without waiting: SIGTERM
	// for a graceful shutdown, SIGSTOP / SIGCONT to freeze and resume it.
	SignalNode func(i int, sig syscall.Signal) error
	// AwaitNodeExit waits up to within for node i's process to exit, reaping
	// it through the monitor's exit signal. It returns an error if the
	// process is still running. within 0 checks without waiting.
	AwaitNodeExit func(i int, within time.Duration) error
```

and beside `killNode` (:1027-1039):

```go
	signalNode := func(i int, sig syscall.Signal) error {
		if i < 0 || i >= len(nodes) || nodes[i] == nil || nodes[i].cmd == nil || nodes[i].cmd.Process == nil {
			return fmt.Errorf("signal node %d: no such node", i)
		}
		pgid, err := syscall.Getpgid(nodes[i].cmd.Process.Pid)
		if err != nil {
			return fmt.Errorf("signal node %d: %w", i, err)
		}
		if err := syscall.Kill(-pgid, sig); err != nil {
			return fmt.Errorf("signal node %d with %v: %w", i, sig, err)
		}
		return nil
	}
	awaitNodeExit := func(i int, within time.Duration) error {
		if i < 0 || i >= len(nodes) || nodes[i] == nil || nodes[i].exitedCh == nil {
			return fmt.Errorf("await node %d: no such node", i)
		}
		select {
		case <-nodes[i].exitedCh:
			return nil
		case <-time.After(within):
			return fmt.Errorf("node %d is still running %s after the wait began", i, within)
		}
	}
```

and publish both in the returned `ClusterLaunchResult` (:1086-1095):
`SignalNode: signalNode, AwaitNodeExit: awaitNodeExit,`.

A restartable single node. Add, above `LaunchCyodaAndCompute` (:528):

```go
// NodeProc is one running cyoda-go process launched by LaunchCyodaNode.
type NodeProc struct {
	BaseURL      string
	GRPCEndpoint string
	// Logs is the process's combined output, also tee'd to os.Stderr. Never
	// assert on token or secret material read from it (Gate 3).
	Logs     *SyncBuffer
	cmd      *exec.Cmd
	exitedCh chan struct{}
	killOnce sync.Once
}

// Kill SIGKILLs the process group and reaps it. Calling it again is harmless.
func (p *NodeProc) Kill() {
	p.killOnce.Do(func() {
		killProcessGroupNoWait(p.cmd)
		<-p.exitedCh
	})
}

// LaunchCyodaNode starts one cyoda-go process with extraEnv on fresh ports and
// waits until it is healthy. It starts no compute client. A scenario that
// restarts a node on the same storage calls it twice with the same env.
// readiness 0 means the default readiness timeout.
func LaunchCyodaNode(cyodaBin string, ks *JWTKeySet, extraEnv []string, readiness time.Duration) (*NodeProc, error) {
	if readiness == 0 {
		readiness = defaultCyodaReadinessTimeout
	}
	var proc *NodeProc
	err := retryLaunch(clusterLaunchAttempts, func() error {
		hPort, e := FreePort()
		if e != nil {
			return fmt.Errorf("failed to get HTTP port: %w", e)
		}
		gPort, e := FreePort()
		if e != nil {
			return fmt.Errorf("failed to get gRPC port: %w", e)
		}
		aPort, e := FreePort()
		if e != nil {
			return fmt.Errorf("failed to get admin port: %w", e)
		}
		cmd := exec.Command(cyodaBin)
		cmd.WaitDelay = 3 * time.Second
		cmd.Env = append(CyodaEnv(hPort, gPort, ks), extraEnv...)
		cmd.Env = append(cmd.Env, fmt.Sprintf("CYODA_ADMIN_PORT=%d", aPort))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		logs := &SyncBuffer{}
		cmd.Stdout = io.MultiWriter(os.Stderr, logs)
		cmd.Stderr = io.MultiWriter(os.Stderr, logs)
		if e := cmd.Start(); e != nil {
			return fmt.Errorf("failed to start cyoda-go: %w", e)
		}
		exitedCh := make(chan struct{})
		var exitErr error
		go func() {
			exitErr = cmd.Wait()
			close(exitedCh)
		}()
		url := fmt.Sprintf("http://127.0.0.1:%d", hPort)
		healthDoneCh := make(chan error, 1)
		go func() { healthDoneCh <- WaitForHTTPHealth(url+"/api/health", readiness) }()
		if e := nodeOutcome(0, healthDoneCh, exitedCh, func() error { return exitErr }); e != nil {
			killProcessGroupNoWait(cmd)
			<-exitedCh
			return e
		}
		proc = &NodeProc{BaseURL: url, GRPCEndpoint: fmt.Sprintf("127.0.0.1:%d", gPort), Logs: logs, cmd: cmd, exitedCh: exitedCh}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cyoda launch failed: %w", err)
	}
	slog.Info("cyoda-go is ready", "pkg", "fixtureutil", "baseURL", proc.BaseURL)
	return proc, nil
}
```

`LaunchCyodaAndComputeWithBinaries` (:550-697) now uses it: its launch block
(from `var (cyodaCmd …` :561 through the `slog.Info("cyoda-go is ready" …)`
:647) and `killCyoda` (:652-655) are replaced by

```go
	node, err := LaunchCyodaNode(cyodaBin, ks, extraEnv, opt.ReadinessTimeout)
	if err != nil {
		return nil, nil, err
	}
	cleanup := node.Kill
```

and the rest of the function reads `node.BaseURL`, `node.GRPCEndpoint`,
`node.cmd` where it read `baseURL`, `grpcEndpoint`, `cyodaCmd`, and calls
`node.Kill()` where it called `killCyoda()`. One launch path, not two.

`e2e/parity/postgres/multinode_fixture.go`: add fields
`signalNode func(int, syscall.Signal) error`, `awaitNodeExit func(int, time.Duration) error`,
`containerID string` to `pgMultiNode` (:23-41), set them from `result` and
`pgContainer.GetContainerID()` in the constructor, and add:

```go
// SignalNode sends sig to node i (SIGTERM, SIGSTOP, SIGCONT). Off the shared
// interface, like KillNode: a scheduler scenario type-asserts for it.
func (f *pgMultiNode) SignalNode(i int, sig syscall.Signal) error { return f.signalNode(i, sig) }

// AwaitNodeExit waits up to within for node i to exit.
func (f *pgMultiNode) AwaitNodeExit(i int, within time.Duration) error {
	return f.awaitNodeExit(i, within)
}

// PauseDatabase freezes the PostgreSQL container: every pnode's statements
// hang until UnpauseDatabase, as in a network partition from the database.
func (f *pgMultiNode) PauseDatabase(t *testing.T) {
	t.Helper()
	dc, err := testcontainers.NewDockerClientWithOpts(context.Background())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer dc.Close()
	if _, err := dc.ContainerPause(context.Background(), f.containerID, client.ContainerPauseOptions{}); err != nil {
		t.Fatalf("pause the database container: %v", err)
	}
}

// UnpauseDatabase resumes the PostgreSQL container.
func (f *pgMultiNode) UnpauseDatabase(t *testing.T) {
	t.Helper()
	dc, err := testcontainers.NewDockerClientWithOpts(context.Background())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer dc.Close()
	if _, err := dc.ContainerUnpause(context.Background(), f.containerID, client.ContainerUnpauseOptions{}); err != nil {
		t.Fatalf("unpause the database container: %v", err)
	}
}

// Incarnation returns the scheduler incarnation node i announced at start.
func (f *pgMultiNode) Incarnation(t *testing.T, i int) uuid.UUID {
	t.Helper()
	id, err := fixtureutil.IncarnationFromLog(f.NodeLogs(i))
	if err != nil {
		t.Fatalf("node %d: %v", i, err)
	}
	return id
}
```

(`client` is `github.com/moby/moby/client`; it is `// indirect` in `go.mod:61`
today and becomes direct — run `go mod tidy` and stage `go.mod`/`go.sum`.)
`MustSetupMultiNodeWithEnv` becomes a call to a new
`MustSetupMultiNodeWithOpts(t, n, extraEnv, fixtureutil.LaunchOpts{})`, which
is the current body with `opts` passed to `LaunchCyodaClusterAndCompute`
(:187).

**Connections.** Each pnode now opens up to 11 more PostgreSQL connections
(the scheduler pool, `CYODA_POSTGRES_SCHEDULER_CONNS` default 10, plus one for
the heartbeat, spec §10.2), beside its main pool (`CYODA_POSTGRES_MAX_CONNS`,
default 25, `plugins/postgres/config.go:68`). The largest cluster here has six
pnodes (T-10): 6 × 36 = 216, above PostgreSQL's default `max_connections`
of 100. The fixture **raises `max_connections` to 400 on the multi-node
container** and leaves the pool sizes at their defaults. Lowering
`CYODA_POSTGRES_SCHEDULER_CONNS` instead would run the cluster tests below
the pool size the spec sizes for `MAX_RUNS` 8 plus the claim loop (§10.2), so
T-11's contention and T-10's four concurrent runs would test a starved pool
nobody deploys. In `MustSetupMultiNodeWithOpts`, the options (:150-154)
become:

```go
	opts := append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("cyoda_parity_multinode"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
	}, testpg.HardenedOptions()...)
	// Up to six pnodes, each with a main pool (25), a scheduler pool (10) and
	// a heartbeat connection (1), plus the test's own reader: above the
	// default of 100.
	opts = append(opts, testcontainers.WithCmdArgs("-c", "max_connections=400"))
```

(`testcontainers.WithCmdArgs` appends to the arguments `HardenedOptions`
already set, `internal/testpg/testpg.go:45-48`.) The single-node parity and
e2e containers keep the default: one pnode, or sequential e2e stacks. The `NodeLogs` comment (:91-99) and the `NodeLogs` field comment in
`fixtureutil.go` (:713-720) lose the "peer-RPC fire path" sentence: the logs
now serve the incarnation lookup.

Run: `go test -count=1 ./e2e/parity/fixtureutil/ && go vet ./e2e/...`
Expected: PASS; vet clean.

- [ ] **Step 7: The isolated e2e scheduler stack**

The parity scenarios read tasks with stream Q's parity client
(`client.ListScheduledTasks`, Q-5); T adds no client method.

`internal/e2e/callback_harness_test.go`, `newCalloutHarness` — directly before
`if configure != nil {` (:217):

```go
	// No scheduler unless the test asks for one. A claim is cross-tenant, and
	// every stack but the one a scheduler test builds on a database of its own
	// (newSchedulerHarness) shares one PostgreSQL database: a scheduler here
	// would claim, and run through this stack's engine, tasks another test's
	// stack armed.
	cfg.Scheduler.Enabled = false
```

The comment at :245-250 keeps its point (Shutdown before Close) and drops
"the scheduler's 1s scan loop keeps ticking": "Shutdown stops the scheduler, if
the test enabled one, and the TTL/tx reapers before Close tears down the store
pool." The comment at :268-274 (`newCallbackHarnessConfigured`) drops the
Task 9.2 story and says: "configure may be nil (identical to
newCallbackHarness)."

`internal/e2e/scheduler_harness_test.go`:

```go
package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/app"
)

// scheduler_harness_test.go gives a scheduler test a stack of its own on a
// PostgreSQL database of its own. The scheduler claims across tenants
// (ClaimDue is cross-tenant), so a scheduler on the shared test database
// would claim tasks other tests' stacks armed and run them through the wrong
// engine. A database per test makes "the only scheduler that can see this
// task" true by construction.

// harnessTenant is the tenant every harness stack bootstraps.
const harnessTenant = "test-tenant"

// schedDB is one test's database: its URL for the stack, a pool for reads.
type schedDB struct {
	name string
	url  string
	pool *pgxpool.Pool
}

// newSchedDB creates an empty database in the shared container. The stack
// that opens it migrates it (CYODA_POSTGRES_AUTO_MIGRATE is set by TestMain).
func newSchedDB(t *testing.T) *schedDB {
	t.Helper()
	ctx := context.Background()
	name := "sched_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	if _, err := dbPool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	u, err := url.Parse(pgURLFromEnv(t))
	if err != nil {
		t.Fatalf("parse postgres URL: %v", err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool for %s: %v", name, err)
	}
	s := &schedDB{name: name, url: u.String(), pool: pool}
	// Registered first, so it runs after the stack's own cleanups.
	t.Cleanup(func() {
		pool.Close()
		if _, err := dbPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})
	return s
}

// schedulerTuning is the scheduler timing every scheduler stack runs with:
// the parity values (fixtureutil.Tuned*), one try per callout, and 200ms of
// patience for a compute node.
func schedulerTuning(cfg *app.Config) {
	cfg.Scheduler.Enabled = true
	cfg.Scheduler.ScanInterval = 50 * time.Millisecond
	cfg.Scheduler.HeartbeatInterval = time.Second
	cfg.Scheduler.StaleAfter = 53 * time.Second
	cfg.Scheduler.RetryDelay = time.Second
	cfg.Scheduler.RetryDelayMax = 4 * time.Second
	cfg.Scheduler.ShutdownDrain = time.Second
	cfg.Callout.FixedNumRetries = 0
	cfg.Cluster.DispatchWaitTimeout = 200 * time.Millisecond
}

// newSchedulerHarness is a stack with a live scheduler on a database of its
// own and no cnode. configure runs after schedulerTuning and may override it.
func newSchedulerHarness(t *testing.T, configure func(*app.Config)) (*callbackHarness, *schedDB) {
	t.Helper()
	s := newSchedDB(t)
	h := newCalloutHarness(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
		schedulerTuning(cfg)
		if configure != nil {
			configure(cfg)
		}
	})
	return h, s
}

// newSchedulerCallbackHarness is newSchedulerHarness plus the default cnode of
// newCallbackHarness (tag "sched-fn", serving registered closures).
func newSchedulerCallbackHarness(t *testing.T, configure func(*app.Config)) (*callbackHarness, *schedDB) {
	t.Helper()
	h, s := newSchedulerHarness(t, configure)
	h.member = h.AttachCnode(t, cnodeSpec{name: "default", tags: []string{scheduledFnTag}, script: h.registeredScript}).m
	return h, s
}

// newStackOn opens a further stack on an existing test database — the
// restart of a pnode on the same storage.
func newStackOn(t *testing.T, s *schedDB, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	return newCalloutHarness(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", s.url)
		schedulerTuning(cfg)
		if configure != nil {
			configure(cfg)
		}
	})
}

// taskRow is a scheduled_tasks row as a test sees it, with whether a mark
// exists for its current life.
type taskRow struct {
	ID, Status, LastError, FailureReason string
	ArmToken, ClaimToken, ClaimOwner     string
	Attempts, LostOwners                 int
	PartialCommit, Marked                bool
}

// task reads the entity's task for transition. ok is false when there is none.
func (s *schedDB) task(t *testing.T, entityID, transition string) (taskRow, bool) {
	t.Helper()
	var r taskRow
	err := s.pool.QueryRow(context.Background(), `
		SELECT st.id, st.status, COALESCE(st.last_error, ''), COALESCE(st.failure_reason, ''),
		       st.arm_token::text, COALESCE(st.claim_token::text, ''), COALESCE(st.claim_owner::text, ''),
		       st.attempts, st.lost_owners, st.partial_commit,
		       EXISTS (SELECT 1 FROM scheduled_task_marks m WHERE m.task_id = st.id AND m.arm_token = st.arm_token)
		  FROM scheduled_tasks st
		 WHERE st.tenant_id = $1 AND st.entity_id = $2 AND st.transition = $3`,
		harnessTenant, entityID, transition,
	).Scan(&r.ID, &r.Status, &r.LastError, &r.FailureReason, &r.ArmToken, &r.ClaimToken, &r.ClaimOwner,
		&r.Attempts, &r.LostOwners, &r.PartialCommit, &r.Marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskRow{}, false
	}
	if err != nil {
		t.Fatalf("read task of %s/%s: %v", entityID, transition, err)
	}
	return r, true
}

// awaitTask polls the task every 20ms until cond holds and returns the first
// row that satisfies it.
func (s *schedDB) awaitTask(t *testing.T, entityID, transition string, within time.Duration, what string, cond func(taskRow, bool) bool) taskRow {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r, ok := s.task(t, entityID, transition)
		if cond(r, ok) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s/%s: %s not seen within %s; last: present=%t %+v", entityID, transition, what, within, ok, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// count runs a count(*) query on this test's database.
func (s *schedDB) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	return n
}

// awaitDBCondition polls cond every 20ms until it is true.
func awaitDBCondition(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not seen within %s", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// schedDoc wraps states in a schema 1.5 import document (the minor that
// accepts a processor's idempotent) whose initial state is Open.
func schedDoc(wfName string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": wfName, "initialState": "Open", "active": true, "states": states,
		}},
	})
	return string(b)
}

// sProc is one processor of a scheduler test workflow. idempotent false is
// the unsafe processor of spec §3.
func sProc(name, mode, tag string, idempotent bool) map[string]any {
	return map[string]any{"type": "calculator", "name": name, "executionMode": mode,
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag,
			"idempotent": idempotent, "responseTimeoutMs": 60000}}
}

// fireOpenToDone is Open -[Fire, scheduled]-> Done carrying procs. timeoutMs
// 0 leaves timeoutMs off.
func fireOpenToDone(wfName string, delayMs, timeoutMs int64, procs ...map[string]any) string {
	sched := map[string]any{"delayMs": delayMs}
	if timeoutMs > 0 {
		sched["timeoutMs"] = timeoutMs
	}
	fire := map[string]any{"name": "Fire", "next": "Done", "manual": false, "schedule": sched}
	if len(procs) > 0 {
		list := make([]any, 0, len(procs))
		for _, p := range procs {
			list = append(list, p)
		}
		fire["processors"] = list
	}
	return schedDoc(wfName, map[string]any{
		"Open": map[string]any{"transitions": []any{fire}},
		"Done": map[string]any{},
	})
}

// scriptHoldFirst holds this cnode's first callout until release is closed
// and answers every later one at once.
func scriptHoldFirst(release <-chan struct{}) cnodeScript {
	var calls atomic.Int32
	return func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return neverAnswer()
			}
		}
		return answerOK()
	}
}

// failEvent returns the data of the entity's SCHEDULED_TRANSITION_FAIL event.
func failEvent(t *testing.T, h *callbackHarness, entityID string) map[string]any {
	t.Helper()
	for _, ev := range smEventsOfType(h.GetSMAuditEvents(t, entityID), "SCHEDULED_TRANSITION_FAIL") {
		data, _ := ev["data"].(map[string]any)
		return data
	}
	t.Fatalf("entity %s has no SCHEDULED_TRANSITION_FAIL event", entityID)
	return nil
}

// uniq returns prefix plus a short random suffix: model names and tags.
func uniq(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }
```

A self-test of the isolation in `internal/e2e/scheduler_harness_test.go`:

```go
// TestSchedulerHarness_OwnDatabase: a task armed on a scheduler stack lives
// in that stack's database only.
func TestSchedulerHarness_OwnDatabase(t *testing.T) {
	h, s := newSchedulerHarness(t, func(cfg *app.Config) { cfg.Scheduler.Enabled = false })
	model := uniq("sh-own")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sh-own-wf", 600_000, 0))
	id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	if r, ok := s.task(t, id, "Fire"); !ok || r.Status != "WAITING" || r.Attempts != 0 {
		t.Fatalf("task in the harness database = %+v present=%t; want WAITING, attempts 0", r, ok)
	}
	if n := queryDB(t, harnessTenant, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1", id); n != 0 {
		t.Fatalf("the shared database holds %d rows of this entity; want 0", n)
	}
}
```

Run: `go test ./internal/e2e/ -run TestSchedulerHarness_OwnDatabase`
Expected (before R-10 and BP land): FAIL to compile — `cfg.Scheduler.HeartbeatInterval undefined`.
T-1 lands after R-10 and before R-11 (README "Order of work", C-P5). Until
R-11, `internal/e2e` does not compile for another reason (the callers of the
removed `scheduler.NewService`), so this test's GREEN is observed in R-11
Step 4. Steps 1–6 are verified here with their own packages' tests.

- [ ] **Step 8: Commit**

```bash
git add e2e/parity/fixtureutil/tuned_env.go e2e/parity/fixtureutil/tuned_env_test.go \
  e2e/parity/fixtureutil/incarnation.go e2e/parity/fixtureutil/incarnation_test.go \
  e2e/parity/fixtureutil/fixtureutil.go e2e/parity/postgres/multinode_fixture.go \
  e2e/parity/compute_client.go \
  cmd/compute-test-client/behaviour.go cmd/compute-test-client/dispatch.go \
  cmd/compute-test-client/behaviour_test.go cmd/compute-test-client/main.go \
  internal/e2e/scheduler_harness_test.go internal/e2e/callback_harness_test.go go.mod go.sum
git commit -m "test(scheduler): harness for the ownership scenarios

Tuned scheduler timing for every parity and cluster fixture, a hold
compute client, per-node env, signals and database pause for a cluster,
max_connections for the cluster database, a restartable single node,
and an e2e stack with a scheduler on a database of its own.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-2: rewrite the existing scheduled tests that rely on removed behaviour (V2)

> **Executed inside R-11 (README C-T1, C-P5).** R-11 Step 1 applies this
> task's Steps 1–4, R-11 Step 4 runs this task's Step 5 (with its teeth), and
> R-11 Step 6 makes the one commit for both tasks. This task has no commit of
> its own. T-1, T-3 and Q-5 land before R-11.

**Spec:** §16 V2; §6.6 (removed: coordinator, distribution, scheduler RPC,
throttle); §5.2 "Removed from the fire path" (grace band, `ArmedBy`
verify-or-abort).

**Depends on:** R (removes `scheduler.NewService`, `scheduler.Deps`,
`cluster.NewClusterExecutor`, `cluster.NewSchedulerEngine`,
`scheduler.LowestLiveNodeID`, `scheduler.Self`, the scheduler RPC), S
(removes `ScanDue`), T-1.

**Ordering constraint.** The files below stop compiling the moment R or S
removes those symbols, and `internal/e2e` is outside `make test`'s run
(`Makefile:122-125`) but inside `go vet ./...`. So this task's edits land in
R-11's commit (README C-T1, C-P5). It carries its own tests only as the
rewritten tests.

**The list (every existing e2e or parity test that relies on removed behaviour):**

| # | Test or code | Relies on | Rewrite |
|---|---|---|---|
| 1 | `internal/e2e/scheduled_transition_test.go:35-66` `startTestScheduler` | `scheduler.NewService`, `RedispatchBackoff`, `BatchSize`, `LowestLiveNodeID`, `Self`, `NewClusterExecutor` | deleted; its callers use `newSchedulerHarness` |
| 2 | `scheduled_transition_test.go:68-85` `awaitEntityStateE2E` | only callers are 3–5 | deleted; callers use `awaitCallbackEntityState` (`scheduled_function_test.go:177`) |
| 3 | `TestE2E_ScheduledTransition_FiresThroughHTTPStack` (:252-311) | #1 | on `newSchedulerHarness`; adds "the task is removed" |
| 4 | `TestE2E_ScheduledTransition_LoopbackDefersTimer` (:329-410) | #1 | on `newSchedulerHarness`; reads events with `schedEvents` |
| 5 | `TestE2E_ScheduledTransition_RestartDurability` (:430-468) | #1, "a brand-new scheduler.Service" as a restart stand-in | a real restart: stack A (scheduler off) arms, is shut down and closed; stack B on the same database fires |
| 6 | `scheduled_transition_test.go:15-33` header | the disabled-testApp and bespoke-scheduler story | rewritten |
| 7 | `TestScheduledFunction_ExpiryElapsedBeforeScan_ExpiresNoFire` (`scheduled_function_test.go:550-613`) | the grace band (`timeout + grace`, :536-547, :570-572, :602), `scheduler.NewService`, `RedispatchBackoff` | restart pattern as in 5; arithmetic without grace (spec §5.1 step 4) |
| 8 | `TestScheduledFunction_ArmAbsoluteFireAt_FiresThroughHTTPStack` (:200), `…ArmRelativeFireAfterMs…` (:242), `…ArmWithExpiry_TimeoutMsStoredAndStillFires` (:282), `…PastFireAt_FiresPromptly` (:501) | the stack's built-in scheduler on the shared database, which T-1 turns off | `newSchedulerCallbackHarness`; `queryDB(t, "test-tenant", …)` → `s.count(t, …)` |
| 9 | `scheduled_function_test.go:38-44` header | "the shared harness's scheduler runs with its default cadence" | rewritten |
| 10 | `scheduled_attribution_test.go` `TestAttribution_ScheduledUserArmed` (:79), `…ScheduledChain` (:115), `…ScheduledAnchorStamped` (:174), subtests `SaveBodyArmedBySpoofed` (:249) and `ValidFunctionResultArmsTrueOrigin` (:327) | built-in scheduler; `dbPool` at :346 | `newSchedulerCallbackHarness`; `dbPool.QueryRow` → `s.pool.QueryRow`. `FunctionResultCannotCarryPrincipal` (:296) fires nothing and stays on `newCallbackHarness` |
| 11 | `scheduled_attribution_test.go:13-33` header | "re-verified unchanged under the re-read guard" (the verify-or-abort, spec §5.2 removed list) | "read from the claimed task" |
| 12 | `internal/e2e/callout_modes_test.go:108-162` `TestCalloutModes_ScheduledFire` | the stack's built-in scheduler on the shared database | `newSchedulerHarness(t, calloutTuning(3, 100*time.Millisecond))` (configure runs after `schedulerTuning`, so 3 retries hold) |
| 13 | `internal/e2e/tx_lifecycle_e2e_test.go:510-736` `TestE2E_SchedulerRPCPanic_RecoveredAndRolledBack` with `schedulerTaskPathForTest`, `newClusterHarness`, `txLifeSchedDelayMs`, `txLifeSchedGateWF`, `findScheduledTask`, `postSchedulerRPC` | the scheduler RPC route (removed, §6.6), `ScanDue` | deleted. A panicking run is U-only in §13 ("panicking run → FAILED `RUN_PANICKED`, node latched"). `clusterHMACSecret32` (:516) stays: `callout_handover_lost_test.go:123, 151, 350` uses it; its comment loses the scheduler-RPC reference |
| 14 | `internal/e2e/callout_handover_lost_test.go:41` comment | names `schedulerTaskPathForTest` | names the handover route only |
| 15 | `internal/e2e/e2e_test.go:158-172` comment | `ScanDue`, `RedispatchBackoff`, coordinators | rewritten; `cfg.Scheduler.Enabled = false` stays |
| 16 | `e2e/parity/scheduledtransition/scheduledtransition.go:15-17` package doc | "grace-band boundaries are Unit-only" | "retry and deadline arithmetic is Unit-only" |
| 17 | `RunScheduledTransition_FiresOnTime` (:232), `…DeclineCriterionFalse` (:269) | nothing removed; the spec adds "task removed" to both endings (§4 table) | each asserts the task is gone (`taskOf == nil`) |
| 18 | `e2e/parity/scheduledfunction/scheduledfunction.go:24-32` doc, `RunScheduledFunction_ExpiryElapsedExpiresNoFire` (:396-438) | "timeoutMs+grace" | text without grace; asserts the task is removed |
| 19 | `cmd/compute-test-client/catalog.go:283-295` `expiryElapsed` comment | "timeoutMs+grace" | "exceeds timeoutMs at arm time" |
| 20 | `e2e/parity/multinode/attribution.go:26-31, 185-275` `RunAttribution_ScheduledFire` | round-robin fire across members, the peer-RPC log line `scheduled task peer fire resolved` | the log-level raise and the peer-fire assertion go; a claimed task carries `ArmedBy` whichever pnode claims it (§5.2), so there is no cross-node hop left to prove; the scenario keeps the attribution assertions |
| 21 | `e2e/parity/multinode/attribution.go:61-79` `AttributionCapable.NodeLogs` | only #20 used it | removed from the interface; `pgMultiNode.NodeLogs` stays (T-1 uses it for incarnations); `attribution_skip_test.go:30` comment updated |

Not in this stream: `internal/domain/workflow/fire_scheduled_concurrency_test.go`
(V2's "dual-coordinator tests become claim-race tests") is a U-layer file of
stream E. `scheduled_function_concurrency_test.go` needs no scheduler and is
unchanged.

**Files:** every file named in the table.

**Interfaces:**
- Consumes (T-1): `newSchedulerHarness`, `newSchedulerCallbackHarness`, `newStackOn`, `schedDB.{task,count,pool}`, `uniq`. (Q-5): `client.ListScheduledTasks`.
- Produces: `schedEvents(t, h, entityID)` in `internal/e2e/scheduler_harness_test.go`. Consumes `taskOf` from `e2e/parity/scheduledtransition` (defined in T-3's file; T-3 lands before R-11).

- [ ] **Step 1: Rewrite `scheduled_transition_test.go`**

Header (:15-33):

```go
// --- Polling / audit helpers (running-backend e2e).
//
// Every test here that needs a fire builds a stack with a live scheduler on a
// database of its own (newSchedulerHarness): a claim is cross-tenant, so a
// scheduler on the shared database would run other tests' tasks. A small
// Schedule.DelayMs plus generous, bounded polling — never a bare time.Sleep as
// the sole detector of a positive outcome.

// scheduledFireTimeout bounds every poll loop that waits for a fire. The
// scheduler stacks claim every 50ms (schedulerTuning); the bound is sized for
// slow CI, so a real bug — not the scan cadence — trips it.
const scheduledFireTimeout = 15 * time.Second
```

Delete `startTestScheduler` and `awaitEntityStateE2E`; drop the
`internal/cluster` and `internal/scheduler` imports. Add to
`scheduler_harness_test.go`:

```go
// schedEvents returns up to 500 StateMachine events of the entity on h's
// stack, newest first — enough for a write-heavy history (the endpoint's
// default page is 20).
func schedEvents(t *testing.T, h *callbackHarness, entityID string) []map[string]any {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, "/api/audit/entity/"+entityID+"?eventType=StateMachine&limit=500", "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit GET %s: %d %s", entityID, resp.StatusCode, body)
	}
	var r struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode audit page: %v (body %s)", err, body)
	}
	return r.Items
}
```

`TestE2E_ScheduledTransition_FiresThroughHTTPStack` becomes (the comment keeps
its first paragraph, "a scheduler.Service of this test's own" → "the
scheduler of this test's own stack"):

```go
func TestE2E_ScheduledTransition_FiresThroughHTTPStack(t *testing.T) {
	const delayMs = 200
	h, s := newSchedulerHarness(t, nil)
	model := uniq("e2e-scheduled-fires-http")
	h.SetupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-fires-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 200}}]},
				"Closed": {}
			}
		}]
	}`)

	beforeCreate := time.Now().Truncate(time.Millisecond)
	entityID, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	arms := smEventsOfType(schedEvents(t, h, entityID), "SCHEDULED_TRANSITION_ARM")
	if len(arms) == 0 {
		t.Fatalf("expected a SCHEDULED_TRANSITION_ARM audit event after creation (transition must be scheduled, not fired inline)")
	}
	scheduledFor := smEventScheduledTime(t, arms[0])
	if min := beforeCreate.Add(delayMs * time.Millisecond); scheduledFor.Before(min) {
		t.Errorf("armed fire time %s is before %s, the create's start plus %dms — the delay was not applied",
			scheduledFor.Format(time.RFC3339Nano), min.Format(time.RFC3339Nano), delayMs)
	}

	awaitCallbackEntityState(t, h, entityID, "Closed", scheduledFireTimeout)

	events := schedEvents(t, h, entityID)
	fires := smEventsOfType(events, "SCHEDULED_TRANSITION_FIRE")
	if len(fires) != 1 || !hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Fatalf("want exactly one SCHEDULED_TRANSITION_FIRE with state Closed; got events: %+v", events)
	}
	if firedAt := smEventTime(t, fires[0]); firedAt.Before(scheduledFor) {
		t.Errorf("fired at %s, before its armed fire time %s", firedAt.Format(time.RFC3339Nano), scheduledFor.Format(time.RFC3339Nano))
	}
	if r, ok := s.task(t, entityID, "AutoClose"); ok {
		t.Errorf("the fired task is still stored: %+v; spec §4 removes it", r)
	}
}
```

`TestE2E_ScheduledTransition_LoopbackDefersTimer`: `h, _ := newSchedulerHarness(t, nil)`;
`model := uniq("e2e-scheduled-loopback-defers")`; `setupModelWithWorkflow(t, model, wf)`
→ `h.SetupModelWithWorkflow(t, model, wf)`; `createEntityE2E` → `h.CreateEntity`
(assert 200); `doAuth(t, …)` → `h.DoAuth(t, …, "")` and `readBody` →
`h.readBody`; `getSMAuditEventsWithLimit(t, entityID, 500)` and
`getSMAuditEvents` → `schedEvents(t, h, entityID)`; `getEntityState(t, id)` →
`h.GetEntityState(t, id)`; `awaitEntityStateE2E(t, …)` →
`awaitCallbackEntityState(t, h, …)`. The assertions stay as they are.

`TestE2E_ScheduledTransition_RestartDurability` (doc rewritten: "A task armed
by one pnode process survives that process and is fired by the next one on the
same storage."):

```go
func TestE2E_ScheduledTransition_RestartDurability(t *testing.T) {
	first, s := newSchedulerHarness(t, func(cfg *app.Config) { cfg.Scheduler.Enabled = false })
	model := uniq("e2e-scheduled-restart-durability")
	first.SetupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-restart-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 800}}]},
				"Closed": {}
			}
		}]
	}`)
	entityID, status, body := first.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	if r, ok := s.task(t, entityID, "AutoClose"); !ok || r.Status != "WAITING" {
		t.Fatalf("armed task = %+v present=%t; want a stored WAITING task before any scheduler ran", r, ok)
	}

	// The first process goes away: its scheduler never ran, nothing of the
	// task lives in its memory.
	first.app.Shutdown()
	if err := first.app.Close(); err != nil {
		t.Fatalf("close the first stack: %v", err)
	}

	second := newStackOn(t, s, nil)
	awaitCallbackEntityState(t, second, entityID, "Closed", scheduledFireTimeout)
	if !hasSMEventType(schedEvents(t, second, entityID), "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Error("the restarted stack fired the task without a SCHEDULED_TRANSITION_FIRE event")
	}
}
```

(`first`'s `t.Cleanup` still calls `Shutdown` and `Close` again; T-1's Open
point 4 records that both must be safe to call twice.)

- [ ] **Step 2: Rewrite `scheduled_function_test.go`**

Header (:38-44):

```go
// Tests that need a fire run on newSchedulerCallbackHarness: a stack with a
// live scheduler on a database of their own, and the default cnode (tag
// sched-fn). Storage-level assertions (exact scheduled_time/timeout_ms) query
// that database's scheduled_tasks table.
```

Tests 200, 242, 282, 501: `h := newCallbackHarness(t)` →
`h, s := newSchedulerCallbackHarness(t, nil)`; each
`queryDB(t, "test-tenant", SQL, args…)` → `s.count(t, SQL, args…)`;
`const model = "…"` → `model := uniq("…")`.

`TestScheduledFunction_ExpiryElapsedBeforeScan_ExpiresNoFire` (comment
rewritten: "…once the deadline — scheduledTime + timeoutMs — has passed before
the first claim, the task is expired, never fired (spec §5.1 step 4, first
rule). The first stack has no scheduler; the second opens the same database
after the deadline, so its first claim is late whatever the scan cadence."):

```go
func TestScheduledFunction_ExpiryElapsedBeforeScan_ExpiresNoFire(t *testing.T) {
	first, s := newSchedulerHarness(t, func(cfg *app.Config) { cfg.Scheduler.Enabled = false })
	first.member = first.AttachCnode(t, cnodeSpec{name: "default", tags: []string{scheduledFnTag}, script: first.registeredScript}).m
	model := uniq("e2e-schedfn-expiry-elapsed")
	const fireAfterMs = int64(50)
	const expireAfterMs = int64(60) // timeoutMs = 10ms

	first.RegisterFunction("calcExpireElapsed", func(rc *reqCtx) (string, map[string]any, error) {
		return "Schedule", map[string]any{"fireAfterMs": fireAfterMs, "expireAfterMs": expireAfterMs}, nil
	})
	first.SetupModelWithWorkflow(t, model, scheduleFunctionWorkflowJSON("schedfn-expireelapsed-wf", validScheduleFunctionJSON("calcExpireElapsed")))
	entityID, status, body := first.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}

	// Past fire (50ms) + timeoutMs (10ms), with room for a slow machine,
	// before any scheduler exists.
	time.Sleep(500 * time.Millisecond)
	second := newStackOn(t, s, nil)

	awaitCallbackSMEventType(t, second, entityID, "SCHEDULED_TRANSITION_EXPIRE", "Open", scheduledFireTimeout)
	if hasSMEventType(second.GetSMAuditEvents(t, entityID), "SCHEDULED_TRANSITION_FIRE", "") {
		t.Error("the task fired; its deadline had passed before the first claim")
	}
	if st, _ := second.GetEntityState(t, entityID); st != "Open" {
		t.Errorf("state = %q; want Open (expired, not fired)", st)
	}
	if n := s.count(t, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1", entityID); n != 0 {
		t.Errorf("%d scheduled_tasks rows after the expiry; want 0 (spec §4: removed)", n)
	}
}
```

Drop the `internal/cluster` and `internal/scheduler` imports.

- [ ] **Step 3: Rewrite `scheduled_attribution_test.go`, `callout_modes_test.go`, `tx_lifecycle_e2e_test.go`, `callout_handover_lost_test.go`, `e2e_test.go`**

`scheduled_attribution_test.go`: the five harness constructions named in row
10 become `h, s := newSchedulerCallbackHarness(t, nil)` (`_` for `s` except in
`ValidFunctionResultArmsTrueOrigin`); :346 `dbPool.QueryRow(context.Background(), …)`
→ `s.pool.QueryRow(context.Background(), …)`. Header :18-24:

```go
// A scheduled transition is armed by whoever's causal origin was in effect at
// arm time (spi.ResolveOrigin -> ScheduledTask.ArmedBy, arm.go), and fired
// later by the platform scheduler. The claimed task carries ArmedBy, and
// FireScheduledTransition stamps the fired anchor's attributed principal from
// it and its EXECUTOR as the system principal {id:"system", kind:"system"} —
// never the literal string "scheduler".
```

`callout_modes_test.go:111-118`:

```go
func TestCalloutModes_ScheduledFire(t *testing.T) {
	// A stack of its own with a live scheduler on a database of its own; three
	// retries after the first try, 100ms of patience.
	h, _ := newSchedulerHarness(t, calloutTuning(3, 100*time.Millisecond))
```

`tx_lifecycle_e2e_test.go`: delete :510-736 except `clusterHMACSecret32`,
whose comment becomes "clusterHMACSecret32 is the fixed cluster secret of the
single-node cluster stacks the handover tests build. Exactly 32 bytes: both
dispatch.NewAEADPeerAuth (>= 32) and memberlist's AES key (16/24/32) accept
it." Remove imports that become unused (`internal/cluster` and
`internal/admin` if no other user remains — the compiler says which).
`callout_handover_lost_test.go:41`: drop "schedulerTaskPathForTest
(tx_lifecycle_e2e_test.go) sets" and say what the handover route constant
itself is.

`e2e_test.go:158-172`:

```go
	// No scheduler on the package-global testApp. A claim is cross-tenant,
	// and this database is shared by every per-test stack; a scheduler here
	// would claim tasks those stacks armed and run them through testApp's
	// engine, whose processors (procSvc) are not theirs. Tests that need a
	// fire build a stack with a scheduler on a database of their own
	// (newSchedulerHarness). Plain config, no test hook.
	cfg.Scheduler.Enabled = false
```

- [ ] **Step 4: Rewrite the parity files**

`scheduledtransition.go:15-17`: "Exact scheduledTime, lateness, retry-delay and
deadline arithmetic is therefore Unit-only (internal/domain/workflow and
internal/scheduler unit tests) — never asserted here."

`RunScheduledTransition_FiresOnTime`, after the FIRE assertion:

```go
	if task := taskOf(t, c, entityID, "AutoClose"); task != nil {
		t.Errorf("the fired task is still listed: %+v; a fired task is removed", *task)
	}
```

`RunScheduledTransition_DeclineCriterionFalse`, after its last assertion: the
same three lines ("a declined task is removed").

`scheduledfunction.go:24-32`: "…instead it arms with an ALREADY-past absolute
fireAt/expireAt pair, so the deadline (scheduledTime + timeoutMs) has passed
at arm time and the very first claim, whatever its cadence, finds it late."
`RunScheduledFunction_ExpiryElapsedExpiresNoFire` comment: the same without
"grace"; the inline comment "(~5000ms) already vastly exceeds
timeoutMs(110ms)+grace(100ms)" → "(~5000ms) already exceeds timeoutMs
(110ms)"; the error text "lateness already exceeded timeout+grace" → "the
deadline had passed before the first claim". After the last assertion:

```go
	page, err := c.ListScheduledTasks(t, url.Values{"entityId": {entityID.String()}}) // add "net/url" to the imports
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("an expired task is still listed: %+v; it is removed", page.Items)
	}
```

`cmd/compute-test-client/catalog.go:288-295`: the expiryElapsed bullet ends
"…a VALID (non-born-expired) arm whose deadline has already passed at arm
time, so the very first claim expires it deterministically regardless of scan
cadence."

`multinode/attribution.go`: `RunAttribution_ScheduledFire` loses :205-216
(log level) and :252-270 (peer fire); its doc becomes "arms several tasks as a
USER; whichever pnode claims each, the claimed task carries ArmedBy, so every
fired change attributes to the arming user, executed by the system." The
package comment's scenario 2 (:26-31) says the same. `AttributionCapable`
keeps `ComputeUser` only; `NodeLogs` goes from the interface (:74-78), and
`attrRequireCapable` is unchanged.

- [ ] **Step 5: Run and see them pass**

Run: `go vet ./... && go test ./internal/e2e/ -run 'TestE2E_ScheduledTransition|TestScheduledFunction|TestAttribution_Scheduled|TestCalloutModes_ScheduledFire'`
Expected: vet clean; PASS.
Run: `go test -count=1 ./e2e/parity/memory/ ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -run 'TestParity/Scheduled|TestMultiNode/Attribution_ScheduledFire'`
Expected: PASS.
Run: `git grep -n -e startTestScheduler -e NewClusterExecutor -e NewSchedulerEngine -e schedulerTaskPathForTest -e postSchedulerRPC -e 'peer fire resolved' -e 'grace' -- internal/e2e e2e cmd/compute-test-client | grep -iv -e 'gracePeriod' -e 'Grace\b' -e 'graceful'`
Expected: no output. (`oauth_keys_test.go`, `token_exchange_test.go` use
"grace period" for signing keys, and `torn_connection_e2e_test.go:35`,
`async_stream_test.go:838, 1129` say "graceful"; the filter keeps those.)

**Teeth.** Row 5: in R's scheduler `Start`, return before the claim loop
starts (`return nil` as the first statement) — `RestartDurability` times out
in `awaitCallbackEntityState`. Row 7: in E's §5.1 step 4, compare against
`deadline + RetryDelay` on the first attempt too — the test sees a FIRE.

- [ ] **Step 6: Commit** — R-11 Step 6 makes this commit, with R-11's files
  (the list below is part of it; do not commit it separately).

```bash
git add internal/e2e/scheduled_transition_test.go internal/e2e/scheduled_function_test.go \
  internal/e2e/scheduled_attribution_test.go internal/e2e/callout_modes_test.go \
  internal/e2e/tx_lifecycle_e2e_test.go internal/e2e/callout_handover_lost_test.go \
  internal/e2e/e2e_test.go internal/e2e/scheduler_harness_test.go \
  e2e/parity/scheduledtransition/scheduledtransition.go \
  e2e/parity/scheduledfunction/scheduledfunction.go \
  e2e/parity/multinode/attribution.go e2e/parity/multinode/attribution_skip_test.go \
  cmd/compute-test-client/catalog.go
git commit -m "test(scheduler): rewrite the scheduled tests for one owner per run

The bespoke scan loops, the grace band, the scheduler RPC and the
peer-fire log line are gone. Tests that need a fire run on a stack with
its own scheduler and database; the restart tests restart a real stack.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-3: parity — the endings on every backend

**Spec:** §4 (statuses and endings), §5.1, §5.5, §5.6, §5.7, §7; §13
"Endings", column P; §13 "Entity writes and workflow import", the P cells of
"FAILED task re-armed by an update in the state", "FAILED task cancelled when
the entity leaves the state" (assigned to T by the lead; W-writes.md Open point
5) and "a task whose transition is no longer scheduled is removed at the next
write" (W-writes.md Open point 5 proposes T; see this section's Open point 6);
§13 "`GET /scheduled-tasks`" P cell "200, a FAILED item" (assigned to T by the
lead; Q-query.md Open point 2).

**Depends on:** S, BM, BQ, BP (store), K (the `NotHandedOff` proof), E (fire
path, mark, endings), R-1…R-10 (claim loop, bookkeeping, wiring), Q-5 (the
query client the scenarios read), T-1. Lands before R-11, whose T-2 rewrites
use `taskOf` (README C-P5).

**Files:**
- Create: `e2e/parity/scheduledtransition/ownership.go`

**Interfaces:**
- Consumes (Q-5): `(*client.Client).ListScheduledTasks`, `client.ScheduledTask`. (T-1): `parity.ComputeBehaviourHold`. (H of #254) `parity.StartComputeClientOrSkip`, `parity.AwaitReceived`, `parity.ComputeBehaviourFail`, `parity.ComputeClient.Received`.
- Consumes (catalog, `cmd/compute-test-client/catalog.go`): processor `noop` (:79-81), criterion `inject-criterion-error-retryable` (:222-224, message `inject-criterion-error-retryable: deliberate failure`); the `fail` behaviour's message `scripted failure: fail` (`dispatch.go:277`).
- Produces: `taskOf`, `awaitTask`, `ownWorkflow`, `ownProc`, `receivedFor`, `countEvents`, `eventFailed` in package `scheduledtransition`.

Each scenario: a fresh tenant (`fixture.NewTenant`), a tag of its own (tags
are short: `st-` plus 6 characters), compute clients started under that
tenant, and a `t.Cleanup` that deletes an entity whose task would otherwise
keep retrying on the shared server.

**Why the first non-zero `attempts` is exactly 1.** The tuned retry delay is
1 s; the scenarios poll every 50 ms. The first poll that sees `attempts ≥ 1`
sees 1 unless the poll loop stalled a whole second.

- [ ] **Step 1: Write the scenarios**

`e2e/parity/scheduledtransition/ownership.go`:

```go
package scheduledtransition

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// ownership.go — the endings of a scheduled run on every backend (spec §4):
// retried safe failures, FAILED tasks and why, the audit event of a failure,
// and the fire-time cancel. Each scenario owns a tenant and a tag; nothing
// here runs two things at once (concurrency lives in internal/e2e and the
// multi-node tests).

func init() {
	parity.Register(
		parity.NamedTest{Name: "ScheduledTransition_NoComputeNodeThenFires", Fn: RunScheduledTransition_NoComputeNodeThenFires},
		parity.NamedTest{Name: "ScheduledTransition_SelfLoopReArmsNewLife", Fn: RunScheduledTransition_SelfLoopReArmsNewLife},
		parity.NamedTest{Name: "ScheduledTransition_CriterionErrorRetried", Fn: RunScheduledTransition_CriterionErrorRetried},
		parity.NamedTest{Name: "ScheduledTransition_IdempotentFailureRetried", Fn: RunScheduledTransition_IdempotentFailureRetried},
		parity.NamedTest{Name: "ScheduledTransition_LateAfterFailedAttemptsFails", Fn: RunScheduledTransition_LateAfterFailedAttemptsFails},
		parity.NamedTest{Name: "ScheduledTransition_UnsafeFailureFails", Fn: RunScheduledTransition_UnsafeFailureFails},
		parity.NamedTest{Name: "ScheduledTransition_LaterStepFailsAfterUnsafeHandOff", Fn: RunScheduledTransition_LaterStepFailsAfterUnsafeHandOff},
		parity.NamedTest{Name: "ScheduledTransition_FireTimeCancelNotScheduled", Fn: RunScheduledTransition_FireTimeCancelNotScheduled},
		parity.NamedTest{Name: "ScheduledTransition_FailedTaskReArmedByUpdate", Fn: RunScheduledTransition_FailedTaskReArmedByUpdate},
		parity.NamedTest{Name: "ScheduledTransition_FailedTaskCancelledWhenEntityLeaves", Fn: RunScheduledTransition_FailedTaskCancelledWhenEntityLeaves},
		parity.NamedTest{Name: "ScheduledTransition_NoLongerScheduledRemovedAtNextWrite", Fn: RunScheduledTransition_NoLongerScheduledRemovedAtNextWrite},
	)
}

const eventFailed = "SCHEDULED_TRANSITION_FAIL"

// ownSample declares every field the scenarios' entities carry.
const ownSample = `{"k":1,"flavor":"one"}`

// taskOf returns the entity's task for transition as GET /scheduled-tasks
// shows it, or nil when there is none.
func taskOf(t *testing.T, c *client.Client, entityID uuid.UUID, transition string) *client.ScheduledTask {
	t.Helper()
	page, err := c.ListScheduledTasks(t, url.Values{"entityId": {entityID.String()}, "limit": {"1000"}})
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	for i := range page.Items {
		if page.Items[i].Transition == transition {
			return &page.Items[i]
		}
	}
	return nil
}

// awaitTask polls the entity's task every 50ms and returns the first view for
// which cond holds.
func awaitTask(t *testing.T, c *client.Client, entityID uuid.UUID, transition string, within time.Duration, what string, cond func(*client.ScheduledTask) bool) *client.ScheduledTask {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		task := taskOf(t, c, entityID, transition)
		if cond(task) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s/%s: %s not seen within %s; last: %+v", entityID, transition, what, within, task)
		}
		time.Sleep(pollInterval)
	}
}

// ownWorkflow wraps states in a schema 1.5 import document (the minor that
// accepts a processor's idempotent and retryPolicy) with initial state Open.
func ownWorkflow(wfName string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.5", "name": wfName, "initialState": "Open", "active": true, "states": states,
		}},
	})
	return string(b)
}

// ownProc is one SYNC catalog processor routed to tag, with one try.
func ownProc(name, tag string, idempotent bool) map[string]any {
	return map[string]any{"type": "calculator", "name": name, "executionMode": "SYNC",
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag,
			"idempotent": idempotent, "retryPolicy": "NONE", "responseTimeoutMs": 10000}}
}

// fireToDone is Open -[Fire, scheduled]-> Done. timeoutMs 0 leaves it off.
func fireToDone(delayMs, timeoutMs int64, procs ...map[string]any) map[string]any {
	sched := map[string]any{"delayMs": delayMs}
	if timeoutMs > 0 {
		sched["timeoutMs"] = timeoutMs
	}
	fire := map[string]any{"name": "Fire", "next": "Done", "manual": false, "schedule": sched}
	if len(procs) > 0 {
		list := make([]any, 0, len(procs))
		for _, p := range procs {
			list = append(list, p)
		}
		fire["processors"] = list
	}
	return map[string]any{
		"Open": map[string]any{"transitions": []any{fire}},
		"Done": map[string]any{},
	}
}

// receivedFor counts the requests cc received for entityID.
func receivedFor(t *testing.T, cc parity.ComputeClient, entityID uuid.UUID) int {
	t.Helper()
	n := 0
	for _, r := range cc.Received(t) {
		if r.EntityID == entityID.String() {
			n++
		}
	}
	return n
}

// countEvents counts the entity's StateMachine events of eventType.
func countEvents(t *testing.T, c *client.Client, id uuid.UUID, eventType string) int {
	t.Helper()
	n := 0
	for _, ev := range stateMachineEvents(t, c, id) {
		if ev.EventType == eventType {
			n++
		}
	}
	return n
}

// failEventData decodes the data of the entity's SCHEDULED_TRANSITION_FAIL.
func failEventData(t *testing.T, c *client.Client, id uuid.UUID) map[string]any {
	t.Helper()
	ev := awaitStateMachineEvent(t, c, id, eventFailed, "", fireTimeout)
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode %s data: %v (%s)", eventFailed, err, ev.Data)
	}
	return data
}

// ownCase is one scenario's tenant, client and tag.
type ownCase struct {
	tenant parity.Tenant
	c      *client.Client
	tag    string
}

func newOwnCase(t *testing.T, fixture parity.BackendFixture) ownCase {
	t.Helper()
	tenant := fixture.NewTenant(t)
	return ownCase{tenant: tenant, c: client.NewClient(fixture.BaseURL(), tenant.Token), tag: "st-" + uuid.NewString()[:6]}
}

func (oc ownCase) start(t *testing.T, fixture parity.BackendFixture, tag, behaviour string) parity.ComputeClient {
	t.Helper()
	return parity.StartComputeClientOrSkip(t, fixture, parity.ComputeClientSpec{TenantID: oc.tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
}

func (oc ownCase) create(t *testing.T, model, wf string) uuid.UUID {
	t.Helper()
	setupModelWithWorkflow(t, oc.c, model, 1, ownSample, wf)
	id, err := oc.c.CreateEntity(t, model, 1, ownSample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	return id
}

// deleteOnCleanup removes an entity whose task keeps retrying, so it does
// not run for the rest of the shared server's life.
func (oc ownCase) deleteOnCleanup(t *testing.T, id uuid.UUID) {
	t.Cleanup(func() { _ = oc.c.DeleteEntity(t, id) })
}

// RunScheduledTransition_NoComputeNodeThenFires: an unsafe processor whose tag
// has no compute node. The coordinator proves nothing was handed off
// (NotHandedOff), so the run clears the mark it wrote and the task goes back
// to WAITING, attempts 1 (spec §5.5, §5.6). When a compute node appears the
// retry fires. Had the mark stayed, the next claim would have ended the task
// FAILED UNSAFE_WORK_NOT_COMPLETED (§5.1 step 1) — the fire is the proof it
// was cleared.
func RunScheduledTransition_NoComputeNodeThenFires(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	id := oc.create(t, "st-own-nocn", ownWorkflow("st-own-nocn-wf", fireToDone(100, 0, ownProc("noop", oc.tag, false))))

	first := awaitTask(t, oc.c, id, "Fire", fireTimeout, "a recorded attempt",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Attempts >= 1 })
	if first.Status != "WAITING" || first.Attempts != 1 || first.FailureReason != "" {
		t.Fatalf("after the first attempt: %+v; want WAITING, attempts 1, no failure reason", *first)
	}
	if !strings.HasPrefix(first.LastError, "NO_COMPUTE_MEMBER_FOR_TAG: ") {
		t.Errorf("lastError = %q; want the NO_COMPUTE_MEMBER_FOR_TAG text", first.LastError)
	}
	if first.NextAttemptTime == nil || first.LastAttemptTime == nil {
		t.Errorf("a WAITING task after a failed attempt shows nextAttemptTime and lastAttemptTime: %+v", *first)
	}

	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourCatalog)
	awaitEntityState(t, oc.c, id, "Done", fireTimeout)
	if n := countEvents(t, oc.c, id, eventFired); n != 1 {
		t.Errorf("%d %s events; want 1", n, eventFired)
	}
	if n := countEvents(t, oc.c, id, eventFailed); n != 0 {
		t.Errorf("%d %s events; want 0", n, eventFailed)
	}
	if n := receivedFor(t, cc, id); n != 1 {
		t.Errorf("the compute node received %d requests; want 1 (the first attempt reached none)", n)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task != nil {
		t.Errorf("the fired task is still listed: %+v", *task)
	}
}

// RunScheduledTransition_SelfLoopReArmsNewLife: Open -[Tick]-> Open. Each fire
// lands in the source state again and re-arms the same task id as a new life
// (spec §5.2 "a self-loop re-arms the same id"): same taskId, a later
// armedTime, attempts back to 0.
func RunScheduledTransition_SelfLoopReArmsNewLife(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	id := oc.create(t, "st-own-loop", ownWorkflow("st-own-loop-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Tick", "next": "Open", "manual": false, "schedule": map[string]any{"delayMs": 300},
		}}},
	}))
	oc.deleteOnCleanup(t, id)

	armed := taskOf(t, oc.c, id, "Tick")
	if armed == nil {
		t.Fatal("no task after the create")
	}
	deadline := time.Now().Add(fireTimeout)
	for countEvents(t, oc.c, id, eventFired) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than two fires within %s", fireTimeout)
		}
		time.Sleep(pollInterval)
	}
	later := awaitTask(t, oc.c, id, "Tick", fireTimeout, "the re-armed task",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.ArmedTime.After(armed.ArmedTime) })
	if later.TaskID != armed.TaskID {
		t.Errorf("taskId changed across lives: %s then %s; a self-loop re-arms the same id", armed.TaskID, later.TaskID)
	}
	if later.Attempts != 0 || later.LostOwners != 0 || later.LastError != "" {
		t.Errorf("re-armed task = %+v; a new life starts at attempts 0, lostOwners 0, no error", *later)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_CriterionErrorRetried: a criterion callout fails.
// Criteria are repeat-safe (spec §3), so this is a safe failure: WAITING,
// attempts 1, the compute node's message recorded; never FAILED.
func RunScheduledTransition_CriterionErrorRetried(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	oc.start(t, fixture, oc.tag, parity.ComputeBehaviourCatalog)
	states := map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": map[string]any{"type": "function", "function": map[string]any{
				"name":   "inject-criterion-error-retryable",
				"config": map[string]any{"calculationNodesTags": oc.tag, "attachEntity": true},
			}},
		}}},
		"Done": map[string]any{},
	}
	id := oc.create(t, "st-own-crit", ownWorkflow("st-own-crit-wf", states))
	oc.deleteOnCleanup(t, id)

	first := awaitTask(t, oc.c, id, "Fire", fireTimeout, "a recorded attempt",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Attempts >= 1 })
	if first.Status != "WAITING" || first.Attempts != 1 || first.FailureReason != "" {
		t.Fatalf("after the failed criterion: %+v; want WAITING, attempts 1", *first)
	}
	if !strings.Contains(first.LastError, "inject-criterion-error-retryable: deliberate failure") {
		t.Errorf("lastError = %q; want the compute node's own message", first.LastError)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_IdempotentFailureRetried: a processor declared
// idempotent fails on its compute node. That is a safe failure: WAITING,
// attempts 1, and the platform may send it again — it does.
func RunScheduledTransition_IdempotentFailureRetried(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-idem", ownWorkflow("st-own-idem-wf", fireToDone(100, 0, ownProc("noop", oc.tag, true))))
	oc.deleteOnCleanup(t, id)

	first := awaitTask(t, oc.c, id, "Fire", fireTimeout, "a recorded attempt",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Attempts >= 1 })
	if first.Status != "WAITING" || first.Attempts != 1 || first.FailureReason != "" {
		t.Fatalf("after the failed idempotent processor: %+v; want WAITING, attempts 1", *first)
	}
	if !strings.Contains(first.LastError, "scripted failure: fail") {
		t.Errorf("lastError = %q; want the compute node's own message", first.LastError)
	}
	deadline := time.Now().Add(fireTimeout)
	for receivedFor(t, cc, id) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the idempotent processor was not sent again within %s", fireTimeout)
		}
		time.Sleep(pollInterval)
	}
	if n := countEvents(t, oc.c, id, eventFailed); n != 0 {
		t.Errorf("%d %s events; a safe failure never fails the task", n, eventFailed)
	}
}

// RunScheduledTransition_LateAfterFailedAttemptsFails: timeoutMs 1500 and a
// processor that always fails. The retries are clamped to the deadline, the
// last attempt runs up to RETRY_DELAY past it, and the counted attempt after
// the deadline ends the task FAILED EXPIRED_AFTER_FAILED_ATTEMPTS (spec §5.1,
// §5.6). The entity never moves.
func RunScheduledTransition_LateAfterFailedAttemptsFails(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-late", ownWorkflow("st-own-late-wf", fireToDone(100, 1500, ownProc("noop", oc.tag, true))))

	failed := awaitTask(t, oc.c, id, "Fire", 20*time.Second, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "EXPIRED_AFTER_FAILED_ATTEMPTS" || failed.Attempts < 1 || failed.FailedTime == nil {
		t.Errorf("failed task = %+v; want EXPIRED_AFTER_FAILED_ATTEMPTS, attempts >= 1, failedTime set", *failed)
	}
	if data := failEventData(t, oc.c, id); data["reason"] != "EXPIRED_AFTER_FAILED_ATTEMPTS" {
		t.Errorf("%s data = %v; want reason EXPIRED_AFTER_FAILED_ATTEMPTS", eventFailed, data)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; a FAILED task never moves the entity", got.Meta.State)
	}
}

// RunScheduledTransition_UnsafeFailureFails: a processor not declared
// idempotent reached its compute node and the run did not commit. The task
// ends FAILED UNSAFE_WORK_NOT_COMPLETED, with a SCHEDULED_TRANSITION_FAIL
// event carrying {transition, sourceState, reason, attempts, lostOwners}, and
// the processor is never sent again (spec §5.5, §5.7).
func RunScheduledTransition_UnsafeFailureFails(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-unsafe", ownWorkflow("st-own-unsafe-wf", fireToDone(100, 0, ownProc("noop", oc.tag, false))))

	// The FAILED item as GET /scheduled-tasks shows it (§8): status, reason,
	// error, times and attempts. Fail does not count an attempt (§10.1: only
	// RecordAttempt adds 1), and this was the first run: attempts 0.
	failed := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", failed.FailureReason)
	}
	if failed.Attempts != 0 || failed.LostOwners != 0 {
		t.Errorf("attempts %d, lostOwners %d; want 0 and 0", failed.Attempts, failed.LostOwners)
	}
	if failed.FailedTime == nil || failed.FailedTime.Before(failed.ScheduledTime) {
		t.Errorf("failedTime = %v; want a time at or after scheduledTime %v", failed.FailedTime, failed.ScheduledTime)
	}
	if failed.NextAttemptTime != nil {
		t.Errorf("nextAttemptTime = %v; a FAILED item has none (§8: WAITING only)", failed.NextAttemptTime)
	}
	if !strings.Contains(failed.LastError, "scripted failure: fail") {
		t.Errorf("lastError = %q; want the compute node's own message", failed.LastError)
	}
	data := failEventData(t, oc.c, id)
	if data["transition"] != "Fire" || data["sourceState"] != "Open" || data["reason"] != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("%s data = %v; want transition Fire, sourceState Open, reason UNSAFE_WORK_NOT_COMPLETED", eventFailed, data)
	}
	if data["attempts"] != float64(0) || data["lostOwners"] != float64(0) {
		t.Errorf("%s data attempts/lostOwners = %v/%v; want 0/0, as the item", eventFailed, data["attempts"], data["lostOwners"])
	}

	// Never re-run: three retry delays later the compute node still has one
	// request, and the task is still FAILED.
	time.Sleep(3 * time.Second)
	if n := receivedFor(t, cc, id); n != 1 {
		t.Errorf("the unsafe processor was sent %d times; want exactly 1", n)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task == nil || task.Status != "FAILED" {
		t.Errorf("task = %+v; a FAILED task is kept", task)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_LaterStepFailsAfterUnsafeHandOff: the unsafe first
// processor succeeds on its compute node; the idempotent second one fails, so
// the run does not commit. Unsafe work reached a compute node: FAILED
// UNSAFE_WORK_NOT_COMPLETED, and neither processor is sent again.
func RunScheduledTransition_LaterStepFailsAfterUnsafeHandOff(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	tagB := oc.tag + "b"
	unsafe := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourCatalog)
	failing := oc.start(t, fixture, tagB, parity.ComputeBehaviourFail)
	id := oc.create(t, "st-own-later", ownWorkflow("st-own-later-wf",
		fireToDone(100, 0, ownProc("noop", oc.tag, false), ownProc("noop", tagB, true))))

	failed := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", failed.FailureReason)
	}
	time.Sleep(3 * time.Second)
	if a, b := receivedFor(t, unsafe, id), receivedFor(t, failing, id); a != 1 || b != 1 {
		t.Errorf("requests: unsafe %d, failing %d; want 1 and 1", a, b)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// RunScheduledTransition_FireTimeCancelNotScheduled: the model has two
// workflows selected by $.flavor. The entity (flavor one) arms Fire under W1.
// A re-import makes Fire manual in W1 and scheduled in W2, so the import keeps
// the task (it is scheduled in some workflow of the model, spec §7), but at
// fire time the selected workflow W1 no longer schedules it: the task is
// removed with SCHEDULED_TRANSITION_CANCEL and the entity stays (spec §4).
func RunScheduledTransition_FireTimeCancelNotScheduled(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	const model = "st-own-cancel"
	doc := func(scheduledIn string) string {
		wf := func(name, flavor string, scheduled bool) map[string]any {
			fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
			if scheduled {
				fire["schedule"] = map[string]any{"delayMs": 2500}
			}
			return map[string]any{
				"version": "1.5", "name": name, "initialState": "Open", "active": true,
				"criterion": map[string]any{"type": "simple", "jsonPath": "$.flavor", "operatorType": "EQUALS", "value": flavor},
				"states": map[string]any{
					"Open": map[string]any{"transitions": []any{fire}},
					"Done": map[string]any{},
				},
			}
		}
		b, _ := json.Marshal(map[string]any{"importMode": "REPLACE", "workflows": []any{
			wf("st-own-cancel-w1", "one", scheduledIn == "w1"),
			wf("st-own-cancel-w2", "two", scheduledIn == "w2"),
		}})
		return string(b)
	}
	id := oc.create(t, model, doc("w1"))
	if taskOf(t, oc.c, id, "Fire") == nil {
		t.Fatal("no task after the create under W1")
	}
	if err := oc.c.ImportWorkflow(t, model, 1, doc("w2")); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if taskOf(t, oc.c, id, "Fire") == nil {
		t.Fatal("the re-import removed the task; it is still scheduled in W2, so the import keeps it")
	}

	awaitStateMachineEvent(t, oc.c, id, eventCancelled, "Open", fireTimeout)
	awaitTask(t, oc.c, id, "Fire", fireTimeout, "removal", func(tk *client.ScheduledTask) bool { return tk == nil })
	if n := countEvents(t, oc.c, id, eventFired); n != 0 {
		t.Errorf("%d %s events; want 0", n, eventFired)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Open" {
		t.Errorf("state = %q; want Open", got.Meta.State)
	}
}

// failOnce builds the shared start of the two FAILED-task scenarios: Open
// carries a scheduled Fire whose unsafe processor always fails, and a manual
// Leave. It waits for the task to end FAILED UNSAFE_WORK_NOT_COMPLETED.
func failOnce(t *testing.T, fixture parity.BackendFixture, model string) (ownCase, parity.ComputeClient, uuid.UUID, *client.ScheduledTask) {
	t.Helper()
	oc := newOwnCase(t, fixture)
	cc := oc.start(t, fixture, oc.tag, parity.ComputeBehaviourFail)
	states := fireToDone(1500, 0, ownProc("noop", oc.tag, false))
	open := states["Open"].(map[string]any)
	open["transitions"] = append(open["transitions"].([]any),
		map[string]any{"name": "Leave", "next": "Left", "manual": true})
	states["Left"] = map[string]any{}
	id := oc.create(t, model, ownWorkflow(model+"-wf", states))
	failed := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Fatalf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", failed.FailureReason)
	}
	return oc, cc, id, failed
}

// RunScheduledTransition_FailedTaskReArmedByUpdate: an entity write in the
// source state re-arms a FAILED task as a new life (spec §4, §7): WAITING,
// attempts 0, no reason, no error, a later armedTime — and no mark, so the new
// life sends its processor again instead of failing on the old life's mark.
func RunScheduledTransition_FailedTaskReArmedByUpdate(t *testing.T, fixture parity.BackendFixture) {
	oc, cc, id, failed := failOnce(t, fixture, "st-own-rearm")
	oc.deleteOnCleanup(t, id)

	if err := oc.c.UpdateEntityData(t, id, `{"k":2,"flavor":"one"}`); err != nil {
		t.Fatalf("update in the source state: %v", err)
	}
	fresh := taskOf(t, oc.c, id, "Fire")
	if fresh == nil {
		t.Fatal("the update removed the task; it re-arms it")
	}
	if fresh.Status != "WAITING" || fresh.Attempts != 0 || fresh.LostOwners != 0 ||
		fresh.FailureReason != "" || fresh.LastError != "" || fresh.FailedTime != nil {
		t.Errorf("re-armed task = %+v; want a new life: WAITING, attempts 0, no reason, no error", *fresh)
	}
	if !fresh.ArmedTime.After(failed.ArmedTime) || fresh.TaskID != failed.TaskID {
		t.Errorf("armedTime %v (was %v), taskId %s (was %s); want a later arm of the same id",
			fresh.ArmedTime, failed.ArmedTime, fresh.TaskID, failed.TaskID)
	}

	// The new life runs: its processor is sent (a second request), and it ends
	// FAILED again, for its own reason.
	deadline := time.Now().Add(fireTimeout)
	for receivedFor(t, cc, id) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the new life never sent its processor; an old-life mark would end it FAILED unsent")
		}
		time.Sleep(pollInterval)
	}
	again := awaitTask(t, oc.c, id, "Fire", fireTimeout, "FAILED again",
		func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if again.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || !again.ArmedTime.Equal(fresh.ArmedTime) {
		t.Errorf("second ending = %+v; want the new life FAILED UNSAFE_WORK_NOT_COMPLETED", *again)
	}
}

// RunScheduledTransition_FailedTaskCancelledWhenEntityLeaves: the entity
// leaves the source state; the FAILED task is removed with
// SCHEDULED_TRANSITION_CANCEL (spec §4, "A FAILED task is never claimed. One
// of these ends it").
func RunScheduledTransition_FailedTaskCancelledWhenEntityLeaves(t *testing.T, fixture parity.BackendFixture) {
	oc, _, id, _ := failOnce(t, fixture, "st-own-leave")
	if err := oc.c.UpdateEntity(t, id, "Leave", `{"k":1,"flavor":"one"}`); err != nil {
		t.Fatalf("manual Leave: %v", err)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task != nil {
		t.Errorf("the FAILED task outlived the state exit: %+v", *task)
	}
	if !hasStateMachineEvent(stateMachineEvents(t, oc.c, id), eventCancelled, "Open") {
		t.Errorf("no %s event for the FAILED task of state Open", eventCancelled)
	}
	if got, _ := oc.c.GetEntity(t, id); got.Meta.State != "Left" {
		t.Errorf("state = %q; want Left", got.Meta.State)
	}
}

// RunScheduledTransition_NoLongerScheduledRemovedAtNextWrite: as
// FireTimeCancelNotScheduled, the re-import keeps a task its entity's
// workflow no longer schedules. The next write of the entity removes it with
// SCHEDULED_TRANSITION_CANCEL — the reconcile removes every task not in the
// new arm set (spec §7) — long before it is due.
func RunScheduledTransition_NoLongerScheduledRemovedAtNextWrite(t *testing.T, fixture parity.BackendFixture) {
	oc := newOwnCase(t, fixture)
	const model = "st-own-nextwrite"
	doc := func(scheduledIn string) string {
		wf := func(name, flavor string, scheduled bool) map[string]any {
			fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
			if scheduled {
				fire["schedule"] = map[string]any{"delayMs": 3600000}
			}
			return map[string]any{
				"version": "1.5", "name": name, "initialState": "Open", "active": true,
				"criterion": map[string]any{"type": "simple", "jsonPath": "$.flavor", "operatorType": "EQUALS", "value": flavor},
				"states":    map[string]any{"Open": map[string]any{"transitions": []any{fire}}, "Done": map[string]any{}},
			}
		}
		b, _ := json.Marshal(map[string]any{"importMode": "REPLACE", "workflows": []any{
			wf("st-own-nw-w1", "one", scheduledIn == "w1"),
			wf("st-own-nw-w2", "two", scheduledIn == "w2"),
		}})
		return string(b)
	}
	id := oc.create(t, model, doc("w1"))
	if err := oc.c.ImportWorkflow(t, model, 1, doc("w2")); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if taskOf(t, oc.c, id, "Fire") == nil {
		t.Fatal("the re-import removed the task; it is still scheduled in W2")
	}
	if err := oc.c.UpdateEntityData(t, id, `{"k":2,"flavor":"one"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if task := taskOf(t, oc.c, id, "Fire"); task != nil {
		t.Errorf("the write kept a task its workflow no longer schedules: %+v", *task)
	}
	if !hasStateMachineEvent(stateMachineEvents(t, oc.c, id), eventCancelled, "Open") {
		t.Errorf("no %s event for the removed task", eventCancelled)
	}
}
```

The file uses `eventFired`, `eventCancelled`, `pollInterval`, `fireTimeout`,
`setupModelWithWorkflow`, `awaitEntityState`, `awaitStateMachineEvent`,
`stateMachineEvents` of `scheduledtransition.go:62-182`.

- [ ] **Step 2: Run on each backend**

Run: `go test -count=1 ./e2e/parity/memory/ -run 'TestParity/ScheduledTransition_'`
then `./e2e/parity/sqlite/`, then `./e2e/parity/postgres/`.
Expected (merge-base plus T-1): FAIL — the query endpoint answers 404 and
`ListScheduledTasks` returns `status 404`; the processor failures leave no
WAITING/FAILED task. After the streams: PASS on all three.

**Teeth.** In R's `decideBookkeeping`, make the row "holds a mark; unsafe work
reached a compute node" return `RecordAttemptKind` instead of `FailKind` —
`UnsafeFailureFails` and `LaterStepFailsAfterUnsafeHandOff` fail with "want
UNSAFE_WORK_NOT_COMPLETED" and a second request. In K's coordinator, drop the
`NotHandedOff` attachment on the no-member exit — `NoComputeNodeThenFires`
ends FAILED instead of firing. In the memory store's `ReconcileForEntity`
(BM), keep the existing row's mark and status when re-arming —
`FailedTaskReArmedByUpdate` fails on "want a new life".

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/scheduledtransition/ownership.go
git commit -m "test(parity): endings of a scheduled run on every backend

Safe failures retried, unsafe work never repeated, FAILED with its
reason and audit event, the fire-time cancel.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-4: e2e — endings, the recorded error, and FAILED tasks under entity writes

**Spec:** §4, §5.1, §5.5, §5.6, §5.7, §5.8, §7; §13 "Endings" column E; the E
cells of "FAILED task re-armed by an update in the state", "FAILED task
cancelled when the entity leaves the state", "a task whose transition is no
longer scheduled is removed at the next write".

**Depends on:** S, BP, K, E, R, T-1.

**Files:**
- Create: `internal/e2e/scheduled_run_endings_test.go`

**Interfaces:**
- Consumes (T-1): `newSchedulerHarness`, `schedDB.{task,awaitTask,count,pool}`, `fireOpenToDone`, `schedDoc`, `sProc`, `failEvent`, `uniq`, `harnessTenant`; (T-2) `schedEvents`; (#254 harness) `AttachCnode`, `cnodeSpec`, `scriptAlways`, `scriptSequence`, `scriptHold`, `answerOK`, `answerFail`, `answerData`, `answerMatches`, `neverAnswer`, `(c) Received`, `(h) setupModelSampleWithWorkflow`, `workflowSampleWith`, `awaitCallbackEntityState`, `awaitCallbackSMEventType`, `hasSMEventType`, `smEventsOfType`.
- Produces: `firstAttempt(t, s, id, transition) taskRow`, `awaitFailed(t, s, id, transition) taskRow`, `receivedFor(recs []receivedCallout, entityID string) int`.

The tests assert against the task row in the test's database (`s.task`),
which shows what the query does not (the mark, the claim, `partial_commit`),
and against the entity's audit trail.

- [ ] **Step 1: Write the tests**

`internal/e2e/scheduled_run_endings_test.go`:

```go
package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// scheduled_run_endings_test.go — every ending of a scheduled run through the
// full stack on PostgreSQL (spec §4), and the text a failed attempt records
// (§5.8). Each test builds a stack with a scheduler on a database of its own.

// firstAttempt waits for the first recorded attempt of the task and returns
// the row as it was then. The retry delay is 1s and the poll 20ms, so the
// first row with attempts >= 1 has attempts 1.
func firstAttempt(t *testing.T, s *schedDB, entityID, transition string) taskRow {
	t.Helper()
	return s.awaitTask(t, entityID, transition, scheduledFireTimeout, "a recorded attempt",
		func(r taskRow, ok bool) bool { return ok && r.Attempts >= 1 })
}

// awaitFailed waits for the task to be FAILED.
func awaitFailed(t *testing.T, s *schedDB, entityID, transition string) taskRow {
	t.Helper()
	return s.awaitTask(t, entityID, transition, scheduledFireTimeout, "FAILED",
		func(r taskRow, ok bool) bool { return ok && r.Status == "FAILED" })
}

// receivedFor counts the callouts in recs that carry entityID.
func receivedFor(recs []receivedCallout, entityID string) int {
	n := 0
	for _, r := range recs {
		if r.EntityID == entityID {
			n++
		}
	}
	return n
}

// createOpen creates one entity of model and asserts 200.
func createOpen(t *testing.T, h *callbackHarness, model, payload string) string {
	t.Helper()
	id, status, body := h.CreateEntity(t, model, 1, payload)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	return id
}

// requireState asserts the entity's state.
func requireState(t *testing.T, h *callbackHarness, id, want string) {
	t.Helper()
	if st, _ := h.GetEntityState(t, id); st != want {
		t.Errorf("state = %q; want %q", st, want)
	}
}

// TestSchedRun_NoComputeNodeThenFires: an unsafe processor whose tag has no
// cnode. MarkUnsafe writes a mark, the dispatch returns the NotHandedOff
// proof, and RecordAttempt{ClearOwnMark} removes the mark in the same write:
// WAITING, attempts 1, no mark, the NO_COMPUTE_MEMBER_FOR_TAG text (§5.5,
// §5.6, §5.8). A cnode attached later receives the retry and the task fires.
func TestSchedRun_NoComputeNodeThenFires(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-nocn"), uniq("sr-nocn-tag")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-nocn-wf", 100, 0, sProc("p", "SYNC", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.Attempts != 1 || r.FailureReason != "" || r.Marked || r.ClaimToken != "" {
		t.Fatalf("after the first attempt: %+v; want WAITING, attempts 1, no mark, no claim", r)
	}
	if !strings.HasPrefix(r.LastError, "NO_COMPUTE_MEMBER_FOR_TAG: ") {
		t.Errorf("lastError = %q; want the NO_COMPUTE_MEMBER_FOR_TAG text", r.LastError)
	}

	cn := h.AttachCnode(t, cnodeSpec{name: "late", tags: []string{tag}})
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the cnode received %d callouts; want 1", n)
	}
	if len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")) != 1 {
		t.Error("want exactly one SCHEDULED_TRANSITION_FIRE")
	}
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("the fired task is still stored")
	}
}

// TestSchedRun_Declined: a criterion answers false. The task is removed with
// TRANSITION_NOT_MATCH_CRITERION; the entity stays (§4). Not a failure.
func TestSchedRun_Declined(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-decl"), uniq("sr-decl-tag")
	h.AttachCnode(t, cnodeSpec{name: "crit", tags: []string{tag}, script: scriptAlways(answerMatches(false))})
	h.SetupModelWithWorkflow(t, model, schedDoc("sr-decl-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": map[string]any{"type": "function", "function": map[string]any{
				"name": "c", "config": map[string]any{"calculationNodesTags": tag, "attachEntity": true}}},
		}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitCallbackSMEventType(t, h, id, "TRANSITION_NOT_MATCH_CRITERION", "Open", scheduledFireTimeout)
	s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
	requireState(t, h, id, "Open")
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("a declined task recorded a failure")
	}
}

// TestSchedRun_SelfLoopReArmsNewLife: Open -[Tick]-> Open fires and re-arms
// the same id as a new life: a new arm token, attempts 0 (§5.2).
func TestSchedRun_SelfLoopReArmsNewLife(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model := uniq("sr-loop")
	h.SetupModelWithWorkflow(t, model, schedDoc("sr-loop-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Tick", "next": "Open", "manual": false, "schedule": map[string]any{"delayMs": 300},
		}}},
	}))
	id := createOpen(t, h, model, workflowSampleModel)
	t.Cleanup(func() { h.DoAuth(t, http.MethodDelete, "/api/entity/"+id, "", "").Body.Close() })

	first, ok := s.task(t, id, "Tick")
	if !ok {
		t.Fatal("no task after the create")
	}
	awaitDBCondition(t, scheduledFireTimeout, "two fires", func() bool {
		return len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")) >= 2
	})
	later := s.awaitTask(t, id, "Tick", scheduledFireTimeout, "a new life",
		func(r taskRow, ok bool) bool { return ok && r.ArmToken != first.ArmToken })
	if later.ID != first.ID || later.Attempts != 0 || later.LostOwners != 0 || later.Marked || later.PartialCommit {
		t.Errorf("re-armed task = %+v (first %+v); want the same id as a fresh life", later, first)
	}
}

// TestSchedRun_CriterionErrorRetried: a criterion callout fails; criteria are
// repeat-safe: WAITING, attempts 1, the cnode's own message (§5.8, MemberFailed).
func TestSchedRun_CriterionErrorRetried(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-crit"), uniq("sr-crit-tag")
	h.AttachCnode(t, cnodeSpec{name: "crit", tags: []string{tag}, script: scriptAlways(answerFail("crit boom"))})
	h.SetupModelWithWorkflow(t, model, schedDoc("sr-crit-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": map[string]any{"type": "function", "function": map[string]any{
				"name": "c", "config": map[string]any{"calculationNodesTags": tag, "attachEntity": true}}},
		}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.Attempts != 1 || r.FailureReason != "" || r.LastError != "crit boom" {
		t.Errorf("after the failed criterion: %+v; want WAITING, attempts 1, lastError \"crit boom\"", r)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_IdempotentFailureRetried: an idempotent processor fails: a safe
// failure, WAITING attempts 1, no mark, and the processor is sent again.
func TestSchedRun_IdempotentFailureRetried(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-idem"), uniq("sr-idem-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("proc boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-idem-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.Attempts != 1 || r.Marked || r.LastError != "proc boom" {
		t.Errorf("after the failed idempotent processor: %+v; want WAITING, attempts 1, no mark, \"proc boom\"", r)
	}
	awaitDBCondition(t, scheduledFireTimeout, "a second send", func() bool { return len(cn.Received()) >= 2 })
}

// TestSchedRun_LateAfterFailedAttemptsFails: timeoutMs 1500, an idempotent
// processor that always fails: FAILED EXPIRED_AFTER_FAILED_ATTEMPTS (§5.1
// step 4, §5.6), with its audit event; the entity stays.
func TestSchedRun_LateAfterFailedAttemptsFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-late"), uniq("sr-late-tag")
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("late boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-late-wf", 100, 1500, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := s.awaitTask(t, id, "Fire", 20*time.Second, "FAILED", func(r taskRow, ok bool) bool { return ok && r.Status == "FAILED" })
	if r.FailureReason != "EXPIRED_AFTER_FAILED_ATTEMPTS" || r.Attempts < 1 || r.ClaimToken != "" {
		t.Errorf("failed task = %+v; want EXPIRED_AFTER_FAILED_ATTEMPTS, attempts >= 1, no claim", r)
	}
	if data := failEvent(t, h, id); data["reason"] != "EXPIRED_AFTER_FAILED_ATTEMPTS" {
		t.Errorf("SCHEDULED_TRANSITION_FAIL data = %v", data)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_UnsafeFailureFails: an unsafe processor reached its cnode and
// failed: FAILED UNSAFE_WORK_NOT_COMPLETED with SCHEDULED_TRANSITION_FAIL
// {transition, sourceState, reason, attempts, lostOwners}; never sent again.
func TestSchedRun_UnsafeFailureFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-unsafe"), uniq("sr-unsafe-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("unsafe boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-unsafe-wf", 100, 0, sProc("p", "SYNC", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := awaitFailed(t, s, id, "Fire")
	if r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || r.LastError != "unsafe boom" || r.ClaimToken != "" {
		t.Errorf("failed task = %+v; want UNSAFE_WORK_NOT_COMPLETED, \"unsafe boom\", no claim", r)
	}
	data := failEvent(t, h, id)
	for k, want := range map[string]any{"transition": "Fire", "sourceState": "Open", "reason": "UNSAFE_WORK_NOT_COMPLETED",
		"attempts": float64(0), "lostOwners": float64(0)} {
		if data[k] != want {
			t.Errorf("SCHEDULED_TRANSITION_FAIL data[%q] = %v; want %v (data %v)", k, data[k], want, data)
		}
	}
	time.Sleep(3 * time.Second) // three retry delays
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the unsafe processor was sent %d times; want 1", n)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_LaterStepFailsAfterUnsafeHandOff: the unsafe first processor
// succeeds, the idempotent second fails: unsafe work reached a cnode and the
// run did not commit — FAILED UNSAFE_WORK_NOT_COMPLETED; neither is re-sent.
func TestSchedRun_LaterStepFailsAfterUnsafeHandOff(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sr-later"), uniq("sr-later-a"), uniq("sr-later-b")
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptAlways(answerFail("b boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-later-wf", 100, 0,
		sProc("pa", "SYNC", tagA, false), sProc("pb", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	if r := awaitFailed(t, s, id, "Fire"); r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", r.FailureReason)
	}
	time.Sleep(3 * time.Second)
	if na, nb := len(a.Received()), len(b.Received()); na != 1 || nb != 1 {
		t.Errorf("sent: a %d, b %d; want 1 and 1", na, nb)
	}
}

// TestSchedRun_FailureAfterUnsafeDispatchSameStep: the unsafe processor's
// dispatch succeeds but its answer carries a field the locked model does not
// declare, so the step fails after the dispatch (engine_processors.go:239).
// No NotHandedOff proof: FAILED UNSAFE_WORK_NOT_COMPLETED.
func TestSchedRun_FailureAfterUnsafeDispatchSameStep(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-same"), uniq("sr-same-tag")
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerData(map[string]any{
		"name": "Test Order", "amount": 100, "status": "draft", "undeclared": "x"}))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-same-wf", 100, 0, sProc("p", "SYNC", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	if r := awaitFailed(t, s, id, "Fire"); r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", r.FailureReason)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_UnsafeAsyncNewTxFailureStillCompletes: an unsafe ASYNC_NEW_TX
// processor fails; its failure does not fail the transition, the run commits,
// and the task is completed (§5.5 "If the run commits, the task is completed").
func TestSchedRun_UnsafeAsyncNewTxFailureStillCompletes(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-async"), uniq("sr-async-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("async boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-async-wf", 100, 0, sProc("p", "ASYNC_NEW_TX", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("the completed task is still stored")
	}
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("a committed run recorded a failure")
	}
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the ASYNC_NEW_TX processor was sent %d times; want 1", n)
	}
}

// TestSchedRun_FireTimeCancel: the two CANCEL endings a run decides itself
// (§4): the selected workflow no longer schedules the transition, and the
// stored entity carries no transaction id to guard the fire.
func TestSchedRun_FireTimeCancel(t *testing.T) {
	t.Run("NotScheduledInSelectedWorkflow", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model := uniq("sr-cancel")
		doc := func(scheduledIn string) string {
			wf := func(name, flavor string, scheduled bool) map[string]any {
				fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
				if scheduled {
					fire["schedule"] = map[string]any{"delayMs": 2000}
				}
				return map[string]any{"version": "1.5", "name": name, "initialState": "Open", "active": true,
					"criterion": map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": flavor},
					"states":    map[string]any{"Open": map[string]any{"transitions": []any{fire}}, "Done": map[string]any{}}}
			}
			return string(mustJSON(t, map[string]any{"importMode": "REPLACE", "workflows": []any{
				wf("sr-cancel-w1", "draft", scheduledIn == "w1"), wf("sr-cancel-w2", "final", scheduledIn == "w2")}}))
		}
		h.SetupModelWithWorkflow(t, model, doc("w1"))
		id := createOpen(t, h, model, workflowSampleModel)
		resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), doc("w2"), "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("re-import: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); !ok {
			t.Fatal("the re-import removed the task; it is scheduled in W2")
		}
		awaitCallbackSMEventType(t, h, id, "SCHEDULED_TRANSITION_CANCEL", "Open", scheduledFireTimeout)
		s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
		requireState(t, h, id, "Open")
	})

	t.Run("NoTransactionID", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model := uniq("sr-notx")
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-notx-wf", 1500, 0))
		id := createOpen(t, h, model, workflowSampleModel)
		// Legacy data: the API never writes an entity without a transaction id.
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE entities SET doc = doc #- '{_meta,transaction_id}' WHERE tenant_id = $1 AND entity_id = $2`,
			harnessTenant, id); err != nil {
			t.Fatalf("strip the transaction id: %v", err)
		}
		awaitCallbackSMEventType(t, h, id, "SCHEDULED_TRANSITION_CANCEL", "Open", scheduledFireTimeout)
		s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
		requireState(t, h, id, "Open")
	})
}

// TestSchedRun_LastErrorText: what a failed attempt records (§5.8). Every
// case uses an idempotent processor, so each failure is a safe one and the
// row is WAITING with attempts 1.
func TestSchedRun_LastErrorText(t *testing.T) {
	ticketOnly := regexp.MustCompile(`^internal error \[ticket: [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\]$`)
	codeText := regexp.MustCompile(`^[A-Z][A-Z0-9_]+: \S`)

	run := func(t *testing.T, configure func(*app.Config), script cnodeScript, attach bool, check func(t *testing.T, h *callbackHarness, s *schedDB, id string, r taskRow)) {
		t.Helper()
		h, s := newSchedulerHarness(t, configure)
		model, tag := uniq("sr-err"), uniq("sr-err-tag")
		if attach {
			h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: script})
		}
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-err-wf", 100, 0, sProc("p", "SYNC", tag, true)))
		id := createOpen(t, h, model, workflowSampleModel)
		check(t, h, s, id, firstAttempt(t, s, id, "Fire"))
	}

	t.Run("MemberFailedMessage", func(t *testing.T) {
		run(t, nil, scriptAlways(answerFail("the cnode's own words")), true, func(t *testing.T, _ *callbackHarness, _ *schedDB, _ string, r taskRow) {
			if r.LastError != "the cnode's own words" {
				t.Errorf("lastError = %q; want the cnode's message", r.LastError)
			}
		})
	})
	t.Run("CalloutTimeout", func(t *testing.T) {
		// sProc sets a 60s answer limit; this case lowers it to 300ms so the
		// silent cnode's one try times out quickly.
		h, s := newSchedulerHarness(t, nil)
		model, tag := uniq("sr-to"), uniq("sr-to-tag")
		h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(neverAnswer())})
		p := sProc("p", "SYNC", tag, true)
		p["config"].(map[string]any)["responseTimeoutMs"] = 300
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-to-wf", 100, 0, p))
		id := createOpen(t, h, model, workflowSampleModel)
		r := firstAttempt(t, s, id, "Fire")
		if !strings.HasPrefix(r.LastError, "DISPATCH_TIMEOUT: ") {
			t.Errorf("lastError = %q; want DISPATCH_TIMEOUT: <detail>", r.LastError)
		}
	})
	t.Run("OperationalAppError", func(t *testing.T) {
		run(t, nil, scriptAlways(answerData(map[string]any{"name": "Test Order", "amount": 100, "status": "draft", "undeclared": "x"})), true,
			func(t *testing.T, _ *callbackHarness, _ *schedDB, _ string, r taskRow) {
				if !codeText.MatchString(r.LastError) || strings.HasPrefix(r.LastError, "internal error") {
					t.Errorf("lastError = %q; want the AppError's CODE: detail", r.LastError)
				}
			})
	})
	t.Run("NoComputeMember", func(t *testing.T) {
		run(t, nil, nil, false, func(t *testing.T, _ *callbackHarness, _ *schedDB, _ string, r taskRow) {
			if !strings.HasPrefix(r.LastError, "NO_COMPUTE_MEMBER_FOR_TAG: ") {
				t.Errorf("lastError = %q; want NO_COMPUTE_MEMBER_FOR_TAG: <detail>", r.LastError)
			}
		})
	})
	t.Run("NonSentinelStoreErrorIsTicketOnly", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model, tag := uniq("sr-tk"), uniq("sr-tk-tag")
		gotWork, release := make(chan struct{}, 1), make(chan struct{})
		rel := closeOnce(release)
		t.Cleanup(rel)
		var calls atomic.Int32
		h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
			if calls.Add(1) > 1 {
				return neverAnswer()
			}
			gotWork <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return neverAnswer()
			}
			return answerOK()
		}})
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-tk-wf", 100, 0, sProc("p", "SYNC", tag, true)))
		id := createOpen(t, h, model, workflowSampleModel)

		<-gotWork
		// The run's transaction is the only one open in this database while
		// its processor is held.
		var n int
		if err := s.pool.QueryRow(context.Background(), `
			SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
			 WHERE datname = current_database() AND state = 'idle in transaction' AND pid <> pg_backend_pid()`,
		).Scan(&n); err != nil || n != 1 {
			t.Fatalf("terminated %d backends (err %v); want exactly the run's", n, err)
		}
		rel()
		r := firstAttempt(t, s, id, "Fire")
		if !ticketOnly.MatchString(r.LastError) {
			t.Errorf("lastError = %q; want exactly \"internal error [ticket: <uuid>]\"", r.LastError)
		}
	})
	t.Run("Conflict", func(t *testing.T) {
		// A joined callback of the run updates a second entity F; the test
		// updated F after the run's snapshot, so the run's write of F loses
		// first-committer-wins. The run's task is untouched, so the re-read
		// classifies an ordinary failure (§5.2) with the fixed CONFLICT text.
		h, s := newSchedulerHarness(t, nil)
		fModel, model, tag := uniq("sr-cf-f"), uniq("sr-cf"), uniq("sr-cf-tag")
		h.SetupModelWithWorkflow(t, fModel, schedDoc("sr-cf-f-wf", map[string]any{"Open": map[string]any{}}))
		fID := createOpen(t, h, fModel, workflowSampleModel)

		gotWork, release := make(chan struct{}, 1), make(chan struct{})
		rel := closeOnce(release)
		t.Cleanup(rel)
		var calls atomic.Int32
		h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
			if calls.Add(1) > 1 {
				return answerOK()
			}
			gotWork <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return neverAnswer()
			}
			_, _ = rc.UpdateEntity(fID, `{"name":"Test Order","amount":2,"status":"draft"}`)
			return answerOK()
		}})
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-cf-wf", 100, 0, sProc("p", "SYNC", tag, true)))
		id := createOpen(t, h, model, workflowSampleModel)

		<-gotWork
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+fID, `{"name":"Test Order","amount":1,"status":"draft"}`, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("client update of F: %d %s", resp.StatusCode, body)
		}
		rel()
		r := firstAttempt(t, s, id, "Fire")
		if r.LastError != "CONFLICT: a concurrent write changed the entity or its task" {
			t.Errorf("lastError = %q; want the fixed CONFLICT text", r.LastError)
		}
	})
}

// TestSchedRun_FailedTaskUnderEntityWrites: the two ways an entity write ends
// a FAILED task (§4), and the removal of a task its workflow no longer
// schedules at the next write (§7).
func TestSchedRun_FailedTaskUnderEntityWrites(t *testing.T) {
	failedStack := func(t *testing.T) (*callbackHarness, *schedDB, *scriptedCnode, string, taskRow) {
		t.Helper()
		h, s := newSchedulerHarness(t, nil)
		model, tag := uniq("sr-fw"), uniq("sr-fw-tag")
		cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("fw boom"))})
		h.SetupModelWithWorkflow(t, model, schedDoc("sr-fw-wf", map[string]any{
			"Open": map[string]any{"transitions": []any{
				map[string]any{"name": "Fire", "next": "Done", "manual": false,
					"schedule":   map[string]any{"delayMs": 1500},
					"processors": []any{sProc("p", "SYNC", tag, false)}},
				map[string]any{"name": "Leave", "next": "Left", "manual": true},
			}},
			"Done": map[string]any{},
			"Left": map[string]any{},
		}))
		id := createOpen(t, h, model, workflowSampleModel)
		r := awaitFailed(t, s, id, "Fire")
		if r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
			t.Fatalf("failureReason = %q", r.FailureReason)
		}
		return h, s, cn, id, r
	}

	t.Run("ReArmedByUpdateInTheState", func(t *testing.T) {
		h, s, cn, id, failed := failedStack(t)
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":5,"status":"draft"}`, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("update: %d %s", resp.StatusCode, body)
		}
		r, ok := s.task(t, id, "Fire")
		if !ok || r.Status != "WAITING" || r.ArmToken == failed.ArmToken || r.Attempts != 0 || r.LostOwners != 0 ||
			r.FailureReason != "" || r.LastError != "" || r.Marked || r.PartialCommit || r.ClaimToken != "" {
			t.Fatalf("after the update: %+v present=%t; want a new life with no mark", r, ok)
		}
		// The new life runs and sends its processor: no old-life mark stops it.
		awaitDBCondition(t, scheduledFireTimeout, "the new life's send", func() bool { return len(cn.Received()) >= 2 })
	})

	t.Run("CancelledWhenEntityLeaves", func(t *testing.T) {
		h, s, _, id, _ := failedStack(t)
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/Leave", workflowSampleModel, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("Leave: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); ok {
			t.Error("the FAILED task outlived the state exit")
		}
		if !hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_CANCEL", "Open") {
			t.Error("no SCHEDULED_TRANSITION_CANCEL for the FAILED task")
		}
		requireState(t, h, id, "Left")
	})

	t.Run("NoLongerScheduledRemovedAtNextWrite", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model := uniq("sr-nw")
		doc := func(scheduledIn string) string {
			wf := func(name, flavor string, scheduled bool) map[string]any {
				fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
				if scheduled {
					fire["schedule"] = map[string]any{"delayMs": 3600000}
				}
				return map[string]any{"version": "1.5", "name": name, "initialState": "Open", "active": true,
					"criterion": map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": flavor},
					"states":    map[string]any{"Open": map[string]any{"transitions": []any{fire}}, "Done": map[string]any{}}}
			}
			return string(mustJSON(t, map[string]any{"importMode": "REPLACE", "workflows": []any{
				wf("sr-nw-w1", "draft", scheduledIn == "w1"), wf("sr-nw-w2", "final", scheduledIn == "w2")}}))
		}
		h.SetupModelWithWorkflow(t, model, doc("w1"))
		id := createOpen(t, h, model, workflowSampleModel)
		resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), doc("w2"), "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("re-import: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); !ok {
			t.Fatal("the re-import removed the task; it is scheduled in W2")
		}
		resp = h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":6,"status":"draft"}`, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("update: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); ok {
			t.Error("the write kept a task its workflow no longer schedules")
		}
		if !hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_CANCEL", "Open") {
			t.Error("no SCHEDULED_TRANSITION_CANCEL for the removed task")
		}
	})
}
```

`mustJSON` (`internal/e2e/oauth_keys_test.go:60`) and `closeOnce`
(`internal/e2e/callout_fencing_test.go:36`, returns the closing func) are the
package's existing helpers. Add `"sync/atomic"` to the imports.


- [ ] **Step 2: Run them**

Run: `go test ./internal/e2e/ -run 'TestSchedRun_'`
Expected (merge-base plus T-1): FAIL to compile (`cfg.Scheduler.HeartbeatInterval`,
`scheduled_task_marks`). After the streams: PASS.

**Teeth.** In R, make `recordedError` return `err.Error()` for its last row —
`NonSentinelStoreErrorIsTicketOnly` fails with the driver text. In K,
set `HandedOff` on the failed-`Send` branch (`dispatch.go:189-205`) —
`NoComputeNodeThenFires` ends FAILED.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/scheduled_run_endings_test.go
git commit -m "test(e2e): endings of a scheduled run and the recorded error

Safe failures, FAILED reasons with their audit event, fire-time cancels,
the lastError allow-list, and FAILED tasks under entity writes.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-5: e2e — segmented runs, partial commits, joined callbacks, cancellation

**Spec:** §5.2 (re-read, stamp, `RemoveLife`), §5.3 (checkpoints), §5.4, §5.5
"The callback anti-pattern", §5.6; §13 "Endings" rows on
`COMMIT_BEFORE_DISPATCH`, `PartialCommit`, cancellation after TX_pre and the
four joined-callback rows, column E.

**Depends on:** S, BP, E (stamp, `PartialCommit`, checkpoints, re-read), R (bookkeeping,
shutdown step 3), T-1, T-4 (helpers).

**Files:**
- Create: `internal/e2e/scheduled_run_segments_test.go`

**Interfaces:**
- Consumes: T-1 and T-4 helpers; `(rc *reqCtx) UpdateEntity`, `rc.entityID`,
  `(h) callback` (`callback_harness_test.go:82-120, 393`).
- Produces: `holdScript(gotWork chan<- struct{}, release <-chan struct{}) cnodeScript`.

- [ ] **Step 1: Write the tests**

`internal/e2e/scheduled_run_segments_test.go`:

```go
package e2e_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// scheduled_run_segments_test.go — runs that commit in segments
// (COMMIT_BEFORE_DISPATCH), the PartialCommit rule, a run cut by the
// scheduler, and the callback anti-pattern (a joined callback that writes the
// entity being fired), through the full stack on PostgreSQL.

// holdScript signals gotWork on the first callout and holds it until release
// is closed; later callouts are answered at once.
func holdScript(gotWork chan<- struct{}, release <-chan struct{}) cnodeScript {
	var calls atomic.Int32
	return func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		if calls.Add(1) > 1 {
			return answerOK()
		}
		gotWork <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}
}

// TestSchedRun_CBDOnFiredTransitionRetriedFromTXPre: a COMMIT_BEFORE_DISPATCH
// processor of the fired transition itself commits TX_pre with the entity
// still in the source state — no PartialCommit (§5.4). A later idempotent
// processor fails once: a safe failure, retried from that committed state,
// and the retry fires.
func TestSchedRun_CBDOnFiredTransitionRetriedFromTXPre(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sg-txpre"), uniq("sg-txpre-a"), uniq("sg-txpre-b")
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptSequence(answerFail("once"), answerOK())})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-txpre-wf", 100, 0,
		sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.PartialCommit || r.FailureReason != "" {
		t.Fatalf("after the first attempt: %+v; want WAITING, no PartialCommit (the fired transition's own segment)", r)
	}
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if na, nb := len(a.Received()), len(b.Received()); na != 2 || nb != 2 {
		t.Errorf("sent: p1 %d, p2 %d; want 2 and 2 (the retry started again from TX_pre)", na, nb)
	}
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("an all-idempotent run with a fired-transition segment recorded a failure")
	}
}

// TestSchedRun_CBDInCascadeStepThenFailureFails: the fired transition lands in
// Mid; Mid's automated step commits the entity in Mid (COMMIT_BEFORE_DISPATCH),
// then its next processor fails. The run stopped after committing the entity
// into another state: FAILED STOPPED_AFTER_PARTIAL_COMMIT, entity in Mid.
func TestSchedRun_CBDInCascadeStepThenFailureFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sg-step"), uniq("sg-step-a"), uniq("sg-step-b")
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptAlways(answerFail("step boom"))})
	h.SetupModelWithWorkflow(t, model, schedDoc("sg-step-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Mid", "manual": false, "schedule": map[string]any{"delayMs": 100}}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{
			"name": "Step", "next": "Done", "manual": false,
			"processors": []any{sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)}}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	r := awaitFailed(t, s, id, "Fire")
	if r.FailureReason != "STOPPED_AFTER_PARTIAL_COMMIT" || !r.PartialCommit {
		t.Errorf("failed task = %+v; want STOPPED_AFTER_PARTIAL_COMMIT with PartialCommit", r)
	}
	requireState(t, h, id, "Mid")
	if data := failEvent(t, h, id); data["reason"] != "STOPPED_AFTER_PARTIAL_COMMIT" {
		t.Errorf("SCHEDULED_TRANSITION_FAIL data = %v", data)
	}
	time.Sleep(3 * time.Second)
	if n := len(b.Received()); n != 1 {
		t.Errorf("p2 sent %d times; a FAILED task is never run again", n)
	}
}

// TestSchedRun_CascadeLoopBackWithCBDSetsPartialCommit: Fire lands in Mid,
// Mid's step writes looped=true and returns to Open, and Open's Onward step
// (criterion looped == true) commits the entity in Open — the source state —
// before its next processor fails. That segment is a cascade step, not the
// fired transition, so it sets PartialCommit (§5.4): FAILED
// STOPPED_AFTER_PARTIAL_COMMIT, never retried as if nothing had committed.
func TestSchedRun_CascadeLoopBackWithCBDSetsPartialCommit(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB, tagC := uniq("sg-loop"), uniq("sg-loop-a"), uniq("sg-loop-b"), uniq("sg-loop-c")
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptAlways(answerFail("onward boom"))})
	h.AttachCnode(t, cnodeSpec{name: "c", tags: []string{tagC}, script: scriptAlways(answerData(map[string]any{
		"name": "Test Order", "amount": 100, "status": "draft", "looped": true}))})
	h.setupModelSampleWithWorkflow(t, model, workflowSampleWith(`"looped":false`), schedDoc("sg-loop-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{
			map[string]any{"name": "Fire", "next": "Mid", "manual": false, "schedule": map[string]any{"delayMs": 100}},
			map[string]any{"name": "Onward", "next": "Done", "manual": false,
				"criterion":  map[string]any{"type": "simple", "jsonPath": "$.looped", "operatorType": "EQUALS", "value": true},
				"processors": []any{sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)}},
		}},
		"Mid": map[string]any{"transitions": []any{map[string]any{
			"name": "Back", "next": "Open", "manual": false,
			"processors": []any{sProc("p0", "SYNC", tagC, true)}}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleWith(`"looped":false`))
	requireState(t, h, id, "Open")

	r := awaitFailed(t, s, id, "Fire")
	if r.FailureReason != "STOPPED_AFTER_PARTIAL_COMMIT" || !r.PartialCommit {
		t.Errorf("failed task = %+v; want STOPPED_AFTER_PARTIAL_COMMIT with PartialCommit", r)
	}
	requireState(t, h, id, "Open")
	if looped, _ := h.GetEntityData(t, id)["looped"].(bool); !looped {
		t.Error("the committed segment is not visible: looped is still false")
	}
	time.Sleep(3 * time.Second)
	if n := len(b.Received()); n != 1 {
		t.Errorf("p2 sent %d times; want 1", n)
	}
}

// TestSchedRun_CancelAfterTXPreStopsAtNextStep: the run's TX_pre is committed
// and its COMMIT_BEFORE_DISPATCH processor (idempotent) is in flight when the
// scheduler shuts down. After the drain, step 3 cuts it; the next processor is
// never dispatched, nothing unsafe reached a cnode, so the attempt is not
// counted: WAITING, attempts 0, the fixed CANCELLED text (§5.3, §5.6, §5.8).
func TestSchedRun_CancelAfterTXPreStopsAtNextStep(t *testing.T) {
	h, s := newSchedulerHarness(t, nil) // ShutdownDrain 1s
	model, tagA, tagB := uniq("sg-cut"), uniq("sg-cut-a"), uniq("sg-cut-b")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: holdScript(gotWork, release)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-cut-wf", 100, 0,
		sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	<-gotWork
	done := make(chan struct{})
	go func() { h.app.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Shutdown did not return within the drain, the cut and the bookkeeping")
	}

	r, ok := s.task(t, id, "Fire")
	if !ok || r.Status != "WAITING" || r.Attempts != 0 || r.ClaimToken != "" || r.PartialCommit {
		t.Fatalf("after the cut: %+v present=%t; want WAITING, attempts 0, no claim, no PartialCommit", r, ok)
	}
	if r.LastError != "CANCELLED: the run was stopped by the scheduler" {
		t.Errorf("lastError = %q; want the fixed CANCELLED text", r.LastError)
	}
	if n := len(b.Received()); n != 0 {
		t.Errorf("p2 was dispatched %d times after the cut; want 0", n)
	}
	requireState(t, h, id, "Open")
}

// joinedWriteScript is the fired transition's first processor: an idempotent
// SYNC processor that, through a joined callback, writes the entity being
// fired (the pattern help/workflows.md:185-192 advises against) and answers.
func joinedWriteScript(result chan<- int) cnodeScript {
	var calls atomic.Int32
	return func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.UpdateEntity(rc.entityID, `{"name":"Test Order","amount":7,"status":"draft"}`)
		if calls.Add(1) == 1 {
			status := -1
			if err == nil {
				status = res.StatusCode
			}
			result <- status
		}
		return answerOK()
	}
}

// TestSchedRun_JoinedCallbackWritesFiredEntity_OrdinaryOutcome: with no unsafe
// processor after it, the anti-pattern ends exactly as the same processor on
// an ordinary automated transition does: both commit, or neither does.
func TestSchedRun_JoinedCallbackWritesFiredEntity_OrdinaryOutcome(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	tag := uniq("sg-jw")
	ordinaryCB, scheduledCB := make(chan int, 1), make(chan int, 1)
	h.AttachCnode(t, cnodeSpec{name: "ordinary", tags: []string{tag + "-o"}, script: joinedWriteScript(ordinaryCB)})
	h.AttachCnode(t, cnodeSpec{name: "scheduled", tags: []string{tag + "-s"}, script: joinedWriteScript(scheduledCB)})

	// Ordinary: Open -[Go, automated]-> Done at create.
	ordModel := uniq("sg-jw-ord")
	h.SetupModelWithWorkflow(t, ordModel, schedDoc("sg-jw-ord-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
			"processors": []any{sProc("p1", "SYNC", tag+"-o", true)}}}},
		"Done": map[string]any{},
	}))
	_, ordStatus, _ := h.CreateEntity(t, ordModel, 1, workflowSampleModel)
	<-ordinaryCB

	// Scheduled: the same processor on the fired transition.
	model := uniq("sg-jw-sched")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-jw-wf", 100, 0, sProc("p1", "SYNC", tag+"-s", true)))
	id := createOpen(t, h, model, workflowSampleModel)
	<-scheduledCB

	fired := func() bool { st, _ := h.GetEntityState(t, id); return st == "Done" }
	attempted := func() bool { r, ok := s.task(t, id, "Fire"); return ok && r.Attempts >= 1 }
	awaitDBCondition(t, scheduledFireTimeout, "an outcome", func() bool { return fired() || attempted() })

	if ordStatus == http.StatusOK {
		if !fired() {
			t.Fatalf("the ordinary transition committed (200) and the scheduled run did not fire: %+v", mustTask(t, s, id))
		}
		if _, ok := s.task(t, id, "Fire"); ok {
			t.Error("the fired task is still stored")
		}
	} else {
		r := mustTask(t, s, id)
		if fired() || r.Status != "WAITING" || r.Attempts < 1 {
			t.Fatalf("the ordinary transition failed (%d) and the scheduled run did not record a safe failure: %+v", ordStatus, r)
		}
	}
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("an idempotent run recorded a failure")
	}
}

// mustTask reads the task and fails the test if there is none.
func mustTask(t *testing.T, s *schedDB, id string) taskRow {
	t.Helper()
	r, ok := s.task(t, id, "Fire")
	if !ok {
		t.Fatalf("no task for %s", id)
	}
	return r
}

// TestSchedRun_JoinedCallbackThenUnsafe_TaskBusySafeFailure: the joined
// callback's write holds the task row in the run's transaction, so the
// MarkUnsafe before the unsafe processor gets ErrTaskBusy: no dispatch, a
// counted safe failure, no hang (§5.5). With timeoutMs set, the attempts end
// FAILED EXPIRED_AFTER_FAILED_ATTEMPTS — visible, never stuck.
func TestSchedRun_JoinedCallbackThenUnsafe_TaskBusySafeFailure(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sg-busy"), uniq("sg-busy-a"), uniq("sg-busy-b")
	cb := make(chan int, 1)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: joinedWriteScript(cb)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-busy-wf", 100, 4000,
		sProc("p1", "SYNC", tagA, true), sProc("p2", "SYNC", tagB, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	start := time.Now()
	if st := <-cb; st != http.StatusOK {
		t.Fatalf("the joined write answered %d; the scenario needs it to succeed", st)
	}
	r := firstAttempt(t, s, id, "Fire")
	if since := time.Since(start); since > 10*time.Second {
		t.Errorf("the first attempt took %s to be recorded; the run must not hang on its own row lock", since)
	}
	if r.Status != "WAITING" || r.Marked {
		t.Errorf("after ErrTaskBusy: %+v; want WAITING and no mark", r)
	}
	f := s.awaitTask(t, id, "Fire", 20*time.Second, "FAILED", func(r taskRow, ok bool) bool { return ok && r.Status == "FAILED" })
	if f.FailureReason != "EXPIRED_AFTER_FAILED_ATTEMPTS" || f.Attempts < 2 {
		t.Errorf("ending = %+v; want EXPIRED_AFTER_FAILED_ATTEMPTS after counted attempts", f)
	}
	if n := len(b.Received()); n != 0 {
		t.Errorf("the unsafe processor was dispatched %d times; want 0", n)
	}
}

// TestSchedRun_JoinedCallbackInSegmentedRun_StampRefused: the joined
// callback's write re-armed the task inside the run's transaction; the next
// COMMIT_BEFORE_DISPATCH segment's stamp names the old life and is refused, so
// the segment does not commit and its processor is not dispatched. The
// rollback undoes the re-arm; the non-joining re-read finds the life and claim
// unchanged and classifies an ordinary failure (§5.2, §5.5).
func TestSchedRun_JoinedCallbackInSegmentedRun_StampRefused(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sg-stamp"), uniq("sg-stamp-a"), uniq("sg-stamp-b")
	cb := make(chan int, 1)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: joinedWriteScript(cb)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-stamp-wf", 100, 4000,
		sProc("p1", "SYNC", tagA, true), sProc("p2", "COMMIT_BEFORE_DISPATCH", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	start := time.Now()
	if st := <-cb; st != http.StatusOK {
		t.Fatalf("the joined write answered %d; the scenario needs it to succeed", st)
	}
	r := firstAttempt(t, s, id, "Fire")
	if since := time.Since(start); since > 10*time.Second {
		t.Errorf("the first attempt took %s to be recorded", since)
	}
	if r.Status != "WAITING" || r.PartialCommit {
		t.Errorf("after the refused stamp: %+v; want WAITING and nothing committed", r)
	}
	if n := len(b.Received()); n != 0 {
		t.Errorf("the segment's processor was dispatched %d times; a refused stamp stops the segment", n)
	}
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 100 {
		t.Errorf("amount = %v; the callback's write rolled back with the segment, want 100", amount)
	}
}

// TestSchedRun_JoinedCallbackDeletesFiredEntity_RunCommits: the callback
// deletes the entity being fired; the run commits (§13): the entity stays
// deleted, its tasks are gone, and the processor is not sent again.
func TestSchedRun_JoinedCallbackDeletesFiredEntity_RunCommits(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sg-del"), uniq("sg-del-tag")
	deleted := make(chan int, 1)
	var calls atomic.Int32
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.h.callback(http.MethodDelete, "/api/entity/"+rc.entityID, "", rc.token)
		if calls.Add(1) == 1 {
			status := -1
			if err == nil {
				status = res.StatusCode
			}
			deleted <- status
		}
		return answerOK()
	}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-del-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	if st := <-deleted; st != http.StatusOK {
		t.Fatalf("the joined delete answered %d; want 200", st)
	}
	awaitDBCondition(t, scheduledFireTimeout, "the entity gone", func() bool {
		_, status := h.GetEntityState(t, id)
		return status == http.StatusNotFound
	})
	time.Sleep(3 * time.Second) // a rolled-back run would bring it back and retry
	if _, status := h.GetEntityState(t, id); status != http.StatusNotFound {
		t.Errorf("GET after the run = %d; want 404", status)
	}
	if n := s.count(t, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1", id); n != 0 {
		t.Errorf("%d task rows remain for the deleted entity", n)
	}
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the processor was sent %d times; want 1", n)
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test ./internal/e2e/ -run 'TestSchedRun_(CBD|Cascade|Cancel|Joined)'`
Expected (merge-base plus T-1): FAIL to compile. After the streams: PASS.

**Teeth.** In E's stamp, pass `partial = false` for every segment —
`CBDInCascadeStepThenFailureFails` and `CascadeLoopBackWithCBDSetsPartialCommit`
end WAITING and retry. In E's `MarkUnsafe` call, treat `ErrTaskBusy` as
accepted — `JoinedCallbackThenUnsafe_TaskBusySafeFailure` sees p2 dispatched.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/scheduled_run_segments_test.go
git commit -m "test(e2e): segmented scheduled runs, partial commits, joined callbacks

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-6: multi-node — a database outage longer than `STALE_AFTER`

**Spec:** §6.1 "Lost-owner claims" (only after a full stale period of clean
heartbeats), §6.2 "Heartbeat" (an upsert recreates a swept record), §6.3
(self-cancel at `W`), §5.6 (bookkeeping retried through an outage), §5.8
(`CANCELLED` text); §13 "Ownership, fencing and liveness" rows "a liveness
record swept during a long outage is recreated by the next heartbeat" (M) and
"database outage longer than `STALE_AFTER` → no lost-owner claims before a
full stale period of healthy heartbeats" (M). README Review Focus 4.

**Depends on:** S, BP (owners table, scheduler pool, heartbeat connection), E,
R (heartbeat, watchdog, claim gating, bookkeeping retry), T-1.

**Files:**
- Create: `e2e/parity/postgres/scheduler_mn_helpers_test.go`
- Create: `e2e/parity/postgres/scheduler_mn_outage_test.go`

**Interfaces:**
- Consumes (T-1): `MustSetupMultiNodeWithOpts`, `fixtureutil.LaunchOpts.NodeEnv`,
  `(*pgMultiNode) KillNode, SignalNode, AwaitNodeExit, PauseDatabase,
  UnpauseDatabase, Incarnation, NodeLogs, ConnString, StartComputeClient`,
  `fixtureutil.TunedStaleAfter`, `TunedHeartbeatInterval`, `TunedRetryDelay`,
  `fixtureutil.IncarnationFromLog`, `parity.ComputeBehaviourHold`.
- Produces (package `postgres`, test files): `schedMN`, `newSchedMN`,
  `hostOnly`, `mnTask`, `(s) task`, `(s) awaitTask`, `(s) ownerNode`,
  `(s) client`, `mnWorkflow`, `mnProc`, `mnSetup`, `mnReceived`,
  `mnCountEvents`, `mnShort`.

**The host pnode.** Every scenario here that counts a processor attaches its
compute clients to a pnode whose scheduler is off (`hostOnly`). That pnode
never owns a task, so killing, stopping or signalling owners never
disconnects a client, and a client's `/record` stays readable.

- [ ] **Step 1: The helpers**

`e2e/parity/postgres/scheduler_mn_helpers_test.go`:

```go
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// scheduler_mn_helpers_test.go — shared by the scheduler multi-node tests.
// Each test boots a cluster of its own (they kill, stop, signal and pause
// pnodes, so none belongs in the shared multinode registry) and reads the
// task rows straight from PostgreSQL: which pnode holds a claim is invisible
// at the API (spec §8 never returns node ids or tokens).

// schedMN is one scenario's cluster and a reader on its database.
type schedMN struct {
	pg     *pgMultiNode
	db     *pgxpool.Pool
	bootAt time.Time
}

// newSchedMN boots n pnodes with opts and extraEnv on top of the tuned
// cluster env, and opens a reader on the shared database.
func newSchedMN(t *testing.T, n int, opts fixtureutil.LaunchOpts, extraEnv ...string) *schedMN {
	t.Helper()
	fix, cleanup := MustSetupMultiNodeWithOpts(t, n, extraEnv, opts)
	t.Cleanup(cleanup)
	pg, ok := fix.(*pgMultiNode)
	if !ok {
		t.Fatalf("fixture is %T; want *pgMultiNode", fix)
	}
	db, err := pgxpool.New(context.Background(), pg.ConnString()) // never logged: it carries credentials
	if err != nil {
		t.Fatalf("reader pool: %v", err)
	}
	t.Cleanup(db.Close)
	return &schedMN{pg: pg, db: db, bootAt: time.Now()}
}

// hostOnly turns the scheduler off on pnode host.
func hostOnly(host int) func(int) []string {
	return func(i int) []string {
		if i == host {
			return []string{"CYODA_SCHEDULER_ENABLED=false"}
		}
		return nil
	}
}

// mnShort is a short random suffix for tags and model names.
func mnShort() string { return uuid.NewString()[:6] }

// mnTask is a scheduled_tasks row with whether a mark exists for its life.
type mnTask struct {
	Status, LastError, FailureReason, ClaimToken, ClaimOwner string
	Attempts, LostOwners                                     int
	PartialCommit, Marked                                    bool
}

// task reads the entity's task for transition.
func (s *schedMN) task(t *testing.T, entityID uuid.UUID, transition string) (mnTask, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r mnTask
	err := s.db.QueryRow(ctx, `
		SELECT st.status, COALESCE(st.last_error, ''), COALESCE(st.failure_reason, ''),
		       COALESCE(st.claim_token::text, ''), COALESCE(st.claim_owner::text, ''),
		       st.attempts, st.lost_owners, st.partial_commit,
		       EXISTS (SELECT 1 FROM scheduled_task_marks m WHERE m.task_id = st.id AND m.arm_token = st.arm_token)
		  FROM scheduled_tasks st WHERE st.entity_id = $1 AND st.transition = $2`,
		entityID.String(), transition,
	).Scan(&r.Status, &r.LastError, &r.FailureReason, &r.ClaimToken, &r.ClaimOwner,
		&r.Attempts, &r.LostOwners, &r.PartialCommit, &r.Marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return mnTask{}, false
	}
	if err != nil {
		t.Fatalf("read task %s/%s: %v", entityID, transition, err)
	}
	return r, true
}

// awaitTask polls every 100ms until cond holds.
func (s *schedMN) awaitTask(t *testing.T, id uuid.UUID, transition string, within time.Duration, what string, cond func(mnTask, bool) bool) mnTask {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r, ok := s.task(t, id, transition)
		if cond(r, ok) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s/%s: %s not seen within %s; last: present=%t %+v", id, transition, what, within, ok, r)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ownerNode maps a claim_owner to the pnode whose scheduler announced that
// incarnation, or -1. A pnode with its scheduler off announces none.
func (s *schedMN) ownerNode(t *testing.T, owner string) int {
	t.Helper()
	for i := 0; i < s.pg.NodeCount(); i++ {
		id, err := fixtureutil.IncarnationFromLog(s.pg.NodeLogs(i))
		if err == nil && id.String() == owner {
			return i
		}
	}
	return -1
}

// client is a parity client for tenant on pnode i.
func (s *schedMN) client(i int, tenant parity.Tenant) *client.Client {
	return client.NewClient(s.pg.BaseURLs()[i], tenant.Token)
}

// startClient starts a compute client for tenant on pnode node.
func (s *schedMN) startClient(t *testing.T, node int, tenant parity.Tenant, tag, behaviour string) parity.ComputeClient {
	t.Helper()
	cc := s.pg.StartComputeClient(t, node, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
	t.Cleanup(cc.Stop)
	return cc
}

// mnWorkflow wraps states in a schema 1.5 import document, initial state Open.
func mnWorkflow(wfName string, states map[string]any) string {
	b, _ := json.Marshal(map[string]any{"importMode": "REPLACE", "workflows": []any{map[string]any{
		"version": "1.5", "name": wfName, "initialState": "Open", "active": true, "states": states}}})
	return string(b)
}

// mnProc is one catalog processor routed to tag with one try and the given
// answer limit.
func mnProc(name, mode, tag string, idempotent bool, answerLimitMs int) map[string]any {
	return map[string]any{"type": "calculator", "name": name, "executionMode": mode,
		"config": map[string]any{"attachEntity": true, "calculationNodesTags": tag, "idempotent": idempotent,
			"retryPolicy": "NONE", "responseTimeoutMs": answerLimitMs}}
}

// mnFire is Open -[Fire, scheduled]-> Done carrying procs.
func mnFire(delayMs, timeoutMs int64, procs ...map[string]any) map[string]any {
	sched := map[string]any{"delayMs": delayMs}
	if timeoutMs > 0 {
		sched["timeoutMs"] = timeoutMs
	}
	list := make([]any, 0, len(procs))
	for _, p := range procs {
		list = append(list, p)
	}
	return map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": sched, "processors": list}}},
		"Done": map[string]any{},
	}
}

// mnSample declares every field the scenarios' entities carry.
const mnSample = `{"k":1,"amount":1}`

// mnSetup imports, locks and gives the model its workflow, then creates one
// entity.
func mnSetup(t *testing.T, c *client.Client, model, wf string) uuid.UUID {
	t.Helper()
	if err := c.ImportModel(t, model, 1, mnSample); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, model, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, model, 1, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	id, err := c.CreateEntity(t, model, 1, mnSample)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	return id
}

// mnReceived counts the requests cc received for id.
func mnReceived(t *testing.T, cc parity.ComputeClient, id uuid.UUID) int {
	t.Helper()
	n := 0
	for _, r := range cc.Received(t) {
		if r.EntityID == id.String() {
			n++
		}
	}
	return n
}

// mnCountEvents counts the entity's StateMachine events of eventType.
func mnCountEvents(t *testing.T, c *client.Client, id uuid.UUID, eventType string) int {
	t.Helper()
	resp, err := c.GetAuditEvents(t, id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	n := 0
	for i := range resp.Items {
		if resp.Items[i].AuditEventType != "StateMachine" {
			continue
		}
		sm, err := resp.Items[i].AsStateMachine()
		if err != nil {
			t.Fatalf("AsStateMachine: %v", err)
		}
		if sm.EventType == eventType {
			n++
		}
	}
	return n
}

// mnAwait polls cond every 100ms.
func mnAwait(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not seen within %s", what, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mnLongAnswer is the answer limit of a processor a scenario keeps in flight
// across a stale period; the cluster allows it through
// CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS (mnLongAnswerEnv).
const (
	mnLongAnswer    = 240000
	mnLongAnswerEnv = "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS=300000"
)
```

- [ ] **Step 2: The outage scenario**

`e2e/parity/postgres/scheduler_mn_outage_test.go`:

```go
package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_DatabaseOutageLongerThanStaleAfter: pnodes 0 and 1 claim,
// pnode 2 hosts the compute client. Task T1 runs on its owner O, which is
// killed; task T2 then runs on the survivor S. The database is paused for
// longer than STALE_AFTER.
//
//   - During the outage S's heartbeats fail; its watchdog cancels T2's run,
//     and T2's bookkeeping is retried until the database is back: T2 records
//     a counted attempt with the fixed CANCELLED text (§5.6, §5.8).
//   - S's liveness record, removed as a sweep would remove it, is recreated by
//     the next heartbeat (§6.2).
//   - O is stale when the database returns, but S makes no lost-owner claim
//     until its own heartbeats have run clean for a whole STALE_AFTER (§6.1):
//     T1 keeps O's claim until then, and is claimed with lostOwners 1 after.
func TestSchedulerMN_DatabaseOutageLongerThanStaleAfter(t *testing.T) {
	t.Parallel()
	const host = 2
	s := newSchedMN(t, 3, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)}, mnLongAnswerEnv)
	tenant := s.pg.NewTenant(t)
	c := s.client(host, tenant)
	tag := "mo-" + mnShort()
	stall := s.startClient(t, host, tenant, tag, parity.ComputeBehaviourStall)
	wf := mnWorkflow("mn-outage-wf", mnFire(300, 0, mnProc("noop", "SYNC", tag, true, mnLongAnswer)))

	t1 := mnSetup(t, c, "mn-outage-1", wf)
	r1 := s.awaitTask(t, t1, "Fire", 30*time.Second, "T1 running",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	owner := s.ownerNode(t, r1.ClaimOwner)
	if owner != 0 && owner != 1 {
		t.Fatalf("T1's owner %q is not a claiming pnode (got node %d)", r1.ClaimOwner, owner)
	}
	survivor := 1 - owner
	survivorInc := s.pg.Incarnation(t, survivor).String()
	s.pg.KillNode(owner)

	// T2 can only be claimed by the survivor now.
	t2 := mnSetup(t, c, "mn-outage-2", wf)
	r2 := s.awaitTask(t, t2, "Fire", 30*time.Second, "T2 running on the survivor",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	if r2.ClaimOwner != survivorInc {
		t.Fatalf("T2 is held by %q; want the survivor %q", r2.ClaimOwner, survivorInc)
	}

	s.pg.PauseDatabase(t)
	time.Sleep(fixtureutil.TunedStaleAfter + 5*time.Second)
	s.pg.UnpauseDatabase(t)
	resumeAt := time.Now()

	// T2: the self-cancelled run's attempt, recorded after recovery.
	r2 = s.awaitTask(t, t2, "Fire", 30*time.Second, "T2's recorded attempt",
		func(r mnTask, ok bool) bool { return ok && r.Attempts >= 1 })
	if r2.LastError != "CANCELLED: the run was stopped by the scheduler" || r2.FailureReason != "" {
		t.Errorf("T2 after the outage = %+v; want a counted attempt with the CANCELLED text", r2)
	}

	// The survivor's liveness record: removed, then recreated by a heartbeat.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var removedAt time.Time
	if err := s.db.QueryRow(ctx,
		`WITH d AS (DELETE FROM scheduler_owners WHERE owner::text = $1 RETURNING 1) SELECT now() FROM d`,
		survivorInc).Scan(&removedAt); err != nil {
		t.Fatalf("remove the survivor's liveness record: %v", err)
	}
	mnAwait(t, 5*fixtureutil.TunedHeartbeatInterval, "the recreated liveness record", func() bool {
		var n int
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM scheduler_owners WHERE owner::text = $1 AND heartbeat_at > $2`,
			survivorInc, removedAt).Scan(&n); err != nil {
			t.Fatalf("read liveness: %v", err)
		}
		return n == 1
	})

	// T1: no lost-owner claim before a whole stale period of clean heartbeats.
	gate := resumeAt.Add(fixtureutil.TunedStaleAfter - 3*time.Second)
	for time.Now().Before(gate) {
		r, ok := s.task(t, t1, "Fire")
		if !ok || r.ClaimOwner != r1.ClaimOwner || r.LostOwners != 0 {
			t.Fatalf("T1 was claimed %s after the database returned (%+v); the survivor had not yet heartbeated clean for %s",
				time.Since(resumeAt), r, fixtureutil.TunedStaleAfter)
		}
		time.Sleep(500 * time.Millisecond)
	}
	r1 = s.awaitTask(t, t1, "Fire", 30*time.Second, "T1 claimed as a lost owner",
		func(r mnTask, ok bool) bool { return ok && r.ClaimOwner == survivorInc && r.LostOwners == 1 })
	if r1.Status != "RUNNING" {
		t.Errorf("T1 = %+v; want RUNNING under the survivor", r1)
	}
	mnAwait(t, 10*time.Second, "T1's processor sent again", func() bool { return mnReceived(t, stall, t1) == 2 })
}
```

- [ ] **Step 3: Run it**

Run: `go test -count=1 -timeout 20m ./e2e/parity/postgres/ -run TestSchedulerMN_DatabaseOutageLongerThanStaleAfter`
Expected (merge-base plus T-1): FAIL — no `scheduler started … incarnation=`
line, so `ownerNode` returns -1 ("is not a claiming pnode"); with R but not
the gating: "T1 was claimed … after the database returned". After the
streams: PASS in about two and a half minutes.

**Teeth.** In R's claim loop, set `AllowLostOwner` whenever the last
heartbeat succeeded (drop the clean-window check) — the gate loop fails.
In BP's `Heartbeat`, use `UPDATE` instead of the upsert — the liveness await
times out.

- [ ] **Step 4: Commit**

```bash
git add e2e/parity/postgres/scheduler_mn_helpers_test.go e2e/parity/postgres/scheduler_mn_outage_test.go
git commit -m "test(multinode): a database outage longer than STALE_AFTER

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-7: e2e — fencing, run limits, the scheduler pool

**Spec:** §5.2 (every commit writes the task row; C1), §5.5 (a superseded run
sends no unsafe processor), §6.1 (limits, the immediate claim), §10.1 C1,
§10.2 "Scheduler pool (C4)" (`lock_timeout` 2 s); §13 "Ownership, fencing and
liveness" rows with an E cell: re-armed life refuses the old run's writes;
reclaimed or re-armed task fails the old commit; a replaced owner's segment
commit is refused by its stamp; a superseded owner sends no unsafe processor;
heartbeats not starved (C4); async-search heartbeats and claims not starved;
a scheduler-pool statement blocked on a task-row lock gives up after
`lock_timeout`; at most `MAX_RUNS`, a freed slot claims at once.

**Depends on:** S, BP (C1 on task rows, the scheduler pool and its session
settings, the async-search store on that pool), E (stamp, re-read,
`MarkUnsafe`), R (limits, immediate claim, bookkeeping retry), T-1, T-4, T-5.

**Files:**
- Create: `internal/e2e/scheduled_run_fencing_test.go`

**Interfaces:**
- Consumes: T-1, T-4, T-5 helpers; `app.Config.SearchJobHeartbeatInterval`,
  `SearchJobStaleAfter` (`internal/e2e/e2e_test.go:154-155`).
- Produces: `injectReclaim(t, s, entityID) (claimToken, owner string)`,
  `holdMainPool(t, h, n) (release func())`.

**How a reclaim is simulated in E.** A real reclaim needs a stale owner, i.e.
a stale period (53 s) — that is the M layer (T-10). Here the test writes what
a reclaiming pnode's `ClaimDue` writes: a new `claim_token` and a
`claim_owner` with no liveness record, on the RUNNING row, while the run's
processor is held. The running pnode's code path is the real one.

- [ ] **Step 1: Write the tests**

`internal/e2e/scheduled_run_fencing_test.go`:

```go
package e2e_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/app"
)

// scheduled_run_fencing_test.go — a run that lost its life or its claim
// cannot commit, stamp, mark or record; the per-pnode run limits; and the
// scheduler's own PostgreSQL pool under pressure.

// injectReclaim gives the entity's RUNNING task a new claim, as another
// pnode's lost-owner claim would, and returns it.
func injectReclaim(t *testing.T, s *schedDB, entityID string) (token, owner string) {
	t.Helper()
	token, owner = uuid.NewString(), uuid.NewString()
	tag, err := s.pool.Exec(context.Background(),
		`UPDATE scheduled_tasks SET claim_token = $2, claim_owner = $3, lost_owners = lost_owners + 1
		  WHERE tenant_id = $1 AND entity_id = $4 AND status = 'RUNNING'`,
		harnessTenant, token, owner, entityID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("inject a reclaim: rows %d, err %v", tag.RowsAffected(), err)
	}
	return token, owner
}

// TestSchedFence_ReArmedLifeRefusesOldRun: while the run's processor is held,
// a client update in the source state re-arms the task (a new life). The old
// run's commit writes the task row, which the client's commit changed after
// the run began: C1 refuses it; the re-read sees a new life: superseded,
// nothing recorded (§5.2). The new life fires; exactly one fire.
func TestSchedFence_ReArmedLifeRefusesOldRun(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sf-rearm"), uniq("sf-rearm-tag")
	release := make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptHoldFirst(release)})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sf-rearm-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitDBCondition(t, scheduledFireTimeout, "the run's processor", func() bool { return len(cn.Received()) == 1 })

	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":3,"status":"draft"}`, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("client update: %d %s", resp.StatusCode, body)
	}
	// Watch the row from here on: the old run must never record an attempt
	// or a failure on it.
	var worst atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if r, ok := s.task(t, id, "Fire"); ok && (r.Attempts > 0 || r.Status == "FAILED") {
				worst.Store(1)
			}
		}
	}()
	rel()
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	close(stop)
	wg.Wait()

	if worst.Load() != 0 {
		t.Error("the superseded run recorded an attempt or a failure on the new life")
	}
	events := schedEvents(t, h, id)
	if n := len(smEventsOfType(events, "SCHEDULED_TRANSITION_FIRE")); n != 1 {
		t.Errorf("%d fires; want exactly 1 (the new life's)", n)
	}
	if n := len(smEventsOfType(events, "SCHEDULED_TRANSITION_ARM")); n != 2 {
		t.Errorf("%d arms; want 2 (the create and the update)", n)
	}
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 3 {
		t.Errorf("amount = %v; want the client's 3", amount)
	}
	if n := len(cn.Received()); n != 2 {
		t.Errorf("the processor was sent %d times; want 2 (old run, new life)", n)
	}
}

// TestSchedFence_ReclaimedTaskRefusesOldRun: the RUNNING task is claimed from
// under the run. (a) Its final commit writes the task row: C1 refuses it and
// the re-read sees another claim — superseded, nothing recorded. (b) Its next
// COMMIT_BEFORE_DISPATCH segment's stamp is fenced by the claim token and
// refused, so the segment never commits and its processor is never sent.
func TestSchedFence_ReclaimedTaskRefusesOldRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		procs func(tagA, tagB string) []map[string]any
	}{
		{"FinalCommit", func(a, _ string) []map[string]any { return []map[string]any{sProc("p1", "SYNC", a, true)} }},
		{"SegmentStamp", func(a, b string) []map[string]any {
			return []map[string]any{sProc("p1", "SYNC", a, true), sProc("p2", "COMMIT_BEFORE_DISPATCH", b, true)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s := newSchedulerHarness(t, nil)
			model, tagA, tagB := uniq("sf-reclaim"), uniq("sf-reclaim-a"), uniq("sf-reclaim-b")
			gotWork, release := make(chan struct{}, 1), make(chan struct{})
			rel := closeOnce(release)
			t.Cleanup(rel)
			h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: holdScript(gotWork, release)})
			b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
			h.SetupModelWithWorkflow(t, model, fireOpenToDone("sf-reclaim-wf", 100, 0, tc.procs(tagA, tagB)...))
			id := createOpen(t, h, model, workflowSampleModel)

			<-gotWork
			token, owner := injectReclaim(t, s, id)
			rel()

			// The run ends; give it a few retry delays to try to record.
			time.Sleep(4 * time.Second)
			r := mustTask(t, s, id)
			if r.Status != "RUNNING" || r.ClaimToken != token || r.ClaimOwner != owner || r.Attempts != 0 || r.LastError != "" {
				t.Errorf("task = %+v; want the injected claim untouched and nothing recorded by the old run", r)
			}
			requireState(t, h, id, "Open")
			if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE", "") {
				t.Error("the reclaimed run committed a fire")
			}
			if n := len(b.Received()); n != 0 {
				t.Errorf("the segment's processor was sent %d times; want 0", n)
			}
		})
	}
}

// TestSchedFence_SupersededOwnerSendsNoUnsafe: the old run is held in its
// idempotent first processor while a client update re-arms the task; the new
// life runs to completion. When the old run resumes, its MarkUnsafe before the
// unsafe second processor is refused (a stale arm token): the unsafe
// processor is sent once, by the new life only (§5.5).
func TestSchedFence_SupersededOwnerSendsNoUnsafe(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sf-noun"), uniq("sf-noun-a"), uniq("sf-noun-b")
	release := make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: scriptHoldFirst(release)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sf-noun-wf", 100, 0,
		sProc("p1", "SYNC", tagA, true), sProc("p2", "SYNC", tagB, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitDBCondition(t, scheduledFireTimeout, "the old run's p1", func() bool { return len(a.Received()) == 1 })
	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":4,"status":"draft"}`, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("client update: %d %s", resp.StatusCode, body)
	}
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout) // the new life
	rel()                                                            // the old run resumes
	time.Sleep(3 * time.Second)

	if n := len(b.Received()); n != 1 {
		t.Errorf("the unsafe processor was sent %d times; want 1 (the new life)", n)
	}
	if n := len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")); n != 1 {
		t.Errorf("%d fires; want 1", n)
	}
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("a task remains after the fire")
	}
}

// TestSchedLimits_MaxRunsAndFreedSlotClaimsAtOnce: MAX_RUNS 2 on a 10s scan
// interval. Three due tasks: two runs start, the third waits; when one run
// ends, the third is claimed at once — long before the next scan (§6.1).
func TestSchedLimits_MaxRunsAndFreedSlotClaimsAtOnce(t *testing.T) {
	h, _ := newSchedulerHarness(t, func(cfg *app.Config) {
		cfg.Scheduler.MaxRuns = 2
		cfg.Scheduler.MaxRunsPerTenant = 2
		cfg.Scheduler.ScanInterval = 10 * time.Second
	})
	model, tag := uniq("sl-max"), uniq("sl-max-tag")
	tokens := make(chan struct{}, 3)
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		select {
		case <-tokens:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}})
	t.Cleanup(func() {
		for i := 0; i < 3; i++ {
			select {
			case tokens <- struct{}{}:
			default:
			}
		}
	})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sl-max-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	ids := []string{createOpen(t, h, model, workflowSampleModel), createOpen(t, h, model, workflowSampleModel), createOpen(t, h, model, workflowSampleModel)}

	awaitDBCondition(t, 25*time.Second, "two runs", func() bool { return len(cn.Received()) >= 2 })
	time.Sleep(time.Second)
	if n := len(cn.Received()); n != 2 {
		t.Fatalf("%d runs in progress; want MAX_RUNS 2", n)
	}
	freedAt := time.Now()
	tokens <- struct{}{}
	awaitDBCondition(t, 3*time.Second, "the third run, claimed when the slot freed", func() bool { return len(cn.Received()) == 3 })
	if since := time.Since(freedAt); since > 3*time.Second {
		t.Errorf("the third run started %s after the slot freed; want at once, not at the next 10s scan", since)
	}
	tokens <- struct{}{}
	tokens <- struct{}{}
	for _, id := range ids {
		awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	}
}

// holdMainPool starts n creates of a model whose SYNC processor is held, so
// each keeps one main-pool connection in an open transaction. It returns once
// the cnode has all n, and a func that releases them and waits for the creates.
func holdMainPool(t *testing.T, h *callbackHarness, n int) func() {
	t.Helper()
	model, tag := uniq("pool-hold"), uniq("pool-hold-tag")
	gotWork, release := make(chan struct{}, n), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "holder", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		gotWork <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}})
	h.SetupModelWithWorkflow(t, model, schedDoc("pool-hold-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
			"processors": []any{sProc("h", "SYNC", tag, true)}}}},
		"Done": map[string]any{},
	}))
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	}
	for i := 0; i < n; i++ {
		select {
		case <-gotWork:
		case <-time.After(15 * time.Second):
			t.Fatalf("only %d of %d holders reached their processor", i, n)
		}
	}
	return func() { rel(); wg.Wait() }
}
```

The rest of the file: the probe that shows the main pool is exhausted (its
model is imported while the pool is still free, because the import itself
needs a connection), and the pool tests.

```go
// poolProbe sets up a model for requirePoolExhausted while the pool is free.
func poolProbe(t *testing.T, h *callbackHarness) string {
	t.Helper()
	model := uniq("pool-probe")
	h.SetupModelWithWorkflow(t, model, schedDoc("pool-probe-wf", map[string]any{"Open": map[string]any{}}))
	return model
}

// requirePoolExhausted proves the main pool has no free connection: a create
// of the probe model does not succeed within a second.
func requirePoolExhausted(t *testing.T, h *callbackHarness, probeModel string) {
	t.Helper()
	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(probeModel, 1, workflowSampleModel) }()
	select {
	case res := <-done:
		if res.status == http.StatusOK {
			t.Fatalf("a create succeeded while the main pool should be exhausted")
		}
	case <-time.After(time.Second):
	}
}
```

```go
// TestSchedPool_HeartbeatNotStarvedByMainPool: every main-pool connection is
// held by an open entity transaction; the heartbeat, on its own connection
// (C4), keeps advancing the liveness record.
func TestSchedPool_HeartbeatNotStarvedByMainPool(t *testing.T) {
	h, s := newSchedulerHarness(t, func(*app.Config) { t.Setenv("CYODA_POSTGRES_MAX_CONNS", "2") })
	probe := poolProbe(t, h)
	release := holdMainPool(t, h, 2)
	defer release()
	requirePoolExhausted(t, h, probe)

	var before time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT max(heartbeat_at) FROM scheduler_owners`).Scan(&before); err != nil {
		t.Fatalf("read liveness: %v", err)
	}
	time.Sleep(3500 * time.Millisecond)
	var after time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT max(heartbeat_at) FROM scheduler_owners`).Scan(&after); err != nil {
		t.Fatalf("read liveness: %v", err)
	}
	if after.Sub(before) < 2*time.Second {
		t.Errorf("heartbeat advanced %s in 3.5s with the main pool exhausted; want >= 2s (one per second)", after.Sub(before))
	}
}

// TestSchedPool_AsyncSearchReclaimNotStarved: an async-search job whose owner
// is gone is reclaimed, and then heartbeated, on the scheduler pool while every
// main-pool connection is held (§10.2).
func TestSchedPool_AsyncSearchReclaimNotStarved(t *testing.T) {
	h, s := newSchedulerHarness(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_MAX_CONNS", "2")
		cfg.SearchJobHeartbeatInterval = 250 * time.Millisecond
		cfg.SearchJobStaleAfter = time.Second
	})
	model := uniq("pool-search")
	h.SetupModelWithWorkflow(t, model, schedDoc("pool-search-wf", map[string]any{"Open": map[string]any{}}))
	createOpen(t, h, model, workflowSampleModel)
	probe := poolProbe(t, h)

	jobID := uuid.NewString()
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO search_jobs (id, tenant_id, status, model_name, model_ver, condition, created_at, heartbeat_time, epoch)
		VALUES ($1, $2, 'RUNNING', $3, '1', '{"type":"group","operator":"AND","conditions":[]}'::jsonb,
		        now() - interval '1 hour', now() - interval '1 hour', 1)`,
		jobID, harnessTenant, model); err != nil {
		t.Fatalf("seed an orphaned job: %v", err)
	}
	release := holdMainPool(t, h, 2)
	defer release()
	requirePoolExhausted(t, h, probe)

	readJob := func() (epoch int64, hb time.Time) {
		if err := s.pool.QueryRow(context.Background(),
			`SELECT epoch, heartbeat_time FROM search_jobs WHERE tenant_id = $1 AND id = $2`, harnessTenant, jobID).Scan(&epoch, &hb); err != nil {
			t.Fatalf("read the job: %v", err)
		}
		return epoch, hb
	}
	awaitDBCondition(t, 10*time.Second, "the reclaim", func() bool { e, _ := readJob(); return e >= 2 })
	_, first := readJob()
	time.Sleep(time.Second)
	if _, second := readJob(); !second.After(first) {
		t.Errorf("the reclaimed job's heartbeat did not advance (%v then %v) while the main pool was exhausted", first, second)
	}
}

// TestSchedPool_LockTimeoutOnTaskRowLock: another entity's transaction holds
// the scheduled task's row (its joined callback updated the entity in the
// source state, which re-arms the task) and stays open. The run fails; its
// RecordAttempt on the scheduler pool waits on that row lock and gives up
// after lock_timeout (2s), then retries — no scheduler-pool statement waits
// longer. When the holder commits, the re-arm stands and the old run's
// bookkeeping is refused: the new life has attempts 0.
func TestSchedPool_LockTimeoutOnTaskRowLock(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("pool-lock"), uniq("pool-lock-tag")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	var runCalls atomic.Int32
	h.AttachCnode(t, cnodeSpec{name: "run", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
		if runCalls.Add(1) > 1 {
			return neverAnswer()
		}
		gotWork <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerFail("run boom")
	}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("pool-lock-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)
	<-gotWork
	before := mustTask(t, s, id)

	// The holder: a create whose processor updates the scheduled entity
	// through its joined callback, then waits.
	hModel, hTag := uniq("pool-lock-holder"), uniq("pool-lock-holder-tag")
	updated, holdRelease := make(chan int, 1), make(chan struct{})
	holdRel := closeOnce(holdRelease)
	t.Cleanup(holdRel)
	h.AttachCnode(t, cnodeSpec{name: "holder", tags: []string{hTag}, script: func(ctx context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.UpdateEntity(id, `{"name":"Test Order","amount":9,"status":"draft"}`)
		st := -1
		if err == nil {
			st = res.StatusCode
		}
		updated <- st
		select {
		case <-holdRelease:
		case <-ctx.Done():
			return neverAnswer()
		}
		return answerOK()
	}})
	h.SetupModelWithWorkflow(t, hModel, schedDoc("pool-lock-holder-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
			"processors": []any{sProc("h", "SYNC", hTag, true)}}}},
		"Done": map[string]any{},
	}))
	holderDone := make(chan createEntityResult, 1)
	go func() { holderDone <- h.CreateEntityRaw(hModel, 1, workflowSampleModel) }()
	if st := <-updated; st != http.StatusOK {
		t.Fatalf("the holder's joined update answered %d; want 200", st)
	}

	rel() // the run fails; its RecordAttempt meets the holder's row lock
	var seen, longest float64
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		var wait float64
		if err := s.pool.QueryRow(context.Background(), `
			SELECT count(*), COALESCE(max(extract(epoch FROM now() - query_start)), 0)
			  FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND query ILIKE '%scheduled_tasks%' AND pid <> pg_backend_pid()`).Scan(&n, &wait); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			seen++
		}
		if wait > longest {
			longest = wait
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seen == 0 {
		t.Fatal("no scheduler statement ever waited on the task row; the scenario did not hold the lock")
	}
	if longest > 3 {
		t.Errorf("a statement waited %.1fs on the task-row lock; lock_timeout is 2s", longest)
	}

	holdRel()
	if res := <-holderDone; res.status != http.StatusOK {
		t.Fatalf("holder create: %d %s", res.status, res.body)
	}
	r := s.awaitTask(t, id, "Fire", 10*time.Second, "the new life",
		func(r taskRow, ok bool) bool { return ok && r.ArmToken != before.ArmToken })
	if r.Attempts != 0 || r.LastError != "" {
		t.Errorf("new life = %+v; the old run's refused bookkeeping must not reach it", r)
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test ./internal/e2e/ -run 'TestSchedFence_|TestSchedLimits_|TestSchedPool_'`
Expected (merge-base plus T-1): FAIL to compile. After the streams: PASS.

**Teeth.** In BP, write the task row outside the entity transaction
(`RemoveLife` on the scheduler pool) — `ReArmedLifeRefusesOldRun` sees two
fires. In R, leave `lock_timeout` unset on the scheduler pool —
`LockTimeoutOnTaskRowLock` fails "waited …s". In R's claim loop, drop the
claim-on-freed-slot — `MaxRunsAndFreedSlotClaimsAtOnce` fails on the 3s bound.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/scheduled_run_fencing_test.go
git commit -m "test(e2e): fencing of superseded runs, run limits, the scheduler pool

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-8: shutdown — the drain, the exemption for unsafe work in flight, the cut

**Spec:** §6.4 (steps 1–6), §5.3 (cancellation), §5.6 (bookkeeping of a cut
run); §13 "Shutdown" rows with an E or M cell. README Review Focus 5 (a
rolling deploy while an unsafe processor is in flight).

**Depends on:** R (`Drain`, the step-3 exemption, the `run.go` signal path —
V4), E (checkpoints), K (`HandedOff`), T-1, T-4, T-5, T-6 (M helpers).

**Files:**
- Create: `internal/e2e/scheduled_run_shutdown_test.go`
- Create: `e2e/parity/postgres/scheduler_mn_shutdown_test.go`

**Interfaces:**
- Consumes: `(*app.App).Shutdown` running the scheduler's `Drain` (spec §6.4
  "When a server fails, the same sequence runs from `a.Shutdown()`"), safe to
  call twice (Open point 4); T-6's M helpers; `(*pgMultiNode) SignalNode,
  AwaitNodeExit`; `parity.ComputeBehaviourHold`, `ComputeClient.Release`.
- Produces: `shutdownAsync(h) <-chan time.Duration`.

In E, `h.app.Shutdown()` runs the drain while the harness's gRPC server —
started by the test, not by the app — keeps every cnode stream open, as the
signal path does before the server drains (§6.4 step 6). In M, `SIGTERM`
drives the real `run.go` path.

- [ ] **Step 1: The e2e tests**

`internal/e2e/scheduled_run_shutdown_test.go`:

```go
package e2e_test

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// scheduled_run_shutdown_test.go — the scheduler's drain on one stack
// (spec §6.4), driven through App.Shutdown while the cnode streams stay open.

// shutdownAsync runs h.app.Shutdown on a goroutine and reports how long it took.
func shutdownAsync(h *callbackHarness) <-chan time.Duration {
	out := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		h.app.Shutdown()
		out <- time.Since(start)
	}()
	return out
}

// TestSchedShutdown_RunFinishesWithinDrainStreamsOpen: a run in flight when
// the drain starts finishes inside it — its cnode's answer still arrives —
// and commits; Shutdown returns without waiting out the drain (§6.4 step 2).
func TestSchedShutdown_RunFinishesWithinDrainStreamsOpen(t *testing.T) {
	h, s := newSchedulerHarness(t, func(cfg *app.Config) { cfg.Scheduler.ShutdownDrain = 10 * time.Second })
	model, tag := uniq("sd-drain"), uniq("sd-drain-tag")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: holdScript(gotWork, release)})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sd-drain-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	<-gotWork
	took := shutdownAsync(h)
	time.Sleep(500 * time.Millisecond)
	rel() // answered over the still-open stream
	select {
	case d := <-took:
		if d >= 10*time.Second {
			t.Errorf("Shutdown took %s; the run ended long before the 10s drain", d)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	requireState(t, h, id, "Done")
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("the drained run's task is still stored")
	}
}

// TestSchedShutdown_UnsafeInFlightNotCut: the run's unsafe processor has been
// handed off and is in flight when the drain (1s) ends. Step 3 does not cut
// that run; its callout finishes, the run continues with a safe processor and
// commits; Shutdown returns after it (§6.4 step 3).
func TestSchedShutdown_UnsafeInFlightNotCut(t *testing.T) {
	h, s := newSchedulerHarness(t, nil) // ShutdownDrain 1s
	model, tagA, tagB := uniq("sd-unsafe"), uniq("sd-unsafe-a"), uniq("sd-unsafe-b")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: holdScript(gotWork, release)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sd-unsafe-wf", 100, 0,
		sProc("p1", "SYNC", tagA, false), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	<-gotWork
	took := shutdownAsync(h)
	time.Sleep(3 * time.Second) // past the 1s drain and step 3
	select {
	case d := <-took:
		t.Fatalf("Shutdown returned after %s while an unsafe callout was in flight", d)
	default:
	}
	if r := mustTask(t, s, id); r.Status == "FAILED" {
		t.Fatalf("the run was cut: %+v", r)
	}
	rel()
	select {
	case <-took:
	case <-time.After(90 * time.Second):
		t.Fatal("Shutdown did not return after the exempt run ended")
	}
	requireState(t, h, id, "Done")
	if n := len(b.Received()); n != 1 {
		t.Errorf("the safe processor after the unsafe one was sent %d times; want 1", n)
	}
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("a deploy turned the run into a FAILED task")
	}
}

// TestSchedShutdown_CutAfterEarlierUnsafeHandOffFails: the unsafe first
// processor completed; the idempotent second is in flight when the drain ends.
// No unsafe callout is in flight, so step 3 cuts the run; unsafe work reached a
// cnode and the run did not commit: FAILED UNSAFE_WORK_NOT_COMPLETED (§5.6).
func TestSchedShutdown_CutAfterEarlierUnsafeHandOffFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil) // ShutdownDrain 1s
	model, tagA, tagB := uniq("sd-cut"), uniq("sd-cut-a"), uniq("sd-cut-b")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: holdScript(gotWork, release)})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sd-cut-wf", 100, 0,
		sProc("p1", "SYNC", tagA, false), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	<-gotWork
	select {
	case <-shutdownAsync(h):
	case <-time.After(90 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	r := mustTask(t, s, id)
	if r.Status != "FAILED" || r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("after the cut: %+v; want FAILED UNSAFE_WORK_NOT_COMPLETED", r)
	}
	if data := failEvent(t, h, id); data["reason"] != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("SCHEDULED_TRANSITION_FAIL data = %v", data)
	}
	requireState(t, h, id, "Open")
}
```

- [ ] **Step 2: The multi-node test**

`e2e/parity/postgres/scheduler_mn_shutdown_test.go`:

```go
package postgres

import (
	"syscall"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_ShutdownDrain: pnodes 0-2 claim, pnode 3 hosts the compute
// clients. T1's idempotent processor stalls; T2's unsafe processor is held and
// followed by a safe one. Every pnode that owns one of them gets SIGTERM (the
// signal path of run.go, spec §6.4), with a 2s drain.
//
//   - T1: cut after the drain with nothing unsafe handed off: WAITING,
//     uncounted, and claimed at once by a surviving pnode — within seconds,
//     not after STALE_AFTER — which sends its processor again.
//   - T2: its unsafe callout is in flight, so its run is not cut. Released
//     after the drain, it continues with the safe processor, commits, and its
//     owner then exits. The unsafe processor was sent once.
func TestSchedulerMN_ShutdownDrain(t *testing.T) {
	t.Parallel()
	const host = 3
	s := newSchedMN(t, 4, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)},
		mnLongAnswerEnv, "CYODA_SCHEDULER_SHUTDOWN_DRAIN=2s")
	tenant := s.pg.NewTenant(t)
	c := s.client(host, tenant)
	tag1, tag2a, tag2b := "sd1-"+mnShort(), "sd2a-"+mnShort(), "sd2b-"+mnShort()
	stall := s.startClient(t, host, tenant, tag1, parity.ComputeBehaviourStall)
	hold := s.startClient(t, host, tenant, tag2a, parity.ComputeBehaviourHold)
	safe := s.startClient(t, host, tenant, tag2b, parity.ComputeBehaviourCatalog)

	t1 := mnSetup(t, c, "mn-sd-1", mnWorkflow("mn-sd-1-wf", mnFire(300, 0, mnProc("noop", "SYNC", tag1, true, mnLongAnswer))))
	t2 := mnSetup(t, c, "mn-sd-2", mnWorkflow("mn-sd-2-wf", mnFire(300, 0,
		mnProc("noop", "SYNC", tag2a, false, mnLongAnswer), mnProc("noop", "SYNC", tag2b, true, 10000))))
	r1 := s.awaitTask(t, t1, "Fire", 30*time.Second, "T1 running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	r2 := s.awaitTask(t, t2, "Fire", 30*time.Second, "T2 running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.Marked })
	mnAwait(t, 10*time.Second, "both processors at their clients", func() bool {
		return mnReceived(t, stall, t1) == 1 && mnReceived(t, hold, t2) == 1
	})

	owners := map[int]bool{s.ownerNode(t, r1.ClaimOwner): true, s.ownerNode(t, r2.ClaimOwner): true}
	for n := range owners {
		if n < 0 || n == host {
			t.Fatalf("an owner is not a claiming pnode: %v", owners)
		}
		if err := s.pg.SignalNode(n, syscall.SIGTERM); err != nil {
			t.Fatalf("SIGTERM node %d: %v", n, err)
		}
	}
	signalAt := time.Now()

	// T1: cut, recorded uncounted, claimed at once elsewhere.
	again := s.awaitTask(t, t1, "Fire", 20*time.Second, "T1 claimed by a survivor",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.ClaimOwner != r1.ClaimOwner })
	if since := time.Since(signalAt); since > 15*time.Second {
		t.Errorf("T1 was claimed again %s after SIGTERM; want at once, not after STALE_AFTER", since)
	}
	if n := s.ownerNode(t, again.ClaimOwner); owners[n] || n == host || n < 0 {
		t.Errorf("T1 was claimed by node %d; want a pnode that was not signalled", n)
	}
	if again.Attempts != 0 || again.LostOwners != 0 || again.LastError != "CANCELLED: the run was stopped by the scheduler" {
		t.Errorf("T1 after the cut = %+v; want attempts 0, lostOwners 0 and the CANCELLED text", again)
	}
	mnAwait(t, 10*time.Second, "T1's processor sent again", func() bool { return mnReceived(t, stall, t1) == 2 })

	// T2: not cut. After the drain and step 3, release the unsafe callout.
	time.Sleep(time.Until(signalAt.Add(4 * time.Second)))
	owner2 := s.ownerNode(t, r2.ClaimOwner)
	if err := s.pg.AwaitNodeExit(owner2, 0); err == nil {
		t.Fatalf("T2's owner (node %d) exited with an unsafe callout in flight", owner2)
	}
	if r, _ := s.task(t, t2, "Fire"); r.Status == "FAILED" {
		t.Fatalf("T2 was cut: %+v", r)
	}
	hold.Release(t)
	mnAwait(t, 30*time.Second, "T2 fired", func() bool {
		got, err := c.GetEntity(t, t2)
		return err == nil && got.Meta.State == "Done"
	})
	if n := mnReceived(t, hold, t2); n != 1 {
		t.Errorf("T2's unsafe processor was sent %d times; want 1", n)
	}
	if n := mnReceived(t, safe, t2); n != 1 {
		t.Errorf("T2's safe processor was sent %d times; want 1", n)
	}
	if n := mnCountEvents(t, c, t2, "SCHEDULED_TRANSITION_FIRE"); n != 1 {
		t.Errorf("T2 fired %d times; want 1", n)
	}
	if err := s.pg.AwaitNodeExit(owner2, 60*time.Second); err != nil {
		t.Errorf("T2's owner did not exit after its run committed: %v", err)
	}
}
```

- [ ] **Step 3: Run them**

Run: `go test ./internal/e2e/ -run 'TestSchedShutdown_'`
Run: `go test -count=1 -timeout 20m ./e2e/parity/postgres/ -run TestSchedulerMN_ShutdownDrain`
Expected (merge-base plus T-1): FAIL (compile in E; in M, no incarnation line).
After the streams: PASS.

**Teeth.** In R's `Drain`, cancel every run at step 3 with no exemption —
`UnsafeInFlightNotCut` (E) and T2 (M) end FAILED. In R's `decideBookkeeping`,
count the cut attempt (drop `NotCounted`) — T1's `attempts` is 1.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/scheduled_run_shutdown_test.go e2e/parity/postgres/scheduler_mn_shutdown_test.go
git commit -m "test(scheduler): the shutdown drain on one stack and on a cluster

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-9: e2e (isolated) — client writes against a live scheduler never answer 5xx and never lose a timer

**Spec:** §7 "A client write can get a retryable 409 when it races the
scheduler" and the server retries of delete; §13 Rules ("Concurrency cases
stay out of P"). README Review Focus 2 names this task beside W-2 and W-3.
This is a soak over a live scheduler; the deterministic 409 and retry cells of
"Entity writes and workflow import" are W's (W-2 … W-7).

**Depends on:** W (server retry, the 409), E, R, BP, T-1, T-4.

**Files:**
- Create: `internal/e2e/scheduled_run_race_test.go`

**Interfaces:**
- Consumes: T-1/T-4 helpers; `isRetryableConflict` (`internal/e2e/helpers_test.go:197`); `(h) callback` (goroutine-safe).
- Produces: nothing.

- [ ] **Step 1: Write the test**

`internal/e2e/scheduled_run_race_test.go`:

```go
package e2e_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestSchedRace_ClientWritesAgainstLiveScheduler: eight entities carry a
// self-loop timer that fires every 50ms, so the scheduler is always claiming,
// firing and re-arming their task rows. Four writers update the entities for
// five seconds, then every entity is deleted while its timer still runs.
// Every answer is 200 or a retryable 409 — never a 5xx, never a
// non-retryable 409. No timer is lost: after the writers stop, each entity
// has its task and fires again. A delete that answers 200 leaves no task.
func TestSchedRace_ClientWritesAgainstLiveScheduler(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model := uniq("sx-race")
	h.SetupModelWithWorkflow(t, model, schedDoc("sx-race-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Tick", "next": "Open", "manual": false, "schedule": map[string]any{"delayMs": 50}}}},
	}))
	const n = 8
	ids := make([]string, n)
	for i := range ids {
		ids[i] = createOpen(t, h, model, workflowSampleModel)
	}

	var mu sync.Mutex
	var bad []string
	conflicts, oks := 0, 0
	record := func(what string, res callbackResult, err error) bool {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", what, err))
		case res.StatusCode == http.StatusOK:
			oks++
			return true
		case res.StatusCode == http.StatusConflict && isRetryableConflict([]byte(res.Body)):
			conflicts++
		default:
			bad = append(bad, fmt.Sprintf("%s: %d %s", what, res.StatusCode, res.Body))
		}
		return false
	}

	deadline := time.Now().Add(5 * time.Second)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				id := ids[(w+i)%n]
				res, err := h.callback(http.MethodPut, "/api/entity/JSON/"+id,
					fmt.Sprintf(`{"name":"Test Order","amount":%d,"status":"draft"}`, i), "")
				record("update "+id, res, err)
			}
		}(w)
	}
	wg.Wait()
	if len(bad) > 0 {
		t.Fatalf("%d answers were neither 200 nor a retryable 409; first: %s", len(bad), bad[0])
	}
	if oks == 0 {
		t.Fatal("no update succeeded; the scenario exercised nothing")
	}
	t.Logf("updates: %d ok, %d retryable 409", oks, conflicts)

	// No lost timer: each entity has its task and fires again.
	for _, id := range ids {
		if _, ok := s.task(t, id, "Tick"); !ok {
			t.Errorf("entity %s lost its timer", id)
			continue
		}
		fires := len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE"))
		awaitDBCondition(t, 5*time.Second, "a further fire of "+id, func() bool {
			return len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")) > fires
		})
	}

	// Deletes while the timers run.
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for try := 0; try < 5; try++ {
				res, err := h.callback(http.MethodDelete, "/api/entity/"+id, "", "")
				if record("delete "+id, res, err) {
					return
				}
				mu.Lock()
				failed := len(bad) > 0
				mu.Unlock()
				if failed {
					return
				}
			}
		}(id)
	}
	wg.Wait()
	if len(bad) > 0 {
		t.Fatalf("a delete answered neither 200 nor a retryable 409: %s", bad[0])
	}
	for _, id := range ids {
		if _, status := h.GetEntityState(t, id); status != http.StatusNotFound {
			t.Errorf("entity %s: GET %d after its delete; want 404", id, status)
		}
		if c := s.count(t, "SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1", id); c != 0 {
			t.Errorf("entity %s: %d task rows after its delete", id, c)
		}
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/e2e/ -run TestSchedRace_ClientWritesAgainstLiveScheduler`
Expected (merge-base plus T-1): FAIL to compile. With E, R, BP and without
W's delete retry: a delete answers a retryable 409 more than five times in a
row only rarely; the failing run shows up instead as a 500 when a task-row
conflict is not classified. After W: PASS.

**Teeth.** In BP's `classifySQLState`, drop the 40001 mapping for statements
on `scheduled_tasks` (return the raw error) — an update answers 500.

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/scheduled_run_race_test.go
git commit -m "test(e2e): client writes against a live scheduler

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task T-10: multi-node — lost owners, a run that outlives three heartbeats, the processor sent once

**Spec:** §5.1 steps 1–4, §5.4, §5.5, §6.1, §6.2 "Hung runs"; §13 rows "a run
longer than 3 × heartbeat interval is not claimed elsewhere" (M), "owner
killed, no mark → claimed after `STALE_AFTER`, `lostOwners` 1, fires" (M),
"owner killed with a mark → FAILED; the processor was sent once" (M), "owner
killed, short `timeoutMs` → FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS`" (M),
"owner killed after a cascade-step commit → next claim FAILED
`STOPPED_AFTER_PARTIAL_COMMIT`" (M).

**Depends on:** S, BP, K, E, R, T-1, T-6.

**Files:**
- Create: `e2e/parity/postgres/scheduler_mn_lost_owner_test.go`

**Interfaces:** consumes T-1 and T-6; produces nothing.

**Why six pnodes.** Four tasks, each owned by some claiming pnode; every
owner is killed. With five claiming pnodes at least one survives whatever
the claims were, and the sixth pnode hosts the compute clients and is never
killed. Nothing in the test depends on which pnode claims what.

- [ ] **Step 1: Write the test**

`e2e/parity/postgres/scheduler_mn_lost_owner_test.go`:

```go
package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// lostSubject is one task of TestSchedulerMN_LostOwners, in a tenant of its own.
type lostSubject struct {
	tenant parity.Tenant
	c      *client.Client
	tag    string
	id     uuid.UUID
	stall  parity.ComputeClient
	claim  mnTask
}

// TestSchedulerMN_LostOwners: four tasks run on live owners with their
// processors stalled on the host pnode:
//
//	A  idempotent processor, no mark
//	B  unsafe processor: a mark is written before it is sent
//	C  idempotent, timeoutMs 5000
//	D  a cascade step commits the entity in Mid (COMMIT_BEFORE_DISPATCH),
//	   then its idempotent second processor stalls
//
// First, for longer than STALE_AFTER — while every other pnode may make
// lost-owner claims — nobody claims them: the owners heartbeat, and liveness
// is not progress (§6.2). Then every owner is killed. After STALE_AFTER a
// survivor claims each with lostOwners 1 and decides (§5.1): B is FAILED
// UNSAFE_WORK_NOT_COMPLETED and its processor was sent exactly once; C is
// FAILED EXPIRED_AFTER_FAILED_ATTEMPTS; D is FAILED STOPPED_AFTER_PARTIAL_COMMIT;
// A runs again and fires.
func TestSchedulerMN_LostOwners(t *testing.T) {
	t.Parallel()
	const host = 5
	s := newSchedMN(t, 6, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)}, mnLongAnswerEnv)

	subject := func() *lostSubject {
		tenant := s.pg.NewTenant(t)
		sub := &lostSubject{tenant: tenant, c: s.client(host, tenant), tag: "lo-" + mnShort()}
		sub.stall = s.startClient(t, host, tenant, sub.tag, parity.ComputeBehaviourStall)
		return sub
	}
	a, b, cc, d := subject(), subject(), subject(), subject()
	a.id = mnSetup(t, a.c, "mn-lo-a", mnWorkflow("mn-lo-a-wf", mnFire(300, 0, mnProc("noop", "SYNC", a.tag, true, mnLongAnswer))))
	b.id = mnSetup(t, b.c, "mn-lo-b", mnWorkflow("mn-lo-b-wf", mnFire(300, 0, mnProc("noop", "SYNC", b.tag, false, mnLongAnswer))))
	cc.id = mnSetup(t, cc.c, "mn-lo-c", mnWorkflow("mn-lo-c-wf", mnFire(300, 5000, mnProc("noop", "SYNC", cc.tag, true, mnLongAnswer))))
	dStep := "lod-" + mnShort()
	dCommit := s.startClient(t, host, d.tenant, dStep, parity.ComputeBehaviourCatalog)
	d.id = mnSetup(t, d.c, "mn-lo-d", mnWorkflow("mn-lo-d-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Mid", "manual": false, "schedule": map[string]any{"delayMs": 300}}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{
			"name": "Step", "next": "Done", "manual": false, "processors": []any{
				mnProc("noop", "COMMIT_BEFORE_DISPATCH", dStep, true, 10000),
				mnProc("noop", "SYNC", d.tag, true, mnLongAnswer)}}}},
		"Done": map[string]any{},
	}))

	// All four running, each with its processor at its stalled client.
	a.claim = s.awaitTask(t, a.id, "Fire", 30*time.Second, "A running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	b.claim = s.awaitTask(t, b.id, "Fire", 30*time.Second, "B running with its mark", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.Marked })
	cc.claim = s.awaitTask(t, cc.id, "Fire", 30*time.Second, "C running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	d.claim = s.awaitTask(t, d.id, "Fire", 30*time.Second, "D running after its partial commit", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.PartialCommit })
	all := []*lostSubject{a, b, cc, d}
	for _, sub := range all {
		mnAwait(t, 10*time.Second, "the stalled processor", func() bool { return mnReceived(t, sub.stall, sub.id) == 1 })
	}
	runningAt := time.Now()

	// Not claimed elsewhere: the owners heartbeat through a whole stale
	// period in which every other pnode is allowed lost-owner claims.
	until := s.bootAt.Add(fixtureutil.TunedStaleAfter + 5*time.Second)
	if min := runningAt.Add(fixtureutil.TunedStaleAfter); until.Before(min) {
		until = min
	}
	for time.Now().Before(until) {
		for _, sub := range all {
			r, ok := s.task(t, sub.id, "Fire")
			if !ok || r.ClaimToken != sub.claim.ClaimToken || r.LostOwners != 0 {
				t.Fatalf("task %s was claimed while its owner heartbeated: %+v (was %+v)", sub.id, r, sub.claim)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, sub := range all {
		if n := mnReceived(t, sub.stall, sub.id); n != 1 {
			t.Fatalf("task %s's processor was sent %d times during the run; want 1", sub.id, n)
		}
	}

	// Kill every owner.
	owners := map[int]bool{}
	for _, sub := range all {
		n := s.ownerNode(t, sub.claim.ClaimOwner)
		if n < 0 || n == host {
			t.Fatalf("task %s's owner %q is not a claiming pnode", sub.id, sub.claim.ClaimOwner)
		}
		owners[n] = true
	}
	for n := range owners {
		s.pg.KillNode(n)
	}
	survivors := map[string]bool{}
	for i := 0; i < host; i++ {
		if !owners[i] {
			survivors[s.pg.Incarnation(t, i).String()] = true
		}
	}

	// A must fire when reclaimed: its stalled client goes, a hold client takes
	// its tag. B, C and D keep their stalled clients (their counts must stay
	// 1) and gain healthy clients that must never be sent anything.
	a.stall.Stop()
	aHold := s.startClient(t, host, a.tenant, a.tag, parity.ComputeBehaviourHold)
	bFresh := s.startClient(t, host, b.tenant, b.tag, parity.ComputeBehaviourCatalog)
	cFresh := s.startClient(t, host, cc.tenant, cc.tag, parity.ComputeBehaviourCatalog)
	dFresh := s.startClient(t, host, d.tenant, d.tag, parity.ComputeBehaviourCatalog)

	within := fixtureutil.TunedStaleAfter + 30*time.Second
	ra := s.awaitTask(t, a.id, "Fire", within, "A claimed as a lost owner",
		func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" && r.LostOwners == 1 })
	if !survivors[ra.ClaimOwner] {
		t.Errorf("A was claimed by %q; want a surviving pnode", ra.ClaimOwner)
	}
	mnAwait(t, 10*time.Second, "A's processor at the hold client", func() bool { return mnReceived(t, aHold, a.id) == 1 })
	aHold.Release(t)
	mnAwait(t, 20*time.Second, "A fired", func() bool {
		got, err := a.c.GetEntity(t, a.id)
		return err == nil && got.Meta.State == "Done"
	})

	for _, tc := range []struct {
		sub    *lostSubject
		reason string
	}{
		{b, "UNSAFE_WORK_NOT_COMPLETED"},
		{cc, "EXPIRED_AFTER_FAILED_ATTEMPTS"},
		{d, "STOPPED_AFTER_PARTIAL_COMMIT"},
	} {
		r := s.awaitTask(t, tc.sub.id, "Fire", within, "FAILED", func(r mnTask, ok bool) bool { return ok && r.Status == "FAILED" })
		if r.FailureReason != tc.reason || r.LostOwners != 1 {
			t.Errorf("task %s = %+v; want FAILED %s with lostOwners 1", tc.sub.id, r, tc.reason)
		}
		if n := mnCountEvents(t, tc.sub.c, tc.sub.id, "SCHEDULED_TRANSITION_FAIL"); n != 1 {
			t.Errorf("task %s: %d SCHEDULED_TRANSITION_FAIL events; want 1", tc.sub.id, n)
		}
	}
	if got, err := d.c.GetEntity(t, d.id); err != nil || got.Meta.State != "Mid" {
		t.Errorf("D's entity = %+v (err %v); want Mid, the committed step", got.Meta, err)
	}

	// Sent once: three retry delays later nothing more has been sent.
	time.Sleep(3 * fixtureutil.TunedRetryDelay)
	for _, x := range []struct {
		name  string
		cc    parity.ComputeClient
		id    uuid.UUID
		wantN int
	}{
		{"B stalled", b.stall, b.id, 1}, {"B fresh", bFresh, b.id, 0},
		{"C stalled", cc.stall, cc.id, 1}, {"C fresh", cFresh, cc.id, 0},
		{"D stalled", d.stall, d.id, 1}, {"D fresh", dFresh, d.id, 0},
		{"D step commit", dCommit, d.id, 1},
	} {
		if n := mnReceived(t, x.cc, x.id); n != x.wantN {
			t.Errorf("%s: %d requests; want %d", x.name, n, x.wantN)
		}
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -count=1 -timeout 20m ./e2e/parity/postgres/ -run TestSchedulerMN_LostOwners`
Expected (merge-base plus T-1): FAIL (no incarnation line). After the streams:
PASS in about two and a half minutes.

**Teeth.** In E's §5.1, skip step 1 (the mark check) — B runs again and
`B fresh` receives a request. In E's stamp, never set `PartialCommit` — D is
retried instead of FAILED. In R's heartbeat, stop after the first beat —
the "claimed while its owner heartbeated" loop fails.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/postgres/scheduler_mn_lost_owner_test.go
git commit -m "test(multinode): lost owners — reclaim, mark, deadline, partial commit

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task T-11: multi-node — contended claims, due siblings, a pnode with a partial cluster view

**Spec:** §6.1 ("One task per entity", disjoint claims), §10.2 `ClaimDue`
steps 1–3, A4 (an empty or partial cluster view is safe); §13 rows
"concurrent `ClaimDue` calls get disjoint sets" (M), "two due siblings, two
pnodes at once: one wins, the other claims nothing that tick" (M), "contended
claim loop: a task claimed elsewhere between ranking and locking is never
re-claimed" (M), "an empty cluster view has no effect" (M).

**Depends on:** S, BP (`ClaimDue`, the unique RUNNING index), E, R, T-1, T-6.

**Files:**
- Create: `e2e/parity/postgres/scheduler_mn_contention_test.go`

**Interfaces:** consumes T-1 and T-6; produces nothing.

**What M adds to S.** The store cases (S) race `ClaimDue` calls in one
process. Here three real pnodes' claim loops, 50 ms apart, race over one
table, and the observable result is end to end: each entity's processor is
sent once and each entity fires once. Pnode 2 is started with no gossip seeds,
so its cluster view holds only itself. Under the removed coordinator a node
with such a view scanned and dispatched on its own; under claims it is one
more claimer. "The other claims nothing that tick" is a statement about one
`ClaimDue` call; M cannot see a tick, and asserts its consequence: no entity
ever has two RUNNING tasks and no processor is sent twice.

- [ ] **Step 1: Write the test**

`e2e/parity/postgres/scheduler_mn_contention_test.go`:

```go
package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_ClaimContention: 45 entities, each with two scheduled
// transitions out of Open due at the same instant, on three claiming pnodes —
// one of them gossip-isolated. Every processor is unsafe and takes 200ms.
// Each entity fires exactly once, down exactly one of its two transitions; its
// processor is sent exactly once; no entity ever has two RUNNING tasks; no
// task is left, none FAILED; and more than one pnode held claims.
func TestSchedulerMN_ClaimContention(t *testing.T) {
	t.Parallel()
	s := newSchedMN(t, 3, fixtureutil.LaunchOpts{NodeEnv: func(i int) []string {
		if i == 2 {
			return []string{"CYODA_SEED_NODES="} // a cluster of one: its view holds only itself
		}
		return nil
	}})
	tenant := s.pg.NewTenant(t)
	c := s.client(0, tenant)
	tag := "mc-" + mnShort()
	// A compute client on every pnode: the isolated pnode cannot hand work
	// to a client it does not know of.
	var clients []parity.ComputeClient
	for i := 0; i < 3; i++ {
		clients = append(clients, s.startClient(t, i, tenant, tag, parity.ComputeBehaviourCatalog))
	}
	slow := func() map[string]any {
		p := mnProc("slow-configurable", "SYNC", tag, false, 10000)
		p["config"].(map[string]any)["context"] = `{"sleep_ms":200}`
		return p
	}
	const model = "mn-contention"
	wf := mnWorkflow("mn-contention-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{
			map[string]any{"name": "ToA", "next": "A", "manual": false, "schedule": map[string]any{"delayMs": 2000}, "processors": []any{slow()}},
			map[string]any{"name": "ToB", "next": "B", "manual": false, "schedule": map[string]any{"delayMs": 2000}, "processors": []any{slow()}},
		}},
		"A": map[string]any{},
		"B": map[string]any{},
	})
	first := mnSetup(t, c, model, wf)
	ids := []uuid.UUID{first}
	for i := 1; i < 45; i++ {
		id, err := c.CreateEntity(t, model, 1, mnSample)
		if err != nil {
			t.Fatalf("CreateEntity %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	// Watch the table while the tasks run.
	var mu sync.Mutex
	owners := map[string]bool{}
	doubles := 0
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			var n int
			if err := s.db.QueryRow(ctx, `
				SELECT count(*) FROM (SELECT entity_id FROM scheduled_tasks
				 WHERE tenant_id = $1 AND status = 'RUNNING' GROUP BY entity_id HAVING count(*) > 1) d`,
				tenant.ID).Scan(&n); err == nil && n > 0 {
				mu.Lock()
				doubles += n
				mu.Unlock()
			}
			rows, err := s.db.Query(ctx, `SELECT DISTINCT claim_owner::text FROM scheduled_tasks WHERE tenant_id = $1 AND status = 'RUNNING'`, tenant.ID)
			if err != nil {
				continue
			}
			for rows.Next() {
				var o string
				if rows.Scan(&o) == nil {
					mu.Lock()
					owners[o] = true
					mu.Unlock()
				}
			}
			rows.Close()
		}
	}()

	mnAwait(t, 90*time.Second, "every entity settled", func() bool {
		for _, id := range ids {
			got, err := c.GetEntity(t, id)
			if err != nil || (got.Meta.State != "A" && got.Meta.State != "B") {
				return false
			}
		}
		return true
	})
	close(stop)
	wg.Wait()

	if doubles != 0 {
		t.Errorf("an entity had two RUNNING tasks %d times", doubles)
	}
	if len(owners) < 2 {
		t.Errorf("claims came from %d pnode(s); the scenario needs contention between pnodes", len(owners))
	}
	for _, id := range ids {
		sent := 0
		for _, cl := range clients {
			sent += mnReceived(t, cl, id)
		}
		if sent != 1 {
			t.Errorf("entity %s: processor sent %d times; want 1", id, sent)
		}
		if n := mnCountEvents(t, c, id, "SCHEDULED_TRANSITION_FIRE"); n != 1 {
			t.Errorf("entity %s: %d fires; want 1", id, n)
		}
	}
	var left, failed int
	if err := s.db.QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE status = 'FAILED') FROM scheduled_tasks WHERE tenant_id = $1`, tenant.ID).Scan(&left, &failed); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if left != 0 || failed != 0 {
		t.Errorf("%d tasks left (%d FAILED); want none: the fired sibling is removed, the other cancelled", left, failed)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -count=1 -timeout 20m ./e2e/parity/postgres/ -run TestSchedulerMN_ClaimContention`
Expected (merge-base plus T-1): FAIL — with the old coordinator the isolated
pnode scans on its own and some entity's processor is sent twice; before R,
the `claim_owner` column does not exist. After the streams: PASS.

**Teeth.** In BP's `ClaimDue` step 3, drop the repeated claim condition from
the `UPDATE`'s `WHERE` — a row claimed elsewhere after ranking is claimed
again and a processor is sent twice (run the test three times; the window is
narrow, which is why S carries the deterministic case).

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/postgres/scheduler_mn_contention_test.go
git commit -m "test(multinode): contended claims, due siblings, a partial cluster view

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task T-12: multi-node — a frozen owner sends no unsafe processor

**Spec:** §6.3 last paragraph (a frozen VM: "For unsafe work, the mark covers
it (C3). Its commits are fenced"), §5.5 (`MarkUnsafe` before every unsafe
dispatch, fenced), §10.1 C3; §13 rows "`MarkUnsafe` racing `ClaimDue` (C3)"
(M) and — its multi-node form — "a superseded owner sends no unsafe
processor".

**Depends on:** S, BP (`MarkUnsafe` serialised with `ClaimDue`), E, R, T-1, T-6.

**Files:**
- Create: `e2e/parity/postgres/scheduler_mn_frozen_owner_test.go`

**Interfaces:** consumes T-1 (`SignalNode`) and T-6; produces nothing.

**How the race is made real.** The owner is frozen (`SIGSTOP`) while its run
waits on an idempotent processor. Its heartbeat stops; after `STALE_AFTER` a
survivor claims the task (`lostOwners` 1), runs it to completion, and sends
the unsafe second processor. The frozen owner is then resumed (`SIGCONT`):
its run continues where it stopped, reaches the unsafe processor, and its
`MarkUnsafe` meets a claim that is no longer its own (C3). It must not send.

- [ ] **Step 1: Write the test**

`e2e/parity/postgres/scheduler_mn_frozen_owner_test.go`:

```go
package postgres

import (
	"syscall"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// TestSchedulerMN_FrozenOwnerSendsNoUnsafe: pnodes 0-2 claim, pnode 3 hosts
// the clients. The run's first processor (idempotent) is held; the owner is
// frozen; a survivor reclaims, runs both processors and fires. The resumed
// owner sends nothing: the unsafe processor was sent once, the entity fired
// once.
func TestSchedulerMN_FrozenOwnerSendsNoUnsafe(t *testing.T) {
	t.Parallel()
	const host = 3
	s := newSchedMN(t, 4, fixtureutil.LaunchOpts{NodeEnv: hostOnly(host)}, mnLongAnswerEnv)
	tenant := s.pg.NewTenant(t)
	c := s.client(host, tenant)
	tagHold, tagUnsafe := "fh-"+mnShort(), "fu-"+mnShort()
	hold := s.startClient(t, host, tenant, tagHold, parity.ComputeBehaviourHold)
	unsafe := s.startClient(t, host, tenant, tagUnsafe, parity.ComputeBehaviourCatalog)
	id := mnSetup(t, c, "mn-frozen", mnWorkflow("mn-frozen-wf", mnFire(300, 0,
		mnProc("noop", "SYNC", tagHold, true, mnLongAnswer),
		mnProc("noop", "SYNC", tagUnsafe, false, 10000))))

	first := s.awaitTask(t, id, "Fire", 30*time.Second, "running", func(r mnTask, ok bool) bool { return ok && r.Status == "RUNNING" })
	mnAwait(t, 10*time.Second, "the held processor", func() bool { return mnReceived(t, hold, id) == 1 })
	owner := s.ownerNode(t, first.ClaimOwner)
	if owner < 0 || owner == host {
		t.Fatalf("owner %q is not a claiming pnode", first.ClaimOwner)
	}
	if err := s.pg.SignalNode(owner, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP node %d: %v", owner, err)
	}
	t.Cleanup(func() { _ = s.pg.SignalNode(owner, syscall.SIGCONT) })

	re := s.awaitTask(t, id, "Fire", fixtureutil.TunedStaleAfter+40*time.Second, "the reclaim",
		func(r mnTask, ok bool) bool { return ok && r.ClaimOwner != first.ClaimOwner && r.LostOwners == 1 })
	if n := s.ownerNode(t, re.ClaimOwner); n == owner || n < 0 || n == host {
		t.Fatalf("reclaimed by node %d; want another claiming pnode", n)
	}
	mnAwait(t, 10*time.Second, "the reclaimed run's held processor", func() bool { return mnReceived(t, hold, id) == 2 })
	hold.Release(t) // answers both held requests
	mnAwait(t, 30*time.Second, "the fire", func() bool {
		got, err := c.GetEntity(t, id)
		return err == nil && got.Meta.State == "Done"
	})
	if n := mnReceived(t, unsafe, id); n != 1 {
		t.Fatalf("the unsafe processor was sent %d times before the resume; want 1", n)
	}

	if err := s.pg.SignalNode(owner, syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT node %d: %v", owner, err)
	}
	time.Sleep(10 * time.Second) // the resumed run reaches its unsafe processor, or its watchdog fires
	if n := mnReceived(t, unsafe, id); n != 1 {
		t.Errorf("the unsafe processor was sent %d times after the frozen owner resumed; want 1", n)
	}
	if n := mnCountEvents(t, c, id, "SCHEDULED_TRANSITION_FIRE"); n != 1 {
		t.Errorf("%d fires; want 1", n)
	}
	if got, err := c.GetEntity(t, id); err != nil || got.Meta.State != "Done" {
		t.Errorf("entity = %+v (err %v); want Done", got.Meta, err)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -count=1 -timeout 20m ./e2e/parity/postgres/ -run TestSchedulerMN_FrozenOwnerSendsNoUnsafe`
Expected (merge-base plus T-1): FAIL (no incarnation line). After the streams:
PASS in about ninety seconds.

**Teeth.** Two lines together: in E, dispatch when `MarkUnsafe` returns
`ErrStaleClaim`; in R, make the watchdog's timer a no-op. The resumed owner then
reaches its unsafe processor and sends it — `unsafe` counts 2. Either line
alone may not bite: on resume the overdue watchdog and the run race, and the
watchdog alone also stops the send.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/postgres/scheduler_mn_frozen_owner_test.go
git commit -m "test(multinode): a frozen owner sends no unsafe processor

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task T-13: SQLite — a single pnode restarts on the same file

**Spec:** §10.3 "SQLite durability" (marks and liveness survive a restart;
"a restart with a mark set ends FAILED. It is never re-run"), §6.1 (the
restarted process's lost-owner claims wait a stale period); §13 rows
"single-node SQLite restart reclaims its own RUNNING tasks as lost owners" (1)
and "single-node SQLite restart with a mark set → FAILED, never re-run" (1).

**Depends on:** S, BQ (the SQLite migration: task columns, durable marks and
owners), E, R, Q (the query), T-1.

**Files:**
- Create: `e2e/parity/sqlite/scheduler_restart_test.go`

**Interfaces:** consumes T-1 (`LaunchCyodaNode`, `NodeProc`,
`TunedServerEnv`, `TunedStaleAfter`, `ComputeBehaviourHold`) and Q-5
(`ListScheduledTasks`); produces nothing.

- [ ] **Step 1: Write the test**

`e2e/parity/sqlite/scheduler_restart_test.go`:

```go
package sqlite

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// restartTask reads the entity's Fire task through GET /scheduled-tasks.
func restartTask(t *testing.T, c *client.Client, id uuid.UUID) *client.ScheduledTask {
	t.Helper()
	page, err := c.ListScheduledTasks(t, url.Values{"entityId": {id.String()}})
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	for i := range page.Items {
		if page.Items[i].Transition == "Fire" {
			return &page.Items[i]
		}
	}
	return nil
}

func awaitRestartTask(t *testing.T, c *client.Client, id uuid.UUID, within time.Duration, what string, cond func(*client.ScheduledTask) bool) *client.ScheduledTask {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if task := restartTask(t, c, id); cond(task) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task of %s: %s not seen within %s; last %+v", id, what, within, restartTask(t, c, id))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSchedulerRestart_ReclaimsOwnRunningTasks: one SQLite pnode runs two
// tasks whose processors stall — R1 idempotent, R2 unsafe (a mark is set).
// The process is killed and started again on the same file. The new process
// is a new incarnation; after its own heartbeats have run a stale period it
// claims both as lost owners. R2's mark survived the restart: FAILED
// UNSAFE_WORK_NOT_COMPLETED, never sent again. R1 runs again and fires.
func TestSchedulerRestart_ReclaimsOwnRunningTasks(t *testing.T) {
	t.Parallel()
	cyodaBin, err := fixtureutil.BuildCyodaBinary()
	if err != nil {
		t.Fatalf("build cyoda: %v", err)
	}
	computeBin, err := fixtureutil.BuildComputeBinary()
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	ks, err := fixtureutil.GenerateJWTKeySet()
	if err != nil {
		t.Fatalf("key set: %v", err)
	}
	env := append([]string{
		"CYODA_STORAGE_BACKEND=sqlite",
		"CYODA_SQLITE_PATH=" + filepath.Join(t.TempDir(), "restart.db"),
		"CYODA_SQLITE_AUTO_MIGRATE=true",
		"CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS=300000",
	}, fixtureutil.TunedServerEnv()...)

	first, err := fixtureutil.LaunchCyodaNode(cyodaBin, ks, env, 0)
	if err != nil {
		t.Fatalf("launch the first process: %v", err)
	}
	t.Cleanup(first.Kill)
	tenant := fixtureutil.MintTenantJWT(t, ks)
	start := func(node *fixtureutil.NodeProc, tag, behaviour string) parity.ComputeClient {
		cc := fixtureutil.StartComputeClientForFixture(t, ks, computeBin, node.GRPCEndpoint, node.BaseURL,
			parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
		t.Cleanup(cc.Stop)
		return cc
	}
	tag1, tag2 := "r1-"+uuid.NewString()[:6], "r2-"+uuid.NewString()[:6]
	stall1 := start(first, tag1, parity.ComputeBehaviourStall)
	stall2 := start(first, tag2, parity.ComputeBehaviourStall)

	c := client.NewClient(first.BaseURL, tenant.Token)
	setup := func(model, tag string, idempotent bool) uuid.UUID {
		wf := restartWorkflow(model+"-wf", tag, idempotent)
		if err := c.ImportModel(t, model, 1, `{"k":1}`); err != nil {
			t.Fatalf("ImportModel: %v", err)
		}
		if err := c.LockModel(t, model, 1); err != nil {
			t.Fatalf("LockModel: %v", err)
		}
		if err := c.ImportWorkflow(t, model, 1, wf); err != nil {
			t.Fatalf("ImportWorkflow: %v", err)
		}
		id, err := c.CreateEntity(t, model, 1, `{"k":1}`)
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		return id
	}
	r1 := setup("sq-restart-1", tag1, true)
	r2 := setup("sq-restart-2", tag2, false)
	for _, id := range []uuid.UUID{r1, r2} {
		awaitRestartTask(t, c, id, 15*time.Second, "running", func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "RUNNING" })
	}
	parity.AwaitReceived(t, stall1, 1, 10*time.Second)
	parity.AwaitReceived(t, stall2, 1, 10*time.Second)

	first.Kill()
	second, err := fixtureutil.LaunchCyodaNode(cyodaBin, ks, env, 0)
	if err != nil {
		t.Fatalf("launch the second process: %v", err)
	}
	t.Cleanup(second.Kill)
	restartAt := time.Now()
	c2 := client.NewClient(second.BaseURL, tenant.Token)
	hold1 := start(second, tag1, parity.ComputeBehaviourHold)
	fresh2 := start(second, tag2, parity.ComputeBehaviourCatalog)

	within := fixtureutil.TunedStaleAfter + 30*time.Second
	failed := awaitRestartTask(t, c2, r2, within, "R2 FAILED", func(tk *client.ScheduledTask) bool { return tk != nil && tk.Status == "FAILED" })
	if since := time.Since(restartAt); since < fixtureutil.TunedStaleAfter-2*time.Second {
		t.Errorf("R2 was decided %s after the restart; the new process may not claim a lost owner's task before a stale period of its own heartbeats", since)
	}
	if failed.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || failed.LostOwners != 1 {
		t.Errorf("R2 = %+v; want FAILED UNSAFE_WORK_NOT_COMPLETED, lostOwners 1", *failed)
	}

	awaitRestartTask(t, c2, r1, within, "R1 reclaimed", func(tk *client.ScheduledTask) bool {
		return tk != nil && tk.Status == "RUNNING" && tk.LostOwners == 1
	})
	parity.AwaitReceived(t, hold1, 1, 10*time.Second)
	hold1.Release(t)
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := c2.GetEntity(t, r1)
		if err == nil && got.Meta.State == "Done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("R1 did not fire after its reclaim; last %+v err %v", got.Meta, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(3 * fixtureutil.TunedRetryDelay)
	if n := len(stall2.Received(t)); n != 1 {
		t.Errorf("R2's unsafe processor reached the first process's client %d times; want 1", n)
	}
	if n := len(fresh2.Received(t)); n != 0 {
		t.Errorf("R2's unsafe processor was sent %d times after the restart; want 0", n)
	}
	if n := len(stall1.Received(t)); n != 1 {
		t.Errorf("R1's processor reached the first process's client %d times; want 1", n)
	}
}

// restartWorkflow is Open -[Fire, 300ms]-> Done with one processor on tag.
func restartWorkflow(wfName, tag string, idempotent bool) string {
	return `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"` + wfName + `","initialState":"Open","active":true,"states":{` +
		`"Open":{"transitions":[{"name":"Fire","next":"Done","manual":false,"schedule":{"delayMs":300},"processors":[` +
		`{"type":"calculator","name":"noop","executionMode":"SYNC","config":{"attachEntity":true,"calculationNodesTags":"` + tag +
		`","retryPolicy":"NONE","responseTimeoutMs":240000,"idempotent":` + boolJSON(idempotent) + `}}]}]},"Done":{}}}]}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
```

- [ ] **Step 2: Run it**

Run: `go test -count=1 ./e2e/parity/sqlite/ -run TestSchedulerRestart_ReclaimsOwnRunningTasks`
Expected (merge-base plus T-1): FAIL — the query endpoint answers 404. With
the streams but marks kept in memory: R2 runs again and `fresh2` receives a
request. After the streams: PASS in about seventy seconds.

**Teeth.** In BQ, create `scheduled_task_marks` as a `TEMP` table — R2's
mark is gone after the restart; it runs again.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/sqlite/scheduler_restart_test.go
git commit -m "test(sqlite): a scheduler restart reclaims its own tasks; a mark survives it

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
## Order and cost

- T-1 and T-3 land after Q-5 and R-10, before R-11 (README "Order of work",
  C-P5); T-1 Steps 1–6 need no other stream. T-2 is executed inside R-11, in
  its commit. T-4 … T-13 after R, W and Q, in any order; T-6 before T-8,
  T-10, T-11, T-12 (it creates the M helpers).
- Wall time added to `make test` (the parity packages run there,
  `Makefile:108, 122-127`): the five M tests and the SQLite restart are
  `t.Parallel()` and each waits one or two stale periods; together they add
  about two and a half minutes of wall time and run 6 + 4 + 4 + 3 + 3
  cyoda-go processes and one SQLite process at once (Open point 7).

## §13 cells owned by T, row by row

| Row (spec §13) | Layer | Test |
|---|---|---|
| fired on time | E | `TestE2E_ScheduledTransition_FiresThroughHTTPStack` (T-2) |
| fired on time | P | `ScheduledTransition_FiresOnTime` (T-2 adds "task removed") |
| fired after one safe failure (compute node down, then up) | E | `TestSchedRun_NoComputeNodeThenFires` (T-4) |
| fired after one safe failure (compute node down, then up) | P | `ScheduledTransition_NoComputeNodeThenFires` (T-3) |
| self-loop fires and re-arms the same id as a new life | E | `TestSchedRun_SelfLoopReArmsNewLife` (T-4) |
| self-loop fires and re-arms the same id as a new life | P | `ScheduledTransition_SelfLoopReArmsNewLife` (T-3) |
| declined | E | `TestSchedRun_Declined` (T-4) |
| declined | P | `ScheduledTransition_DeclineCriterionFalse` (T-2 adds "task removed") |
| expired, late on the first attempt | E | `TestScheduledFunction_ExpiryElapsedBeforeScan_ExpiresNoFire` (T-2) |
| expired, late on the first attempt | P | `ScheduledFunction_ExpiryElapsedExpiresNoFire` (T-2) |
| criterion error → WAITING, attempts 1, error recorded | E | `TestSchedRun_CriterionErrorRetried` (T-4) |
| criterion error → WAITING, attempts 1, error recorded | P | `ScheduledTransition_CriterionErrorRetried` (T-3) |
| no compute node → `NotHandedOff`, mark cleared, WAITING | E | `TestSchedRun_NoComputeNodeThenFires` (T-4) |
| no compute node → `NotHandedOff`, mark cleared, WAITING | P | `ScheduledTransition_NoComputeNodeThenFires` (T-3) |
| idempotent processor fails → WAITING | E | `TestSchedRun_IdempotentFailureRetried` (T-4) |
| idempotent processor fails → WAITING | P | `ScheduledTransition_IdempotentFailureRetried` (T-3) |
| late after failed attempts → FAILED | E | `TestSchedRun_LateAfterFailedAttemptsFails` (T-4) |
| late after failed attempts → FAILED | P | `ScheduledTransition_LateAfterFailedAttemptsFails` (T-3) |
| unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`, never re-run | E | `TestSchedRun_UnsafeFailureFails` (T-4) |
| unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`, never re-run | P | `ScheduledTransition_UnsafeFailureFails` (T-3) |
| a later step fails after an unsafe hand-off → FAILED | E | `TestSchedRun_LaterStepFailsAfterUnsafeHandOff` (T-4) |
| a later step fails after an unsafe hand-off → FAILED | P | `ScheduledTransition_LaterStepFailsAfterUnsafeHandOff` (T-3) |
| failure after a successful unsafe dispatch in the same step → FAILED | E | `TestSchedRun_FailureAfterUnsafeDispatchSameStep` (T-4) |
| unsafe `ASYNC_NEW_TX` fails, the run commits → completed | E | `TestSchedRun_UnsafeAsyncNewTxFailureStillCompletes` (T-4) |
| CBD on the fired transition, later failure, all idempotent → retried from TX_pre | E | `TestSchedRun_CBDOnFiredTransitionRetriedFromTXPre` (T-5) |
| CBD in a cascade step, later failure → FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | E | `TestSchedRun_CBDInCascadeStepThenFailureFails` (T-5) |
| cascade looping back into the source state with a CBD step sets `PartialCommit` | E | `TestSchedRun_CascadeLoopBackWithCBDSetsPartialCommit` (T-5) |
| owner killed after a cascade-step commit → next claim FAILED `STOPPED_AFTER_PARTIAL_COMMIT` | M | `TestSchedulerMN_LostOwners`, task D (T-10) |
| cancellation after TX_pre stops the run at the next step | E | `TestSchedRun_CancelAfterTXPreStopsAtNextStep` (T-5) |
| joined callback writes the fired entity, no unsafe processor follows | E | `TestSchedRun_JoinedCallbackWritesFiredEntity_OrdinaryOutcome` (T-5) |
| joined callback writes the fired entity, then an unsafe processor → `ErrTaskBusy` | E | `TestSchedRun_JoinedCallbackThenUnsafe_TaskBusySafeFailure` (T-5) |
| joined callback writes the fired entity in a segmented run → stamp refused | E | `TestSchedRun_JoinedCallbackInSegmentedRun_StampRefused` (T-5) |
| joined callback deletes the fired entity → the run commits | E | `TestSchedRun_JoinedCallbackDeletesFiredEntity_RunCommits` (T-5) |
| `SCHEDULED_TRANSITION_FAIL` recorded with its reason | E | `TestSchedRun_UnsafeFailureFails`, `…LateAfterFailedAttemptsFails` (T-4), `TestSchedRun_CBDInCascadeStepThenFailureFails` (T-5) |
| `SCHEDULED_TRANSITION_FAIL` recorded with its reason | P | `ScheduledTransition_UnsafeFailureFails`, `…LateAfterFailedAttemptsFails` (T-3) |
| `lastError` of a non-sentinel store error is "internal error [ticket]" only | E | `TestSchedRun_LastErrorText/NonSentinelStoreErrorIsTicketOnly` (T-4) |
| `lastError` of `MemberFailed`, a callout timeout, an Operational `AppError`, `NO_COMPUTE_MEMBER_FOR_TAG` | E | `TestSchedRun_LastErrorText/{MemberFailedMessage,CalloutTimeout,OperationalAppError,NoComputeMember}` (T-4) |
| `lastError` for a cancelled run and for a conflict: fixed texts | E | `TestSchedRun_CancelAfterTXPreStopsAtNextStep` (T-5), `TestSchedRun_LastErrorText/Conflict` (T-4); "WARN, no ticket" waived in E (Open point 9) |
| fire-time CANCEL: transition no longer scheduled; entity with no transaction id | E | `TestSchedRun_FireTimeCancel/{NotScheduledInSelectedWorkflow,NoTransactionID}` (T-4) |
| fire-time CANCEL: transition no longer scheduled | P | `ScheduledTransition_FireTimeCancelNotScheduled` (T-3); "no transaction id" waived in P (Open point 10) |
| concurrent `ClaimDue` calls get disjoint sets | M | `TestSchedulerMN_ClaimContention` (T-11) |
| two due siblings, two pnodes at once | M | `TestSchedulerMN_ClaimContention` (T-11) |
| contended claim loop: claimed elsewhere between ranking and locking, never re-claimed | M | `TestSchedulerMN_ClaimContention` (T-11) |
| a run longer than 3 × heartbeat interval is not claimed elsewhere | M | `TestSchedulerMN_LostOwners`, the pre-kill window (T-10) |
| owner killed, no mark → claimed after `STALE_AFTER`, `lostOwners` 1, fires | M | `TestSchedulerMN_LostOwners`, task A (T-10) |
| owner killed with a mark → FAILED; the processor was sent once | M | `TestSchedulerMN_LostOwners`, task B (T-10) |
| owner killed, short `timeoutMs` → FAILED `EXPIRED_AFTER_FAILED_ATTEMPTS` | M | `TestSchedulerMN_LostOwners`, task C (T-10) |
| single-node SQLite restart reclaims its own RUNNING tasks as lost owners | 1 | `TestSchedulerRestart_ReclaimsOwnRunningTasks`, R1 (T-13) |
| single-node SQLite restart with a mark set → FAILED, never re-run | 1 | `TestSchedulerRestart_ReclaimsOwnRunningTasks`, R2 (T-13) |
| a liveness record swept during a long outage is recreated by the next heartbeat | M | `TestSchedulerMN_DatabaseOutageLongerThanStaleAfter` (T-6) |
| database outage longer than `STALE_AFTER` → no lost-owner claims before a full stale period | M | `TestSchedulerMN_DatabaseOutageLongerThanStaleAfter` (T-6) |
| after a re-arm, every fenced write of the old life is refused | E | `TestSchedFence_ReArmedLifeRefusesOldRun`, `TestSchedPool_LockTimeoutOnTaskRowLock` (T-7) |
| a reclaimed or re-armed task makes the old run's commit fail (C1) | E | `TestSchedFence_ReArmedLifeRefusesOldRun`, `TestSchedFence_ReclaimedTaskRefusesOldRun/FinalCommit` (T-7) |
| a replaced owner's segment commit is refused by its stamp | E | `TestSchedFence_ReclaimedTaskRefusesOldRun/SegmentStamp` (T-7) |
| `MarkUnsafe` racing `ClaimDue` (C3) | M | `TestSchedulerMN_FrozenOwnerSendsNoUnsafe` (T-12) |
| a superseded owner sends no unsafe processor | E | `TestSchedFence_SupersededOwnerSendsNoUnsafe` (T-7); also on a cluster in T-12 |
| heartbeats are not starved with every main-pool connection busy (C4) | E | `TestSchedPool_HeartbeatNotStarvedByMainPool` (T-7) |
| async-search heartbeats and claims run on the scheduler pool and are not starved | E | `TestSchedPool_AsyncSearchReclaimNotStarved` (T-7) |
| a scheduler-pool statement blocked on a task-row lock gives up after `lock_timeout` | E | `TestSchedPool_LockTimeoutOnTaskRowLock` (T-7) |
| at most `MAX_RUNS` runs; a freed slot triggers an immediate claim | E | `TestSchedLimits_MaxRunsAndFreedSlotClaimsAtOnce` (T-7) |
| an empty cluster view has no effect | M | `TestSchedulerMN_ClaimContention`, the gossip-isolated pnode (T-11) |
| runs finish within the drain; streams stay open | E | `TestSchedShutdown_RunFinishesWithinDrainStreamsOpen` (T-8) |
| run cut after the drain, nothing handed off → WAITING, uncounted, claimed at once elsewhere | M | `TestSchedulerMN_ShutdownDrain`, T1 (T-8) |
| an unsafe callout in flight at shutdown is not cut; the run continues and commits | E | `TestSchedShutdown_UnsafeInFlightNotCut` (T-8) |
| an unsafe callout in flight at shutdown is not cut; the run continues and commits | M | `TestSchedulerMN_ShutdownDrain`, T2 (T-8) |
| a run with no unsafe callout in flight, cut after the drain, unsafe work handed off earlier → FAILED | E | `TestSchedShutdown_CutAfterEarlierUnsafeHandOffFails` (T-8) |
| FAILED task re-armed by an update in the state (assigned by the lead) | E | `TestSchedRun_FailedTaskUnderEntityWrites/ReArmedByUpdateInTheState` (T-4) |
| FAILED task re-armed by an update in the state | P | `ScheduledTransition_FailedTaskReArmedByUpdate` (T-3) |
| FAILED task cancelled when the entity leaves the state (assigned by the lead) | E | `TestSchedRun_FailedTaskUnderEntityWrites/CancelledWhenEntityLeaves` (T-4) |
| FAILED task cancelled when the entity leaves the state | P | `ScheduledTransition_FailedTaskCancelledWhenEntityLeaves` (T-3) |
| a task whose transition is no longer scheduled is removed at the next write (W's proposal, Open point 6) | E | `TestSchedRun_FailedTaskUnderEntityWrites/NoLongerScheduledRemovedAtNextWrite` (T-4) |
| a task whose transition is no longer scheduled is removed at the next write | P | `ScheduledTransition_NoLongerScheduledRemovedAtNextWrite` (T-3) |
| `GET /scheduled-tasks` 200, a FAILED item with its reason, error, times and attempts (assigned by the lead) | P | `ScheduledTransition_UnsafeFailureFails` (T-3) |

Not T's (other streams): every U and S cell; the "Entity writes and workflow
import" rows other than the three above (W); the "`GET /scheduled-tasks`"
rows other than the P FAILED item (Q).

README Review Focus: 2 → T-9 (`TestSchedRace_ClientWritesAgainstLiveScheduler`)
with W-2, W-3; 4 → T-6 with R-5; 5 → T-8 with R-7.

## Stream interface summary

**Produced** — test code only; no production code consumes it.
- `e2e/parity/fixtureutil`: `TunedHeartbeatInterval`, `TunedStaleAfter`,
  `TunedRetryDelay`, `TunedRetryDelayMax`; the scheduler timing in
  `TunedServerEnv` / `TunedClusterEnv` (scan 50 ms also for clusters);
  `LaunchOpts.NodeEnv`; `ClusterLaunchResult.SignalNode`, `.AwaitNodeExit`;
  `NodeProc`, `LaunchCyodaNode` (the single-node launch path now goes through
  it); `IncarnationFromLog`.
- `e2e/parity/postgres`: `MustSetupMultiNodeWithOpts`; `(*pgMultiNode)
  SignalNode, AwaitNodeExit, PauseDatabase, UnpauseDatabase, Incarnation`;
  the multi-node container's `max_connections=400`; test helpers `schedMN`,
  `newSchedMN`, `hostOnly`, `mnTask`, `mnWorkflow`, `mnProc`, `mnFire`,
  `mnSetup`, `mnReceived`, `mnCountEvents`, `mnAwait`, `mnShort`,
  `mnLongAnswer`, `mnLongAnswerEnv`.
- `e2e/parity`: `ComputeBehaviourHold`; `cmd/compute-test-client` behaviour
  `hold` (answers from the catalog on `/release`).
- `e2e/parity/scheduledtransition`: eleven registered scenarios (T-3) and the
  helpers `taskOf`, `awaitTask`, `ownWorkflow`, `ownProc`, `fireToDone`,
  `receivedFor`, `countEvents`, `failEventData`.
- `internal/e2e`: `schedDB`, `newSchedDB`, `schedulerTuning`,
  `newSchedulerHarness`, `newSchedulerCallbackHarness`, `newStackOn`,
  `taskRow`, `schedEvents`, `schedDoc`, `sProc`, `fireOpenToDone`,
  `scriptHoldFirst`, `holdScript`, `failEvent`, `uniq`, `firstAttempt`,
  `awaitFailed`, `createOpen`, `requireState`, `mustTask`, `injectReclaim`,
  `holdMainPool`, `poolProbe`, `requirePoolExhausted`, `shutdownAsync`;
  `newCalloutHarness` now starts with the scheduler off.

**Consumed**
- R: `app.SchedulerConfig{Enabled, ScanInterval, MaxRuns, MaxRunsPerTenant,
  HeartbeatInterval, StaleAfter, MaxLostOwners, RetryDelay, RetryDelayMax,
  ShutdownDrain}`; the env names of spec §11; the INFO line
  `scheduler started` with `incarnation=<uuid>` (R-6, README C-R2); `App.Shutdown`
  runs `Drain` and is safe to call twice (Open point 4); the `run.go` signal
  path runs the drain before the servers (V4); `recordedError`'s fixed texts
  `CANCELLED: the run was stopped by the scheduler` and `CONFLICT: a
  concurrent write changed the entity or its task`.
- BP: the columns of spec §10.2 (`status, arm_token, attempts, lost_owners,
  last_error, failure_reason, partial_commit, claim_token, claim_owner,
  model_name`), `scheduled_task_marks (task_id, arm_token, claim_token)`,
  `scheduler_owners (owner, heartbeat_at)`; `CYODA_POSTGRES_SCHEDULER_CONNS`
  (default 10); `lock_timeout` 2 s on the scheduler pool.
- BQ: the SQLite migration with durable marks and owners.
- Q (Q-5): `(*client.Client).ListScheduledTasks`, `client.ScheduledTask`,
  `client.ScheduledTaskArmedBy`, `client.ScheduledTaskPage`; the endpoint on
  every backend.
- E, K, S, W: behaviour through the doors. The named reverts touch R's
  `decideBookkeeping`, `recordedError`, claim loop, watchdog and `Drain`; E's
  §5.1 step order, stamp, `MarkUnsafe` call; K's `NotHandedOff` attachment and
  `HandedOff` bit; BP's `ClaimDue` step 3, `Heartbeat`, task-row writes,
  `classifySQLState`; BQ's marks table; BM's `ReconcileForEntity`.
- #254 harness: `newCalloutHarness`, `AttachCnode`, `cnodeSpec`, scripts and
  replies, `closeOnce`, `parity.StartComputeClientOrSkip`, `AwaitReceived`,
  `ComputeClient{Received, Release, Stop}`.

## Open points

1. **Closed (README C-R2).** R-6's `Start` logs
   `slog.Info("scheduler started", "pkg", "scheduler", "incarnation", id.String())`
   with a unit test (`TestService_StartLogsItsIncarnation`); M tests map a
   `claim_owner` to its pnode by it.
2. **Q's parity client is used as is.** T defines no `ListScheduledTasks`.
   T-3 and T-13 therefore land after Q-5.
3. **Closed (README C-T1, C-P5).** T-2 is executed inside R-11, in one
   commit; T-1 and T-3 land before R-11. No scheduler runs on the shared e2e
   database.
4. **`App.Shutdown` twice.** The E shutdown tests call `h.app.Shutdown()`, and
   the harness cleanup calls it again (`callback_harness_test.go:251`). R's
   `Stop` is idempotent by binding (`interfaces.md` "Scheduler"); `Shutdown`
   as a whole must be too (`stopSearchReaperLoop` already is,
   `app/app.go:978-988`; `searchPool.Drain` must be checked).
5. **The CONFLICT text case depends on how the run sees a tx aborted inside a
   joined callback.** The callback's `UPDATE` of F fails with 40001 and
   PostgreSQL aborts the run's transaction; the run's next statement then
   gets 25P02. `Commit` maps 25P02 to `spi.ErrConflict`
   (`plugins/postgres/transaction_manager.go:253-267`), but the engine's
   final `Save` runs first and returns the 25P02 unmarked. If E records that
   as "internal error [ticket]", the test fails — correctly: the cause is a
   conflict. The fix belongs in E (carry the abort cause), not in the test.
6. **Rows W proposes for T.** Besides the two FAILED-task rows the lead
   assigned, W-writes.md Open point 5 proposes T for the E and P cells of "a
   task whose transition is no longer scheduled is removed at the next
   write". T plans them (T-3, T-4). The U cells of all three rows are marked
   "— (T)" in W's matrix; T plans no U tests, so the lead should assign them
   (stream E owns reconcile and its units).
7. **Cost in `make test`.** The M and 1 tests wait one or two stale periods
   (53 s each, the floor of §11). Run in parallel they add about two and a
   half minutes and 20 cyoda-go processes at peak. Alternatives, if CI cannot
   carry that: fewer pnodes in T-10 by splitting it (more wall time), or a
   longer tier. I recommend keeping them as planned.
8. **"The other claims nothing that tick"** is a property of one `ClaimDue`
   call. M observes its consequences (no entity with two RUNNING tasks, one
   send, one fire); the S case is the exact check.
9. **Waiver, row "`lastError` for a cancelled run and for a conflict: fixed
   texts, WARN, no ticket", E cell, the log half:** the log level and the
   absence of a ticket are asserted by R's unit test of `recordedError`
   (`warnOnly`); an e2e assertion on slog output would need a test hook.
10. **Waiver, row "fire-time CANCEL", P cell, the "no transaction id" half:**
    no API writes an entity without a transaction id; the state exists only
    as legacy data, which the API-only parity harness cannot create
    (`e2e/parity/fixture.go:18`, "There is no storage handle"). U and E cover it.
11. **Redundant `cfg.Scheduler.Enabled = false` lines.** With the new default
    of `newCalloutHarness`, the explicit lines in
    `callout_handover_lost_test.go:343`, `lookup_storage_failure_e2e_test.go:58`,
    `storage_ceilings_e2e_test.go:83, 252, 323, 466, 773`,
    `torn_connection_e2e_test.go:147` and `tx_lifecycle_e2e_test.go:539`
    change nothing. Each carries a comment on why that test needs no
    scheduler; T-1 leaves them. If the lead prefers one place, T-1 deletes
    them and their comments.
