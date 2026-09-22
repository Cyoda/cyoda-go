# Stream C — configuration, SPI, workflow import, schema version, OpenAPI

Spec sections: §9, §10; §13 rows listed in the coverage table at the end; §14
items named per task. Evidence base: research §7, §8.

## Facts settled by reading the code (used by the tasks below)

1. **Where the settings live.** `app.Config` (`app/config.go:19`) embeds
   `cluster.Config` (`internal/cluster/config.go:5`) as `Cluster`. The three
   existing dispatch settings are fields of `cluster.Config`
   (`DispatchWaitTimeout`, `DispatchForwardTimeout`, `TxTokenTTL`), populated at
   `app/config.go:411-416`. Placement rule used here, by env-var name:
   `CYODA_DISPATCH_*` → `cluster.Config.Dispatch*`; `CYODA_CALLOUT_*` and
   `CYODA_RETRY_*` → a new `app.CalloutConfig` on `Config.Callout`.
2. **The four guards** (R§7) run in one direction each:
   `TestConfig_EnvVarCoverage` (`cmd/cyoda/help/help_test.go:489`) and
   `TestConfigAll_Complete` (`config_registry_test.go:73`) scan non-test Go
   source under `cmd app plugins internal` for every `CYODA_[A-Z_]+` token —
   **comments and error strings included** — and require it in
   `content/config/**/*.md` and in `rootConfigVars`.
   `TestRootConfigVars_MatchDefaults` (`app/config_registry_binding_test.go:177`)
   requires a `defaultFor` entry per registry row and equality with
   `DefaultConfig()`. `TestRootConfigVars_WellFormed` restricts `Topic` to the
   eight root topics. Consequence: never write a prefix such as
   `CYODA_CALLOUT_` in a Go comment (the coverage guard would demand a doc for
   it), and a retired name left anywhere in Go source turns the guards red —
   which is the exit check for C-11.
3. **`Config.Validate()` is called by `app.New`** (`app/app.go:98`), so every
   in-process embedder is covered. The only `Config{...}` literal that does not
   come from `DefaultConfig()` is `validSearchConfig()` in
   `app/config_validate_test.go:11`; it must carry valid values for every newly
   validated field. `cmd/cyoda/main.go:86-113` repeats each validator by name so
   the startup diagnostic names the area; those calls have no test today
   (`main()` exits the process) and none is added — recorded as a TDD waiver in
   C-1/C-2: the invariant is tested on `Config.Validate()`, the `main.go` lines
   are review-only.
4. **Workflow import is HTTP only.** `workflow.Handler.ImportEntityModelWorkflow`
   (`internal/domain/workflow/handler.go:168`) is the single door; there is no
   gRPC import. The G column is therefore empty for the import rows, as in §13.
5. **How the bound reaches validation.** `workflow.New(factory, engine)`
   (`handler.go:63`) has exactly five callers: `app/app.go:650`,
   `internal/grpc/scheduled_transition_test.go:48`,
   `internal/grpc/scheduled_function_rpc_test.go:80`,
   `internal/domain/workflow/scenarios_test.go:506, 663`. The bound becomes a
   third constructor parameter. `validateImportRequest` /
   `validateWorkflowStructure` keep their signatures (≈110 test call sites); the
   bound is applied by a separate pass, `validateCalloutLimits`, called by the
   handler right after `validateImportRequest` — the same shape as
   `validateAndNormalizeAnnotations`.
6. **Where `responseTimeoutMs` is parsed.** Processor: `spi.ProcessorConfig`
   (typed). Function: `spi.ScheduleFunction` (typed). Criterion: never at
   import — the criterion is `json.RawMessage`; the only parse is an ad-hoc
   struct inside `ProcessorDispatcher.DispatchCriteria`
   (`internal/grpc/dispatch.go:278-293`), which reads the `function` member
   without looking at `type`. `predicate.ParseCondition` returns an empty
   `*predicate.FunctionCondition{}` for `"type":"function"`
   (`cyoda-go-spi/predicate/parse.go:43`), and the engine dispatches a function
   criterion only when it is the **whole** criterion (`engine.go:1006`); one
   nested in a `group` fails evaluation. Import validation therefore inspects
   top-level function criteria only, at the two places a criterion lives:
   `WorkflowDefinition.Criterion` and `TransitionDefinition.Criterion`.
7. **Schema-version ceremony in practice.** 284 fixtures are stamped `"1.1"`
   and stay valid under dual-shape retention; the 1.3→1.4 bump (`0ee93612`)
   touched only: `schemaversion.go`, `default_workflow.json`,
   `internal/e2e/workflow_schema_version_test.go`, `workflows/schema-version.md`,
   `workflows.md` (three literals), the versioning doc, CHANGELOG. Step 7 of the
   doc ("update every fixture") bites only on retirement. C-8 follows that
   precedent. `api/openapi.yaml` carries no `1.4` literal; `CONTRIBUTING.md`'s
   hit is `govulncheck@v1.1.4`.
8. **SPI state.** Local `cyoda-go-spi` is clean on `main` at `1ab57a6` =
   `origin/main` = the pseudo-version pinned in all four `go.mod`s
   (`v0.8.5-0.20260918004138-1ab57a60382c`). `GOWORK=off go build ./...` is
   green at the base of this branch. `go.work` is tracked and holds `.` plus the
   three plugins.
9. **Readers of `cfg.Cluster.TxTokenTTL`** (exact, non-test): `app/app.go:441`
   (`NewProcessorDispatcher`) and `app/app.go:554` (`NewClusterDispatcher`).
   Other references: `app/config.go:413-416`, `internal/cluster/config.go:20`,
   `app/config_registry_binding_test.go:122`,
   `cmd/cyoda/help/config_registry.go:80`,
   `cmd/cyoda/help/content/config/cluster.md:29`, `docs/ARCHITECTURE.md:1616`.
   `README.md`, the Helm chart and the fixtures do not mention it. CHANGELOG
   2689, `COMPATIBILITY.md:37` and `docs/release-notes/v0-8-2.md:140` are
   history and stay.

## Working rules for this stream

- Two repositories. Tasks C-3 and the first half of C-10 run in
  `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`; every other command runs
  in the cyoda-go worktree. Each Bash call `cd`s explicitly.
- `go.work` is tracked. From C-3 Step 6 on it carries an **uncommitted**
  `use` line. Never `git add -A` / `git add .`; every commit step below stages
  named paths. Before every commit: `git status --short go.work` must show
  ` M go.work` (modified, unstaged) and `git diff --cached --name-only` must not
  list it.
- Pushing is the session lead's. Steps marked **LEAD** are not executed by the
  task's implementer.
- Between C-3 Step 6 and C-10 the cyoda-go branch builds only through the local
  `go.work` line, so it is not pushable green in that window; that is the
  consequence of pinning once, near the end, and is why C-10 exists.
- The one place `-count=1` appears is a parity run: the fixture builds the
  server in a subprocess the test cache cannot see (CLAUDE.md, "the one
  exception").

---

### Task C-1: Tries and answer-limit settings

**Spec:** §9 rows `CYODA_RETRY_FIXED_NUM_RETRIES`,
`CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`;
§9 "Out-of-range values are startup errors"; §14 (`config/grpc.md`, README,
`DefaultConfig()`, `config_registry.go`, `search.md:201` and the other mentions
of the 30000 default). §13 row "Each new or newly validated setting" (U).

**Files:**
- Modify: `app/config.go` (`Config` struct :19-90; `DefaultConfig` :287;
  `Config.Validate` :719; new `CalloutConfig`, `ValidateCallout`)
- Modify: `cmd/cyoda/main.go` (validator block :86-113)
- Modify: `cmd/cyoda/help/config_registry.go` (`rootConfigVars`, `// --- grpc ---` block)
- Modify: `app/config_registry_binding_test.go` (`defaultFor`)
- Modify: `app/config_validate_test.go` (`validSearchConfig` → `validConfig`)
- Test: `app/config_callout_test.go` (new)
- Docs: `cmd/cyoda/help/content/config/grpc.md`, `README.md`,
  `docs/ARCHITECTURE.md` (gRPC settings table :1598-1604),
  `cmd/cyoda/help/content/search.md:201`,
  `cmd/cyoda/help/content/workflows.md:191`,
  `cmd/cyoda/help/content/config/database.md:95`,
  `cmd/cyoda/help/content/errors/DISPATCH_TIMEOUT.md:25` (the default phrase only)

**Interfaces:**
- Consumes: nothing.
- Produces (read by the streams that build `internal/callout`, rewrite
  `internal/grpc` dispatch and C-7):
  ```go
  // package app
  type CalloutConfig struct {
      FixedNumRetries    int           // CYODA_RETRY_FIXED_NUM_RETRIES
      ResponseTimeout    time.Duration // CYODA_CALLOUT_RESPONSE_TIMEOUT_MS
      ResponseTimeoutMax time.Duration // CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS
  }
  // Config.Callout CalloutConfig
  func ValidateCallout(c CalloutConfig) error
  ```
  `cfg.Callout.FixedNumRetries` (int, ≥ 0), `cfg.Callout.ResponseTimeout`
  (`time.Duration`, ≥ 1 ms, ≤ max), `cfg.Callout.ResponseTimeoutMax`
  (`time.Duration`, ≥ 1 ms). This task does **not** delete
  `defaultResponseTimeoutMs` (`internal/grpc/dispatch.go:33`); the stream that
  rewrites `dispatchCalloutToMember` does, reading `cfg.Callout.ResponseTimeout`
  instead — exit check there: `grep -rn 'defaultResponseTimeoutMs' internal/ → no hits`.

- [ ] **Step 1: Write the failing tests**

`app/config_callout_test.go`:

```go
package app

import (
	"os"
	"strings"
	"testing"
	"time"
)

// unsetEnv clears keys for the duration of the test, restoring them after.
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// validCalloutConfig is the Callout block DefaultConfig yields under an empty
// environment — the baseline each validation case perturbs by one field.
func validCalloutConfig() CalloutConfig {
	return CalloutConfig{
		FixedNumRetries:    3,
		ResponseTimeout:    30 * time.Second,
		ResponseTimeoutMax: 60 * time.Second,
	}
}

func TestDefaultConfig_CalloutTriesAndAnswerLimit(t *testing.T) {
	unsetEnv(t, "CYODA_RETRY_FIXED_NUM_RETRIES",
		"CYODA_CALLOUT_RESPONSE_TIMEOUT_MS", "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS")

	got := DefaultConfig().Callout
	if got.FixedNumRetries != 3 {
		t.Errorf("default FixedNumRetries = %d, want 3", got.FixedNumRetries)
	}
	if got.ResponseTimeout != 30*time.Second {
		t.Errorf("default ResponseTimeout = %s, want 30s", got.ResponseTimeout)
	}
	if got.ResponseTimeoutMax != 60*time.Second {
		t.Errorf("default ResponseTimeoutMax = %s, want 1m0s", got.ResponseTimeoutMax)
	}

	t.Setenv("CYODA_RETRY_FIXED_NUM_RETRIES", "0")
	t.Setenv("CYODA_CALLOUT_RESPONSE_TIMEOUT_MS", "1500")
	t.Setenv("CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS", "45000")
	got = DefaultConfig().Callout
	if got.FixedNumRetries != 0 {
		t.Errorf("FixedNumRetries override = %d, want 0 (0 is a value, not 'unset')", got.FixedNumRetries)
	}
	if got.ResponseTimeout != 1500*time.Millisecond {
		t.Errorf("ResponseTimeout override = %s, want 1.5s", got.ResponseTimeout)
	}
	if got.ResponseTimeoutMax != 45*time.Second {
		t.Errorf("ResponseTimeoutMax override = %s, want 45s", got.ResponseTimeoutMax)
	}
}

func TestValidateCallout_TriesAndAnswerLimit(t *testing.T) {
	if err := ValidateCallout(validCalloutConfig()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	atBound := validCalloutConfig()
	atBound.ResponseTimeout = atBound.ResponseTimeoutMax
	if err := ValidateCallout(atBound); err != nil {
		t.Fatalf("default answer limit equal to the upper bound rejected: %v", err)
	}
	zeroRetries := validCalloutConfig()
	zeroRetries.FixedNumRetries = 0
	if err := ValidateCallout(zeroRetries); err != nil {
		t.Fatalf("0 retries rejected: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*CalloutConfig)
		wantName string
	}{
		{"negative retries", func(c *CalloutConfig) { c.FixedNumRetries = -1 }, "CYODA_RETRY_FIXED_NUM_RETRIES"},
		{"zero answer limit", func(c *CalloutConfig) { c.ResponseTimeout = 0 }, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MS"},
		{"negative answer limit", func(c *CalloutConfig) { c.ResponseTimeout = -time.Second }, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MS"},
		{"zero upper bound", func(c *CalloutConfig) { c.ResponseTimeoutMax = 0 }, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS"},
		{"answer limit above the upper bound", func(c *CalloutConfig) {
			c.ResponseTimeout = c.ResponseTimeoutMax + time.Millisecond
		}, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCalloutConfig()
			tc.mutate(&c)
			err := ValidateCallout(c)
			if err == nil {
				t.Fatalf("ValidateCallout(%+v) = nil, want an error", c)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}
```

`app/config_validate_test.go` — rename `validSearchConfig` → `validConfig` (four
occurrences, this file only), give it the new block, and add one case:

```go
// validConfig is a Config carrying only the fields Config.Validate inspects,
// all set to accepted values — the baseline each case below perturbs by
// exactly one field.
func validConfig() Config {
	return Config{
		SearchAsync:                SearchAsyncConfig{Workers: 8, QueueLen: 256, MaxPerTenant: 8},
		SearchJobHeartbeatInterval: 15 * time.Second,
		SearchJobStaleAfter:        5 * time.Minute,
		SearchJobMaxAttempts:       3,
		GRPC:                       GRPCConfig{KeepAliveInterval: 10, KeepAliveTimeout: 30},
		Callout:                    validCalloutConfig(),
	}
}
```

and in `TestConfig_Validate`'s `cases`:

```go
		{"answer limit above its upper bound", func(c *Config) { c.Callout.ResponseTimeout = 2 * time.Minute }},
```

`app/config_registry_binding_test.go`, `defaultFor`, `// --- grpc ---` block:

```go
		"CYODA_RETRY_FIXED_NUM_RETRIES":         strconv.Itoa(c.Callout.FixedNumRetries),
		"CYODA_CALLOUT_RESPONSE_TIMEOUT_MS":     renderMillis(c.Callout.ResponseTimeout),
		"CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS": renderMillis(c.Callout.ResponseTimeoutMax),
```

Also update `renderMillis`'s comment (`:65-67`): it no longer serves "the three
`CYODA_OIDC_*_TIMEOUT_MS` vars" only — say "every integer-milliseconds var".

- [ ] **Step 2: Run to verify RED**

Run: `go test ./app/ -run 'TestDefaultConfig_CalloutTriesAndAnswerLimit|TestValidateCallout_TriesAndAnswerLimit|TestConfig_Validate|TestRootConfigVars_MatchDefaults'`
Expected: FAIL — build error `undefined: CalloutConfig` / `c.Callout undefined`.

- [ ] **Step 3: Implement**

`app/config.go` — in `Config`, after `Scheduler SchedulerConfig`:

```go
	// Callout frames one compute-node callout: how many tries it gets and how
	// long a pnode waits for one cnode's answer. See CalloutConfig.
	Callout CalloutConfig
```

New type, placed after `SchedulerConfig`:

```go
// CalloutConfig holds the server-side settings of a compute-node callout
// (a processor, criterion or function request).
type CalloutConfig struct {
	// FixedNumRetries is the number of retries after the first try for a
	// callout whose retryPolicy is FIXED or unset; NONE always means one try.
	// CYODA_RETRY_FIXED_NUM_RETRIES, default 3, must be >= 0.
	FixedNumRetries int
	// ResponseTimeout is the answer limit used when the workflow author set no
	// positive responseTimeoutMs on the callout.
	// CYODA_CALLOUT_RESPONSE_TIMEOUT_MS, default 30000, must be >= 1 and no
	// larger than ResponseTimeoutMax.
	ResponseTimeout time.Duration
	// ResponseTimeoutMax is the largest responseTimeoutMs a workflow may carry.
	// Workflow import refuses a larger value; a stored workflow whose value
	// exceeds a bound lowered after import fails its callout rather than
	// being clamped. CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS, default 60000,
	// must be >= 1.
	ResponseTimeoutMax time.Duration
}
```

`DefaultConfig()`, in the returned literal after the `Scheduler:` block:

```go
		Callout: CalloutConfig{
			FixedNumRetries:    envInt("CYODA_RETRY_FIXED_NUM_RETRIES", 3),
			ResponseTimeout:    envMillis("CYODA_CALLOUT_RESPONSE_TIMEOUT_MS", 30*time.Second),
			ResponseTimeoutMax: envMillis("CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS", 60*time.Second),
		},
```

`Config.Validate()` — before `return ValidateHTTP(c.HTTP)`:

```go
	if err := ValidateCallout(c.Callout); err != nil {
		return err
	}
```

New validator, after `ValidateSearchJobMaxAttempts`:

```go
// ValidateCallout rejects callout settings no callout could be run under.
// Config is a QA'd artefact: an out-of-range value is a hard startup error,
// not a clamp. A default answer limit above the upper bound is rejected
// because import would then refuse a workflow that merely spells out the
// default.
func ValidateCallout(c CalloutConfig) error {
	if c.FixedNumRetries < 0 {
		return fmt.Errorf("CYODA_RETRY_FIXED_NUM_RETRIES must be >= 0, got %d", c.FixedNumRetries)
	}
	if c.ResponseTimeout < time.Millisecond {
		return fmt.Errorf("CYODA_CALLOUT_RESPONSE_TIMEOUT_MS must be >= 1, got %d", c.ResponseTimeout.Milliseconds())
	}
	if c.ResponseTimeoutMax < time.Millisecond {
		return fmt.Errorf("CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS must be >= 1, got %d", c.ResponseTimeoutMax.Milliseconds())
	}
	if c.ResponseTimeout > c.ResponseTimeoutMax {
		return fmt.Errorf("CYODA_CALLOUT_RESPONSE_TIMEOUT_MS (%d) must not exceed CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS (%d)",
			c.ResponseTimeout.Milliseconds(), c.ResponseTimeoutMax.Milliseconds())
	}
	return nil
}
```

`cmd/cyoda/main.go`, after the `ValidateSearchJobMaxAttempts` block (TDD waiver:
`main()` exits the process and has no test seam; the invariant is pinned on
`Config.Validate()` above — this call only names the area in the diagnostic):

```go
	if err := app.ValidateCallout(cfg.Callout); err != nil {
		slog.Error("callout config validation failed", "error", err)
		os.Exit(1)
	}
```

`cmd/cyoda/help/config_registry.go`, `// --- grpc ---` block, after
`CYODA_KEEPALIVE_TIMEOUT`:

```go
	{Name: "CYODA_RETRY_FIXED_NUM_RETRIES", Topic: "grpc", Type: "int", Default: "3", Description: "Retries after the first try of a compute-node callout whose retryPolicy is FIXED or unset; the normal number of tries is this plus one. Must be >= 0; startup fails otherwise."},
	{Name: "CYODA_CALLOUT_RESPONSE_TIMEOUT_MS", Topic: "grpc", Type: "int", Default: "30000", Description: "How long a node waits for one compute member's answer when the callout sets no responseTimeoutMs of its own. Must be >= 1 and <= CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS; startup fails otherwise."},
	{Name: "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS", Topic: "grpc", Type: "int", Default: "60000", Description: "Upper bound on responseTimeoutMs for processors, criterion functions and scheduled-transition functions; workflow import refuses a larger value. Must be >= 1; startup fails otherwise."},
```

`cmd/cyoda/help/content/config/grpc.md` — new subsection between "gRPC
listener" and "Compute-node client":

```markdown
### Compute-node callouts

A callout is one processor, criterion or function request sent to a compute
member. These settings apply on a single node and in a cluster alike.

- `CYODA_RETRY_FIXED_NUM_RETRIES` — retries after the first try, for a callout
  whose `retryPolicy` is `FIXED` or unset; `retryPolicy: NONE` always means one
  try. The normal number of tries is this plus one. Must be `>= 0`; startup
  fails otherwise (default: `3`)
- `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` — how long a node waits for one compute
  member's answer when the callout sets no `responseTimeoutMs` of its own.
  Must be `>= 1` and no larger than `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`;
  startup fails otherwise (default: `30000`)
- `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` — upper bound on `responseTimeoutMs`
  for processors, criterion functions and scheduled-transition functions.
  Workflow import refuses a larger value with `400 VALIDATION_FAILED`. The
  bound is a server setting, so a workflow exported from one deployment can be
  refused by another with a lower bound. A stored workflow whose value exceeds
  a bound lowered after it was imported is not clamped: its callout fails,
  naming this setting. Must be `>= 1`; startup fails otherwise
  (default: `60000`)
```

(The paragraph relating these settings to PostgreSQL's idle-in-transaction
ceiling names settings C-2 introduces, so C-2 adds it.)

`README.md` — new section between "Scheduled transitions" and "Where to go
next":

```markdown
## Compute-node callouts

A processor, criterion or function request to a compute member is a *callout*.

| Variable | Default | Description |
|---|---|---|
| `CYODA_RETRY_FIXED_NUM_RETRIES` | `3` | Retries after the first try when `retryPolicy` is `FIXED` or unset. |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` | `30000` | Answer limit when the callout sets no `responseTimeoutMs`. |
| `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` | `60000` | Upper bound on `responseTimeoutMs`; workflow import refuses more. |

See `cyoda help config grpc` and `cyoda help config cluster`.
```

`docs/ARCHITECTURE.md`, gRPC table (`:1598-1604`) — three rows with the same
names, defaults and one-line descriptions as the README table.

The 30000-default sweep — each phrase now names the setting:
- `search.md:201` → `` `function.config.responseTimeoutMs`: int64 (optional; when absent or `0` the server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` applies, default `30000`; bounded by `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`) — how long to wait for the compute member's answer, in milliseconds ``
- `workflows.md:191` → `` `responseTimeoutMs` — int64 — how long to wait for the compute member's answer, in milliseconds; `0` or absent means the server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`; must not exceed `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` ``
  (the present text says "for `SYNC` processor response", which is wrong: the
  limit applies in every execution mode — `dispatch.go:241`)
- `config/database.md:95` → `` `responseTimeoutMs` (default `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, `30s`; at most `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`, `60s`) ``
- `errors/DISPATCH_TIMEOUT.md:25` → replace `default `30000` ms` with
  `` default `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`, `30000` ms ``. Only this phrase;
  the rest of that topic is revised by the stream that owns §8.2.

Exit check: `grep -rn 'default `30000`\|default `30s`)' cmd/cyoda/help/content/ → no hits`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./app/... ./cmd/cyoda/...`
Expected: PASS, including `TestConfig_EnvVarCoverage`, `TestConfigAll_Complete`,
`TestRootConfigVars_MatchDefaults`, `TestRootConfigVars_WellFormed`.

- [ ] **Step 5: Commit**

```
git add app/config.go app/config_callout_test.go app/config_validate_test.go \
  app/config_registry_binding_test.go cmd/cyoda/main.go \
  cmd/cyoda/help/config_registry.go cmd/cyoda/help/content/config/grpc.md \
  cmd/cyoda/help/content/config/database.md cmd/cyoda/help/content/search.md \
  cmd/cyoda/help/content/workflows.md \
  cmd/cyoda/help/content/errors/DISPATCH_TIMEOUT.md README.md docs/ARCHITECTURE.md
git commit -m "feat(config): the number of tries and the answer limit of a callout are settings (#254, #565)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-2: Allowances, connect timeout, and the two dispatch durations validated

**Spec:** §9 rows `CYODA_DISPATCH_WAIT_TIMEOUT`, `CYODA_DISPATCH_CONNECT_TIMEOUT`,
`CYODA_CALLOUT_HANDOVER_ALLOWANCE`, `CYODA_CALLOUT_PASS_ALLOWANCE`,
`CYODA_DISPATCH_FORWARD_TIMEOUT`; §14 (`config/cluster.md`, README,
`config_registry.go`, CHANGELOG). §13 row "Each new or newly validated setting" (U).

**Files:**
- Modify: `internal/cluster/config.go` (new field `DispatchConnectTimeout`)
- Modify: `app/config.go` (`CalloutConfig`, `DefaultConfig`, `Validate`,
  `ValidateCallout`; new `ValidateDispatch`)
- Modify: `cmd/cyoda/main.go`
- Modify: `cmd/cyoda/help/config_registry.go` (`// --- cluster ---` block :68-80)
- Modify: `app/config_registry_binding_test.go`, `app/config_validate_test.go`
- Test: `app/config_callout_test.go`, `app/config_dispatch_test.go` (new)
- Docs: `cmd/cyoda/help/content/config/cluster.md:27-28`,
  `cmd/cyoda/help/content/config/grpc.md` (closing paragraph of the C-1
  subsection), `README.md`, `docs/ARCHITECTURE.md` (cluster table :1606-1619),
  `CHANGELOG.md`

**Interfaces:**
- Consumes: `CalloutConfig`, `validCalloutConfig`, `unsetEnv`, `validConfig` (C-1).
- Produces:
  ```go
  // package app — added to CalloutConfig
  HandoverAllowance time.Duration // CYODA_CALLOUT_HANDOVER_ALLOWANCE, > 0
  PassAllowance     time.Duration // CYODA_CALLOUT_PASS_ALLOWANCE, > 0
  // package cluster — added to Config
  DispatchConnectTimeout time.Duration // CYODA_DISPATCH_CONNECT_TIMEOUT, > 0
  func ValidateDispatch(c cluster.Config) error // package app
  ```
  Unchanged and still read where they are: `cfg.Cluster.DispatchWaitTimeout`
  (`time.Duration`, ≥ 0; the patience, single pnode and cluster; `0` disables
  waiting) and `cfg.Cluster.DispatchForwardTimeout` (`time.Duration`, > 0;
  after the hand-over stream lands its only reader is
  `cluster.NewSchedulerRPCClient`, `app/app.go:592`).

- [ ] **Step 1: Write the failing tests**

Extend `validCalloutConfig` (`app/config_callout_test.go`) with
`HandoverAllowance: 30 * time.Second, PassAllowance: 30 * time.Second`, and add:

```go
func TestDefaultConfig_CalloutAllowances(t *testing.T) {
	unsetEnv(t, "CYODA_CALLOUT_HANDOVER_ALLOWANCE", "CYODA_CALLOUT_PASS_ALLOWANCE")
	got := DefaultConfig().Callout
	if got.HandoverAllowance != 30*time.Second {
		t.Errorf("default HandoverAllowance = %s, want 30s", got.HandoverAllowance)
	}
	if got.PassAllowance != 30*time.Second {
		t.Errorf("default PassAllowance = %s, want 30s", got.PassAllowance)
	}
	t.Setenv("CYODA_CALLOUT_HANDOVER_ALLOWANCE", "10s")
	t.Setenv("CYODA_CALLOUT_PASS_ALLOWANCE", "45s")
	got = DefaultConfig().Callout
	if got.HandoverAllowance != 10*time.Second || got.PassAllowance != 45*time.Second {
		t.Errorf("overrides not bound: %+v", got)
	}
}

func TestValidateCallout_Allowances(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*CalloutConfig)
		wantName string
	}{
		{"zero hand-over allowance", func(c *CalloutConfig) { c.HandoverAllowance = 0 }, "CYODA_CALLOUT_HANDOVER_ALLOWANCE"},
		{"negative hand-over allowance", func(c *CalloutConfig) { c.HandoverAllowance = -time.Second }, "CYODA_CALLOUT_HANDOVER_ALLOWANCE"},
		{"zero pass allowance", func(c *CalloutConfig) { c.PassAllowance = 0 }, "CYODA_CALLOUT_PASS_ALLOWANCE"},
		{"negative pass allowance", func(c *CalloutConfig) { c.PassAllowance = -time.Second }, "CYODA_CALLOUT_PASS_ALLOWANCE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCalloutConfig()
			tc.mutate(&c)
			err := ValidateCallout(c)
			if err == nil {
				t.Fatalf("ValidateCallout(%+v) = nil, want an error", c)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}
```

`app/config_dispatch_test.go`:

```go
package app

import (
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster"
)

func validDispatchConfig() cluster.Config {
	return cluster.Config{
		DispatchWaitTimeout:    5 * time.Second,
		DispatchConnectTimeout: 2 * time.Second,
		DispatchForwardTimeout: 30 * time.Second,
	}
}

func TestDefaultConfig_DispatchDurations(t *testing.T) {
	unsetEnv(t, "CYODA_DISPATCH_WAIT_TIMEOUT", "CYODA_DISPATCH_CONNECT_TIMEOUT", "CYODA_DISPATCH_FORWARD_TIMEOUT")
	got := DefaultConfig().Cluster
	if got.DispatchWaitTimeout != 5*time.Second {
		t.Errorf("default DispatchWaitTimeout = %s, want 5s", got.DispatchWaitTimeout)
	}
	if got.DispatchConnectTimeout != 2*time.Second {
		t.Errorf("default DispatchConnectTimeout = %s, want 2s", got.DispatchConnectTimeout)
	}
	if got.DispatchForwardTimeout != 30*time.Second {
		t.Errorf("default DispatchForwardTimeout = %s, want 30s", got.DispatchForwardTimeout)
	}
	t.Setenv("CYODA_DISPATCH_CONNECT_TIMEOUT", "500ms")
	if got := DefaultConfig().Cluster.DispatchConnectTimeout; got != 500*time.Millisecond {
		t.Errorf("DispatchConnectTimeout override = %s, want 500ms", got)
	}
	// 0 is the documented "do not wait" value and must survive DefaultConfig.
	t.Setenv("CYODA_DISPATCH_WAIT_TIMEOUT", "0s")
	if got := DefaultConfig().Cluster.DispatchWaitTimeout; got != 0 {
		t.Errorf("DispatchWaitTimeout=0s = %s, want 0", got)
	}
}

func TestValidateDispatch(t *testing.T) {
	if err := ValidateDispatch(validDispatchConfig()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	noWait := validDispatchConfig()
	noWait.DispatchWaitTimeout = 0
	if err := ValidateDispatch(noWait); err != nil {
		t.Fatalf("patience 0 (waiting disabled) rejected: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*cluster.Config)
		wantName string
	}{
		{"negative patience", func(c *cluster.Config) { c.DispatchWaitTimeout = -time.Second }, "CYODA_DISPATCH_WAIT_TIMEOUT"},
		{"zero connect timeout", func(c *cluster.Config) { c.DispatchConnectTimeout = 0 }, "CYODA_DISPATCH_CONNECT_TIMEOUT"},
		{"negative connect timeout", func(c *cluster.Config) { c.DispatchConnectTimeout = -time.Second }, "CYODA_DISPATCH_CONNECT_TIMEOUT"},
		{"zero forward timeout", func(c *cluster.Config) { c.DispatchForwardTimeout = 0 }, "CYODA_DISPATCH_FORWARD_TIMEOUT"},
		{"negative forward timeout", func(c *cluster.Config) { c.DispatchForwardTimeout = -time.Second }, "CYODA_DISPATCH_FORWARD_TIMEOUT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validDispatchConfig()
			tc.mutate(&c)
			err := ValidateDispatch(c)
			if err == nil {
				t.Fatalf("ValidateDispatch = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error must name %s; got: %v", tc.wantName, err)
			}
		})
	}
}
```

`app/config_validate_test.go`: `validConfig()` gains `Cluster: validDispatchConfig(),`;
`TestConfig_Validate` gains

```go
		{"negative patience", func(c *Config) { c.Cluster.DispatchWaitTimeout = -time.Second }},
		{"zero pass allowance", func(c *Config) { c.Callout.PassAllowance = 0 }},
```

`defaultFor` gains:

```go
		"CYODA_DISPATCH_CONNECT_TIMEOUT":   renderDuration(c.Cluster.DispatchConnectTimeout),
		"CYODA_CALLOUT_HANDOVER_ALLOWANCE": renderDuration(c.Callout.HandoverAllowance),
		"CYODA_CALLOUT_PASS_ALLOWANCE":     renderDuration(c.Callout.PassAllowance),
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./app/ -run 'TestDefaultConfig_CalloutAllowances|TestValidateCallout_Allowances|TestDefaultConfig_DispatchDurations|TestValidateDispatch|TestConfig_Validate'`
Expected: FAIL — build error `unknown field HandoverAllowance` /
`undefined: ValidateDispatch`.

- [ ] **Step 3: Implement**

`internal/cluster/config.go` — after `DispatchForwardTimeout`, with comments on
all three (the present fields have none):

```go
	// DispatchWaitTimeout is the patience: how long one callout waits, in
	// total, for a compute member to exist. It applies on a single node as in
	// a cluster. Zero disables waiting.
	DispatchWaitTimeout time.Duration
	// DispatchConnectTimeout bounds opening the connection for a hand-over to
	// another node. A peer that cannot be connected to costs no try.
	DispatchConnectTimeout time.Duration
	// DispatchForwardTimeout is the whole-request timeout of the scheduler's
	// node-to-node RPC client. It does not govern callout hand-overs.
	DispatchForwardTimeout time.Duration
```

`app/config.go` — `CalloutConfig` gains:

```go
	// HandoverAllowance is what the owner allows a hand-over to another node
	// on top of (tries left × answer limit), and the last term of a callout's
	// deadline. CYODA_CALLOUT_HANDOVER_ALLOWANCE, default 30s, must be > 0.
	HandoverAllowance time.Duration
	// PassAllowance is how long a pass outlives its try's answer limit: the
	// margin for routing a callback and for clocks that differ between nodes.
	// CYODA_CALLOUT_PASS_ALLOWANCE, default 30s, must be > 0.
	PassAllowance time.Duration
```

`DefaultConfig()`: in `Callout:` add

```go
			HandoverAllowance:  envDuration("CYODA_CALLOUT_HANDOVER_ALLOWANCE", 30*time.Second),
			PassAllowance:      envDuration("CYODA_CALLOUT_PASS_ALLOWANCE", 30*time.Second),
```

and in `Cluster:` after `DispatchWaitTimeout`:

```go
			DispatchConnectTimeout: envDuration("CYODA_DISPATCH_CONNECT_TIMEOUT", 2*time.Second),
```

`ValidateCallout` — before `return nil`:

```go
	if c.HandoverAllowance <= 0 {
		return fmt.Errorf("CYODA_CALLOUT_HANDOVER_ALLOWANCE must be > 0, got %s", c.HandoverAllowance)
	}
	if c.PassAllowance <= 0 {
		return fmt.Errorf("CYODA_CALLOUT_PASS_ALLOWANCE must be > 0, got %s", c.PassAllowance)
	}
```

New validator:

```go
// ValidateDispatch rejects dispatch durations that cannot be honoured. The
// patience may be zero (waiting disabled) but not negative; the connect and
// forward timeouts bound network calls and must be positive. They are checked
// whether or not clustering is enabled: a value that would fail the moment
// clustering is switched on is a configuration error today.
func ValidateDispatch(c cluster.Config) error {
	if c.DispatchWaitTimeout < 0 {
		return fmt.Errorf("CYODA_DISPATCH_WAIT_TIMEOUT must not be negative (0 disables waiting), got %s", c.DispatchWaitTimeout)
	}
	if c.DispatchConnectTimeout <= 0 {
		return fmt.Errorf("CYODA_DISPATCH_CONNECT_TIMEOUT must be > 0, got %s", c.DispatchConnectTimeout)
	}
	if c.DispatchForwardTimeout <= 0 {
		return fmt.Errorf("CYODA_DISPATCH_FORWARD_TIMEOUT must be > 0, got %s", c.DispatchForwardTimeout)
	}
	return nil
}
```

`Config.Validate()` — after the `ValidateCallout` call:

```go
	if err := ValidateDispatch(c.Cluster); err != nil {
		return err
	}
```

`cmd/cyoda/main.go` — after the `ValidateCallout` block (same TDD waiver as C-1):

```go
	if err := app.ValidateDispatch(cfg.Cluster); err != nil {
		slog.Error("dispatch config validation failed", "error", err)
		os.Exit(1)
	}
```

`cmd/cyoda/help/config_registry.go`, cluster block — replace the two existing
rows and add three:

```go
	{Name: "CYODA_DISPATCH_WAIT_TIMEOUT", Topic: "cluster", Type: "duration", Default: "5s", Description: "How long one callout waits, in total, for a compute member with matching tags to exist — on a single node as in a cluster, whatever its retryPolicy. 0 disables waiting. Must not be negative; startup fails otherwise."},
	{Name: "CYODA_DISPATCH_CONNECT_TIMEOUT", Topic: "cluster", Type: "duration", Default: "2s", Description: "Time allowed to open the connection when a callout is handed over to another node; a node that cannot be connected to costs no try. Must be > 0; startup fails otherwise."},
	{Name: "CYODA_DISPATCH_FORWARD_TIMEOUT", Topic: "cluster", Type: "duration", Default: "30s", Description: "Whole-request timeout of the node-to-node call that delegates a scheduled transition. Does not govern callout hand-overs. Must be > 0; startup fails otherwise."},
	{Name: "CYODA_CALLOUT_HANDOVER_ALLOWANCE", Topic: "cluster", Type: "duration", Default: "30s", Description: "What the owning node allows a callout hand-over on top of (tries left x answer limit); also the last term of a callout's overall deadline. Must be > 0; startup fails otherwise."},
	{Name: "CYODA_CALLOUT_PASS_ALLOWANCE", Topic: "cluster", Type: "duration", Default: "30s", Description: "How long the transaction token given to a compute member outlives its try's answer limit — the margin for routing a callback and for clocks that differ between nodes. Must be > 0; startup fails otherwise."},
```

`cmd/cyoda/help/content/config/cluster.md` — replace lines 27-28 (line 29,
`CYODA_TX_TOKEN_TTL`, is left for C-11) with:

```markdown
- `CYODA_DISPATCH_WAIT_TIMEOUT` (duration, default: `5s`) — the patience: how long one callout waits, in total, for a compute member with matching tags to exist. It applies on a single node as in a cluster and whatever the callout's `retryPolicy` — waiting for a member to exist is not a retry and costs no try. The wait ends the moment a member attaches or a peer announces one. `0` disables waiting: a callout with no member fails at once with `NO_COMPUTE_MEMBER_FOR_TAG`. Must not be negative; startup fails otherwise.
- `CYODA_DISPATCH_CONNECT_TIMEOUT` (duration, default: `2s`) — time allowed to open the connection when a callout is handed over to another node. A node that cannot be connected to costs no try; the next one is asked. Behind a sidecar or an ingress the connection always opens and a dead peer shows as a 502–504, which counts as one try. Must be `> 0`; startup fails otherwise.
- `CYODA_CALLOUT_HANDOVER_ALLOWANCE` (duration, default: `30s`) — what the owning node allows a hand-over on top of `tries left × answer limit`, and the last term of a callout's overall deadline (`tries × answer limit + CYODA_DISPATCH_WAIT_TIMEOUT + this`; 155 s at the defaults). Must be `> 0`; startup fails otherwise.
- `CYODA_CALLOUT_PASS_ALLOWANCE` (duration, default: `30s`) — how long the transaction token given to a compute member outlives its try's answer limit: the margin for routing a callback between nodes and for clocks that differ between them. Must be `> 0`; startup fails otherwise.
- `CYODA_DISPATCH_FORWARD_TIMEOUT` (duration, default: `30s`) — whole-request timeout of the node-to-node call that delegates a scheduled transition to another node. It does not govern callout hand-overs. Must be `> 0`; startup fails otherwise.
```

The tries and answer-limit settings are in `config grpc`; add that sentence and
`config.grpc` to this topic's `see_also`/SEE ALSO.

`cmd/cyoda/help/content/config/grpc.md` — close the "Compute-node callouts"
subsection (C-1) with:

```markdown
Tries multiply the answer limit. With the PostgreSQL backend a callout holds
its transaction's connection idle while it waits, and
`CYODA_POSTGRES_IDLE_IN_TX_TIMEOUT` (default `5m`) reclaims a connection idle
that long. The relation is documented, not enforced: keep
`(retries + 1) × the upper bound`, plus `CYODA_DISPATCH_WAIT_TIMEOUT` and
`CYODA_CALLOUT_HANDOVER_ALLOWANCE`, under that ceiling — 275 s at the upper
bound with every other default. The transaction token a compute member
receives outlives its try by `CYODA_CALLOUT_PASS_ALLOWANCE`, on a single node
too. These three are described in `config cluster`.
```


`README.md` "Compute-node callouts" table — add the five rows (names, defaults,
one line each, same wording as the registry, shortened). `docs/ARCHITECTURE.md`
cluster table — revise the two existing rows to the new meanings and add three.

`CHANGELOG.md` `[Unreleased]` → `### Added`:

```markdown
- **Callout settings.** `CYODA_RETRY_FIXED_NUM_RETRIES` (default `3`),
  `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS` (default `30000`, until now a constant),
  `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (default `60000`),
  `CYODA_DISPATCH_CONNECT_TIMEOUT` (default `2s`),
  `CYODA_CALLOUT_HANDOVER_ALLOWANCE` (default `30s`) and
  `CYODA_CALLOUT_PASS_ALLOWANCE` (default `30s`). Out-of-range values are
  startup errors. `CYODA_DISPATCH_WAIT_TIMEOUT` and
  `CYODA_DISPATCH_FORWARD_TIMEOUT` are validated for the first time (negative,
  respectively non-positive, values now fail startup). See
  `cyoda help config grpc` and `cyoda help config cluster`.
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./app/... ./cmd/cyoda/... ./internal/cluster/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/cluster/config.go app/config.go app/config_callout_test.go \
  app/config_dispatch_test.go app/config_validate_test.go \
  app/config_registry_binding_test.go cmd/cyoda/main.go \
  cmd/cyoda/help/config_registry.go cmd/cyoda/help/content/config/cluster.md \
  cmd/cyoda/help/content/config/grpc.md README.md docs/ARCHITECTURE.md CHANGELOG.md
git commit -m "feat(config): hand-over and pass allowances, a connect timeout, and validated dispatch durations (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-3: SPI — `ProcessorConfig.Idempotent`, `ScheduleFunction.RetryPolicy` (in `cyoda-go-spi`)

**Spec:** §10 bullets 1-3; D2, D3.

**Repository:** `/Users/paul/go-projects/cyoda-light/cyoda-go-spi` (module
`github.com/cyoda-platform/cyoda-go-spi`), branch
`feat/callout-idempotent-retry-policy` cut from `main` (`1ab57a6`).

**Files:**
- Modify: `types.go` (`ProcessorConfig` :238-275; `ScheduleFunction` :324-334)
- Test: `types_test.go`
- Docs: `CHANGELOG.md` `[Unreleased]` → `### Added`

**Interfaces:**
- Produces: `spi.ProcessorConfig.Idempotent bool` (`json:"idempotent,omitempty"`);
  `spi.ScheduleFunction.RetryPolicy string` (`json:"retryPolicy,omitempty"`).
  `ScheduleFunction` stays comparable. No interface, no `spitest` case, no
  plugin change (every backend stores the workflow as one JSON document, R§8).

- [ ] **Step 1: Write the failing tests** — append to `types_test.go`:

```go
func TestProcessorConfig_Idempotent_RoundTrips(t *testing.T) {
	bs, err := json.Marshal(ProcessorConfig{Idempotent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bs), `"idempotent":true`) {
		t.Errorf("missing field in marshalled JSON: %s", bs)
	}
	var back ProcessorConfig
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Idempotent {
		t.Errorf("round-trip dropped the field: %+v", back)
	}

	// The default (false) is omitted, so a workflow that never mentions the
	// field exports byte-identically to one stored before the field existed.
	bs2, _ := json.Marshal(ProcessorConfig{})
	if strings.Contains(string(bs2), "idempotent") {
		t.Errorf("false must be omitted, got %s", bs2)
	}
}

func TestScheduleFunction_RetryPolicy_RoundTrips(t *testing.T) {
	fn := ScheduleFunction{
		Name: "computeFire", ResultKind: "Schedule",
		CalculationNodesTags: "scheduler", RetryPolicy: "NONE",
	}
	bs, err := json.Marshal(fn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bs), `"retryPolicy":"NONE"`) {
		t.Errorf("missing field in marshalled JSON: %s", bs)
	}
	var back ScheduleFunction
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatal(err)
	}
	// == on the struct is part of the contract: consumers compare
	// ScheduleFunction values directly, so every field must stay comparable.
	if back != fn {
		t.Errorf("round-trip mismatch: got %+v, want %+v", back, fn)
	}

	bs2, _ := json.Marshal(ScheduleFunction{Name: "f", ResultKind: "Schedule", CalculationNodesTags: "t"})
	if strings.Contains(string(bs2), "retryPolicy") {
		t.Errorf("unset retryPolicy must be omitted, got %s", bs2)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run (in the SPI repo): `go test . -run 'TestProcessorConfig_Idempotent_RoundTrips|TestScheduleFunction_RetryPolicy_RoundTrips'`
Expected: FAIL — build error `unknown field Idempotent in struct literal` /
`unknown field RetryPolicy`.

- [ ] **Step 3: Implement** — `types.go`.

In `ProcessorConfig`, after `RetryPolicy`:

```go
	// Idempotent is the workflow author's declaration that running this
	// processor again is safe — for the engine's own data and for every
	// system the processor touches. When true, a consuming engine may give
	// the work to another compute member after a member that received it
	// went silent or dropped its connection. When false (the default) it
	// must not: the first member may have acted. The engine takes the
	// declaration on trust and cannot verify it.
	Idempotent bool `json:"idempotent,omitempty"`
```

In `ScheduleFunction`, after `ResponseTimeoutMs`:

```go
	// RetryPolicy selects the server-resolved retry strategy for this
	// callout: "NONE" (one try), "FIXED" or empty (the server-configured
	// number of tries). Same vocabulary as ProcessorConfig.RetryPolicy. A
	// plain string keeps the struct comparable.
	RetryPolicy string `json:"retryPolicy,omitempty"`
```

`CHANGELOG.md`, under `## [Unreleased]` → `### Added`:

```markdown
- **`ProcessorConfig.Idempotent` and `ScheduleFunction.RetryPolicy`.** Two
  optional workflow-configuration fields. `idempotent` (bool, default false,
  omitted when false) is the author's declaration that a processor may be run
  again on another compute member after one that received the work went
  silent. `retryPolicy` on a scheduled-transition function takes the same
  `NONE` / `FIXED` vocabulary a processor's already does. Additive: both are
  `omitempty`, so stored workflows round-trip byte-identically, no interface
  changes, and `ScheduleFunction` stays comparable. Storage plugins persist the
  workflow as one document and need no change.
```

- [ ] **Step 4: Run to verify GREEN**

Run (in the SPI repo): `go test ./... && go vet ./...`   Expected: PASS.
(`spitest` reports `[no test files]` by design.)

- [ ] **Step 5: Commit (SPI repo)**

```
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi checkout -b feat/callout-idempotent-retry-policy
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi add types.go types_test.go CHANGELOG.md
git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi commit -m "feat(workflow): idempotent on a processor, retryPolicy on a schedule function

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

Not pushed, not tagged (C-10).

- [ ] **Step 6: Compose locally (cyoda-go worktree; nothing is committed)**

```
cd <worktree> && go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi
go build ./... && git status --short go.work
```
Expected: build OK; ` M go.work`. The line stays uncommitted until C-10 removes
it locally. From here to C-10, `GOWORK=off go build ./...` fails once cyoda-go
code names the new fields (C-6 on) — that is the stale pin showing, and is what
C-10 closes.

---

### Task C-4: OpenAPI — `idempotent`, function `retryPolicy`, the `responseTimeoutMs` bound

**Spec:** §10 bullet 4; D3 wording for `retryPolicy`.

**Files:**
- Modify: `api/openapi.yaml` (`ExternalizedFunctionConfigDto` :9404-9426;
  `ExternalizedProcessorConfigDto` :10168-10190; `ScheduleFunctionDto` :10366-10409)
- Modify: `api/generated.go` (by `go generate ./api` only)
- Test: `api/schedule_function_test.go`, `api/callout_config_dto_test.go` (new)

**Interfaces:**
- Produces: generated `ExternalizedProcessorConfigDto.Idempotent *bool`,
  `ScheduleFunctionDto.RetryPolicy *ScheduleFunctionDtoRetryPolicy` with
  constants `ScheduleFunctionDtoRetryPolicyFIXED` / `…NONE`. Nothing in
  production code reads them: import decodes straight into SPI types (R§8).

- [ ] **Step 1: Write the failing tests**

`api/schedule_function_test.go` — both hard-coded lists (`:35` and `:88`) gain
`"retryPolicy"`, and the round-trip sets it:

```go
	for _, want := range []string{"name", "resultKind", "calculationNodesTags", "attachEntity", "context", "responseTimeoutMs", "retryPolicy"} {
```

```go
	retry := ScheduleFunctionDtoRetryPolicyNONE
	fn := ScheduleFunctionDto{
		Name:                 "computeNextFireTime",
		ResultKind:           ScheduleFunctionDtoResultKind("Schedule"),
		CalculationNodesTags: "billing",
		AttachEntity:         &attach,
		Context:              strPtr("role=nightly"),
		ResponseTimeoutMs:    &timeout,
		RetryPolicy:          &retry,
	}
```

```go
	for _, key := range []string{"name", "resultKind", "calculationNodesTags", "attachEntity", "context", "responseTimeoutMs", "retryPolicy"} {
```

`api/callout_config_dto_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

// TestCalloutConfigDtos pins the callout fields the three callout DTOs carry:
// idempotent on a processor, retryPolicy with the NONE/FIXED enum on all
// three, and a non-negative responseTimeoutMs whose description names the
// server-side upper bound (a setting, so it cannot be a schema `maximum`).
func TestCalloutConfigDtos(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, name := range []string{"ExternalizedProcessorConfigDto", "ExternalizedFunctionConfigDto", "ScheduleFunctionDto"} {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Fatalf("%s schema missing", name)
		}
		props := ref.Value.Properties

		rp := props["retryPolicy"]
		if rp == nil || rp.Value == nil {
			t.Errorf("%s.retryPolicy missing", name)
		} else {
			got := map[any]bool{}
			for _, v := range rp.Value.Enum {
				got[v] = true
			}
			if len(got) != 2 || !got["NONE"] || !got["FIXED"] {
				t.Errorf("%s.retryPolicy enum = %v, want [NONE FIXED]", name, rp.Value.Enum)
			}
			if strings.Contains(rp.Value.Description, "delay") {
				t.Errorf("%s.retryPolicy description still promises a delay between tries: %q", name, rp.Value.Description)
			}
			if !strings.Contains(rp.Value.Description, "not a hard limit") {
				t.Errorf("%s.retryPolicy description must say the number of tries is not a hard limit: %q", name, rp.Value.Description)
			}
		}

		rt := props["responseTimeoutMs"]
		if rt == nil || rt.Value == nil {
			t.Fatalf("%s.responseTimeoutMs missing", name)
		}
		if rt.Value.Min == nil || *rt.Value.Min != 0 {
			t.Errorf("%s.responseTimeoutMs must declare minimum: 0", name)
		}
		if !strings.Contains(rt.Value.Description, "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
			t.Errorf("%s.responseTimeoutMs description must name the upper-bound setting: %q", name, rt.Value.Description)
		}
	}

	idem := doc.Components.Schemas["ExternalizedProcessorConfigDto"].Value.Properties["idempotent"]
	if idem == nil || idem.Value == nil || !idem.Value.Type.Is("boolean") {
		t.Error("ExternalizedProcessorConfigDto.idempotent must be a boolean property")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./api/ -run 'TestScheduleFunctionDto|TestCalloutConfigDtos'`
Expected: FAIL — build error `undefined: ScheduleFunctionDtoRetryPolicyNONE`.

- [ ] **Step 3: Implement** — `api/openapi.yaml`.

On all three DTOs replace the `responseTimeoutMs` property with:

```yaml
        responseTimeoutMs:
          type: integer
          format: int64
          minimum: 0
          description: |
            How long to wait for the compute member's answer, in milliseconds.
            When absent or 0 the server's CYODA_CALLOUT_RESPONSE_TIMEOUT_MS
            applies (default 30000). Must not exceed the server's
            CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS (default 60000); import
            rejects a larger or a negative value with HTTP 400
            VALIDATION_FAILED. The bound is a server setting, so a workflow
            exported from one deployment can be refused by another with a
            lower bound.
```

On `ExternalizedFunctionConfigDto` and `ExternalizedProcessorConfigDto` replace
the `retryPolicy` description, and add the same property to
`ScheduleFunctionDto` after `responseTimeoutMs`:

```yaml
        retryPolicy:
          type: string
          enum: [NONE, FIXED]
          description: |
            Retry policy selector. NONE → one try. FIXED → the
            server-configured number of tries (CYODA_RETRY_FIXED_NUM_RETRIES
            retries after the first). The number of tries is
            server-configured, and is the normal number, not a hard limit.
            When omitted, FIXED applies. Import-time validation rejects any
            other value with HTTP 400 VALIDATION_FAILED.
```

On `ExternalizedProcessorConfigDto`, after `retryPolicy`:

```yaml
        idempotent:
          type: boolean
          default: false
          description: |
            The workflow author's declaration that running this processor
            again is safe — for cyoda and for every system the processor
            touches. When true, the work may be given to another compute
            member after a member that received it went silent or dropped
            its connection. When false (the default) it is not, because the
            first member may have acted. Criteria and functions are always
            treated as safe to repeat and carry no such field.
```

Then `go generate ./api` and `make check-codegen`.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./api/... ./internal/domain/workflow/ -run 'TestScheduleFunctionDto|TestCalloutConfigDtos|TestOpenAPI'` then `go test ./api/...`
Expected: PASS; `make check-codegen` prints OK.

- [ ] **Step 5: Commit**

```
git add api/openapi.yaml api/generated.go api/schedule_function_test.go api/callout_config_dto_test.go
git commit -m "feat(api): idempotent on a processor, retryPolicy on a schedule function, a bounded responseTimeoutMs (#254, #565)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-5: One parser for a criterion's function envelope

**Spec:** §10 "criterion function config is parsed for `retryPolicy` and
`responseTimeoutMs`"; R§8 last bullet.

**Files:**
- Create: `internal/contract/criterion_function.go`
- Test: `internal/contract/criterion_function_test.go` (the package's first test file)
- Modify: `internal/grpc/dispatch.go` (`DispatchCriteria` :273-341 — the ad-hoc
  `parsed` struct and its five uses)

**Interfaces:**
- Produces (consumed by C-6, C-7 and by the stream that reads a criterion's
  `retryPolicy` to resolve its number of tries):
  ```go
  // package contract
  type CriterionFunctionConfig struct {
      CalculationNodesTags string `json:"calculationNodesTags"`
      AttachEntity         *bool  `json:"attachEntity"` // nil = default true
      ResponseTimeoutMs    int64  `json:"responseTimeoutMs"`
      RetryPolicy          string `json:"retryPolicy"`
      Context              string `json:"context"`
  }
  type CriterionFunction struct {
      Name   string                  `json:"name"`
      Config CriterionFunctionConfig `json:"config"`
  }
  func ParseCriterionFunction(criterion json.RawMessage) (CriterionFunction, error)
  ```
  The parser reads the `function` member and does not look at `type` — exactly
  what `DispatchCriteria` does today (its tests and the cluster hand-over tests
  pass criteria of other `type`s through mocks). Knowing that a criterion *is*
  a function criterion stays with the caller (`predicate.ParseCondition`).

- [ ] **Step 1: Write the failing test** — `internal/contract/criterion_function_test.go`:

```go
package contract

import (
	"encoding/json"
	"testing"
)

func TestParseCriterionFunction(t *testing.T) {
	no := false
	cases := []struct {
		name    string
		in      string
		want    CriterionFunction
		wantErr bool
	}{
		{
			name: "full envelope",
			in: `{"type":"function","function":{"name":"min-amount","config":{
				"calculationNodesTags":"pricing,eu","attachEntity":false,
				"responseTimeoutMs":5000,"retryPolicy":"NONE","context":"role=a"}}}`,
			want: CriterionFunction{Name: "min-amount", Config: CriterionFunctionConfig{
				CalculationNodesTags: "pricing,eu", AttachEntity: &no,
				ResponseTimeoutMs: 5000, RetryPolicy: "NONE", Context: "role=a"}},
		},
		{
			name: "no config: every field at its zero value, attachEntity unset",
			in:   `{"type":"function","function":{"name":"f"}}`,
			want: CriterionFunction{Name: "f"},
		},
		{
			name: "unknown members are ignored; the criterion is client-owned JSON",
			in:   `{"type":"function","function":{"name":"f","calculationNodesTags":"legacy","config":{"x":1}}}`,
			want: CriterionFunction{Name: "f"},
		},
		{name: "responseTimeoutMs of the wrong type", in: `{"type":"function","function":{"name":"f","config":{"responseTimeoutMs":"5s"}}}`, wantErr: true},
		{name: "retryPolicy of the wrong type", in: `{"type":"function","function":{"name":"f","config":{"retryPolicy":3}}}`, wantErr: true},
		{name: "not JSON", in: `{`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCriterionFunction(json.RawMessage(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCriterionFunction = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCriterionFunction: %v", err)
			}
			if got.Name != tc.want.Name ||
				got.Config.CalculationNodesTags != tc.want.Config.CalculationNodesTags ||
				got.Config.ResponseTimeoutMs != tc.want.Config.ResponseTimeoutMs ||
				got.Config.RetryPolicy != tc.want.Config.RetryPolicy ||
				got.Config.Context != tc.want.Config.Context {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			switch {
			case tc.want.Config.AttachEntity == nil && got.Config.AttachEntity != nil:
				t.Errorf("AttachEntity = %v, want nil (unset)", *got.Config.AttachEntity)
			case tc.want.Config.AttachEntity != nil &&
				(got.Config.AttachEntity == nil || *got.Config.AttachEntity != *tc.want.Config.AttachEntity):
				t.Errorf("AttachEntity = %v, want %v", got.Config.AttachEntity, *tc.want.Config.AttachEntity)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/contract/ -run TestParseCriterionFunction`
Expected: FAIL — build error `undefined: ParseCriterionFunction`.

- [ ] **Step 3: Implement** — `internal/contract/criterion_function.go`:

```go
package contract

import (
	"encoding/json"
	"fmt"
)

// CriterionFunctionConfig is the callout configuration of a function
// criterion: {"type":"function","function":{"name":…,"config":{…}}}. The SPI
// stores a criterion as raw JSON, so this is the one typed view of it, shared
// by workflow import (which validates it) and by dispatch (which acts on it).
type CriterionFunctionConfig struct {
	CalculationNodesTags string `json:"calculationNodesTags"`
	// AttachEntity is nil when the author did not set it; the default is true.
	AttachEntity      *bool  `json:"attachEntity"`
	ResponseTimeoutMs int64  `json:"responseTimeoutMs"`
	RetryPolicy       string `json:"retryPolicy"`
	// Context is a pass-through string surfaced verbatim in the request's
	// parameters node.
	Context string `json:"context"`
}

// CriterionFunction is the `function` member of a function criterion.
type CriterionFunction struct {
	Name   string                  `json:"name"`
	Config CriterionFunctionConfig `json:"config"`
}

// ParseCriterionFunction decodes the `function` member of a criterion. It
// does not check the criterion's `type`: whether a criterion is a function
// criterion is the caller's knowledge (predicate.ParseCondition). Members it
// does not name are ignored — the criterion is client-owned JSON.
func ParseCriterionFunction(criterion json.RawMessage) (CriterionFunction, error) {
	var envelope struct {
		Function CriterionFunction `json:"function"`
	}
	if err := json.Unmarshal(criterion, &envelope); err != nil {
		return CriterionFunction{}, fmt.Errorf("failed to parse criterion function: %w", err)
	}
	return envelope.Function, nil
}
```

`internal/grpc/dispatch.go`, `DispatchCriteria` — before:

```go
	// FunctionCondition schema: {"type":"function","function":{"name":"...","config":{...}}}
	var parsed struct {
		Function struct {
			...
		} `json:"function"`
	}
	if err := json.Unmarshal(criterion, &parsed); err != nil {
		return false, "", fmt.Errorf("invalid criterion JSON: %w", err)
	}
```

after:

```go
	fn, err := contract.ParseCriterionFunction(criterion)
	if err != nil {
		return false, "", fmt.Errorf("invalid criterion JSON: %w", err)
	}
```

and every `parsed.Function.X` → `fn.X` (`Name` ×4, `Config.AttachEntity`,
`Config.CalculationNodesTags` ×3, `Config.Context`, `Config.ResponseTimeoutMs`).
Behaviour-preserving; the package's existing tests are the guard.

Exit check: `grep -n 'var parsed struct' internal/grpc/dispatch.go → no hits`.

If the stream that rewrites `DispatchCriteria` lands first, this step applies to
its version of the function instead; the exit check is the same.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/contract/... ./internal/grpc/...`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/contract/criterion_function.go internal/contract/criterion_function_test.go internal/grpc/dispatch.go
git commit -m "refactor(contract): one typed view of a criterion's function envelope (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-6: Import refuses an invalid `retryPolicy` on a criterion and on a function

**Spec:** §10 "Import validation"; §8.2 row "Import: criterion or function
`retryPolicy` not `NONE`/`FIXED`/unset → 400 `VALIDATION_FAILED`, names the
workflow, state, transition"; §13 row "Import: criterion / function
`retryPolicy` invalid → 400" (U, E here; P in C-9); §14 stale comment
`validate.go:66-68`, `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` §M1.

**Files:**
- Modify: `internal/domain/workflow/validate.go` (retry-policy comment :56-74;
  `validateCriterion` :200-278; rule list in `validateImportRequest` doc
  :343-366 and in `validateWorkflowStructure` :451-464; function block :520-532)
- Test: `internal/domain/workflow/validate_import_test.go`,
  `internal/e2e/workflow_callout_import_test.go` (new)
- Docs: `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md` (:34, :319-331),
  `cmd/cyoda/help/content/workflows.md:489`

**Interfaces:**
- Consumes: `contract.ParseCriterionFunction` (C-5);
  `spi.ScheduleFunction.RetryPolicy` (C-3, through the `go.work` line).
- Produces: nothing new for other streams. The existing constants
  `workflow.RetryPolicyNone`, `workflow.RetryPolicyFixed` are what the stream
  resolving the number of tries compares against.

- [ ] **Step 1: Write the failing tests**

`internal/domain/workflow/validate_import_test.go` — after the M1 block
(add `"encoding/json"` and `"fmt"` to the imports):

```go
// --- M1b — retryPolicy on a criterion function and on a schedule function ---

func functionCriterion(retryPolicy string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","retryPolicy":%q}}}`,
		retryPolicy))
}

func scheduleFunctionFixture(retryPolicy string) spi.WorkflowDefinition {
	return spi.WorkflowDefinition{
		Version: "1.1", Name: "wf-fn", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{{
				Name: "t", Next: "S2",
				Schedule: &spi.TransitionSchedule{Function: &spi.ScheduleFunction{
					Name: "computeFire", ResultKind: "Schedule",
					CalculationNodesTags: "scheduler", RetryPolicy: retryPolicy,
				}},
			}}},
			"S2": {},
		},
	}
}

func TestValidateImportRequest_CriterionRetryPolicy(t *testing.T) {
	for _, policy := range []string{"", RetryPolicyNone, RetryPolicyFixed} {
		t.Run("accepts "+policy, func(t *testing.T) {
			for _, wf := range []spi.WorkflowDefinition{
				wfWithTransitionCriterion(functionCriterion(policy)),
				wfWithWorkflowCriterion(functionCriterion(policy)),
			} {
				if err := validateImportRequest([]spi.WorkflowDefinition{wf}); err != nil {
					t.Fatalf("retryPolicy=%q: %v", policy, err)
				}
			}
		})
	}

	t.Run("rejects an unknown value on a transition criterion", func(t *testing.T) {
		err := validateImportRequest([]spi.WorkflowDefinition{
			wfWithTransitionCriterion(functionCriterion("LINEAR_BACKOFF"))})
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		for _, want := range []string{`workflow "wf-regex"`, `state "S1"`, `transition "go"`,
			`criterion function "min-amount"`, "unknown retryPolicy", `"LINEAR_BACKOFF"`, "NONE, FIXED, or empty"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must contain %q; got: %v", want, err)
			}
		}
	})

	t.Run("rejects an unknown value on a workflow criterion", func(t *testing.T) {
		err := validateImportRequest([]spi.WorkflowDefinition{
			wfWithWorkflowCriterion(functionCriterion("fixed"))})
		if err == nil {
			t.Fatal("expected an error: the vocabulary is case-sensitive, as on a processor")
		}
		for _, want := range []string{`workflow "wf-regex"`, `criterion function "min-amount"`, "unknown retryPolicy"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must contain %q; got: %v", want, err)
			}
		}
	})

	t.Run("rejects a function config that cannot be read", func(t *testing.T) {
		crit := json.RawMessage(`{"type":"function","function":{"name":"f","config":{"retryPolicy":3}}}`)
		err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(crit)})
		if err == nil {
			t.Fatal("expected an error: this criterion fails every dispatch with 'invalid criterion JSON'")
		}
		if !strings.Contains(err.Error(), `transition "go"`) {
			t.Errorf("error must name the transition; got: %v", err)
		}
	})

	t.Run("a simple criterion is untouched", func(t *testing.T) {
		crit := json.RawMessage(`{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":1}`)
		if err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(crit)}); err != nil {
			t.Fatalf("simple criterion rejected: %v", err)
		}
	})
}

func TestValidateImportRequest_ScheduleFunctionRetryPolicy(t *testing.T) {
	for _, policy := range []string{"", RetryPolicyNone, RetryPolicyFixed} {
		if err := validateImportRequest([]spi.WorkflowDefinition{scheduleFunctionFixture(policy)}); err != nil {
			t.Errorf("retryPolicy=%q rejected: %v", policy, err)
		}
	}
	err := validateImportRequest([]spi.WorkflowDefinition{scheduleFunctionFixture("EXPONENTIAL")})
	if err == nil {
		t.Fatal("expected an error for an unknown retryPolicy, got nil")
	}
	for _, want := range []string{`workflow "wf-fn"`, `state "S1"`, `transition "t"`,
		"schedule.function", "unknown retryPolicy", `"EXPONENTIAL"`, "NONE, FIXED, or empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q; got: %v", want, err)
		}
	}
}
```

(`wfWithTransitionCriterion` / `wfWithWorkflowCriterion` are in
`criterion_regex_test.go:46, 60`, same package.)

`internal/e2e/workflow_callout_import_test.go`:

```go
package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// importRejection imports payload and asserts 400 VALIDATION_FAILED whose
// detail contains every string in wantInDetail.
func importRejection(t *testing.T, entity string, payload string, wantInDetail ...string) {
	t.Helper()
	status, respBody := importWorkflowE2E(t, entity, 1, payload)
	if status != http.StatusBadRequest {
		t.Fatalf("import status = %d; want 400; body: %s", status, respBody)
	}
	var errBody struct {
		Detail     string `json:"detail"`
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(respBody), &errBody); err != nil {
		t.Fatalf("decode error body: %v; raw: %s", err, respBody)
	}
	if errBody.Properties.ErrorCode != "VALIDATION_FAILED" {
		t.Fatalf("errorCode = %q; want VALIDATION_FAILED; body: %s", errBody.Properties.ErrorCode, respBody)
	}
	for _, want := range wantInDetail {
		if !strings.Contains(errBody.Detail, want) {
			t.Errorf("detail %q must contain %q", errBody.Detail, want)
		}
	}
}

// calloutWorkflow builds a one-transition workflow. criterion and schedule
// are raw JSON members (or ""), processorConfig is the processor's config
// object (or "" for no processor).
func calloutWorkflow(name, criterion, schedule, processorConfig string) string {
	var members []string
	members = append(members, `"name": "t"`, `"next": "Done"`)
	if schedule == "" {
		members = append(members, `"manual": true`)
	} else {
		members = append(members, `"schedule": `+schedule)
	}
	if criterion != "" {
		members = append(members, `"criterion": `+criterion)
	}
	if processorConfig != "" {
		members = append(members, fmt.Sprintf(
			`"processors": [{"type":"externalized","name":"p","executionMode":"SYNC","config": %s}]`, processorConfig))
	}
	return fmt.Sprintf(`{
	  "importMode": "REPLACE",
	  "workflows": [{
	    "version": "1.1", "name": %q, "initialState": "S", "active": true,
	    "states": { "S": { "transitions": [ { %s } ] }, "Done": {} }
	  }]
	}`, name, strings.Join(members, ", "))
}

func TestWorkflowImport_CriterionRetryPolicyInvalid_400(t *testing.T) {
	const entity = "wf-callout-crit-retry"
	importModelE2E(t, entity, 1)
	crit := `{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","retryPolicy":"LINEAR_BACKOFF"}}}`
	importRejection(t, entity, calloutWorkflow("crit-retry-wf", crit, "", ""),
		"crit-retry-wf", `state "S"`, `transition "t"`, "unknown retryPolicy", "LINEAR_BACKOFF")
}

func TestWorkflowImport_FunctionRetryPolicyInvalid_400(t *testing.T) {
	const entity = "wf-callout-fn-retry"
	importModelE2E(t, entity, 1)
	sched := `{"function":{"name":"computeFire","resultKind":"Schedule","calculationNodesTags":"scheduler","retryPolicy":"EXPONENTIAL"}}`
	importRejection(t, entity, calloutWorkflow("fn-retry-wf", "", sched, ""),
		"fn-retry-wf", `state "S"`, `transition "t"`, "schedule.function", "unknown retryPolicy", "EXPONENTIAL")
}

func TestWorkflowImport_CalloutRetryPolicyValid_200(t *testing.T) {
	const entity = "wf-callout-retry-ok"
	importModelE2E(t, entity, 1)
	crit := `{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","retryPolicy":"NONE"}}}`
	if status, body := importWorkflowE2E(t, entity, 1, calloutWorkflow("crit-ok-wf", crit, "", "")); status != http.StatusOK {
		t.Fatalf("criterion retryPolicy NONE: expected 200, got %d: %s", status, body)
	}
	sched := `{"function":{"name":"computeFire","resultKind":"Schedule","calculationNodesTags":"scheduler","retryPolicy":"FIXED"}}`
	if status, body := importWorkflowE2E(t, entity, 1, calloutWorkflow("fn-ok-wf", "", sched, "")); status != http.StatusOK {
		t.Fatalf("function retryPolicy FIXED: expected 200, got %d: %s", status, body)
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/domain/workflow/ -run 'TestValidateImportRequest_CriterionRetryPolicy|TestValidateImportRequest_ScheduleFunctionRetryPolicy'`
Expected: FAIL — "expected an error, got nil" in the three rejecting subtests
and in the schedule-function test.
Run: `go test ./internal/e2e/... -run 'TestWorkflowImport_CriterionRetryPolicyInvalid_400|TestWorkflowImport_FunctionRetryPolicyInvalid_400'`
Expected: FAIL — `import status = 200; want 400`.

- [ ] **Step 3: Implement** — `internal/domain/workflow/validate.go`.

Imports gain `"github.com/cyoda-platform/cyoda-go/internal/contract"`.

Replace the stale comment block `:56-74` with:

```go
// Callout retry-policy tokens: the workflow author's selector between the two
// server-resolved strategies, on a processor, a criterion function and a
// scheduled-transition function alike. Centralised here as untyped strings so
// engine logic, validator rules, and tests compare against a single source —
// the SPI fields are plain strings, so an enum type would not buy
// compile-time safety.
//
//   - NONE  — one try.
//   - FIXED — default when unset. The server-configured number of tries. The
//     number is the normal one, not a hard limit, and there is no pause
//     between tries; neither is carried in the workflow.
const (
	RetryPolicyNone  = "NONE"
	RetryPolicyFixed = "FIXED"
)

// validRetryPolicies is the set of accepted retryPolicy values for
// import-time validation. Empty is accepted — the server treats it as FIXED.
```

Add, below `validRetryPolicies`:

```go
// checkRetryPolicy is the one spelling of the retryPolicy rule. location
// names the callout, e.g. `workflow "w" state "s" transition "t" processor "p"`.
func checkRetryPolicy(policy, location string) error {
	if _, ok := validRetryPolicies[policy]; !ok {
		return fmt.Errorf("%s: unknown retryPolicy %q (allowed: NONE, FIXED, or empty)", location, policy)
	}
	return nil
}
```

Processor rule (`:579-582`) — before/after:

```go
				if _, ok := validRetryPolicies[p.Config.RetryPolicy]; !ok {
					return fmt.Errorf("workflow %q state %q transition %q processor %q: unknown retryPolicy %q (allowed: NONE, FIXED, or empty)",
						wf.Name, stateName, tr.Name, p.Name, p.Config.RetryPolicy)
				}
```
→
```go
				if err := checkRetryPolicy(p.Config.RetryPolicy, fmt.Sprintf(
					"workflow %q state %q transition %q processor %q", wf.Name, stateName, tr.Name, p.Name)); err != nil {
					return err
				}
```
(message text identical; `TestValidateImportRequest_RejectsUnknownRetryPolicy` stays as is.)

Function block — inside `if hasFn {`, after the `name`/`calculationNodesTags` check:

```go
					if err := checkRetryPolicy(f.RetryPolicy, fmt.Sprintf(
						"workflow %q state %q transition %q schedule.function", wf.Name, stateName, tr.Name)); err != nil {
						return err
					}
```

`validateCriterion` — after the successful `predicate.ParseCondition`, before
`walkCriterion`:

```go
	// A function criterion is a callout: its config is checked like a
	// processor's. Only a top-level function criterion is ever dispatched
	// (evaluateCriterion), so only that shape is inspected.
	if _, ok := cond.(*predicate.FunctionCondition); ok {
		fn, err := contract.ParseCriterionFunction(trimmed)
		if err != nil {
			return fmt.Errorf("%s: %w", location, err)
		}
		if err := checkRetryPolicy(fn.Config.RetryPolicy,
			fmt.Sprintf("%s criterion function %q", location, fn.Name)); err != nil {
			return err
		}
	}
```

Update `validateCriterion`'s doc comment ("malformed in any of four ways" →
five; add the bullet: "a function criterion whose `function` member cannot be
read, or whose `retryPolicy` is outside NONE/FIXED/empty — it would fail every
dispatch, or be silently ignored"), and both rule lists: `M1 — retryPolicy ∈
{NONE, FIXED, ""} on every processor, criterion function and schedule function`.

Docs:
- `workflows.md:489` → `- Unknown `retryPolicy` value on any processor, `function`-type criterion or `schedule.function` (allowed: `NONE`, `FIXED`, or empty).`
  and a new bullet: `- A `function`-type criterion whose `function.config` cannot be read (for example a `responseTimeoutMs` that is not an integer).`
- `docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md:34` — M1 row status: `**Resolved** for `RetryPolicy`: validated at import on processors, criterion functions and schedule functions, and honoured at dispatch.` Line `:329` table cell → `**Validated at import and consumed at dispatch.** Rejected unless ∈ {NONE, FIXED, ""} on a processor, a criterion function and a schedule function; selects the number of tries (`CYODA_RETRY_FIXED_NUM_RETRIES`).` Also correct `:335-336` ("with a 30-second default at line 24, `defaultResponseTimeoutMs`") → "with the default taken from `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`". No issue numbers are added; the ones already in this audit document stay (it is a record, not a shipped artefact).

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/domain/workflow/...` then
`go test ./internal/e2e/... -run 'TestWorkflowImport_'`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/validate.go internal/domain/workflow/validate_import_test.go \
  internal/e2e/workflow_callout_import_test.go cmd/cyoda/help/content/workflows.md \
  docs/WORKFLOW_IMPORT_EXPORT_AUDIT.md
git commit -m "feat(workflow): import validates retryPolicy on a criterion function and a schedule function (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-7: Import refuses a `responseTimeoutMs` that is negative or above the bound

**Spec:** §9 `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` ("Import refuses a larger
`responseTimeoutMs`"); §10 "processors gain the `responseTimeoutMs` bound";
§8.2 row "Import: `responseTimeoutMs` above the upper bound, or negative → 400
`VALIDATION_FAILED`, names the bound"; §13 row (U, E here; P in C-9).

**Files:**
- Modify: `internal/domain/workflow/handler.go` (`Handler` :57-65; import
  handler after `validateImportRequest`, :320-323)
- Modify: `internal/domain/workflow/validate.go` (new `validateCalloutLimits`,
  `checkResponseTimeout`, `sortedStateNames`)
- Modify: `app/app.go:650`
- Modify (constructor callers): `internal/grpc/scheduled_transition_test.go:48`,
  `internal/grpc/scheduled_function_rpc_test.go:80`,
  `internal/domain/workflow/scenarios_test.go:506, 663`
- Test: `internal/domain/workflow/validate_limits_test.go` (new),
  `internal/domain/workflow/handler_test.go`,
  `internal/e2e/workflow_callout_import_test.go`
- Docs: `cmd/cyoda/help/content/workflows.md` (validation list; `:283`),
  `cmd/cyoda/help/content/errors/VALIDATION_FAILED.md:32`

**Interfaces:**
- Consumes: `cfg.Callout.ResponseTimeoutMax` (C-1);
  `contract.ParseCriterionFunction` (C-5).
- Produces:
  ```go
  func New(factory spi.StoreFactory, engine *Engine, maxResponseTimeout time.Duration) *Handler
  ```
  Import checks the **incoming** request only, like every structural rule. A
  **stored** workflow over a bound lowered later is the dispatch stream's
  `Terminal` (§9), not this task's.

- [ ] **Step 1: Write the failing tests**

`internal/domain/workflow/validate_limits_test.go`:

```go
package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const testMaxResponseTimeout = 60 * time.Second

func limitsProcessorWF(ms int64) spi.WorkflowDefinition {
	wf := retryPolicyFixture("")
	wf.States["S1"].Transitions[0].Processors[0].Config.ResponseTimeoutMs = ms
	return wf
}

func limitsFunctionWF(ms int64) spi.WorkflowDefinition {
	wf := scheduleFunctionFixture("")
	wf.States["S1"].Transitions[0].Schedule.Function.ResponseTimeoutMs = ms
	return wf
}

func limitsCriterion(ms int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","responseTimeoutMs":%d}}}`, ms))
}

func TestValidateCalloutLimits(t *testing.T) {
	cases := []struct {
		name    string
		wf      func(ms int64) spi.WorkflowDefinition
		wantLoc []string
	}{
		{"processor", limitsProcessorWF, []string{`workflow "wf-rp"`, `state "S1"`, `transition "t"`, `processor "p"`}},
		{"schedule function", limitsFunctionWF, []string{`workflow "wf-fn"`, `state "S1"`, `transition "t"`, "schedule.function"}},
		{"transition criterion", func(ms int64) spi.WorkflowDefinition {
			return wfWithTransitionCriterion(limitsCriterion(ms))
		}, []string{`workflow "wf-regex"`, `state "S1"`, `transition "go"`, `criterion function "min-amount"`}},
		{"workflow criterion", func(ms int64) spi.WorkflowDefinition {
			return wfWithWorkflowCriterion(limitsCriterion(ms))
		}, []string{`workflow "wf-regex"`, `criterion function "min-amount"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, ok := range []int64{0, 1, 60000} {
				if err := validateCalloutLimits([]spi.WorkflowDefinition{tc.wf(ok)}, testMaxResponseTimeout); err != nil {
					t.Errorf("responseTimeoutMs=%d rejected: %v", ok, err)
				}
			}

			err := validateCalloutLimits([]spi.WorkflowDefinition{tc.wf(60001)}, testMaxResponseTimeout)
			if err == nil {
				t.Fatal("responseTimeoutMs=60001 accepted over a 60000 bound")
			}
			for _, want := range append(tc.wantLoc, "responseTimeoutMs 60001", "60000", "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must contain %q; got: %v", want, err)
				}
			}

			err = validateCalloutLimits([]spi.WorkflowDefinition{tc.wf(-1)}, testMaxResponseTimeout)
			if err == nil {
				t.Fatal("responseTimeoutMs=-1 accepted")
			}
			for _, want := range append(tc.wantLoc, "must not be negative") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must contain %q; got: %v", want, err)
				}
			}
		})
	}
}

// A value that would overflow time.Duration when multiplied out must still be
// compared correctly: the comparison is made in milliseconds.
func TestValidateCalloutLimits_HugeValue(t *testing.T) {
	err := validateCalloutLimits([]spi.WorkflowDefinition{limitsProcessorWF(1 << 62)}, testMaxResponseTimeout)
	if err == nil {
		t.Fatal("responseTimeoutMs=2^62 accepted")
	}
}

func TestValidateCalloutLimits_FollowsTheConfiguredBound(t *testing.T) {
	wf := limitsProcessorWF(6000)
	if err := validateCalloutLimits([]spi.WorkflowDefinition{wf}, 5*time.Second); err == nil {
		t.Fatal("6000 accepted under a 5000 bound")
	}
	if err := validateCalloutLimits([]spi.WorkflowDefinition{wf}, 6*time.Second); err != nil {
		t.Fatalf("6000 rejected under a 6000 bound: %v", err)
	}
}
```

`internal/domain/workflow/handler_test.go` — the setting reaches the handler
(`"time"` added to the imports):

```go
// TestImport_ResponseTimeoutBound_FollowsServerSetting pins the wiring from
// the server setting to the import handler: the same payload is accepted or
// refused depending on the bound the App was built with.
func TestImport_ResponseTimeoutBound_FollowsServerSetting(t *testing.T) {
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	cfg.Callout.ResponseTimeout = 5 * time.Second
	cfg.Callout.ResponseTimeoutMax = 5 * time.Second
	srv := httptest.NewServer(app.New(cfg).Handler())
	t.Cleanup(srv.Close)
	importModel(t, srv.URL, "Order", 1)

	body := func(ms int) string {
		return `{"importMode":"REPLACE","workflows":[{
			"version":"1.1","name":"bound-wf","initialState":"S1","active":true,
			"states":{"S1":{"transitions":[{"name":"go","next":"S2","manual":true,
				"processors":[{"type":"externalized","name":"p","executionMode":"SYNC",
					"config":{"calculationNodesTags":"workers","responseTimeoutMs":` + strconv.Itoa(ms) + `}}]}]},
				"S2":{}}}]}`
	}

	resp := doWorkflowImport(t, srv.URL, "Order", 1, body(5000))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responseTimeoutMs at the bound: expected 200, got %d", resp.StatusCode)
	}

	resp = doWorkflowImport(t, srv.URL, "Order", 1, body(5001))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("responseTimeoutMs over the bound: expected 400, got %d", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, common.ErrCodeValidationFailed)
}
```

`internal/e2e/workflow_callout_import_test.go` — append:

```go
// The e2e server runs with the default bound, 60000 ms.
func TestWorkflowImport_ResponseTimeoutOutOfRange_400(t *testing.T) {
	const entity = "wf-callout-timeout-bound"
	importModelE2E(t, entity, 1)

	procCfg := func(ms int) string {
		return fmt.Sprintf(`{"calculationNodesTags":"workers","responseTimeoutMs":%d}`, ms)
	}
	crit := func(ms int) string {
		return fmt.Sprintf(`{"type":"function","function":{"name":"min-amount","config":{"calculationNodesTags":"pricing","responseTimeoutMs":%d}}}`, ms)
	}
	sched := func(ms int) string {
		return fmt.Sprintf(`{"function":{"name":"computeFire","resultKind":"Schedule","calculationNodesTags":"scheduler","responseTimeoutMs":%d}}`, ms)
	}

	for _, tc := range []struct {
		name    string
		payload func(ms int) string
		where   string
	}{
		{"processor", func(ms int) string { return calloutWorkflow("bound-proc-wf", "", "", procCfg(ms)) }, `processor "p"`},
		{"criterion", func(ms int) string { return calloutWorkflow("bound-crit-wf", crit(ms), "", "") }, `criterion function "min-amount"`},
		{"function", func(ms int) string { return calloutWorkflow("bound-fn-wf", "", sched(ms), "") }, "schedule.function"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := importWorkflowE2E(t, entity, 1, tc.payload(60000)); status != http.StatusOK {
				t.Fatalf("at the bound: expected 200, got %d: %s", status, body)
			}
			importRejection(t, entity, tc.payload(60001), tc.where, "60001", "60000", "CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS")
			importRejection(t, entity, tc.payload(-1), tc.where, "must not be negative")
		})
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/domain/workflow/ -run 'TestValidateCalloutLimits|TestImport_ResponseTimeoutBound'`
Expected: FAIL — build error `undefined: validateCalloutLimits`.
Run: `go test ./internal/e2e/... -run TestWorkflowImport_ResponseTimeoutOutOfRange_400`
Expected: FAIL — `import status = 200; want 400`.

- [ ] **Step 3: Implement**

`internal/domain/workflow/handler.go`:

```go
// Handler implements the workflow import/export HTTP endpoints.
type Handler struct {
	factory spi.StoreFactory
	engine  *Engine
	// maxResponseTimeout is the server's upper bound on a callout's
	// responseTimeoutMs; import refuses a workflow that exceeds it.
	maxResponseTimeout time.Duration
}

// New returns a new Handler wired to the given StoreFactory and Engine.
// maxResponseTimeout is the server's upper bound on responseTimeoutMs
// (app.Config.Callout.ResponseTimeoutMax).
func New(factory spi.StoreFactory, engine *Engine, maxResponseTimeout time.Duration) *Handler {
	return &Handler{factory: factory, engine: engine, maxResponseTimeout: maxResponseTimeout}
}
```

(`"time"` joins the imports.) In `ImportEntityModelWorkflow`, directly after the
`validateImportRequest` block:

```go
	// Callout limits are a server setting, so they are checked here rather
	// than in the setting-free structural validator. Incoming request only,
	// like every structural rule.
	if err := validateCalloutLimits(incoming, h.maxResponseTimeout); err != nil {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeValidationFailed, err.Error()))
		return
	}
```

`internal/domain/workflow/validate.go` (`"time"` joins the imports):

```go
// validateCalloutLimits enforces the server's bound on responseTimeoutMs for
// every callout an incoming workflow configures: each processor, each
// schedule function, and each top-level function criterion (workflow-level and
// transition-level). A negative value is refused whatever the bound. Zero
// means "use the server default" and is always accepted.
//
// Separate from validateWorkflowStructure because the bound is a server
// setting (CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS) handed to the Handler at
// construction, and the structural rules are setting-free. States are walked
// in name order so a workflow with several violations always reports the same
// one.
func validateCalloutLimits(workflows []spi.WorkflowDefinition, maxResponseTimeout time.Duration) error {
	maxMs := maxResponseTimeout.Milliseconds()
	for _, wf := range workflows {
		wfLoc := fmt.Sprintf("workflow %q", wf.Name)
		if err := checkCriterionResponseTimeout(wf.Criterion, wfLoc, maxMs); err != nil {
			return err
		}
		for _, stateName := range sortedStateNames(wf) {
			for _, tr := range wf.States[stateName].Transitions {
				trLoc := fmt.Sprintf("workflow %q state %q transition %q", wf.Name, stateName, tr.Name)
				if err := checkCriterionResponseTimeout(tr.Criterion, trLoc, maxMs); err != nil {
					return err
				}
				if tr.Schedule != nil && tr.Schedule.Function != nil {
					if err := checkResponseTimeout(tr.Schedule.Function.ResponseTimeoutMs, maxMs,
						trLoc+" schedule.function"); err != nil {
						return err
					}
				}
				for _, p := range tr.Processors {
					if err := checkResponseTimeout(p.Config.ResponseTimeoutMs, maxMs,
						fmt.Sprintf("%s processor %q", trLoc, p.Name)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// checkCriterionResponseTimeout applies checkResponseTimeout to a top-level
// function criterion; any other criterion has no callout and passes.
func checkCriterionResponseTimeout(criterion json.RawMessage, location string, maxMs int64) error {
	trimmed := bytes.TrimSpace(criterion)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	cond, err := predicate.ParseCondition(trimmed)
	if err != nil {
		return nil // not this rule's to report; see validateCriterion
	}
	if _, ok := cond.(*predicate.FunctionCondition); !ok {
		return nil
	}
	fn, err := contract.ParseCriterionFunction(trimmed)
	if err != nil {
		return fmt.Errorf("%s: %w", location, err)
	}
	return checkResponseTimeout(fn.Config.ResponseTimeoutMs, maxMs,
		fmt.Sprintf("%s criterion function %q", location, fn.Name))
}

// checkResponseTimeout compares in milliseconds: converting an arbitrary
// client int64 to a time.Duration first could overflow.
func checkResponseTimeout(ms, maxMs int64, location string) error {
	if ms < 0 {
		return fmt.Errorf("%s: responseTimeoutMs must not be negative (got %d)", location, ms)
	}
	if ms > maxMs {
		return fmt.Errorf("%s: responseTimeoutMs %d exceeds this server's upper bound of %d ms (CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS)",
			location, ms, maxMs)
	}
	return nil
}

// sortedStateNames returns wf's state names in lexicographic order.
func sortedStateNames(wf spi.WorkflowDefinition) []string {
	names := make([]string, 0, len(wf.States))
	for name := range wf.States {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

`validateWorkflowLoops` (`:687-691`) builds the same sorted slice inline —
replace those five lines with `stateNames := sortedStateNames(wf)`.

Constructor callers:
- `app/app.go:650` → `workflow.New(a.storeFactory, a.workflowEngine, a.config.Callout.ResponseTimeoutMax)`
  (`a.config` is what the neighbouring line reads for `SearchMaxSortKeys`).
- the two `internal/grpc` tests → `workflow.New(factory, engine, 60*time.Second)`
- `scenarios_test.go:506, 663` → `New(factory, engine, 60*time.Second)`

Docs:
- `workflows.md` validation list, new bullet: `- A `responseTimeoutMs` on a processor, a `function`-type criterion or a `schedule.function` that is negative, or larger than the server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (default `60000`). The bound is a server setting: a workflow exported from one deployment can be refused by another with a lower bound.`
- `workflows.md:283` → `- `responseTimeoutMs` (integer, optional) — how long to wait for this callout's answer, in milliseconds; `0` or absent means the server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MS`; must not exceed `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`.`
- `errors/VALIDATION_FAILED.md:32` — append to the workflow-import sentence: `…, an unknown `retryPolicy` on a processor, criterion function or schedule function, or a `responseTimeoutMs` that is negative or above the server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`.`

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/domain/workflow/... ./internal/grpc/... ./app/... ./cmd/cyoda/...`
then `go test ./internal/e2e/... -run 'TestWorkflowImport_'`   Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/handler.go internal/domain/workflow/validate.go \
  internal/domain/workflow/validate_limits_test.go internal/domain/workflow/handler_test.go \
  internal/domain/workflow/scenarios_test.go internal/grpc/scheduled_transition_test.go \
  internal/grpc/scheduled_function_rpc_test.go app/app.go \
  internal/e2e/workflow_callout_import_test.go cmd/cyoda/help/content/workflows.md \
  cmd/cyoda/help/content/errors/VALIDATION_FAILED.md
git commit -m "feat(workflow): import bounds responseTimeoutMs on every callout by a server setting (#565)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-8: Workflow schema 1.4 → 1.5, and the round-trip of the two new fields

**Spec:** §10 "Schema 1.4 → 1.5 … full ceremony"; §15; §13 row "Import / export
round-trip of `idempotent`, function `retryPolicy`; schema 1.5" (U, E here; P in
C-9); §14 (`workflows/schema-version.md`, `docs/workflow-schema-versioning.md`).

**Files:**
- Modify: `internal/domain/workflow/schemaversion.go` (:41-45, :65)
- Modify: `internal/domain/workflow/default_workflow.json:2`
- Test: `internal/domain/workflow/schemaversion_test.go`,
  `internal/domain/workflow/handler_test.go`,
  `internal/e2e/workflow_schema_version_test.go`
- Docs: `docs/workflow-schema-versioning.md`,
  `cmd/cyoda/help/content/workflows/schema-version.md` (:23, :37, :57, :59, :80),
  `cmd/cyoda/help/content/workflows.md` (:51, :569, :621), `CHANGELOG.md`

**Interfaces:**
- Consumes: `spi.ProcessorConfig.Idempotent`, `spi.ScheduleFunction.RetryPolicy` (C-3).
- Produces: `workflow.CurrentSchemaVersion == "1.5"`;
  `SupportedSchemaRanges == {{Major: 1, MinMinor: 1, MaxMinor: 5}}`.

Ceremony checklist (`docs/workflow-schema-versioning.md` §"Bump rules") and
where each step lands: 1, 2 → `schemaversion.go`; 3 → the doc's Changelog; 4 →
`schema-version.md`; 5 → `workflows.md` literals (`WORKFLOW_SCHEMA_VERSION_UNSUPPORTED.md`
uses `"1.1"` as its example and the OpenAPI `version` description names no
number — both unchanged); 6 → `default_workflow.json`; 7 → **nothing to
rewrite**: no MINOR is retired, so the 284 fixtures stamped `"1.1"` (and the 14
at `"1.3"`) stay valid, exactly as at 1.3→1.4; only tests that assert the
*current* version change.

- [ ] **Step 1: Write the failing tests**

`internal/domain/workflow/schemaversion_test.go` — append:

```go
// TestSchemaVersion_15_DualShape pins the 1.5 contract: 1.5 is current, and
// every MINOR from 1.1 stays accepted (nothing retired).
func TestSchemaVersion_15_DualShape(t *testing.T) {
	if CurrentSchemaVersion != "1.5" {
		t.Fatalf("CurrentSchemaVersion = %q, want 1.5", CurrentSchemaVersion)
	}
	for minor := 1; minor <= 5; minor++ {
		if err := Supports(1, minor); err != nil {
			t.Errorf("Supports(1, %d) = %v, want nil (dual-shape retention)", minor, err)
		}
	}
	if err := Supports(1, 0); err == nil {
		t.Error("Supports(1, 0) = nil; 1.0 stays retired")
	}
	if err := Supports(1, 6); err == nil {
		t.Error("Supports(1, 6) = nil; 1.6 does not exist")
	}
}
```

`internal/domain/workflow/handler_test.go` — append:

```go
// TestImportExport_CalloutFields_RoundTrip pins that idempotent on a processor
// and retryPolicy on a schedule function are accepted by the strict decoder,
// stored, and exported, and that a workflow which never mentions them exports
// without them.
func TestImportExport_CalloutFields_RoundTrip(t *testing.T) {
	srv := newTestServer(t)
	importModel(t, srv.URL, "Order", 1)

	body := `{"importMode":"REPLACE","workflows":[{
		"version":"1.5","name":"callout-fields-wf","initialState":"S1","active":true,
		"states":{
			"S1":{"transitions":[
				{"name":"go","next":"S2","manual":true,
				 "processors":[
					{"type":"externalized","name":"safe","executionMode":"SYNC",
					 "config":{"calculationNodesTags":"workers","idempotent":true,"retryPolicy":"FIXED"}},
					{"type":"externalized","name":"plain","executionMode":"SYNC",
					 "config":{"calculationNodesTags":"workers"}}]},
				{"name":"later","next":"S2",
				 "schedule":{"function":{"name":"computeFire","resultKind":"Schedule",
					"calculationNodesTags":"scheduler","retryPolicy":"NONE"}}}]},
			"S2":{}}}]}`
	resp := doWorkflowImport(t, srv.URL, "Order", 1, body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("import: expected 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	exp := doWorkflowExport(t, srv.URL, "Order", 1)
	defer exp.Body.Close()
	raw, err := io.ReadAll(exp.Body)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var out struct {
		Workflows []struct {
			Version string `json:"version"`
			States  map[string]struct {
				Transitions []struct {
					Name       string `json:"name"`
					Processors []struct {
						Name   string         `json:"name"`
						Config map[string]any `json:"config"`
					} `json:"processors"`
					Schedule *struct {
						Function map[string]any `json:"function"`
					} `json:"schedule"`
				} `json:"transitions"`
			} `json:"states"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode export: %v; raw: %s", err, raw)
	}
	if len(out.Workflows) != 1 || out.Workflows[0].Version != "1.5" {
		t.Fatalf("export must stamp 1.5; raw: %s", raw)
	}
	trs := out.Workflows[0].States["S1"].Transitions
	if len(trs) != 2 || len(trs[0].Processors) != 2 {
		t.Fatalf("unexpected export shape: %s", raw)
	}
	if got := trs[0].Processors[0].Config["idempotent"]; got != true {
		t.Errorf(`processor "safe": idempotent = %v, want true`, got)
	}
	if _, present := trs[0].Processors[1].Config["idempotent"]; present {
		t.Errorf(`processor "plain": idempotent must be omitted when never set; raw: %s`, raw)
	}
	if trs[1].Schedule == nil || trs[1].Schedule.Function["retryPolicy"] != "NONE" {
		t.Errorf("schedule.function.retryPolicy lost on round-trip; raw: %s", raw)
	}
}
```

`internal/e2e/workflow_schema_version_test.go`:
- `TestWorkflowSchemaVersion_ExportStampsCurrent` (`:174-175`): `"1.4"` → `"1.5"` (both the comparison and the message).
- `TestWorkflowSchemaVersion_HelpVersionsAction` (`:197-205`): `current` → `1.5`, `maxMinor` → `5`.
- The header comment of `TestWorkflowSchemaVersion_ImportAcceptsCurrent` (`:10-17`): "As of v0.9.0, "1.5" is CurrentSchemaVersion; "1.1" through "1.4" remain accepted … (SupportedSchemaRanges is {1, 1, 5}) … `TestWorkflowSchemaVersion_ImportAccepts15` covers the new "1.5" shape."
- New test, after `ImportAccepts14`:

```go
// TestWorkflowSchemaVersion_ImportAccepts15 proves the new current MINOR is
// accepted with the two fields 1.4 → 1.5 introduces — idempotent on a
// processor and retryPolicy on a schedule function — and that both come back
// on export.
func TestWorkflowSchemaVersion_ImportAccepts15(t *testing.T) {
	const entity, version = "schemaver-15", 1
	importModelE2E(t, entity, version)
	payload := `{
	  "importMode": "REPLACE",
	  "workflows": [{
	    "version": "1.5", "name": "v15-wf", "initialState": "S", "active": true,
	    "states": { "S": { "transitions": [
	      { "name": "go", "next": "Done", "manual": true,
	        "processors": [ { "type": "externalized", "name": "p", "executionMode": "SYNC",
	          "config": { "calculationNodesTags": "workers", "idempotent": true } } ] },
	      { "name": "later", "next": "Done",
	        "schedule": { "function": { "name": "computeFire", "resultKind": "Schedule",
	          "calculationNodesTags": "scheduler", "retryPolicy": "NONE" } } }
	    ] }, "Done": {} }
	  }]
	}`
	if status, body := importWorkflowE2E(t, entity, version, payload); status != http.StatusOK {
		t.Fatalf("import 1.5: expected 200, got %d: %s", status, body)
	}

	status, exported := exportWorkflowE2E(t, entity, version)
	if status != http.StatusOK {
		t.Fatalf("export: expected 200, got %d", status)
	}
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("re-marshal export: %v", err)
	}
	for _, want := range []string{`"idempotent":true`, `"retryPolicy":"NONE"`, `"version":"1.5"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("export must contain %s; got: %s", want, raw)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/domain/workflow/ -run 'TestSchemaVersion_15_DualShape|TestImportExport_CalloutFields_RoundTrip'`
Expected: FAIL — `CurrentSchemaVersion = "1.4", want 1.5`; the round-trip fails
at import with 400 `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` (minor too new).

- [ ] **Step 3: Implement**

`schemaversion.go` — append to the constant's history comment and bump:

```go
// 1.4 → 1.5 in v0.9.0: additive MINOR — `idempotent` on a processor's config
// and `retryPolicy` on a schedule function. Dual-shape: 1.1 through 1.4 stay
// in SupportedSchemaRanges. The same release validates `retryPolicy` on a
// criterion function and bounds `responseTimeoutMs` on every callout; those
// tightenings are taken in this MINOR, with the reason, in
// docs/workflow-schema-versioning.md §"1.5 — v0.9.0 contract".
const CurrentSchemaVersion = "1.5"
```

```go
var SupportedSchemaRanges = []SchemaRange{
	{Major: 1, MinMinor: 1, MaxMinor: 5},
}
```

`default_workflow.json:2` → `"version": "1.5",`.

`docs/workflow-schema-versioning.md` — demote "### 1.4 — v0.8.4 contract
(current)" to "### 1.4 — v0.8.4 contract" and insert above it:

```markdown
### 1.5 — v0.9.0 contract (current)

Additive MINOR — two new optional fields:

- **`idempotent` on `ExternalizedProcessorConfigDto`** (a processor's
  `config`). Boolean, default `false`, omitted on export when false. The
  author's declaration that the processor may be run again on another compute
  member after one that received the work went silent.
- **`retryPolicy` on `ScheduleFunctionDto`** (`schedule.function`). The same
  `NONE` / `FIXED` selector a processor and a criterion function already carry.

Every payload 1.4 accepted that does not fall under the two tightenings below
is byte-identical and remains valid; a workflow that never mentions either
field exports without them.

**Dual-shape retention of 1.1 through 1.4.** Nothing is retired:
`SupportedSchemaRanges` widens in place to
`{Major: 1, MinMinor: 1, MaxMinor: 5}`.

**Two tightenings taken in the same MINOR.** Unlike the v0.8.4 entries under
"When NOT to bump", these are not bug fixes to a validator that was always
meant to reject — the inputs below imported and *worked* — so they are recorded
here, under the rubric of §"Tightening releases":

1. **`retryPolicy` on a `function`-type criterion is validated** (`NONE`,
   `FIXED` or empty), as it has been on a processor since 1.1. Until now any
   string was accepted there and ignored, because nothing consumed the field.
   From this release it selects the number of tries, so an unknown value has no
   meaning to honour. Rubric point 1 applies: fail-loud where the old behaviour
   was a silent no-op. A `function` member that cannot be read at all (for
   example a non-integer `responseTimeoutMs`) is refused on the same pass; that
   criterion already failed every evaluation with "invalid criterion JSON", so
   no working configuration is affected.
2. **`responseTimeoutMs` is bounded** on a processor, a criterion function and
   a schedule function: negative is refused, and so is a value above the
   server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (default `60000`). This
   one does reject configurations that worked. It is taken because tries now
   multiply the answer limit: without a bound, four tries at a generous limit
   outlast the five minutes PostgreSQL allows a transaction to sit idle, and
   the operation fails as "storage unavailable" instead of as a callout
   failure. The bound is a server setting, so whether a given payload imports
   depends on the deployment; the error names the setting and the bound.

**Why dual-shape and not retirement.** Rubric point 2 asks whether an older
payload "happens to import only by coincidence". It does not: the two rules
touch two optional fields, the error names the offending workflow, state,
transition and callout, and there are no deployments of either tier whose
stored workflows could be affected. Retiring 1.1–1.4 would turn 298 fixtures
and every client's existing files into `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED`
to guard two fields. Both rules apply to an import under **any** schema
version, 1.1 through 1.5 alike — they are validation-layer rules, not
DTO-shape ones.

Neither rule is retroactive: a workflow already stored is not re-checked at
import of another. A stored `responseTimeoutMs` above a bound that was lowered
later is not clamped; its callout fails, naming the setting.
```

`cmd/cyoda/help/content/workflows/schema-version.md`: the three `1.4` literals
→ `1.5`; `"maxMinor": 4` → `5`; replace the "Current contract" paragraph with:

```markdown
Current contract: **1.5**, which added `idempotent` to a processor's `config` and `retryPolicy` to `schedule.function` (dual-shape: 1.1 through 1.4 remain accepted). The same release validates `retryPolicy` on a `function`-type criterion and bounds `responseTimeoutMs` on every callout by the server's `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`; both rules apply to an import under any schema version. Because the bound is a server setting, a workflow exported from one deployment can be refused by another with a lower bound. 1.4 added the `NOT` group operator. Not every behaviour change bumps this contract — see `docs/workflow-schema-versioning.md` for the rationale behind each decision.
```

`cmd/cyoda/help/content/workflows.md` lines 51, 569, 621: `"1.4"` → `"1.5"`.

`CHANGELOG.md` `[Unreleased]`:

```markdown
### Added
- **Workflow schema 1.5.** `idempotent` (boolean, default false) on a
  processor's `config`, and `retryPolicy` (`NONE` / `FIXED`) on
  `schedule.function`. 1.1 through 1.4 stay accepted. Needs the matching
  `cyoda-go-spi` release (`ProcessorConfig.Idempotent`,
  `ScheduleFunction.RetryPolicy`); no storage backend changes.

### Breaking
- **Workflow import refuses two things it used to accept**, under every schema
  version: a `retryPolicy` other than `NONE`, `FIXED` or empty on a
  `function`-type criterion (it was accepted and ignored), and a
  `responseTimeoutMs` — on a processor, a criterion function or a schedule
  function — that is negative or above `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`
  (default `60000`). Both answer `400 VALIDATION_FAILED` naming the workflow,
  state, transition and callout. The bound is a server setting: a workflow
  exported from one deployment can be refused by another with a lower bound.
  See `docs/workflow-schema-versioning.md`.
```

Exit check: `grep -rn '"1\.4"' internal/domain/workflow/schemaversion.go internal/domain/workflow/default_workflow.json internal/e2e/workflow_schema_version_test.go cmd/cyoda/help/content/` → only the `ImportAccepts14` payload and the history comment in `schemaversion.go` remain.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/domain/workflow/... ./cmd/cyoda/...` (includes
`TestCurrentSchemaVersionIsSupported`, `TestOpenAPIWorkflowVersionContract`,
`TestDefaultWorkflowFixtureSchemaVersion`, `TestValidateSchemaVersions_*`,
`TestEmitWorkflowSchemaVersions_StructuredJSON`); then
`go test ./internal/e2e/... -run 'TestWorkflowSchemaVersion_'`; then
`go run ./cmd/cyoda help workflows schema-version versions` →
`{"current":"1.5","supported":[{"major":1,"minMinor":1,"maxMinor":5}]}`.

- [ ] **Step 5: Commit**

```
git add internal/domain/workflow/schemaversion.go internal/domain/workflow/schemaversion_test.go \
  internal/domain/workflow/default_workflow.json internal/domain/workflow/handler_test.go \
  internal/e2e/workflow_schema_version_test.go docs/workflow-schema-versioning.md \
  cmd/cyoda/help/content/workflows/schema-version.md cmd/cyoda/help/content/workflows.md CHANGELOG.md
git commit -m "feat(workflow): schema 1.5 — idempotent on a processor, retryPolicy on a schedule function (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-9: Cross-backend parity for the three import rows

**Spec:** §13 rows "Import: criterion / function `retryPolicy` invalid → 400",
"Import: `responseTimeoutMs` over the bound; negative → 400", "Import / export
round-trip … schema 1.5" — P column. `.claude/rules/test-coverage.md`.

**Files:**
- Create: `e2e/parity/workflow_callout_config.go`
- Modify: `e2e/parity/registry.go` (three entries; header count),
  `e2e/parity/registry_count_test.go` (`wantParityScenarioCount`)

**Interfaces:**
- Consumes: the fixture server running with the **default**
  `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (60000). The stream that lowers
  `CYODA_DISPATCH_WAIT_TIMEOUT` for the parity packages must not set this one.
- Produces: scenarios `WorkflowImportCalloutRetryPolicyValidated`,
  `WorkflowImportResponseTimeoutBounded`, `WorkflowCalloutFieldsRoundTrip`. They
  use only import/export, need no compute client, and so run unchanged on the
  commercial backend at its next dependency update (it takes the SPI fields
  with the same update).

Why parity and not single-backend: the rejection must mean "never stored" on
every backend, and the round-trip is the claim that each backend's one-document
persistence carries the new fields (R§8) — a claim about all of them.

- [ ] **Step 1: Write the scenarios and register them** — `e2e/parity/workflow_callout_config.go`:

```go
package parity

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// workflow_callout_config.go pins what workflow import accepts and refuses in
// the configuration of a callout — a processor, a function criterion, a
// schedule function — and that the fields it accepts survive storage.

// calloutConfigWorkflow builds a workflow with one manual transition carrying
// an optional function criterion and an optional processor, and one scheduled
// transition when schedule is non-empty. Arguments are raw JSON or "".
func calloutConfigWorkflow(wfName, criterion, processorConfig, schedule string) string {
	manual := `"name": "go", "next": "DONE", "manual": true`
	if criterion != "" {
		manual += `, "criterion": ` + criterion
	}
	if processorConfig != "" {
		manual += fmt.Sprintf(`, "processors": [{"type":"externalized","name":"p","executionMode":"SYNC","config": %s}]`, processorConfig)
	}
	transitions := "{" + manual + "}"
	if schedule != "" {
		transitions += fmt.Sprintf(`, {"name": "later", "next": "DONE", "schedule": %s}`, schedule)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.5", "name": %q, "initialState": "OPEN", "active": true,
			"states": {"OPEN": {"transitions": [%s]}, "DONE": {}}
		}]
	}`, wfName, transitions)
}

func calloutConfigModel(t *testing.T, c *client.Client, modelName string) {
	t.Helper()
	if err := c.ImportModel(t, modelName, 1, `{"name":"Test","amount":0}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
}

func criterionWithConfig(config string) string {
	return fmt.Sprintf(`{"type":"function","function":{"name":"min-amount","config":%s}}`, config)
}

func scheduleWithFunction(extra string) string {
	return fmt.Sprintf(`{"function":{"name":"computeFire","resultKind":"Schedule","calculationNodesTags":"scheduler"%s}}`, extra)
}

// RunWorkflowImportCalloutRetryPolicyValidated: retryPolicy is NONE, FIXED or
// absent on all three callout kinds; anything else is 400 VALIDATION_FAILED.
func RunWorkflowImportCalloutRetryPolicyValidated(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-callout-retry-policy"
	calloutConfigModel(t, c, modelName)

	for _, policy := range []string{"NONE", "FIXED"} {
		body := calloutConfigWorkflow("retry-ok-wf",
			criterionWithConfig(fmt.Sprintf(`{"calculationNodesTags":"pricing","retryPolicy":%q}`, policy)),
			fmt.Sprintf(`{"calculationNodesTags":"workers","retryPolicy":%q}`, policy),
			scheduleWithFunction(fmt.Sprintf(`,"retryPolicy":%q`, policy)))
		status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, body)
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", policy, err)
		}
		if status != http.StatusOK {
			t.Fatalf("retryPolicy %q on all three callouts: expected 200, got %d; body=%s", policy, status, resp)
		}
	}

	for _, tc := range []struct{ label, body string }{
		{"processor", calloutConfigWorkflow("retry-bad-wf", "", `{"calculationNodesTags":"workers","retryPolicy":"LINEAR"}`, "")},
		{"criterion", calloutConfigWorkflow("retry-bad-wf", criterionWithConfig(`{"calculationNodesTags":"pricing","retryPolicy":"LINEAR"}`), "", "")},
		{"function", calloutConfigWorkflow("retry-bad-wf", "", "", scheduleWithFunction(`,"retryPolicy":"LINEAR"`))},
	} {
		status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, tc.body)
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] unknown retryPolicy: expected 400, got %d; body=%s", tc.label, status, resp)
		}
		if !containsErrorCode(resp, "VALIDATION_FAILED") {
			t.Errorf("[%s] expected errorCode VALIDATION_FAILED, body=%s", tc.label, resp)
		}
		if !bytes.Contains(resp, []byte("retry-bad-wf")) {
			t.Errorf("[%s] detail must name the workflow, body=%s", tc.label, resp)
		}
	}
}

// RunWorkflowImportResponseTimeoutBounded: responseTimeoutMs is accepted up to
// the server's upper bound (the fixture runs with the default, 60000) and
// refused when negative or above it, on all three callout kinds.
func RunWorkflowImportResponseTimeoutBounded(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-callout-timeout-bound"
	calloutConfigModel(t, c, modelName)

	build := map[string]func(ms int) string{
		"processor": func(ms int) string {
			return calloutConfigWorkflow("bound-wf", "", fmt.Sprintf(`{"calculationNodesTags":"workers","responseTimeoutMs":%d}`, ms), "")
		},
		"criterion": func(ms int) string {
			return calloutConfigWorkflow("bound-wf", criterionWithConfig(fmt.Sprintf(`{"calculationNodesTags":"pricing","responseTimeoutMs":%d}`, ms)), "", "")
		},
		"function": func(ms int) string {
			return calloutConfigWorkflow("bound-wf", "", "", scheduleWithFunction(fmt.Sprintf(`,"responseTimeoutMs":%d`, ms)))
		},
	}
	for _, kind := range []string{"processor", "criterion", "function"} {
		status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, build[kind](60000))
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", kind, err)
		}
		if status != http.StatusOK {
			t.Fatalf("[%s] responseTimeoutMs at the bound: expected 200, got %d; body=%s", kind, status, resp)
		}
		for _, bad := range []int{60001, -1} {
			status, resp, err := c.ImportWorkflowRaw(t, modelName, 1, build[kind](bad))
			if err != nil {
				t.Fatalf("[%s %d] ImportWorkflowRaw: %v", kind, bad, err)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("[%s] responseTimeoutMs %d: expected 400, got %d; body=%s", kind, bad, status, resp)
			}
			if !containsErrorCode(resp, "VALIDATION_FAILED") {
				t.Errorf("[%s] responseTimeoutMs %d: expected errorCode VALIDATION_FAILED, body=%s", kind, bad, resp)
			}
		}
	}
}

// RunWorkflowCalloutFieldsRoundTrip: idempotent on a processor and retryPolicy
// on a schedule function are stored and exported by every backend, stamped
// with the current schema version.
func RunWorkflowCalloutFieldsRoundTrip(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelName = "parity-callout-fields-roundtrip"
	calloutConfigModel(t, c, modelName)

	body := calloutConfigWorkflow("callout-fields-wf", "",
		`{"calculationNodesTags":"workers","idempotent":true}`,
		scheduleWithFunction(`,"retryPolicy":"NONE"`))
	if err := c.ImportWorkflow(t, modelName, 1, body); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
	raw, err := c.ExportWorkflow(t, modelName, 1)
	if err != nil {
		t.Fatalf("ExportWorkflow: %v", err)
	}
	for _, want := range []string{`"idempotent":true`, `"retryPolicy":"NONE"`, `"version":"1.5"`} {
		if !bytes.Contains(compactJSON(t, raw), []byte(want)) {
			t.Errorf("export must contain %s; got: %s", want, raw)
		}
	}
}
```

with, in the same file (`"encoding/json"` added to the imports):

```go
func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact export: %v", err)
	}
	return buf.Bytes()
}
```

`registry.go`, after `{"WorkflowImportExport", RunWorkflowImportExport},`:

```go
	// What import accepts and refuses in a callout's configuration, and that
	// the accepted fields survive each backend's storage.
	{"WorkflowImportCalloutRetryPolicyValidated", RunWorkflowImportCalloutRetryPolicyValidated},
	{"WorkflowImportResponseTimeoutBounded", RunWorkflowImportResponseTimeoutBounded},
	{"WorkflowCalloutFieldsRoundTrip", RunWorkflowCalloutFieldsRoundTrip},
```

`registry_count_test.go` `wantParityScenarioCount` and the `registry.go` header
comment: **+3** on the value present when this task runs (275 on the base
branch → 278; other streams add scenarios of their own).

- [ ] **Step 2: Run to verify RED** — the production rules are already green
(C-6..C-8), so the scenarios' teeth are shown against the registry and against
one reverted rule:
  1. With the three registry entries added and the count not yet bumped:
     `go test ./e2e/parity/ -run TestParityScenarioCount` → FAIL
     `parity scenario count = 278, want 275`.
  2. Temporarily delete the `validateCalloutLimits` call in `handler.go`
     (working tree only; no stash — the stash stack is shared between
     worktrees), run
     `go test -count=1 ./e2e/parity/memory/ -run 'TestParity/WorkflowImportResponseTimeoutBounded'`
     → FAIL `responseTimeoutMs 60001: expected 400, got 200`; then restore with
     `git checkout -- internal/domain/workflow/handler.go` and confirm
     `git status --short internal/` is empty.

- [ ] **Step 3: Implement** — no production code: C-6..C-8 are the
implementation. Bump `wantParityScenarioCount` and the `registry.go` header
count by 3.

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./e2e/parity/` then, per backend,
`go test -count=1 ./e2e/parity/memory/ -run 'TestParity/(WorkflowImportCallout|WorkflowImportResponseTimeout|WorkflowCalloutFields)'`
and the same for `./e2e/parity/sqlite/` and `./e2e/parity/postgres/`.
Expected: PASS on all three.

- [ ] **Step 5: Commit**

```
git add e2e/parity/workflow_callout_config.go e2e/parity/registry.go e2e/parity/registry_count_test.go
git commit -m "test(parity): what import accepts in a callout's configuration, on every backend (#254, #565)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task C-10: Pin the pushed SPI commit; `COMPATIBILITY.md`

**Spec:** §10 "Both ride one SPI change, pseudo-pinned per the
coordinated-release procedure; Paul cuts the tag"; §14 "`COMPATIBILITY.md`: the
SPI pin". Procedure: `MAINTAINING.md` §"Coordinated release across sibling
repos", §"Bumping cyoda-go-spi", §3 "Dev-window plugin composition".

**Runs:** after every task of every stream that touches Go code is green
(the pin belongs "near the end"), and before C-11 only in the sense that C-11
needs no SPI change — C-11 may follow it.

**Files:**
- Modify: `go.mod`, `go.sum`, `plugins/memory/go.mod`, `plugins/memory/go.sum`,
  `plugins/sqlite/go.mod`, `plugins/sqlite/go.sum`, `plugins/postgres/go.mod`,
  `plugins/postgres/go.sum`, `COMPATIBILITY.md`
- Not committed, ever: `go.work`

- [ ] **Step 1 — LEAD: publish the SPI commit.**
  `git -C /Users/paul/go-projects/cyoda-light/cyoda-go-spi push -u origin feat/callout-idempotent-retry-policy`,
  open the SPI pull request, and get it merged to SPI `main` **as a merge or
  fast-forward that keeps the commit SHA** — or, if it is squashed, use the
  resulting `main` SHA below. `MAINTAINING.md` (SPI): cyoda-go pins a
  pseudo-version *against `main`*; a pin to a feature-branch commit that a
  squash later orphans stops resolving for every `GOWORK=off` consumer once the
  branch is deleted. No tag is cut: the maintainer cuts the signed SPI tag at
  the end of the milestone.
  Record: `SPI_SHA=<the 40-hex commit on origin/main>`.

- [ ] **Step 2: Show the stale pin (the RED of this task).**
  `GOWORK=off go build ./...`
  Expected: FAIL — `fn.RetryPolicy undefined` / `unknown field Idempotent`-class
  errors from the root module, because all four `go.mod`s still name `1ab57a60382c`.

- [ ] **Step 3: Move all four pins, in one commit.**

```
for d in . plugins/memory plugins/sqlite plugins/postgres; do
  ( cd "$d" && GOWORK=off GOFLAGS=-mod=mod go get github.com/cyoda-platform/cyoda-go-spi@"$SPI_SHA" )
done
make check-spi-pin-sync
GOWORK=off go build ./...
for p in memory sqlite postgres; do ( cd plugins/$p && GOWORK=off go build ./... ); done
git diff --stat -- go.mod go.sum plugins/*/go.mod plugins/*/go.sum
```
Expected: pin-sync OK; all four builds pass; the diff changes only the
`cyoda-go-spi` require line and its two `go.sum` lines per module. If `go get`
moved any other requirement, revert that line by hand — this commit is the pin
and nothing else. No `go mod tidy` per module (it ignores `go.work`).

- [ ] **Step 4: `COMPATIBILITY.md`.** The section "The SPI pin during a
  milestone" says "**Currently pinned: `cyoda-go-spi v0.8.4`**" and the section
  "SPI work in flight for the next release" says the change "exists only on a
  local checkout … not pushed, not tagged, and not pinned" and that the local
  build uses an uncommitted `go.work` `replace`. Both are already false on the
  base branch (all four manifests pin `v0.8.5-0.20260918004138-1ab57a60382c`;
  composition is a `use` line, not a `replace`). Rewrite, present tense:
  - "Currently pinned: a pseudo-version of `cyoda-go-spi` `main` at
    `<first 12 of SPI_SHA>` (`<the pseudo-version string go get wrote>`); the
    real tag replaces it at the release cut."
  - "SPI work in flight": the surface is pushed and pinned; list, additively to
    the entries already there (`ErrEntityModelMismatch`, `ErrTxNotCommitted`,
    the conformance cases, the documented instants): "**`ProcessorConfig.Idempotent`
    and `ScheduleFunction.RetryPolicy`** — two optional workflow-configuration
    fields (workflow schema 1.4 → 1.5). Additive and `omitempty`: no interface
    changes, no conformance case, nothing for a storage plugin to implement;
    an out-of-tree plugin that persists the workflow as one document carries
    them with no change."
  - Delete the sentence about an uncommitted `go.work` `replace`; say
    "composes locally through an uncommitted `go.work` `use` line".

- [ ] **Step 5: Drop the local composition line, verify, commit.**

```
go work edit -dropuse /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git status --short go.work        # expected: no output — go.work equals HEAD again
go build ./... && go vet ./...
git add go.mod go.sum plugins/memory/go.mod plugins/memory/go.sum \
  plugins/sqlite/go.mod plugins/sqlite/go.sum plugins/postgres/go.mod plugins/postgres/go.sum \
  COMPATIBILITY.md
git commit -m "chore(deps): pin cyoda-go-spi at the commit carrying idempotent and the schedule function's retryPolicy (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

- [ ] **Step 6 — LEAD: push the cyoda-go branch.**

- [ ] **Step 7: Re-pin the plugins to the pushed HEAD — a NEW commit, never an amend.**
  The three plugin modules changed (their `go.mod`/`go.sum`), and the
  `GOWORK=off` CI jobs (`per-module-hygiene`, `smoke`) resolve the root
  manifest as a consumer would.

```
make repin-plugins        # resolves plugins@HEAD from origin: HEAD must be pushed (Step 6)
git add go.mod go.sum
git commit -m "chore(deps): pseudo-pin the plugin modules at the SPI pin commit

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
  Amending the commit `repin-plugins` pointed at would orphan the pinned SHA on
  origin; the root pin sitting one commit behind HEAD is the documented state.
  Any later commit that changes a file under `plugins/` repeats Steps 6-7.

- [ ] **Step 8 — LEAD: push again.** Then the whole-branch verification
  (`make test-full`, `go vet ./...`, `make race`) is the lead's final gate, not
  this task's.

---

### Task C-11 (LAST): Remove `CYODA_TX_TOKEN_TTL`

**Spec:** §9 row "`CYODA_TX_TOKEN_TTL` — removed: a pass no longer has a fixed
life"; §15 `### Breaking`.

**Precondition (Consumes):** the stream that rewrites pass minting (§7: a pass
is minted per try with `ExpiresAt = now + answer limit +
cfg.Callout.PassAllowance`) has landed, so that **nothing reads
`cfg.Cluster.TxTokenTTL`**. Today's readers, exactly: `app/app.go:441`
(fifth argument of `internalgrpc.NewProcessorDispatcher`) and `app/app.go:554`
(last argument of `clusterdispatch.NewClusterDispatcher`). Gate before starting:

```
grep -rn 'TxTokenTTL' --include='*.go' . | grep -v '^./docs/'
```
must list only `app/config.go`, `internal/cluster/config.go` and
`app/config_registry_binding_test.go`. If `app/app.go` or anything under
`internal/` still appears, stop: this task is blocked on that stream.

**Files:**
- Modify: `internal/cluster/config.go:20`, `app/config.go:413-416`,
  `app/config_registry_binding_test.go:122`,
  `cmd/cyoda/help/config_registry.go:80`,
  `cmd/cyoda/help/content/config/cluster.md:29`, `docs/ARCHITECTURE.md:1616`,
  `CHANGELOG.md`
- Test: `cmd/cyoda/help/config_registry_test.go`
- Left alone (history): `CHANGELOG.md:2689`, `COMPATIBILITY.md:37`,
  `docs/release-notes/v0-8-2.md:140`, `docs/superpowers/**`

- [ ] **Step 1: Write the failing test** — `cmd/cyoda/help/config_registry_test.go`:

```go
// TestRetiredSettings_Absent pins that a setting the server no longer reads
// is gone from the registry and from the config help, so `cyoda help config`
// does not advertise a knob that does nothing. (A retired name left in Go
// source is caught the other way round, by TestConfig_EnvVarCoverage.)
func TestRetiredSettings_Absent(t *testing.T) {
	retired := []string{"CYODA_TX_TOKEN_TTL"}

	for _, v := range RootConfigVars() {
		for _, name := range retired {
			if v.Name == name {
				t.Errorf("%s is retired but still in rootConfigVars", name)
			}
		}
	}
	documented := scanEnvVarsInConfigDocs(t, filepath.Join(repoRoot(t), "cmd/cyoda/help/content"))
	for _, name := range retired {
		if documented[name] {
			t.Errorf("%s is retired but still documented under content/config", name)
		}
	}
}
```

(`"path/filepath"` is added to the file's imports — it has none today; `repoRoot` and
`scanEnvVarsInConfigDocs` are in `help_test.go`, same package.)

- [ ] **Step 2: Run to verify RED**

Run: `go test ./cmd/cyoda/help/ -run TestRetiredSettings_Absent`
Expected: FAIL — both messages for `CYODA_TX_TOKEN_TTL`.

- [ ] **Step 3: Implement**

- `internal/cluster/config.go` — delete the line `TxTokenTTL time.Duration`.
- `app/config.go` — delete lines 413-416 (the three-line comment and
  `TxTokenTTL: envDuration("CYODA_TX_TOKEN_TTL", 90*time.Second),`).
- `app/config_registry_binding_test.go` — delete the `"CYODA_TX_TOKEN_TTL"` entry.
- `cmd/cyoda/help/config_registry.go` — delete the `CYODA_TX_TOKEN_TTL` row.
- `cmd/cyoda/help/content/config/cluster.md` — delete the `CYODA_TX_TOKEN_TTL` bullet.
- `docs/ARCHITECTURE.md:1616` — delete the table row.
- `CHANGELOG.md` `[Unreleased]` → `### Breaking`:

```markdown
- **`CYODA_TX_TOKEN_TTL` is removed.** The transaction token given to a compute
  member no longer has a fixed life: it is minted per try and lives for that
  try's answer limit plus `CYODA_CALLOUT_PASS_ALLOWANCE` (default `30s`). The
  variable is ignored if set; remove it from deployment configuration.
```

Exit checks:
```
grep -rn 'TxTokenTTL' --include='*.go' .                     → no hits
grep -rn 'CYODA_TX_TOKEN_TTL' --include='*.go' .             → no hits
grep -rn 'CYODA_TX_TOKEN_TTL' cmd/ app/ internal/ plugins/ deploy/ README.md docs/ARCHITECTURE.md → no hits
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go build ./... && go test ./app/... ./cmd/cyoda/... ./internal/cluster/...`
Expected: PASS (`TestRootConfigVars_MatchDefaults` proves the registry and
`defaultFor` lost the same row).

- [ ] **Step 5: Commit**

```
git add internal/cluster/config.go app/config.go app/config_registry_binding_test.go \
  cmd/cyoda/help/config_registry.go cmd/cyoda/help/config_registry_test.go \
  cmd/cyoda/help/content/config/cluster.md docs/ARCHITECTURE.md CHANGELOG.md
git commit -m "feat(config)!: remove CYODA_TX_TOKEN_TTL — a pass lives as long as its try (#254)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

No file under `plugins/` changes, so C-10 Steps 6-7 are not repeated.

---

## Coverage carried forward (§13 rows of this stream)

| Row | U | E | G | P | M |
|---|---|---|---|---|---|
| Import: criterion / function `retryPolicy` invalid → 400 | C-6 (`TestValidateImportRequest_CriterionRetryPolicy`, `…_ScheduleFunctionRetryPolicy`); C-5 (parser) | C-6 (`TestWorkflowImport_CriterionRetryPolicyInvalid_400`, `…_FunctionRetryPolicyInvalid_400`, `…_CalloutRetryPolicyValid_200`) | — no gRPC import door | C-9 `WorkflowImportCalloutRetryPolicyValidated` | — |
| Import: `responseTimeoutMs` over the bound; negative → 400 | C-7 (`TestValidateCalloutLimits*`, `TestImport_ResponseTimeoutBound_FollowsServerSetting`) | C-7 (`TestWorkflowImport_ResponseTimeoutOutOfRange_400`, three callout kinds × over / negative / at-bound) | — | C-9 `WorkflowImportResponseTimeoutBounded` | — |
| Import / export round-trip of `idempotent`, function `retryPolicy`; schema 1.5 | C-3 (SPI), C-4 (DTO), C-8 (`TestImportExport_CalloutFields_RoundTrip`, `TestSchemaVersion_15_DualShape`) | C-8 (`TestWorkflowSchemaVersion_ImportAccepts15`, `…ExportStampsCurrent`, `…HelpVersionsAction`) | — | C-9 `WorkflowCalloutFieldsRoundTrip` | — |
| Each new or newly validated setting: default, valid, out of range → startup error | C-1, C-2 (defaults, env binding, every out-of-range case, `Config.Validate()`; the four guards) ; C-11 (retired setting absent) | — | — | — | — |

Existing tests that pin behaviour this stream changes:
- `TestValidateImportRequest_RejectsUnknownRetryPolicy`, `…AcceptsEmptyRetryPolicy`, `…AcceptsAllKnownRetryPolicies` — **stay**, message text unchanged (C-6 routes the processor rule through `checkRetryPolicy`).
- `TestConfig_Validate` — **extended** (C-1, C-2); helper renamed `validConfig`.
- `TestRootConfigVars_MatchDefaults` — **extended** via `defaultFor` (C-1, C-2), row removed (C-11).
- `TestScheduleFunctionDtoSchema`, `TestScheduleFunctionDtoJSONRoundTrip` — **extended** with `retryPolicy` (C-4).
- `TestSchedule_RoundTrip_Function` (`schedule_roundtrip_test.go:88`) — **stays**; `got != fn` keeps compiling because `RetryPolicy` is a string.
- `TestWorkflowSchemaVersion_ExportStampsCurrent`, `…HelpVersionsAction` — **rewritten** to 1.5 / `maxMinor: 5` (C-8).
- `TestDefaultWorkflowFixtureSchemaVersion`, `TestCurrentSchemaVersionIsSupported`, `TestEmitWorkflowSchemaVersions_StructuredJSON` — **stay**; they follow the constants.
- `TestDefaultTree_ConfigClusterSubtopic` (`help_test.go:927`) — **stays**; `CYODA_DISPATCH_WAIT_TIMEOUT` remains in `config.cluster`.
- `TestParityScenarioCount` — count +3 (C-9).
- The `internal/grpc` criteria-dispatch tests — **stay**; C-5's swap is behaviour-preserving.

## Stream interface summary

**Other streams may consume from C:**

```go
// app.Config — C-1, C-2
cfg.Callout.FixedNumRetries    int            // >= 0; retries after the first try
cfg.Callout.ResponseTimeout    time.Duration  // default answer limit; 1ms..ResponseTimeoutMax
cfg.Callout.ResponseTimeoutMax time.Duration  // upper bound on responseTimeoutMs
cfg.Callout.HandoverAllowance  time.Duration  // > 0
cfg.Callout.PassAllowance      time.Duration  // > 0
cfg.Cluster.DispatchWaitTimeout    time.Duration // >= 0; the patience; 0 = do not wait  (existing field)
cfg.Cluster.DispatchConnectTimeout time.Duration // > 0                                   (new field)
cfg.Cluster.DispatchForwardTimeout time.Duration // > 0; scheduler RPC client only         (existing field)
func app.ValidateCallout(app.CalloutConfig) error
func app.ValidateDispatch(cluster.Config) error

// SPI — C-3 (available through go.work from C-3 Step 6; pinned by C-10)
spi.ProcessorConfig.Idempotent bool    // json:"idempotent,omitempty"
spi.ScheduleFunction.RetryPolicy string // json:"retryPolicy,omitempty"

// internal/contract — C-5
type contract.CriterionFunctionConfig struct{ CalculationNodesTags string; AttachEntity *bool; ResponseTimeoutMs int64; RetryPolicy string; Context string }
type contract.CriterionFunction struct{ Name string; Config CriterionFunctionConfig }
func contract.ParseCriterionFunction(criterion json.RawMessage) (contract.CriterionFunction, error)

// internal/domain/workflow
const workflow.RetryPolicyNone = "NONE"; const workflow.RetryPolicyFixed = "FIXED"  // existing
func workflow.New(factory spi.StoreFactory, engine *workflow.Engine, maxResponseTimeout time.Duration) *workflow.Handler // C-7
workflow.CurrentSchemaVersion == "1.5"  // C-8
```

Guarantees other streams can rely on: every imported callout has
`retryPolicy ∈ {"", "NONE", "FIXED"}` and `0 ≤ responseTimeoutMs ≤ bound at
import time`. Not guaranteed: that a **stored** value is within the *current*
bound — the dispatch stream owns the `Terminal` for that (§9), and the U test in
§13 row "Stored `responseTimeoutMs` over a lowered bound".

**C consumes from other streams:**
- C-11: nothing reads `cfg.Cluster.TxTokenTTL` — i.e. `app/app.go:441` and
  `app/app.go:554` no longer pass it (pass-minting / Coordinator stream).
- C-1: that stream also deletes `defaultResponseTimeoutMs`
  (`internal/grpc/dispatch.go:33`) in favour of `cfg.Callout.ResponseTimeout`.
- C-5: if `DispatchCriteria` is rewritten before C-5 runs, the rewrite uses
  `contract.ParseCriterionFunction` and C-5's dispatch.go step is a no-op.
- C-9: parity fixtures keep the default `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS`.
- C-10: runs after all Go-touching tasks of all streams; LEAD performs the
  three pushes.
- Shared files with other streams (merge by hand, no semantic overlap):
  `CHANGELOG.md`, `cmd/cyoda/help/content/workflows.md` (C touches only the
  version literals, the static-validation list, and the `responseTimeoutMs`
  lines at :191/:283 — the `idempotent`/`retryPolicy` field prose and the
  "captured but not consumed" note at :192-199 are the behaviour stream's),
  `errors/DISPATCH_TIMEOUT.md` (one phrase), `docs/ARCHITECTURE.md` (settings
  tables only), `e2e/parity/registry_count_test.go`.

## Open points

1. **`COMPATIBILITY.md` is already stale on the base branch** (`:77` "Currently
   pinned: `cyoda-go-spi v0.8.4`"; `:86-94` "exists only on a local checkout …
   not pinned … uncommitted `go.work` `replace`"), while all four `go.mod`s pin
   `v0.8.5-0.20260918004138-1ab57a60382c`. C-10 Step 4 corrects it rather than
   appending to a false paragraph. It does not list `ErrTxNotCommitted` either
   (SPI CHANGELOG `[Unreleased]`, first entry).
2. **The SPI pin must name a commit that survives.** The brief says "pins a
   pseudo-version of the PUSHED SPI commit"; SPI `MAINTAINING.md:24-27` says
   the pin is "against `main`". A pin to a feature-branch SHA stops resolving
   for `GOWORK=off` consumers if the SPI PR is squash-merged and the branch
   deleted. C-10 Step 1 therefore asks the lead to merge first and pin the
   `main` SHA. If the lead prefers to pin the branch commit during review, the
   pin commit has to be redone after the merge.
3. **`make repin-plugins` does not move the SPI pin** — it only pseudo-pins the
   three plugin modules in the root `go.mod` (`scripts/repin-plugins.sh`). The
   SPI require line is moved by hand in all four modules (C-10 Step 3). The
   brief's wording reads as if one command did both.
4. **Schema-versioning doc step 7** ("update every workflow-typed test
   fixture") was not followed at 1.2, 1.3 or 1.4 and is not followed here: 284
   fixtures are stamped `"1.1"` and remain valid under dual-shape retention.
   The step only bites on retirement. Suggest the doc say so; not changed in
   this stream because it is a wording decision about the procedure itself.
5. **A `responseTimeoutMs` bound is not a "fail-loud where it was a silent
   no-op" tightening** (rubric point 1): it rejects configurations that work
   today. Spec §10 says to record it as a same-MINOR tightening "with the
   reason"; C-8's doc entry says so plainly rather than forcing it under
   rubric 1. `### Breaking` in the CHANGELOG carries it (§15 lists the two
   refusals under compatibility but not among the `### Breaking` items — the
   plan adds them there).
6. **Unreadable function-criterion config is newly refused at import** (C-6):
   e.g. `"responseTimeoutMs":"5s"`. Not named in the spec, but it falls out of
   "criterion function config is parsed": the alternative is to swallow a parse
   error. Such a criterion already fails every dispatch
   (`dispatch.go:291-293`, "invalid criterion JSON"). Recorded in the
   versioning doc entry.
7. **Only a top-level function criterion is inspected.** A `function` nested in
   a `group` is never dispatched (`engine.go:1006`; `search.md:203` "one nested
   inside a `group` fails the evaluation"), so its config is not validated.
   Whether import should refuse that shape outright is outside §10.
8. **`workflows.md:191` is wrong today** ("timeout … for `SYNC` processor
   response"): the limit applies in every execution mode
   (`dispatch.go:241` passes it unconditionally). Fixed in C-1's sweep.
9. **`main.go` validator calls have no test** (existing seven, plus the two
   added). Recorded as a TDD waiver in C-1/C-2 rather than adding a seam to
   `main()`; `Config.Validate()` carries the tested invariant and `app.New`
   enforces it for every embedder.
10. **`ValidateDispatch` runs whether or not clustering is enabled** (§9 gives
    no condition). `CYODA_DISPATCH_CONNECT_TIMEOUT=0` on a single pnode is thus
    a startup error although nothing dials. Chosen for one rule instead of two;
    flag if the lead wants it conditional on `CYODA_CLUSTER_ENABLED`.
11. **Help topic placement.** §14 names `config/cluster.md` and
    `config/grpc.md` without assigning settings. Chosen: tries and answer limit
    → `grpc` (they matter on a single pnode); the patience stays in `cluster`
    because `TestDefaultTree_ConfigClusterSubtopic` pins it there, with its
    single-pnode meaning stated; hand-over/pass allowances and the connect
    timeout → `cluster`. `CYODA_CALLOUT_PASS_ALLOWANCE` applies on a single
    pnode too (a pass is minted there as well) — it is cross-referenced from
    `config grpc`'s closing paragraph only indirectly; say if it should move.
