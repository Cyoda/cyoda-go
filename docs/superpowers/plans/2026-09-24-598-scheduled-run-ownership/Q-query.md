# Stream Q — `GET /scheduled-tasks`

Spec sections: §8 (parameters, order, cursor, DTO, error table); §12 bullets
"New help topic `scheduled-tasks`" and "`api/openapi.yaml`: the new operation
and its DTOs"; §13 table "`GET /scheduled-tasks`" and its gRPC waiver.
Binding names: `interfaces.md` § "Query".

## Facts settled by reading the code (used by the tasks below)

1. **The model to copy is the audit search.** `internal/domain/audit/handler.go`
   validates `limit` and `cursor` before any store call (`:49-73`), gets the
   store from the factory (`:75-79`), routes every store error through
   `common.Internal` (`:103`), and writes a `{items, pagination}` envelope
   (`:212-223`). Two differences are the spec's, not drift: audit takes
   `limit` as a string and clamps values above 1000 (`:52-63`); this endpoint
   declares `limit` an integer and rejects 0 and 1001 (spec §8).
2. **The cursor model** is `internal/domain/audit/cursor.go:13-114`: versioned
   base64url JSON, a 256-character cap checked before decoding (`:34, :66`),
   `DisallowUnknownFields` (`:74`), and one spelling per position — a string
   is accepted only if re-encoding the decoded key gives the same string
   (`:110`). The handler maps any failure to a fixed 400 text that never
   contains the cursor (`handler.go:69`).
3. **Error rendering.** `common.Operational` (`internal/common/errors.go:134`)
   carries domain detail. `common.Internal` (`:180`) turns an error that
   carries the storage marker into a retryable 503 `STORAGE_UNAVAILABLE`
   (`:157-168`), and anything else into a 500 `SERVER_ERROR`. `WriteError`
   (`:308`) mints the ticket for a 500 and keeps the cause out of the body.
   `ProblemDetail.Instance` is `r.URL.Path` only, so the query string never
   reaches a response.
4. **Binding errors are already 400 `BAD_REQUEST`.** A query parameter the
   generated router cannot bind (an integer that is not one, a `uuid` that is
   not one, a repeated single-value parameter) goes to
   `internal/api/binding_error.go:14-30`, which answers 400 `BAD_REQUEST` and
   names the parameter but not its value. So `limit=abc`,
   `modelVersion=abc` and `entityId=x` need no handler code.
5. **Tenant.** Authenticated handlers read the tenant from
   `spi.MustGetUserContext(ctx)` (`internal/domain/entity/service.go:191`);
   the auth middleware wraps every generated route (`app/app.go:796`).
6. **Routing a generated operation.** `internal/api/server.go:20-30` composes
   domain handlers over an embedded `*Unimplemented`; each method delegates
   when its field is set (`:347-353` for audit). `app/app.go:673-679` sets
   the fields. `internal/api/unimplemented_test.go:15` and
   `internal/api/server_test.go:10` assert at compile time that both types
   satisfy `genapi.ServerInterface`, so `go generate ./api` alone breaks the
   build until `Unimplemented` gains the new method.
7. **The e2e conformance gates.** Every response of the shared stack is
   validated against `api/openapi.yaml` (`internal/e2e/e2e_test.go:215-219`);
   only responses are validated, not requests
   (`internal/e2e/openapivalidator/validator.go:185-197`). At suite end an
   operation must be exercised or carry `x-cyoda-status`, and a marked
   operation must not answer 2xx
   (`internal/e2e/zzz_openapi_conformance_test.go:24-35, 63-71`). So Q-1
   marks the operation `x-cyoda-status: planned` (it answers 501 until Q-6)
   and Q-6 removes the marker.
8. **The error-code matrix** (`internal/e2e/zzz_errorcode_matrix_test.go:28`)
   checks both ways for each operation it lists, on the shared stack only.
   `UNAUTHORIZED` and `SERVER_ERROR` are exempt (`:185-189`). The 503 is
   produced on a private harness that is not behind the validator
   (`internal/e2e/callback_harness_test.go:241`), so the matrix row for
   `listScheduledTasks` lists only 400 `BAD_REQUEST`.
9. **Real storage faults in e2e.** A torn socket gives a retryable 503 on a
   read that opens no transaction (`internal/e2e/torn_connection_e2e_test.go:
   130-167, 206-224`). A terminated session (57P01) is left unmarked by the
   plugin and gives a 500 with a ticket
   (`internal/e2e/lookup_storage_failure_e2e_test.go:17-25, 88-101, 130-155`).
   Both are on the running PostgreSQL backend, so the §13 "store double" cells
   are covered by a double in the unit tests and by real faults in e2e.
10. **Two tenants in e2e.** `createM2MClient` seeds an M2M client of any
    tenant (`internal/e2e/oauth_keys_test.go:582-609`) and `adminRequestAs`
    calls as it (`:611-633`). A fresh tenant per test makes the no-filter
    list deterministic on the shared database.
11. **No scheduler on the shared stack** (`internal/e2e/e2e_test.go:172`), so
    an armed task stays WAITING there. A task that must be FAILED is seeded by
    SQL on `dbPool`, the precedent being `internal/e2e/async_stream_test.go:925`
    (a `search_jobs` row the API cannot produce on demand). A scheduler of a
    per-test harness claims only due tasks and RUNNING tasks of a stale owner
    (spec §6.1); an hour-away WAITING task and a FAILED task are neither.
12. **Task ids** are 32 lower-case hex characters
    (`internal/domain/workflow/arm.go:26-29`); `ScheduledTime = armMs +
    DelayMs` and `ArmedAt = armMs` (`arm.go:152, 158`). The SPI calls the id
    opaque, so the cursor checks only that it is non-empty.
13. **Parity.** `allTests` in `e2e/parity/registry.go` is counted by
    `TestParityScenarioCount` (`e2e/parity/registry_count_test.go:9`, and the
    header comment at `registry.go:5`). The parity client decodes strictly
    (`e2e/parity/client/http.go:84-135`, `audit.go:396-420`). Each scenario
    gets a fresh tenant (`e2e/parity/fixture.go:30-34`). The parity servers
    run a scheduler, so parity tasks use an hour-away schedule.
14. **Help.** `topLevelTopicsV061` (`cmd/cyoda/help/help_test.go:423-427`)
    pins the top-level topics. Help markdown allows no tables
    (`TestContentMarkdownSubsetLinter`, `:628`). `openapi.md:88` says "83
    paths"; `grep -c '^  /' api/openapi.yaml` prints 71 today — stale, fixed in
    Q-7.

## Working rules for this stream

- Every command runs in the worktree
  `/Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/598-scheduler-ownership`;
  each Bash call `cd`s there first.
- Q-1 needs nothing from other streams. Q-2 and Q-3 compile against the S
  stream's SPI through the uncommitted `go.work` `use` line. Q-4 to Q-6 also
  need BM, BQ and BP (the three `Query` implementations and the PostgreSQL
  columns).
- Never `git add -A`. Before each commit, `git diff --cached --name-only`
  must not list `go.work`.
- Run `make preflight` before any e2e or parity run. Parity runs carry
  `-count=1` (the fixture builds the server binary outside the test cache,
  CLAUDE.md). No other command adds `-count=1` or `-v`.

---

### Task Q-1: OpenAPI — the operation, its DTOs, generated types

**Spec:** §8 (all of it), §12 "`api/openapi.yaml`: the new operation and its
DTOs; then `go generate ./api`".

**gRPC waiver (spec §8, §13):** `listScheduledTasks` has no gRPC door. It is an
operator's view no compute node needs, like the audit trail. No
`internal/grpc` test is added for it.

**Files:**
- Modify: `api/openapi.yaml` (tag list `:185-315`; new path before
  `/search/async/{entityName}/{modelVersion}:` at `:7111`; new schemas before
  `EntityAuditEventsResponseDto:` at `:11631`; `SCHEDULED_TRANSITION_FAIL`
  in the `StateMachineAuditEventDto.eventType` enum after
  `- SCHEDULED_TRANSITION_CANCEL` at `:11762`)
- Modify: `api/generated.go` (by `go generate ./api` only)
- Modify: `internal/api/unimplemented.go` (stub after `:38`)
- Test: `api/scheduled_tasks_contract_test.go` (new)

**Interfaces:**
- Consumes: nothing.
- Produces (generated, package `api`):
  ```go
  type ListScheduledTasksParams struct {
      Status       *[]ListScheduledTasksParamsStatus
      ModelName    *string
      ModelVersion *int32
      EntityId     *openapi_types.UUID
      Cursor       *string
      Limit        *int32
  }
  type ListScheduledTasksParamsStatus string
  type ScheduledTaskPageDto struct { Items []ScheduledTaskDto; Pagination CursorPaginationInfoDto }
  type ScheduledTaskDto struct {
      TaskId string; EntityId openapi_types.UUID; ModelName string; ModelVersion int32
      SourceState string; Transition string; Status string
      ScheduledTime time.Time; ArmedTime time.Time; ExpiresTime *time.Time
      Attempts int32; LostOwners int32
      NextAttemptTime *time.Time; LastAttemptTime *time.Time; LastError *string
      FailureReason *string; FailedTime *time.Time; ArmedBy *ScheduledTaskArmedByDto
  }
  type ScheduledTaskArmedByDto struct { Id string; Kind string }
  const SCHEDULEDTRANSITIONFAIL StateMachineAuditEventDtoEventType = "SCHEDULED_TRANSITION_FAIL"
  // ServerInterface gains:
  ListScheduledTasks(w http.ResponseWriter, r *http.Request, params ListScheduledTasksParams)
  ```

- [ ] **Step 1: Write the failing test** — `api/scheduled_tasks_contract_test.go`:

```go
package api

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

func sortedParamNames(m map[string]*openapi3.Parameter) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPropNames(m openapi3.Schemas) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestListScheduledTasks_Contract pins GET /scheduled-tasks: its parameters
// and their rules, and the statuses it can answer. No X-Tx-Token: the
// operation is not a callback target. No 403 (no role is required) and no 404
// (an unknown or foreign model or entity is an empty list).
func TestListScheduledTasks_Contract(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	item := doc.Paths.Find("/scheduled-tasks")
	if item == nil || item.Get == nil {
		t.Fatal("GET /scheduled-tasks is not declared")
	}
	op := item.Get
	if op.OperationID != "listScheduledTasks" {
		t.Errorf("operationId = %q, want listScheduledTasks", op.OperationID)
	}
	if op.Security == nil || len(*op.Security) != 1 {
		t.Errorf("security must be exactly bearerAuth")
	} else if _, ok := (*op.Security)[0]["bearerAuth"]; !ok {
		t.Errorf("security must be bearerAuth, got %v", *op.Security)
	}

	params := map[string]*openapi3.Parameter{}
	for _, ref := range op.Parameters {
		params[ref.Value.Name] = ref.Value
	}
	if got := strings.Join(sortedParamNames(params), ","); got != "cursor,entityId,limit,modelName,modelVersion,status" {
		t.Fatalf("parameters = %s, want cursor,entityId,limit,modelName,modelVersion,status", got)
	}
	for name, p := range params {
		if p.In != "query" || p.Required {
			t.Errorf("%s must be an optional query parameter", name)
		}
	}

	status := params["status"]
	if s := status.Schema.Value; !s.Type.Is("array") || s.Items == nil {
		t.Errorf("status must be an array")
	} else if got := fmt.Sprint(s.Items.Value.Enum); got != "[WAITING RUNNING FAILED]" {
		t.Errorf("status values = %s, want [WAITING RUNNING FAILED]", got)
	}
	if status.Explode == nil || !*status.Explode {
		t.Errorf("status must be exploded, so it is repeatable")
	}
	if s := params["modelName"].Schema.Value; !s.Type.Is("string") || s.MinLength != 1 || s.MaxLength == nil || *s.MaxLength != 256 {
		t.Errorf("modelName must be a string of 1 to 256 characters")
	}
	if s := params["modelVersion"].Schema.Value; !s.Type.Is("integer") || s.Min == nil || *s.Min != 1 {
		t.Errorf("modelVersion must be an integer of at least 1")
	}
	if s := params["entityId"].Schema.Value; !s.Type.Is("string") || s.Format != "uuid" {
		t.Errorf("entityId must be a uuid string")
	}
	if s := params["cursor"].Schema.Value; !s.Type.Is("string") || s.MaxLength == nil || *s.MaxLength != 256 {
		t.Errorf("cursor must be a string of at most 256 characters")
	}
	if s := params["limit"].Schema.Value; !s.Type.Is("integer") || s.Min == nil || *s.Min != 1 ||
		s.Max == nil || *s.Max != 1000 || fmt.Sprint(s.Default) != "20" {
		t.Errorf("limit must be an integer 1..1000 with default 20")
	}

	var codes []string
	for code := range op.Responses.Map() {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if got := strings.Join(codes, ","); got != "200,400,401,500,503" {
		t.Errorf("responses = %s, want 200,400,401,500,503", got)
	}
	ok := op.Responses.Status(200)
	if ok == nil || ok.Value.Content["application/json"] == nil ||
		ok.Value.Content["application/json"].Schema.Ref != "#/components/schemas/ScheduledTaskPageDto" {
		t.Errorf("200 must be application/json ScheduledTaskPageDto")
	}
}

// TestScheduledTaskDtos_TypedButOpen pins the three response schemas: every
// field the server emits is declared, none is sealed (ADR 0003 decision 2),
// status and failureReason are open value sets (decision 4), and no token,
// claim, owner or tenant field exists.
func TestScheduledTaskDtos_TypedButOpen(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	want := map[string]struct{ props, required []string }{
		"ScheduledTaskPageDto": {
			props:    []string{"items", "pagination"},
			required: []string{"items", "pagination"},
		},
		"ScheduledTaskDto": {
			props: []string{"armedBy", "armedTime", "attempts", "entityId", "expiresTime", "failedTime",
				"failureReason", "lastAttemptTime", "lastError", "lostOwners", "modelName", "modelVersion",
				"nextAttemptTime", "scheduledTime", "sourceState", "status", "taskId", "transition"},
			required: []string{"armedTime", "attempts", "entityId", "lostOwners", "modelName", "modelVersion",
				"scheduledTime", "sourceState", "status", "taskId", "transition"},
		},
		"ScheduledTaskArmedByDto": {
			props:    []string{"id", "kind"},
			required: []string{"id", "kind"},
		},
	}
	for name, w := range want {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Errorf("%s is not declared", name)
			continue
		}
		s := ref.Value
		if got := strings.Join(sortedPropNames(s.Properties), ","); got != strings.Join(w.props, ",") {
			t.Errorf("%s properties = %s, want %s", name, got, strings.Join(w.props, ","))
		}
		if got := strings.Join(sortedCopy(s.Required), ","); got != strings.Join(w.required, ",") {
			t.Errorf("%s required = %s, want %s", name, got, strings.Join(w.required, ","))
		}
		if s.AdditionalProperties.Has != nil && !*s.AdditionalProperties.Has {
			t.Errorf("%s is sealed; response schemas stay open (ADR 0003)", name)
		}
	}

	dto := doc.Components.Schemas["ScheduledTaskDto"].Value.Properties
	for _, open := range []string{"status", "failureReason"} {
		if len(dto[open].Value.Enum) != 0 {
			t.Errorf("%s must be an open value set, not an enum", open)
		}
	}
	for _, known := range []string{"WAITING", "RUNNING", "FAILED"} {
		if !strings.Contains(dto["status"].Value.Description, known) {
			t.Errorf("status description does not name %s", known)
		}
	}
	for _, known := range []string{"UNSAFE_WORK_NOT_COMPLETED", "OWNER_LOST_REPEATEDLY",
		"EXPIRED_AFTER_FAILED_ATTEMPTS", "RUN_PANICKED", "STOPPED_AFTER_PARTIAL_COMMIT"} {
		if !strings.Contains(dto["failureReason"].Value.Description, known) {
			t.Errorf("failureReason description does not name %s", known)
		}
	}
	for _, ts := range []string{"scheduledTime", "armedTime", "expiresTime", "nextAttemptTime", "lastAttemptTime", "failedTime"} {
		if dto[ts].Value.Format != "date-time" {
			t.Errorf("%s must be a date-time", ts)
		}
	}
	if dto["entityId"].Value.Format != "uuid" {
		t.Errorf("entityId must be a uuid")
	}
}

// TestScheduledTaskDto_GeneratedTypes pins the Go types the handler builds
// the response from. It fails to compile when a field changes type.
func TestScheduledTaskDto_GeneratedTypes(t *testing.T) {
	now := time.Now()
	s := "x"
	v := int32(1)
	id := openapi_types.UUID{}
	_ = ListScheduledTasksParams{
		Status:       &[]ListScheduledTasksParamsStatus{"WAITING"},
		ModelName:    &s,
		ModelVersion: &v,
		EntityId:     &id,
		Cursor:       &s,
		Limit:        &v,
	}
	_ = ScheduledTaskPageDto{
		Items: []ScheduledTaskDto{{
			TaskId: s, EntityId: id, ModelName: s, ModelVersion: v, SourceState: s, Transition: s, Status: s,
			ScheduledTime: now, ArmedTime: now, ExpiresTime: &now, Attempts: v, LostOwners: v,
			NextAttemptTime: &now, LastAttemptTime: &now, LastError: &s, FailureReason: &s, FailedTime: &now,
			ArmedBy: &ScheduledTaskArmedByDto{Id: s, Kind: s},
		}},
		Pagination: CursorPaginationInfoDto{HasNext: false},
	}
}

// TestStateMachineAuditEventType_HasScheduledTransitionFail pins the audit
// event a FAILED scheduled task records. Every e2e test that reads it through
// the validated audit API needs it in the enum.
func TestStateMachineAuditEventType_HasScheduledTransitionFail(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	sm := doc.Components.Schemas["StateMachineAuditEventDto"]
	if sm == nil || sm.Value == nil {
		t.Fatal("StateMachineAuditEventDto is not declared")
	}
	var found bool
	for _, part := range sm.Value.AllOf {
		et := part.Value.Properties["eventType"]
		if et == nil {
			continue
		}
		for _, v := range et.Value.Enum {
			if v == "SCHEDULED_TRANSITION_FAIL" {
				found = true
			}
		}
	}
	if !found {
		t.Error("StateMachineAuditEventDto.eventType does not list SCHEDULED_TRANSITION_FAIL")
	}
	if !SCHEDULEDTRANSITIONFAIL.Valid() {
		t.Error("generated SCHEDULEDTRANSITIONFAIL is not Valid()")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./api/ -run 'TestListScheduledTasks_Contract|TestScheduledTaskDtos_TypedButOpen|TestScheduledTaskDto_GeneratedTypes|TestStateMachineAuditEventType_HasScheduledTransitionFail'`
Expected: FAIL — build error `undefined: ListScheduledTasksParams` and
`undefined: SCHEDULEDTRANSITIONFAIL`.

- [ ] **Step 3: Implement** — `api/openapi.yaml`.

Add to the top-level `tags:` list, after the `Entity, Audit` entry (`:223-224`):

```yaml
  - name: Scheduled Tasks
    description: The tenant's scheduled-transition tasks — waiting, running and failed.
```

Insert this path immediately before `  /search/async/{entityName}/{modelVersion}:`:

```yaml
  /scheduled-tasks:
    get:
      tags:
        - Scheduled Tasks
      summary: List the tenant's scheduled tasks
      description: >
        Lists the scheduled-transition tasks of the caller's tenant that still
        exist: WAITING, RUNNING and FAILED. A task that fired, was declined,
        expired or was cancelled is removed; its outcome is in the entity's
        audit trail.


        Results are sorted by `scheduledTime`, then `taskId`, ascending, and
        paged with an opaque cursor. Filters combine with AND. An unknown
        model or entity, or one of another tenant, returns an empty list.
        Any authenticated user of the tenant may call it; the tenant is the
        token's. HTTP only: there is no gRPC equivalent.
      operationId: listScheduledTasks
      x-cyoda-status: planned
      parameters:
        - name: status
          in: query
          description: Return tasks in any of these statuses. Repeat the
            parameter for several. Any other value is rejected with 400.
          required: false
          style: form
          explode: true
          schema:
            type: array
            items:
              type: string
              enum:
                - WAITING
                - RUNNING
                - FAILED
        - name: modelName
          in: query
          description: Return tasks of entities of this model.
          required: false
          schema:
            type: string
            minLength: 1
            maxLength: 256
        - name: modelVersion
          in: query
          description: Return tasks of entities of this model version. Only
            together with `modelName`; alone it is rejected with 400.
          required: false
          schema:
            type: integer
            format: int32
            minimum: 1
        - name: entityId
          in: query
          description: Return the tasks of this entity.
          required: false
          schema:
            type: string
            format: uuid
        - name: cursor
          in: query
          description: "Position to continue from: pass `nextCursor` from the
            previous response. Opaque. A cursor that cannot be read is
            rejected with 400, and its value is not echoed. Omit for the
            first page."
          required: false
          schema:
            type: string
            maxLength: 256
        - name: limit
          in: query
          description: Maximum number of tasks per page, 1 to 1000. A value
            outside the range is rejected with 400, not clamped.
          required: false
          schema:
            type: integer
            format: int32
            minimum: 1
            maximum: 1000
            default: 20
      responses:
        "200":
          description: One page of tasks, possibly empty.
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/ScheduledTaskPageDto"
        "400":
          description: Bad Request - Invalid parameters; `properties.errorCode` is BAD_REQUEST
          content:
            application/problem+json:
              schema:
                $ref: "#/components/schemas/ProblemDetail"
        "401":
          $ref: '#/components/responses/Unauthorized'
        "500":
          $ref: '#/components/responses/InternalServerError'
        "503":
          $ref: '#/components/responses/ServiceUnavailable'
      security:
        - bearerAuth: []
```

In the `StateMachineAuditEventDto` schema, add one value to the `eventType`
enum, after `- SCHEDULED_TRANSITION_CANCEL` (`:11762`):

```yaml
                - SCHEDULED_TRANSITION_FAIL
```

Insert these schemas immediately before `    EntityAuditEventsResponseDto:`:

```yaml
    ScheduledTaskPageDto:
      type: object
      properties:
        items:
          type: array
          description: The tasks on this page, in (`scheduledTime`, `taskId`) order.
          items:
            $ref: "#/components/schemas/ScheduledTaskDto"
        pagination:
          $ref: "#/components/schemas/CursorPaginationInfoDto"
      required:
        - items
        - pagination
    ScheduledTaskDto:
      type: object
      description: >
        One scheduled transition's task: fire `transition` of the entity at
        `scheduledTime`. Claim tokens, arm tokens and node identities are
        never returned.
      properties:
        taskId:
          type: string
          description: Opaque, stable identifier. The same (entity, source
            state, transition) keeps the same id when it is armed again.
        entityId:
          type: string
          format: uuid
        modelName:
          type: string
        modelVersion:
          type: integer
          format: int32
        sourceState:
          type: string
          description: The state the entity must be in for the transition to fire.
        transition:
          type: string
        status:
          type: string
          description: >
            Open value set; accept values not listed here. Known values:
            WAITING — due at `nextAttemptTime`; RUNNING — a node has claimed
            it and is running it; FAILED — it will not run again and never
            moves the entity (see `failureReason`). A write to the entity in
            `sourceState` arms it again; the entity leaving `sourceState`
            removes it.
        scheduledTime:
          type: string
          format: date-time
          description: When the transition is due.
        armedTime:
          type: string
          format: date-time
          description: When the task was last armed.
        expiresTime:
          type: string
          format: date-time
          description: Present when the transition's schedule sets `timeoutMs`
            — `scheduledTime` plus `timeoutMs`.
        attempts:
          type: integer
          format: int32
          description: Failed attempts recorded since the task was last armed.
        lostOwners:
          type: integer
          format: int32
          description: Times the node running the task was lost since the task
            was last armed.
        nextAttemptTime:
          type: string
          format: date-time
          description: Present when `status` is WAITING — the earliest time
            the next attempt may start.
        lastAttemptTime:
          type: string
          format: date-time
          description: Present after a failed attempt.
        lastError:
          type: string
          description: Present after a failed attempt. Client-safe text — a
            `CODE: detail` message, a compute node's own message, or
            `internal error [ticket: <uuid>]`.
        failureReason:
          type: string
          description: >
            Present when `status` is FAILED. Open value set; accept values
            not listed here. Known values:
            UNSAFE_WORK_NOT_COMPLETED — a processor not declared `idempotent`
            was handed to a compute node and the run did not commit, so it is
            not repeated;
            OWNER_LOST_REPEATEDLY — the node running the task was lost too
            many times;
            EXPIRED_AFTER_FAILED_ATTEMPTS — `expiresTime` passed after a
            failed attempt or a lost node;
            RUN_PANICKED — the run failed with an internal error;
            STOPPED_AFTER_PARTIAL_COMMIT — the run committed the entity into
            another state and then stopped.
        failedTime:
          type: string
          format: date-time
          description: Present when `status` is FAILED.
        armedBy:
          $ref: "#/components/schemas/ScheduledTaskArmedByDto"
      required:
        - taskId
        - entityId
        - modelName
        - modelVersion
        - sourceState
        - transition
        - status
        - scheduledTime
        - armedTime
        - attempts
        - lostOwners
    ScheduledTaskArmedByDto:
      type: object
      description: The principal whose write armed the task. Present when known.
      properties:
        id:
          type: string
        kind:
          type: string
          description: Open value set. Known values are user, service and system.
      required:
        - id
        - kind
```

Then:

```
go generate ./api
make check-codegen
```

`internal/api/unimplemented.go`, after `GetStateMachineFinishedEvent` (`:36-38`):

```go
func (u *Unimplemented) ListScheduledTasks(w http.ResponseWriter, r *http.Request, params genapi.ListScheduledTasksParams) {
	u.stub(w, r)
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go build ./... && go test ./api/... ./internal/api/... ./cmd/cyoda/help/... ./internal/oasdiffcheck/...`
Expected: PASS. `make check-codegen` prints OK. `go vet ./api/ ./internal/api/` is clean.

- [ ] **Step 5: Commit**

```
git add api/openapi.yaml api/generated.go api/scheduled_tasks_contract_test.go internal/api/unimplemented.go
git commit -m "feat(api): declare GET /scheduled-tasks and its typed-but-open DTOs (#598)

The operation is marked x-cyoda-status: planned until its handler is wired.
HTTP only: no gRPC door, like the audit trail. The audit eventType enum
gains SCHEDULED_TRANSITION_FAIL.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task Q-2: The page cursor

**Spec:** §8 "The cursor is versioned base64url JSON, decoded strictly like
the audit cursor". `interfaces.md`: `{"v":1,"t":<scheduledTime ms>,"i":"<task id>"}`.

**Files:**
- Create: `internal/domain/scheduledtask/cursor.go`
- Test: `internal/domain/scheduledtask/cursor_internal_test.go`

**Interfaces:**
- Consumes: `spi.ScheduledTaskCursor{ScheduledTime int64; ID string}` (stream S).
- Produces (unexported): `encodeCursor(spi.ScheduledTaskCursor) string`,
  `decodeCursor(string) (spi.ScheduledTaskCursor, error)`, `errBadCursor`,
  `maxCursorLen = 256`.

- [ ] **Step 1: Write the failing test** — `internal/domain/scheduledtask/cursor_internal_test.go`:

```go
package scheduledtask

import (
	"encoding/base64"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestCursor_RoundTrip(t *testing.T) {
	for name, c := range map[string]spi.ScheduledTaskCursor{
		"typical":        {ScheduledTime: 1700000000123, ID: "0123456789abcdef0123456789abcdef"},
		"zero time":      {ScheduledTime: 0, ID: "a"},
		"negative time":  {ScheduledTime: -1, ID: "b"},
		"largest time":   {ScheduledTime: 1<<63 - 1, ID: "0123456789abcdef0123456789abcdef"},
		"id needs escape": {ScheduledTime: 5, ID: `a"<b>`},
	} {
		got, err := decodeCursor(encodeCursor(c))
		if err != nil || got != c {
			t.Errorf("%s: round trip %+v -> %+v, %v", name, c, got, err)
		}
	}
}

// TestCursor_LongestRealFitsCap: the longest cursor a store can hand out — the
// most negative time and a 32-character task id — stays under the cap.
func TestCursor_LongestRealFitsCap(t *testing.T) {
	s := encodeCursor(spi.ScheduledTaskCursor{ScheduledTime: -1 << 63, ID: "0123456789abcdef0123456789abcdef"})
	if len(s) > maxCursorLen {
		t.Fatalf("longest real cursor is %d characters, want <= %d", len(s), maxCursorLen)
	}
	if _, err := decodeCursor(s); err != nil {
		t.Fatalf("longest real cursor does not decode: %v", err)
	}
}

func TestCursor_Rejects(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, c := range map[string]string{
		"empty":           "",
		"over the cap":    strings.Repeat("A", maxCursorLen+1),
		"not base64url":   "!!!",
		"padded base64":   base64.URLEncoding.EncodeToString([]byte(`{"v":1,"t":1,"i":"a"}`)),
		"not json":        enc("nope"),
		"wrong version":   enc(`{"v":2,"t":1,"i":"a"}`),
		"no version":      enc(`{"t":1,"i":"a"}`),
		"empty id":        enc(`{"v":1,"t":1,"i":""}`),
		"no id":           enc(`{"v":1,"t":1}`),
		"time as string":  enc(`{"v":1,"t":"1","i":"a"}`),
		"time fractional": enc(`{"v":1,"t":1.5,"i":"a"}`),
		"time exponent":   enc(`{"v":1,"t":1e3,"i":"a"}`),
		"unknown field":   enc(`{"v":1,"t":1,"i":"a","x":1}`),
		"trailing data":   enc(`{"v":1,"t":1,"i":"a"}x`),
		"whitespace":      enc(`{"v":1, "t":1,"i":"a"}`),
		"key order":       enc(`{"t":1,"v":1,"i":"a"}`),
		"upper-case key":  enc(`{"V":1,"t":1,"i":"a"}`),
		"no time":         enc(`{"v":1,"i":"a"}`),
		"leading zero":    enc(`{"v":1,"t":01,"i":"a"}`),
	} {
		if _, err := decodeCursor(c); err == nil {
			t.Errorf("%s: cursor %q accepted, want errBadCursor", name, c)
		}
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/domain/scheduledtask/ -run TestCursor`
Expected: FAIL — build error `undefined: decodeCursor`.

- [ ] **Step 3: Implement** — `internal/domain/scheduledtask/cursor.go`:

```go
package scheduledtask

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// cursorBody is the decoded form of a page cursor: the (scheduledTime, id)
// sort key of the last task on the previous page. Opaque to callers.
type cursorBody struct {
	V int    `json:"v"`
	T int64  `json:"t"`
	I string `json:"i"`
}

// errBadCursor is returned for any cursor that does not decode to a valid
// position. The handler answers 400 BAD_REQUEST without echoing the cursor.
var errBadCursor = errors.New("invalid cursor")

// maxCursorLen bounds the cursor string, checked before any decoding. The
// longest real cursor (most negative time, 32-character task id) is under
// 100 characters.
const maxCursorLen = 256

// encodeCursor renders c as an opaque cursor: a position in the
// (scheduledTime, taskId) order, not an offset into one query's results.
func encodeCursor(c spi.ScheduledTaskCursor) string {
	raw, _ := json.Marshal(cursorBody{V: 1, T: c.ScheduledTime, I: c.ID}) // fixed struct of ints and a string; cannot fail
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses a cursor back into the position it encodes. A string is
// accepted only if it is exactly what encodeCursor produces for that position,
// so every other spelling — whitespace, key order, trailing data, a leading
// zero, padding — is rejected by one check rather than a list of shapes.
func decodeCursor(s string) (spi.ScheduledTaskCursor, error) {
	if s == "" || len(s) > maxCursorLen {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b cursorBody
	if err := dec.Decode(&b); err != nil || b.V != 1 || b.I == "" {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	c := spi.ScheduledTaskCursor{ScheduledTime: b.T, ID: b.I}
	if encodeCursor(c) != s {
		return spi.ScheduledTaskCursor{}, errBadCursor
	}
	return c, nil
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/domain/scheduledtask/ -run TestCursor`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/domain/scheduledtask/cursor.go internal/domain/scheduledtask/cursor_internal_test.go
git commit -m "feat(scheduledtask): strict, versioned page cursor for the task query (#598)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task Q-3: The handler

**Spec:** §8 parameter table, "Access", "Order", DTO table, error table. §5.8
is the scheduler's (the text in `LastError` is already sanitised when it is
stored).

**Files:**
- Create: `internal/domain/scheduledtask/handler.go`
- Test: `internal/domain/scheduledtask/handler_test.go`

**Interfaces:**
- Consumes: `spi.StoreFactory.ScheduledTaskStore`, `spi.ScheduledTaskStore.Query`,
  `spi.ScheduledTaskQuery`, `spi.ScheduledTaskPage`, `spi.ScheduledTask` and its
  status and reason constants (stream S); generated types of Q-1.
- Produces (binding, `interfaces.md`):
  ```go
  func NewHandler(factory spi.StoreFactory) *Handler
  func (h *Handler) ListScheduledTasks(w http.ResponseWriter, r *http.Request, params genapi.ListScheduledTasksParams)
  ```
- Guarantee: the store is called only for a valid request, with the token's
  tenant, `Limit` in 1..1000, `ModelVersion` set only with `ModelName`.

- [ ] **Step 1: Write the failing test** — `internal/domain/scheduledtask/handler_test.go`:

```go
package scheduledtask_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/scheduledtask"
)

const tenantA spi.TenantID = "tenant-a"

// fakeStore answers Query with a canned page or error and records the call.
// Every other method is unimplemented; the handler reaches none of them.
type fakeStore struct {
	spi.ScheduledTaskStore
	page   spi.ScheduledTaskPage
	err    error
	calls  int
	tenant spi.TenantID
	query  spi.ScheduledTaskQuery
}

func (f *fakeStore) Query(_ context.Context, tenant spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	f.calls++
	f.tenant = tenant
	f.query = q
	return f.page, f.err
}

type fakeFactory struct {
	spi.StoreFactory
	store spi.ScheduledTaskStore
	err   error
}

func (f fakeFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return f.store, f.err
}

func ptr[T any](v T) *T { return &v }

func call(t *testing.T, f spi.StoreFactory, params genapi.ListScheduledTasksParams) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/scheduled-tasks", nil)
	r = r.WithContext(spi.WithUserContext(r.Context(),
		&spi.UserContext{UserID: "u1", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: tenantA}}))
	w := httptest.NewRecorder()
	scheduledtask.NewHandler(f).ListScheduledTasks(w, r, params)
	return w
}

func statuses(s ...string) *[]genapi.ListScheduledTasksParamsStatus {
	out := make([]genapi.ListScheduledTasksParamsStatus, len(s))
	for i, v := range s {
		out[i] = genapi.ListScheduledTasksParamsStatus(v)
	}
	return &out
}

func TestList_Defaults(t *testing.T) {
	st := &fakeStore{}
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if st.calls != 1 || st.tenant != tenantA {
		t.Fatalf("store called %d times for tenant %q; want once for %q", st.calls, st.tenant, tenantA)
	}
	if want := (spi.ScheduledTaskQuery{Limit: 20}); !reflect.DeepEqual(st.query, want) {
		t.Errorf("query = %+v, want %+v", st.query, want)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, w.Body.String())
	}
	if items, ok := resp["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("items = %#v, want an empty array (not null)", resp["items"])
	}
	pg, _ := resp["pagination"].(map[string]any)
	if pg["hasNext"] != false {
		t.Errorf("hasNext = %v, want false", pg["hasNext"])
	}
	if _, present := pg["nextCursor"]; present {
		t.Errorf("nextCursor present on the last page: %v", pg)
	}
}

func TestList_PassesFiltersToTheStore(t *testing.T) {
	st := &fakeStore{}
	eid := uuid.MustParse("5f1c1b0e-6d1a-41f1-8000-000000000001")
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{
		Status:       statuses("WAITING", "FAILED"),
		ModelName:    ptr("orders"),
		ModelVersion: ptr(int32(2)),
		EntityId:     &eid,
		Limit:        ptr(int32(5)),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	want := spi.ScheduledTaskQuery{
		Statuses:     []spi.ScheduledTaskStatus{spi.ScheduledTaskWaiting, spi.ScheduledTaskFailed},
		ModelName:    "orders",
		ModelVersion: 2,
		EntityID:     eid.String(),
		Limit:        5,
	}
	if !reflect.DeepEqual(st.query, want) {
		t.Errorf("query = %+v, want %+v", st.query, want)
	}
}

func TestList_BoundaryValuesAccepted(t *testing.T) {
	name256 := strings.Repeat("é", 256) // 256 characters, 512 bytes
	for name, tc := range map[string]struct {
		params genapi.ListScheduledTasksParams
		want   spi.ScheduledTaskQuery
	}{
		"limit 1":                  {genapi.ListScheduledTasksParams{Limit: ptr(int32(1))}, spi.ScheduledTaskQuery{Limit: 1}},
		"limit 1000":               {genapi.ListScheduledTasksParams{Limit: ptr(int32(1000))}, spi.ScheduledTaskQuery{Limit: 1000}},
		"modelName 256 characters": {genapi.ListScheduledTasksParams{ModelName: &name256}, spi.ScheduledTaskQuery{ModelName: name256, Limit: 20}},
		"modelVersion 1": {genapi.ListScheduledTasksParams{ModelName: ptr("m"), ModelVersion: ptr(int32(1))},
			spi.ScheduledTaskQuery{ModelName: "m", ModelVersion: 1, Limit: 20}},
		"every status": {genapi.ListScheduledTasksParams{Status: statuses("RUNNING", "WAITING", "FAILED")},
			spi.ScheduledTaskQuery{Statuses: []spi.ScheduledTaskStatus{spi.ScheduledTaskRunning, spi.ScheduledTaskWaiting, spi.ScheduledTaskFailed}, Limit: 20}},
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			if w := call(t, fakeFactory{store: st}, tc.params); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(st.query, tc.want) {
				t.Errorf("query = %+v, want %+v", st.query, tc.want)
			}
		})
	}
}

// Every §8 BAD_REQUEST case that reaches the handler. The ones the generated
// binder answers (an integer or uuid that is not one) are pinned in
// internal/api and internal/e2e.
func TestList_InvalidParameters_400(t *testing.T) {
	long := strings.Repeat("m", 257)
	for name, params := range map[string]genapi.ListScheduledTasksParams{
		"unknown status":                 {Status: statuses("PENDING")},
		"lower-case status":              {Status: statuses("waiting")},
		"empty status":                   {Status: statuses("")},
		"one valid, one unknown status":  {Status: statuses("WAITING", "DONE")},
		"modelVersion without modelName": {ModelVersion: ptr(int32(1))},
		"modelVersion zero":              {ModelName: ptr("m"), ModelVersion: ptr(int32(0))},
		"modelVersion negative":          {ModelName: ptr("m"), ModelVersion: ptr(int32(-3))},
		"modelName empty":                {ModelName: ptr("")},
		"modelName 257 characters":       {ModelName: &long},
		"modelName invalid UTF-8":        {ModelName: ptr("m\xff")},
		"modelName with NUL":             {ModelName: ptr("m\x00n")},
		"limit zero":                     {Limit: ptr(int32(0))},
		"limit 1001":                     {Limit: ptr(int32(1001))},
		"limit negative":                 {Limit: ptr(int32(-1))},
		"cursor not base64url":           {Cursor: ptr("!!!not-a-cursor!!!")},
		"cursor over 256 characters":     {Cursor: ptr(strings.Repeat("A", 257))},
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			w := call(t, fakeFactory{store: st}, params)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeBadRequest)
			if st.calls != 0 {
				t.Errorf("the store was queried for an invalid request")
			}
			if params.Cursor != nil && strings.Contains(w.Body.String(), *params.Cursor) {
				t.Errorf("the 400 echoes the cursor: %s", w.Body.String())
			}
		})
	}
}

func TestList_NextCursorLeadsToTheNextPage(t *testing.T) {
	next := spi.ScheduledTaskCursor{ScheduledTime: 1700000000123, ID: "0123456789abcdef0123456789abcdef"}
	w := call(t, fakeFactory{store: &fakeStore{page: spi.ScheduledTaskPage{Next: &next}}}, genapi.ListScheduledTasksParams{})
	var resp struct {
		Pagination struct {
			HasNext    bool    `json:"hasNext"`
			NextCursor *string `json:"nextCursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, w.Body.String())
	}
	if !resp.Pagination.HasNext || resp.Pagination.NextCursor == nil {
		t.Fatalf("pagination = %+v, want hasNext with a nextCursor", resp.Pagination)
	}

	st := &fakeStore{}
	if w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{Cursor: resp.Pagination.NextCursor}); w.Code != http.StatusOK {
		t.Fatalf("second page: status = %d; body: %s", w.Code, w.Body.String())
	}
	if st.query.After == nil || *st.query.After != next {
		t.Errorf("second page After = %v, want %+v", st.query.After, next)
	}
}

func TestList_ItemFields(t *testing.T) {
	timeout := int64(60000)
	lastAt := int64(1700000100000)
	failedAt := int64(1700000200000)
	armTok, claimTok, owner := uuid.New(), uuid.New(), uuid.New()
	eW, eR, eF := uuid.New(), uuid.New(), uuid.New()
	base := func(id string, e uuid.UUID, s spi.ScheduledTaskStatus) spi.ScheduledTask {
		return spi.ScheduledTask{
			ID: id, TenantID: tenantA, Type: spi.ScheduledTaskFireTransition,
			ScheduledTime: 1700000000123, EntityID: e.String(), ModelName: "orders", ModelVersion: 3,
			Transition: "AutoClose", SourceState: "OPEN", ArmedAt: 1699999990000,
			Status: s, ArmToken: armTok, NextAttemptTime: 1700000000123,
		}
	}
	waiting := base("t-waiting", eW, spi.ScheduledTaskWaiting)
	waiting.TimeoutMs = &timeout
	waiting.ArmedBy = spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	running := base("t-running", eR, spi.ScheduledTaskRunning)
	running.Claim = &spi.TaskClaim{Token: claimTok, Owner: owner}
	running.UnsafeMarked = true
	running.PartialCommit = true
	failed := base("t-failed", eF, spi.ScheduledTaskFailed)
	failed.Attempts, failed.LostOwners = 2, 1
	failed.LastAttemptTime = &lastAt
	failed.LastError = "PROCESSOR_ERROR: boom"
	failed.FailureReason = spi.FailureUnsafeWorkNotCompleted
	failed.FailedTime = &failedAt

	st := &fakeStore{page: spi.ScheduledTaskPage{Items: []spi.ScheduledTask{waiting, running, failed}}}
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{armTok.String(), claimTok.String(), owner.String(), string(tenantA)} {
		if strings.Contains(body, secret) {
			t.Errorf("response carries %q, which is never returned; body: %s", secret, body)
		}
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(resp.Items))
	}
	fields := func(taskID string, e uuid.UUID, status string) map[string]any {
		return map[string]any{
			"taskId": taskID, "entityId": e.String(), "modelName": "orders", "modelVersion": float64(3),
			"sourceState": "OPEN", "transition": "AutoClose", "status": status,
			"scheduledTime": "2023-11-14T22:13:20.123Z", "armedTime": "2023-11-14T22:13:10Z",
		}
	}
	wantW := fields("t-waiting", eW, "WAITING")
	wantW["attempts"], wantW["lostOwners"] = float64(0), float64(0)
	wantW["expiresTime"] = "2023-11-14T22:14:20.123Z"
	wantW["nextAttemptTime"] = "2023-11-14T22:13:20.123Z"
	wantW["armedBy"] = map[string]any{"id": "alice", "kind": "user"}

	wantR := fields("t-running", eR, "RUNNING")
	wantR["attempts"], wantR["lostOwners"] = float64(0), float64(0)

	wantF := fields("t-failed", eF, "FAILED")
	wantF["attempts"], wantF["lostOwners"] = float64(2), float64(1)
	wantF["lastAttemptTime"] = "2023-11-14T22:15:00Z"
	wantF["lastError"] = "PROCESSOR_ERROR: boom"
	wantF["failureReason"] = "UNSAFE_WORK_NOT_COMPLETED"
	wantF["failedTime"] = "2023-11-14T22:16:40Z"

	for i, want := range []map[string]any{wantW, wantR, wantF} {
		if !reflect.DeepEqual(resp.Items[i], want) {
			t.Errorf("item %d =\n  %v\nwant\n  %v", i, resp.Items[i], want)
		}
	}
}

// outageErr carries the storage plugin's transient-unavailability marker and,
// in its text, the kind of connection detail a driver error carries.
type outageErr struct{}

func (outageErr) Error() string            { return "acquire timed out: postgres://u:p@db/cyoda" }
func (outageErr) StorageUnavailable() bool { return true }

func TestList_StorageUnavailable_503(t *testing.T) {
	st := &fakeStore{err: fmt.Errorf("failed to query scheduled tasks: %w", outageErr{})}
	w := call(t, fakeFactory{store: st}, genapi.ListScheduledTasksParams{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeStorageUnavailable)
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &pd)
	if r, _ := pd.Properties["retryable"].(bool); !r {
		t.Errorf("503 is not advertised as retryable; body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "postgres://") {
		t.Errorf("response leaked storage internals: %s", w.Body.String())
	}
}

func TestList_Failure_500WithTicket(t *testing.T) {
	for name, f := range map[string]fakeFactory{
		"query fails":             {store: &fakeStore{err: errors.New("scan: postgres://u:p@db/cyoda")}},
		"factory fails":           {err: errors.New("open: postgres://u:p@db/cyoda")},
		"stored entity id broken": {store: &fakeStore{page: spi.ScheduledTaskPage{Items: []spi.ScheduledTask{{ID: "t1", EntityID: "not-a-uuid", Status: spi.ScheduledTaskWaiting}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			w := call(t, f, genapi.ListScheduledTasksParams{})
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeServerError)
			var pd struct {
				Ticket string `json:"ticket"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &pd)
			if pd.Ticket == "" {
				t.Errorf("500 carries no ticket: %s", w.Body.String())
			}
			for _, leak := range []string{"postgres://", "not-a-uuid"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("response leaked %q: %s", leak, w.Body.String())
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/domain/scheduledtask/`
Expected: FAIL — build error `undefined: scheduledtask.NewHandler`.

- [ ] **Step 3: Implement** — `internal/domain/scheduledtask/handler.go`:

```go
// Package scheduledtask serves GET /scheduled-tasks: the caller's tenant's
// view of its scheduled-transition tasks.
package scheduledtask

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

const (
	defaultLimit    = 20
	maxLimit        = 1000
	maxModelNameLen = 256 // characters, as OpenAPI maxLength counts them
)

// Handler lists scheduled tasks.
type Handler struct {
	factory spi.StoreFactory
}

// NewHandler returns a Handler that reads through factory's ScheduledTaskStore.
func NewHandler(factory spi.StoreFactory) *Handler {
	return &Handler{factory: factory}
}

// ListScheduledTasks answers GET /scheduled-tasks. The tenant is the token's;
// no parameter names one. Every parameter is checked before the store is
// touched, so a malformed request never costs a query.
func (h *Handler) ListScheduledTasks(w http.ResponseWriter, r *http.Request, params genapi.ListScheduledTasksParams) {
	ctx := r.Context()
	tenant := spi.MustGetUserContext(ctx).Tenant.ID

	q, appErr := queryFromParams(params)
	if appErr != nil {
		common.WriteError(w, r, appErr)
		return
	}
	store, err := h.factory.ScheduledTaskStore(ctx)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to get scheduled task store", err))
		return
	}
	page, err := store.Query(ctx, tenant, q)
	if err != nil {
		common.WriteError(w, r, common.Internal("failed to query scheduled tasks", err))
		return
	}

	items := make([]genapi.ScheduledTaskDto, 0, len(page.Items))
	for _, t := range page.Items {
		dto, err := toDTO(t)
		if err != nil {
			common.WriteError(w, r, common.Internal("failed to render scheduled task", err))
			return
		}
		items = append(items, dto)
	}
	resp := genapi.ScheduledTaskPageDto{
		Items:      items,
		Pagination: genapi.CursorPaginationInfoDto{HasNext: page.Next != nil},
	}
	if page.Next != nil {
		next := encodeCursor(*page.Next)
		resp.Pagination.NextCursor = &next
	}
	common.WriteJSON(w, http.StatusOK, resp)
}

func badRequest(msg string) *common.AppError {
	return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, msg)
}

// queryFromParams applies the parameter rules of the published contract. A
// bad value is never echoed: the messages name the parameter and its rule.
func queryFromParams(p genapi.ListScheduledTasksParams) (spi.ScheduledTaskQuery, *common.AppError) {
	q := spi.ScheduledTaskQuery{Limit: defaultLimit}

	if p.Status != nil {
		for _, s := range *p.Status {
			switch st := spi.ScheduledTaskStatus(s); st {
			case spi.ScheduledTaskWaiting, spi.ScheduledTaskRunning, spi.ScheduledTaskFailed:
				q.Statuses = append(q.Statuses, st)
			default:
				return spi.ScheduledTaskQuery{}, badRequest("invalid status parameter: must be WAITING, RUNNING or FAILED")
			}
		}
	}
	if p.ModelName != nil {
		n := *p.ModelName
		if n == "" || !utf8.ValidString(n) || strings.ContainsRune(n, 0) || utf8.RuneCountInString(n) > maxModelNameLen {
			return spi.ScheduledTaskQuery{}, badRequest("invalid modelName parameter: must be 1 to 256 characters of valid UTF-8, without NUL")
		}
		q.ModelName = n
	}
	if p.ModelVersion != nil {
		if p.ModelName == nil {
			return spi.ScheduledTaskQuery{}, badRequest("modelVersion parameter requires modelName")
		}
		if *p.ModelVersion < 1 {
			return spi.ScheduledTaskQuery{}, badRequest("invalid modelVersion parameter: must be an integer of at least 1")
		}
		q.ModelVersion = int(*p.ModelVersion)
	}
	if p.EntityId != nil {
		q.EntityID = p.EntityId.String()
	}
	if p.Limit != nil {
		if *p.Limit < 1 || *p.Limit > maxLimit {
			return spi.ScheduledTaskQuery{}, badRequest("invalid limit parameter: must be an integer from 1 to 1000")
		}
		q.Limit = int(*p.Limit)
	}
	if p.Cursor != nil {
		c, err := decodeCursor(*p.Cursor)
		if err != nil {
			return spi.ScheduledTaskQuery{}, badRequest("invalid cursor parameter")
		}
		q.After = &c
	}
	return q, nil
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// toDTO renders one task. Arm and claim tokens, the claim owner, the tenant,
// the mark and the partial-commit flag are never rendered. Each optional field
// is present exactly when the published contract says it is.
func toDTO(t spi.ScheduledTask) (genapi.ScheduledTaskDto, error) {
	eid, err := uuid.Parse(t.EntityID)
	if err != nil {
		return genapi.ScheduledTaskDto{}, fmt.Errorf("failed to parse entity id of scheduled task %s: %w", t.ID, err)
	}
	d := genapi.ScheduledTaskDto{
		TaskId:        t.ID,
		EntityId:      eid,
		ModelName:     t.ModelName,
		ModelVersion:  int32(t.ModelVersion),
		SourceState:   t.SourceState,
		Transition:    t.Transition,
		Status:        string(t.Status),
		ScheduledTime: msTime(t.ScheduledTime),
		ArmedTime:     msTime(t.ArmedAt),
		Attempts:      int32(t.Attempts),
		LostOwners:    int32(t.LostOwners),
	}
	if t.TimeoutMs != nil {
		expires := msTime(t.ScheduledTime + *t.TimeoutMs)
		d.ExpiresTime = &expires
	}
	if t.Status == spi.ScheduledTaskWaiting {
		next := msTime(t.NextAttemptTime)
		d.NextAttemptTime = &next
	}
	if t.LastAttemptTime != nil {
		last := msTime(*t.LastAttemptTime)
		d.LastAttemptTime = &last
	}
	if t.LastError != "" {
		msg := t.LastError
		d.LastError = &msg
	}
	if t.Status == spi.ScheduledTaskFailed {
		reason := string(t.FailureReason)
		d.FailureReason = &reason
		if t.FailedTime != nil {
			failed := msTime(*t.FailedTime)
			d.FailedTime = &failed
		}
	}
	if t.ArmedBy.ID != "" {
		d.ArmedBy = &genapi.ScheduledTaskArmedByDto{Id: t.ArmedBy.ID, Kind: string(t.ArmedBy.Kind)}
	}
	return d, nil
}
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./internal/domain/scheduledtask/ && go vet ./internal/domain/scheduledtask/`
Expected: PASS; vet clean.

- [ ] **Step 5: Commit**

```
git add internal/domain/scheduledtask/handler.go internal/domain/scheduledtask/handler_test.go
git commit -m "feat(scheduledtask): list the tenant's scheduled tasks (#598)

Parameters are checked before the store is touched; limit is rejected, not
clamped; the cursor is never echoed; tokens and node ids are never rendered.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task Q-4: E2E — every row of the §8 error table, each filter, paging, isolation

**Spec:** §8 error table; §13 "`GET /scheduled-tasks`" column E.
These tests are written against the 501 stub of Q-1, so their RED is real.

**Files:**
- Test: `internal/e2e/scheduled_tasks_query_test.go` (new)
- Test: `internal/e2e/scheduled_tasks_query_faults_test.go` (new)
- Modify: `internal/e2e/zzz_errorcode_matrix_test.go` (`EntityErrorCodeMatrix`, `:28`)

**Interfaces:**
- Consumes: PostgreSQL columns of stream BP (spec §10.2): `status`,
  `failure_reason`, `last_error`, `attempts`, `lost_owners`,
  `last_attempt_time`, `failed_time`, `claim_token`, `claim_owner`. BP's
  `Query` runs on the main pool through the classifying querier (spec §10.2),
  which is what makes the torn-socket 503 reachable.
- Produces: nothing for other streams.

- [ ] **Step 1: Write the failing tests** — `internal/e2e/scheduled_tasks_query_test.go`:

```go
package e2e_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// schedQueryWorkflow parks every entity in OPEN with one scheduled transition
// an hour away. Its task stays WAITING for the whole test: the shared stack
// runs no scheduler, and a scheduler of a per-test harness claims only due
// tasks and RUNNING tasks of a lost owner.
func schedQueryWorkflow(withTimeout bool) string {
	timeout := ""
	if withTimeout {
		timeout = `, "timeoutMs": 60000`
	}
	return `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-query-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 3600000` + timeout + `}}]},
				"CLOSED": {}
			}
		}]
	}`
}

// schedTenant is a fresh tenant with its own M2M client. A fresh tenant has no
// tasks of other tests, so an unfiltered list is deterministic.
type schedTenant struct {
	id, clientID, secret string
}

func newSchedTenant(t *testing.T) schedTenant {
	t.Helper()
	id := "schedq-" + randSuffix(t)
	cid, secret := createM2MClient(t, id, "user-"+id, []string{"ROLE_ADMIN", "ROLE_M2M"})
	return schedTenant{id: id, clientID: cid, secret: secret}
}

// do issues one request as this tenant. path has no /api prefix.
func (s schedTenant) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	resp := adminRequestAs(t, s.clientID, s.secret, method, path, b)
	return resp.StatusCode, readBody(t, resp)
}

func (s schedTenant) setupModel(t *testing.T, name string, version int, withTimeout bool) {
	t.Helper()
	for _, step := range []struct{ method, path, body string }{
		{http.MethodPost, fmt.Sprintf("/model/import/JSON/SAMPLE_DATA/%s/%d", name, version), `{"k":1}`},
		{http.MethodPut, fmt.Sprintf("/model/%s/%d/lock", name, version), ""},
		{http.MethodPost, fmt.Sprintf("/model/%s/%d/workflow/import", name, version), schedQueryWorkflow(withTimeout)},
	} {
		if status, body := s.do(t, step.method, step.path, step.body); status/100 != 2 {
			t.Fatalf("%s %s: %d %s", step.method, step.path, status, body)
		}
	}
}

func (s schedTenant) createEntity(t *testing.T, name string, version int) string {
	t.Helper()
	status, body := s.do(t, http.MethodPost, fmt.Sprintf("/entity/JSON/%s/%d", name, version), `{"k":1}`)
	if status != http.StatusOK {
		t.Fatalf("create entity %s/%d: %d %s", name, version, status, body)
	}
	var res []struct {
		EntityIDs []string `json:"entityIds"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || len(res) == 0 || len(res[0].EntityIDs) != 1 {
		t.Fatalf("create entity: unexpected body %s (%v)", body, err)
	}
	return res[0].EntityIDs[0]
}

func (s schedTenant) list(t *testing.T, query url.Values) (int, string) {
	t.Helper()
	path := "/scheduled-tasks"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return s.do(t, http.MethodGet, path, "")
}

type schedTaskItem struct {
	TaskID          string     `json:"taskId"`
	EntityID        string     `json:"entityId"`
	ModelName       string     `json:"modelName"`
	ModelVersion    int        `json:"modelVersion"`
	SourceState     string     `json:"sourceState"`
	Transition      string     `json:"transition"`
	Status          string     `json:"status"`
	ScheduledTime   time.Time  `json:"scheduledTime"`
	ArmedTime       time.Time  `json:"armedTime"`
	ExpiresTime     *time.Time `json:"expiresTime"`
	Attempts        int        `json:"attempts"`
	LostOwners      int        `json:"lostOwners"`
	NextAttemptTime *time.Time `json:"nextAttemptTime"`
	LastAttemptTime *time.Time `json:"lastAttemptTime"`
	LastError       *string    `json:"lastError"`
	FailureReason   *string    `json:"failureReason"`
	FailedTime      *time.Time `json:"failedTime"`
	ArmedBy         *struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"armedBy"`
}

type schedTaskPage struct {
	Items      []schedTaskItem `json:"items"`
	Pagination struct {
		HasNext    bool    `json:"hasNext"`
		NextCursor *string `json:"nextCursor"`
	} `json:"pagination"`
}

// scheduledTaskKeys are the fields ScheduledTaskDto declares. Tokens, claim
// owners, node ids and the tenant are not among them and must never appear.
var scheduledTaskKeys = map[string]bool{
	"taskId": true, "entityId": true, "modelName": true, "modelVersion": true, "sourceState": true,
	"transition": true, "status": true, "scheduledTime": true, "armedTime": true, "expiresTime": true,
	"attempts": true, "lostOwners": true, "nextAttemptTime": true, "lastAttemptTime": true,
	"lastError": true, "failureReason": true, "failedTime": true, "armedBy": true,
}

func (s schedTenant) listPage(t *testing.T, query url.Values) schedTaskPage {
	t.Helper()
	status, body := s.list(t, query)
	if status != http.StatusOK {
		t.Fatalf("GET /scheduled-tasks?%s: %d %s", query.Encode(), status, body)
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	for _, it := range raw.Items {
		for k := range it {
			if !scheduledTaskKeys[k] {
				t.Errorf("item carries %q, which ScheduledTaskDto does not declare; body: %s", k, body)
			}
		}
	}
	var page schedTaskPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	if page.Items == nil {
		t.Fatalf("items is null, want an array; body: %s", body)
	}
	return page
}

// listAll follows nextCursor to the end and returns every item and each
// page's size.
func (s schedTenant) listAll(t *testing.T, query url.Values, limit int) ([]schedTaskItem, []int) {
	t.Helper()
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("limit", strconv.Itoa(limit))
	var items []schedTaskItem
	var sizes []int
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not end after 100 pages")
		}
		page := s.listPage(t, q)
		items = append(items, page.Items...)
		sizes = append(sizes, len(page.Items))
		if !page.Pagination.HasNext {
			if page.Pagination.NextCursor != nil {
				t.Errorf("the last page carries a nextCursor")
			}
			return items, sizes
		}
		if page.Pagination.NextCursor == nil {
			t.Fatal("hasNext without a nextCursor")
		}
		q.Set("cursor", *page.Pagination.NextCursor)
	}
}

func assertTaskEntities(t *testing.T, label string, items []schedTaskItem, want ...string) {
	t.Helper()
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.EntityID)
	}
	sort.Strings(got)
	w := slices.Clone(want)
	sort.Strings(w)
	if !slices.Equal(got, w) {
		t.Errorf("%s: entities %v, want %v", label, got, w)
	}
}

func assertTaskOrder(t *testing.T, items []schedTaskItem) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		a, b := items[i-1], items[i]
		if a.ScheduledTime.After(b.ScheduledTime) || (a.ScheduledTime.Equal(b.ScheduledTime) && a.TaskID >= b.TaskID) {
			t.Errorf("items %d and %d are out of (scheduledTime, taskId) order: %s/%s then %s/%s", i-1, i,
				a.ScheduledTime.Format(time.RFC3339Nano), a.TaskID, b.ScheduledTime.Format(time.RFC3339Nano), b.TaskID)
		}
	}
}

// markFailed turns an armed task into a FAILED one in PostgreSQL. Reading a
// FAILED task back needs no run, and the shared stack runs no scheduler.
func markFailed(t *testing.T, tenant, entityID string, lastAttemptAt, failedAt time.Time) {
	t.Helper()
	tag, err := dbPool.Exec(e2eCtx(t), `UPDATE scheduled_tasks
		SET status = 'FAILED', failure_reason = 'UNSAFE_WORK_NOT_COMPLETED',
		    last_error = 'PROCESSOR_ERROR: seeded failure', attempts = 2, lost_owners = 1,
		    last_attempt_time = $3, failed_time = $4, claim_token = NULL, claim_owner = NULL
		WHERE tenant_id = $1 AND entity_id = $2`,
		tenant, entityID, lastAttemptAt.UnixMilli(), failedAt.UnixMilli())
	if err != nil {
		t.Fatalf("seed FAILED task: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("seed FAILED task: %d rows, want 1", tag.RowsAffected())
	}
}

func TestScheduledTasks_Paging_200(t *testing.T) {
	ten := newSchedTenant(t)
	const model = "sched-page"
	ten.setupModel(t, model, 1, false)
	var want []string
	for i := 0; i < 5; i++ {
		want = append(want, ten.createEntity(t, model, 1))
	}

	first := ten.listPage(t, nil)
	if len(first.Items) != 5 || first.Pagination.HasNext {
		t.Fatalf("default page: %d items, hasNext %v; want 5 items and no next page", len(first.Items), first.Pagination.HasNext)
	}

	items, sizes := ten.listAll(t, nil, 2)
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("page sizes = %v, want [2 2 1]", sizes)
	}
	assertTaskEntities(t, "paged walk", items, want...)
	assertTaskOrder(t, items)
	for _, it := range items {
		if it.Status != "WAITING" || it.ModelName != model || it.ModelVersion != 1 || it.SourceState != "OPEN" ||
			it.Transition != "AutoClose" || it.Attempts != 0 || it.LostOwners != 0 {
			t.Errorf("unexpected item %+v", it)
		}
		if !it.ScheduledTime.Equal(it.ArmedTime.Add(time.Hour)) {
			t.Errorf("scheduledTime %s is not armedTime %s + 1h", it.ScheduledTime, it.ArmedTime)
		}
		if it.NextAttemptTime == nil || !it.NextAttemptTime.Equal(it.ScheduledTime) {
			t.Errorf("a new WAITING task's nextAttemptTime must equal scheduledTime: %+v", it)
		}
		if it.ExpiresTime != nil || it.FailureReason != nil || it.FailedTime != nil || it.LastError != nil || it.LastAttemptTime != nil {
			t.Errorf("fields present that the contract leaves absent here: %+v", it)
		}
		if it.ArmedBy == nil || it.ArmedBy.ID == "" || it.ArmedBy.Kind == "" {
			t.Errorf("armedBy missing for a task armed by a client write: %+v", it)
		}
	}
}

func TestScheduledTasks_Filters_200(t *testing.T) {
	ten := newSchedTenant(t)
	const modelA, modelB = "sched-filter-a", "sched-filter-b"
	ten.setupModel(t, modelA, 1, true)
	ten.setupModel(t, modelA, 2, false)
	ten.setupModel(t, modelB, 1, false)
	a1 := []string{ten.createEntity(t, modelA, 1), ten.createEntity(t, modelA, 1)}
	a2 := ten.createEntity(t, modelA, 2)
	b1 := ten.createEntity(t, modelB, 1)
	lastAt := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	failedAt := lastAt.Add(5 * time.Second)
	markFailed(t, ten.id, b1, lastAt, failedAt)

	waiting := []string{a1[0], a1[1], a2}
	all := []string{a1[0], a1[1], a2, b1}
	for _, tc := range []struct {
		label string
		query url.Values
		want  []string
	}{
		{"status WAITING", url.Values{"status": {"WAITING"}}, waiting},
		{"status FAILED", url.Values{"status": {"FAILED"}}, []string{b1}},
		{"status WAITING or FAILED", url.Values{"status": {"WAITING", "FAILED"}}, all},
		{"status RUNNING", url.Values{"status": {"RUNNING"}}, nil},
		{"modelName", url.Values{"modelName": {modelA}}, waiting},
		{"modelName and modelVersion", url.Values{"modelName": {modelA}, "modelVersion": {"2"}}, []string{a2}},
		{"entityId", url.Values{"entityId": {a1[0]}}, []string{a1[0]}},
		{"status and modelName", url.Values{"status": {"WAITING"}, "modelName": {modelB}}, nil},
	} {
		t.Run(tc.label, func(t *testing.T) {
			items, _ := ten.listAll(t, tc.query, 2)
			assertTaskEntities(t, tc.label, items, tc.want...)
			assertTaskOrder(t, items)
		})
	}

	t.Run("expiresTime from timeoutMs", func(t *testing.T) {
		page := ten.listPage(t, url.Values{"entityId": {a1[0]}})
		if len(page.Items) != 1 {
			t.Fatalf("items = %d, want 1", len(page.Items))
		}
		it := page.Items[0]
		if it.ExpiresTime == nil || !it.ExpiresTime.Equal(it.ScheduledTime.Add(time.Minute)) {
			t.Errorf("expiresTime = %v, want scheduledTime %s + 60s", it.ExpiresTime, it.ScheduledTime)
		}
	})

	t.Run("FAILED item", func(t *testing.T) {
		page := ten.listPage(t, url.Values{"entityId": {b1}})
		if len(page.Items) != 1 {
			t.Fatalf("items = %d, want 1", len(page.Items))
		}
		it := page.Items[0]
		if it.Status != "FAILED" || it.Attempts != 2 || it.LostOwners != 1 {
			t.Errorf("status/attempts/lostOwners = %s/%d/%d, want FAILED/2/1", it.Status, it.Attempts, it.LostOwners)
		}
		if it.FailureReason == nil || *it.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
			t.Errorf("failureReason = %v, want UNSAFE_WORK_NOT_COMPLETED", it.FailureReason)
		}
		if it.LastError == nil || *it.LastError != "PROCESSOR_ERROR: seeded failure" {
			t.Errorf("lastError = %v", it.LastError)
		}
		if it.FailedTime == nil || !it.FailedTime.Equal(failedAt) {
			t.Errorf("failedTime = %v, want %s", it.FailedTime, failedAt)
		}
		if it.LastAttemptTime == nil || !it.LastAttemptTime.Equal(lastAt) {
			t.Errorf("lastAttemptTime = %v, want %s", it.LastAttemptTime, lastAt)
		}
		if it.NextAttemptTime != nil {
			t.Errorf("a FAILED task carries nextAttemptTime %v", it.NextAttemptTime)
		}
	})
}

func TestScheduledTasks_EmptyList_200(t *testing.T) {
	a, b := newSchedTenant(t), newSchedTenant(t)
	const model = "sched-empty"
	b.setupModel(t, model, 1, false)
	bEntity := b.createEntity(t, model, 1)

	for label, q := range map[string]url.Values{
		"unknown model":           {"modelName": {"sched-never-imported"}},
		"unknown entity":          {"entityId": {"00000000-0000-4000-8000-00000000abcd"}},
		"another tenant's entity": {"entityId": {bEntity}},
		"another tenant's model":  {"modelName": {model}},
	} {
		t.Run(label, func(t *testing.T) {
			status, body := a.list(t, q)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", status, body)
			}
			if !strings.Contains(body, `"items":[]`) {
				t.Errorf("want an empty items array; body: %s", body)
			}
			page := a.listPage(t, q)
			if len(page.Items) != 0 || page.Pagination.HasNext {
				t.Errorf("want an empty last page; got %d items, hasNext %v", len(page.Items), page.Pagination.HasNext)
			}
		})
	}
}

func TestScheduledTasks_TenantIsolation(t *testing.T) {
	a, b := newSchedTenant(t), newSchedTenant(t)
	const model = "sched-iso"
	a.setupModel(t, model, 1, false)
	b.setupModel(t, model, 1, false)
	aIDs := []string{a.createEntity(t, model, 1), a.createEntity(t, model, 1)}
	bIDs := []string{b.createEntity(t, model, 1), b.createEntity(t, model, 1)}

	aTasks := map[string]bool{}
	aItems, _ := a.listAll(t, nil, 10)
	assertTaskEntities(t, "tenant A", aItems, aIDs...)
	for _, it := range aItems {
		aTasks[it.TaskID] = true
	}

	for label, q := range map[string]url.Values{
		"no filter":                  nil,
		"status":                     {"status": {"WAITING"}},
		"modelName":                  {"modelName": {model}},
		"modelName and modelVersion": {"modelName": {model}, "modelVersion": {"1"}},
	} {
		t.Run(label, func(t *testing.T) {
			items, sizes := b.listAll(t, q, 1)
			assertTaskEntities(t, "tenant B, "+label, items, bIDs...)
			if !slices.Equal(sizes, []int{1, 1}) {
				t.Errorf("page sizes = %v, want [1 1]", sizes)
			}
			for _, it := range items {
				if aTasks[it.TaskID] {
					t.Errorf("tenant B sees tenant A's task %s", it.TaskID)
				}
			}
		})
	}
	for _, id := range aIDs {
		if page := b.listPage(t, url.Values{"entityId": {id}}); len(page.Items) != 0 {
			t.Errorf("tenant B sees tasks of tenant A's entity %s", id)
		}
	}

	// A cursor is a position, not a capability: tenant A's cursor, used by
	// tenant B, returns only tenant B's tasks.
	pageA := a.listPage(t, url.Values{"limit": {"1"}})
	if pageA.Pagination.NextCursor == nil {
		t.Fatal("tenant A's first page of one has no nextCursor")
	}
	pageB := b.listPage(t, url.Values{"cursor": {*pageA.Pagination.NextCursor}})
	for _, it := range pageB.Items {
		if !slices.Contains(bIDs, it.EntityID) {
			t.Errorf("tenant B, with tenant A's cursor, sees entity %s", it.EntityID)
		}
	}
}

// Every 400 BAD_REQUEST case of the error table, through the generated binder
// and the handler. A bad cursor is never echoed.
func TestScheduledTasks_InvalidParameters_400(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	wrongVersion := enc(`{"v":2,"t":1,"i":"x"}`)
	longCursor := strings.Repeat("A", 257)
	for _, tc := range []struct{ name, query, cursor string }{
		{"unknown status", "status=PENDING", ""},
		{"lower-case status", "status=waiting", ""},
		{"empty status", "status=", ""},
		{"one valid, one unknown status", "status=WAITING&status=DONE", ""},
		{"modelVersion without modelName", "modelVersion=1", ""},
		{"modelVersion zero", "modelName=m&modelVersion=0", ""},
		{"modelVersion negative", "modelName=m&modelVersion=-1", ""},
		{"modelVersion not an integer", "modelName=m&modelVersion=abc", ""},
		{"modelVersion decimal", "modelName=m&modelVersion=1.5", ""},
		{"entityId not a UUID", "entityId=not-a-uuid", ""},
		{"modelName empty", "modelName=", ""},
		{"modelName 257 characters", "modelName=" + strings.Repeat("m", 257), ""},
		{"modelName with NUL", "modelName=m%00n", ""},
		{"modelName invalid UTF-8", "modelName=m%FF", ""},
		{"limit zero", "limit=0", ""},
		{"limit 1001", "limit=1001", ""},
		{"limit not an integer", "limit=abc", ""},
		{"limit empty", "limit=", ""},
		{"cursor not base64url", "cursor=%21%21%21", "!!!"},
		{"cursor wrong version", "cursor=" + wrongVersion, wrongVersion},
		{"cursor over 256 characters", "cursor=" + longCursor, longCursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuth(t, http.MethodGet, "/api/scheduled-tasks?"+tc.query, "")
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, body)
			}
			if code := problemErrorCode(body); code != "BAD_REQUEST" {
				t.Errorf("errorCode = %q, want BAD_REQUEST; body: %s", code, body)
			}
			if tc.cursor != "" && strings.Contains(body, tc.cursor) {
				t.Errorf("the 400 echoes the cursor: %s", body)
			}
		})
	}
}

func TestScheduledTasks_Unauthorized_401(t *testing.T) {
	for name, header := range map[string]string{
		"no token":            "",
		"garbage token":       "Bearer not-a-jwt",
		"untrusted signature": "Bearer " + untrustedJWT,
	} {
		t.Run(name, func(t *testing.T) {
			resp := unauthRequest(t, http.MethodGet, "/api/scheduled-tasks", header)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			assertUnauthorizedProblem(t, resp)
		})
	}
}
```

`internal/e2e/scheduled_tasks_query_faults_test.go`:

```go
package e2e_test

// The 500 and 503 cells of GET /scheduled-tasks, injected for real on
// PostgreSQL. Harnesses and their isolation: lookup_storage_failure_e2e_test.go
// (terminated session → unmarked 57P01 → 500 with a ticket) and
// torn_connection_e2e_test.go (torn socket → marked → retryable 503).

import (
	"net/http"
	"testing"
)

func TestScheduledTasks_StorageFailure_500(t *testing.T) {
	h := newLookupFailureHarness(t)
	k := newSessionKiller(t)
	list := func() (int, string) {
		resp := h.DoAuth(t, http.MethodGet, "/api/scheduled-tasks", "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	saw500 := false
	for i := 0; i < lookupFailureCycles && !saw500; i++ {
		if status, body := list(); status != http.StatusOK {
			t.Fatalf("cycle %d: warm-up: %d %s", i, status, body)
		}
		if k.kill(t) == 0 {
			t.Fatalf("cycle %d: no harness session to terminate", i)
		}
		status, body := list()
		t.Logf("cycle %d: status=%d body=%s", i, status, body)
		assertNotSubstitutedNotFound(t, status, body) // ticket on a 5xx, no driver detail
		if status == http.StatusInternalServerError {
			if code := problemErrorCode(body); code != "SERVER_ERROR" {
				t.Errorf("errorCode = %q, want SERVER_ERROR; body: %s", code, body)
			}
			saw500 = true
		}
	}
	if !saw500 {
		t.Fatalf("no probe answered 500 in %d cycles; the fault was never injected", lookupFailureCycles)
	}
}

func TestScheduledTasks_TornConnection_503(t *testing.T) {
	h := newTornHarness(t)
	list := h.get("/api/scheduled-tasks")
	probeTorn(t, h, list, list) // asserts 503 STORAGE_UNAVAILABLE, retryable, no leak
}
```

`internal/e2e/zzz_errorcode_matrix_test.go`, add to `EntityErrorCodeMatrix`
after the `patchSingleWithLoopback` entry:

```go
	// GET /scheduled-tasks. 401 and 500 are cross-cutting (below). The 503 is
	// produced on a private torn-connection harness that is not behind the
	// conformance validator (TestScheduledTasks_TornConnection_503), so it is
	// not a cell here.
	"listScheduledTasks": {
		{Status: 400, Code: "BAD_REQUEST"}, // TestScheduledTasks_InvalidParameters_400
	},
```

- [ ] **Step 2: Run to verify RED**

Run: `make preflight && go test ./internal/e2e/ -run 'TestScheduledTasks'`
Expected: FAIL — every request that reaches the handler answers `501`
(`NOT_IMPLEMENTED` from the Q-1 stub): the 200 tests fail on status, the 400
table fails on status, the 500 test fails "no probe answered 500", the 503
test fails in `assertRetryable503`. The 401 cases pass (the auth middleware
answers before the stub); they are pinned here for the endpoint's own route.

- [ ] **Step 3: Implement** — nothing in this task; Q-6 wires the handler.

- [ ] **Step 4: Commit the RED tests** (the lead may instead squash Q-4 into
  Q-6; the tests are committed first so Q-6's diff shows only the wiring)

```
git add internal/e2e/scheduled_tasks_query_test.go internal/e2e/scheduled_tasks_query_faults_test.go internal/e2e/zzz_errorcode_matrix_test.go
git commit -m "test(e2e): GET /scheduled-tasks — every error cell, each filter, paging, isolation (#598)

RED until the handler is wired.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task Q-5: Parity — client method and three scenarios

**Spec:** §13 "`GET /scheduled-tasks`" column P: several pages; each filter;
another tenant's tasks never returned.

**Files:**
- Create: `e2e/parity/client/scheduled_tasks.go`
- Test: `e2e/parity/client/scheduled_tasks_test.go`
- Create: `e2e/parity/scheduled_tasks_query.go`
- Modify: `e2e/parity/registry.go` (header count `:5`; entries after `:542`)
- Modify: `e2e/parity/registry_count_test.go` (`wantParityScenarioCount`, `:9`)

**Interfaces:**
- Produces (for stream T and the FAILED-item scenario, see Open point 2):
  ```go
  // package client
  type ScheduledTask struct { TaskID, EntityID, ModelName string; ModelVersion int; SourceState, Transition, Status string
      ScheduledTime, ArmedTime time.Time; ExpiresTime *time.Time; Attempts, LostOwners int
      NextAttemptTime, LastAttemptTime *time.Time; LastError, FailureReason string; FailedTime *time.Time
      ArmedBy *ScheduledTaskArmedBy }
  type ScheduledTaskArmedBy struct { ID, Kind string }
  type ScheduledTaskPage struct { Items []ScheduledTask; Pagination CursorPaginationInfo }
  func (c *Client) ListScheduledTasks(t *testing.T, query url.Values) (ScheduledTaskPage, error)
  ```

- [ ] **Step 1: Write the failing tests**

`e2e/parity/client/scheduled_tasks_test.go`:

```go
package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

const scheduledTaskPageBody = `{"items":[{"taskId":"t1","entityId":"5f1c1b0e-6d1a-41f1-8000-000000000001",` +
	`"modelName":"m","modelVersion":1,"sourceState":"OPEN","transition":"AutoClose","status":"FAILED",` +
	`"scheduledTime":"2023-11-14T22:13:20.123Z","armedTime":"2023-11-14T22:13:10Z","attempts":2,"lostOwners":1,` +
	`"lastError":"PROCESSOR_ERROR: boom","failureReason":"UNSAFE_WORK_NOT_COMPLETED",` +
	`"failedTime":"2023-11-14T22:16:40Z","armedBy":{"id":"alice","kind":"user"}}],` +
	`"pagination":{"hasNext":true,"nextCursor":"abc"}}`

func TestListScheduledTasks_SendsQueryAndDecodesPage(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, scheduledTaskPageBody)
	}))
	defer srv.Close()

	page, err := NewClient(srv.URL, "tok").ListScheduledTasks(t, url.Values{"status": {"FAILED", "WAITING"}, "limit": {"1"}})
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	if gotPath != "/api/scheduled-tasks" || gotQuery != "limit=1&status=FAILED&status=WAITING" {
		t.Errorf("request = %s?%s", gotPath, gotQuery)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	it := page.Items[0]
	if it.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || it.Attempts != 2 || it.LostOwners != 1 ||
		it.ArmedBy == nil || it.ArmedBy.Kind != "user" || it.FailedTime == nil || it.NextAttemptTime != nil {
		t.Errorf("decoded item = %+v", it)
	}
	if !page.Pagination.HasNext || page.Pagination.NextCursor != "abc" {
		t.Errorf("pagination = %+v", page.Pagination)
	}
}

// An undeclared field in an item is drift, as for every parity client type.
func TestListScheduledTasks_UnknownFieldIsDrift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"taskId":"t1","armToken":"x"}],"pagination":{"hasNext":false}}`)
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "tok").ListScheduledTasks(t, nil); err == nil {
		t.Fatal("an item with an undeclared field decoded without error")
	}
}
```

`e2e/parity/scheduled_tasks_query.go`:

```go
package parity

import (
	"net/url"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// scheduledQueryWorkflow parks each entity in OPEN with one scheduled
// transition an hour away, so its task stays WAITING while the scenario runs.
const scheduledQueryWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "sched-query-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
				"schedule": {"delayMs": 3600000}}]},
			"CLOSED": {}
		}
	}]
}`

func setupScheduledQueryModel(t *testing.T, c *client.Client, name string, version int) {
	t.Helper()
	if err := c.ImportModel(t, name, version, `{"k":1}`); err != nil {
		t.Fatalf("ImportModel %s/%d: %v", name, version, err)
	}
	if err := c.LockModel(t, name, version); err != nil {
		t.Fatalf("LockModel %s/%d: %v", name, version, err)
	}
	if err := c.ImportWorkflow(t, name, version, scheduledQueryWorkflow); err != nil {
		t.Fatalf("ImportWorkflow %s/%d: %v", name, version, err)
	}
}

func createScheduledQueryEntities(t *testing.T, c *client.Client, name string, version, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := c.CreateEntity(t, name, version, `{"k":1}`)
		if err != nil {
			t.Fatalf("CreateEntity %s/%d: %v", name, version, err)
		}
		ids = append(ids, id.String())
	}
	return ids
}

func listScheduledTasksPage(t *testing.T, c *client.Client, query url.Values) client.ScheduledTaskPage {
	t.Helper()
	page, err := c.ListScheduledTasks(t, query)
	if err != nil {
		t.Fatalf("ListScheduledTasks %s: %v", query.Encode(), err)
	}
	if page.Items == nil {
		t.Fatalf("ListScheduledTasks %s: items is null, want an array", query.Encode())
	}
	return page
}

// walkScheduledTasks follows nextCursor to the end and returns every item and
// each page's size.
func walkScheduledTasks(t *testing.T, c *client.Client, query url.Values, limit int) ([]client.ScheduledTask, []int) {
	t.Helper()
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("limit", strconv.Itoa(limit))
	var items []client.ScheduledTask
	var sizes []int
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not end after 100 pages")
		}
		page := listScheduledTasksPage(t, c, q)
		items = append(items, page.Items...)
		sizes = append(sizes, len(page.Items))
		if !page.Pagination.HasNext {
			if page.Pagination.NextCursor != "" {
				t.Errorf("the last page carries a nextCursor")
			}
			return items, sizes
		}
		if page.Pagination.NextCursor == "" {
			t.Fatal("hasNext without a nextCursor")
		}
		q.Set("cursor", page.Pagination.NextCursor)
	}
}

func assertScheduledTaskEntities(t *testing.T, label string, items []client.ScheduledTask, want ...string) {
	t.Helper()
	got := make([]string, 0, len(items))
	for _, it := range items {
		got = append(got, it.EntityID)
	}
	sort.Strings(got)
	w := slices.Clone(want)
	sort.Strings(w)
	if !slices.Equal(got, w) {
		t.Errorf("%s: entities %v, want %v", label, got, w)
	}
}

func assertScheduledTaskOrder(t *testing.T, items []client.ScheduledTask) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		a, b := items[i-1], items[i]
		if a.ScheduledTime.After(b.ScheduledTime) || (a.ScheduledTime.Equal(b.ScheduledTime) && a.TaskID >= b.TaskID) {
			t.Errorf("items %d and %d are out of (scheduledTime, taskId) order", i-1, i)
		}
	}
}

// RunScheduledTasksQueryPaging: an unfiltered walk returns every task of the
// tenant once, in (scheduledTime, taskId) order, with exact page sizes.
func RunScheduledTasksQueryPaging(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const model = "sched-query-paging"
	setupScheduledQueryModel(t, c, model, 1)
	want := createScheduledQueryEntities(t, c, model, 1, 5)

	first := listScheduledTasksPage(t, c, nil)
	if len(first.Items) != 5 || first.Pagination.HasNext {
		t.Fatalf("default page: %d items, hasNext %v; want 5 and no next page", len(first.Items), first.Pagination.HasNext)
	}
	items, sizes := walkScheduledTasks(t, c, nil, 2)
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("page sizes = %v, want [2 2 1]", sizes)
	}
	assertScheduledTaskEntities(t, "paged walk", items, want...)
	assertScheduledTaskOrder(t, items)
	for _, it := range items {
		if it.Status != "WAITING" || it.NextAttemptTime == nil || it.FailureReason != "" ||
			!it.ScheduledTime.Equal(it.ArmedTime.Add(time.Hour)) {
			t.Errorf("unexpected item %+v", it)
		}
	}
}

// RunScheduledTasksQueryFilters: each filter, alone and combined.
func RunScheduledTasksQueryFilters(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)
	const modelA, modelB = "sched-query-filter-a", "sched-query-filter-b"
	setupScheduledQueryModel(t, c, modelA, 1)
	setupScheduledQueryModel(t, c, modelA, 2)
	setupScheduledQueryModel(t, c, modelB, 1)
	a1 := createScheduledQueryEntities(t, c, modelA, 1, 2)
	a2 := createScheduledQueryEntities(t, c, modelA, 2, 1)
	b1 := createScheduledQueryEntities(t, c, modelB, 1, 1)
	all := slices.Concat(a1, a2, b1)
	modelAAll := slices.Concat(a1, a2)

	for _, tc := range []struct {
		label string
		query url.Values
		want  []string
	}{
		{"status WAITING", url.Values{"status": {"WAITING"}}, all},
		{"status RUNNING", url.Values{"status": {"RUNNING"}}, nil},
		{"status FAILED", url.Values{"status": {"FAILED"}}, nil},
		{"status WAITING or RUNNING", url.Values{"status": {"WAITING", "RUNNING"}}, all},
		{"modelName", url.Values{"modelName": {modelA}}, modelAAll},
		{"modelName and modelVersion", url.Values{"modelName": {modelA}, "modelVersion": {"2"}}, a2},
		{"entityId", url.Values{"entityId": {a1[0]}}, a1[:1]},
		{"entityId and a model it is not in", url.Values{"entityId": {a1[0]}, "modelName": {modelB}}, nil},
	} {
		t.Run(tc.label, func(t *testing.T) {
			items, _ := walkScheduledTasks(t, c, tc.query, 2)
			assertScheduledTaskEntities(t, tc.label, items, tc.want...)
			assertScheduledTaskOrder(t, items)
		})
	}
}

// RunScheduledTasksQueryTenantIsolation: two tenants with the same model name;
// under every filter each sees only its own tasks, and a cursor minted for one
// tenant reveals nothing of it to the other.
func RunScheduledTasksQueryTenantIsolation(t *testing.T, fixture BackendFixture) {
	tenantA, tenantB := fixture.NewTenant(t), fixture.NewTenant(t)
	cA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	cB := client.NewClient(fixture.BaseURL(), tenantB.Token)
	const model = "sched-query-iso"
	setupScheduledQueryModel(t, cA, model, 1)
	setupScheduledQueryModel(t, cB, model, 1)
	aIDs := createScheduledQueryEntities(t, cA, model, 1, 2)
	bIDs := createScheduledQueryEntities(t, cB, model, 1, 2)

	aItems, _ := walkScheduledTasks(t, cA, nil, 10)
	assertScheduledTaskEntities(t, "tenant A", aItems, aIDs...)
	aTasks := map[string]bool{}
	for _, it := range aItems {
		aTasks[it.TaskID] = true
	}

	for label, q := range map[string]url.Values{
		"no filter":                  nil,
		"status":                     {"status": {"WAITING"}},
		"modelName":                  {"modelName": {model}},
		"modelName and modelVersion": {"modelName": {model}, "modelVersion": {"1"}},
	} {
		items, sizes := walkScheduledTasks(t, cB, q, 1)
		assertScheduledTaskEntities(t, "tenant B, "+label, items, bIDs...)
		if !slices.Equal(sizes, []int{1, 1}) {
			t.Errorf("tenant B, %s: page sizes = %v, want [1 1]", label, sizes)
		}
		for _, it := range items {
			if aTasks[it.TaskID] {
				t.Errorf("tenant B, %s: sees tenant A's task %s", label, it.TaskID)
			}
		}
	}
	for _, id := range aIDs {
		if page := listScheduledTasksPage(t, cB, url.Values{"entityId": {id}}); len(page.Items) != 0 {
			t.Errorf("tenant B sees tasks of tenant A's entity %s", id)
		}
	}

	pageA := listScheduledTasksPage(t, cA, url.Values{"limit": {"1"}})
	if pageA.Pagination.NextCursor == "" {
		t.Fatal("tenant A's first page of one has no nextCursor")
	}
	for _, it := range listScheduledTasksPage(t, cB, url.Values{"cursor": {pageA.Pagination.NextCursor}}).Items {
		if !slices.Contains(bIDs, it.EntityID) {
			t.Errorf("tenant B, with tenant A's cursor, sees entity %s", it.EntityID)
		}
	}
}
```

`e2e/parity/registry.go`, after `{"AsyncOrderingRespected", RunAsyncOrderingRespected},`:

```go

	// GET /scheduled-tasks: paging, filters and tenant isolation of the
	// scheduled-task query.
	{"ScheduledTasksQueryPaging", RunScheduledTasksQueryPaging},
	{"ScheduledTasksQueryFilters", RunScheduledTasksQueryFilters},
	{"ScheduledTasksQueryTenantIsolation", RunScheduledTasksQueryTenantIsolation},
```

Raise `wantParityScenarioCount` (`registry_count_test.go:9`) and the number in
the `registry.go` header comment (`:5`) by 3 from their current values (292 on
the base of this branch; other streams also add entries).

- [ ] **Step 2: Run to verify RED**

Run: `go test ./e2e/parity/client/ -run TestListScheduledTasks`
Expected: FAIL — build error `c.ListScheduledTasks undefined`.

- [ ] **Step 3: Implement the client** — `e2e/parity/client/scheduled_tasks.go`:

```go
package client

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// ScheduledTask mirrors ScheduledTaskDto in api/openapi.yaml.
type ScheduledTask struct {
	TaskID          string                `json:"taskId"`
	EntityID        string                `json:"entityId"`
	ModelName       string                `json:"modelName"`
	ModelVersion    int                   `json:"modelVersion"`
	SourceState     string                `json:"sourceState"`
	Transition      string                `json:"transition"`
	Status          string                `json:"status"`
	ScheduledTime   time.Time             `json:"scheduledTime"`
	ArmedTime       time.Time             `json:"armedTime"`
	ExpiresTime     *time.Time            `json:"expiresTime,omitempty"`
	Attempts        int                   `json:"attempts"`
	LostOwners      int                   `json:"lostOwners"`
	NextAttemptTime *time.Time            `json:"nextAttemptTime,omitempty"`
	LastAttemptTime *time.Time            `json:"lastAttemptTime,omitempty"`
	LastError       string                `json:"lastError,omitempty"`
	FailureReason   string                `json:"failureReason,omitempty"`
	FailedTime      *time.Time            `json:"failedTime,omitempty"`
	ArmedBy         *ScheduledTaskArmedBy `json:"armedBy,omitempty"`
}

// ScheduledTaskArmedBy mirrors ScheduledTaskArmedByDto.
type ScheduledTaskArmedBy struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// ScheduledTaskPage mirrors ScheduledTaskPageDto.
type ScheduledTaskPage struct {
	Items      []ScheduledTask      `json:"items"`
	Pagination CursorPaginationInfo `json:"pagination"`
}

// ListScheduledTasks issues GET /api/scheduled-tasks with the given query
// (status, modelName, modelVersion, entityId, cursor, limit).
func (c *Client) ListScheduledTasks(t *testing.T, query url.Values) (ScheduledTaskPage, error) {
	t.Helper()
	path := "/api/scheduled-tasks"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var page ScheduledTaskPage
	if _, err := c.doJSON(t, http.MethodGet, path, nil, &page); err != nil {
		return ScheduledTaskPage{}, err
	}
	return page, nil
}
```

Run: `go test ./e2e/parity/client/ -run TestListScheduledTasks && go test ./e2e/parity/ -run 'TestParityScenarioCount|TestParityScenarioNamesUnique'`
Expected: PASS.

Run: `make preflight && go test -count=1 ./e2e/parity/memory/ -run 'TestParity/ScheduledTasksQuery'`
Expected: FAIL — each scenario's first `ListScheduledTasks` returns
`status 501` (the Q-1 stub).

- [ ] **Step 4: Commit**

```
git add e2e/parity/client/scheduled_tasks.go e2e/parity/client/scheduled_tasks_test.go e2e/parity/scheduled_tasks_query.go e2e/parity/registry.go e2e/parity/registry_count_test.go
git commit -m "test(parity): GET /scheduled-tasks paging, filters and tenant isolation (#598)

RED until the handler is wired.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task Q-6: Wiring

**Spec:** §8. Turns Q-4 and Q-5 green.

**Files:**
- Modify: `internal/api/server.go` (field in `Server`, `:20-30`; delegation after the audit block, `:362`)
- Modify: `app/app.go` (after `server.Audit = …`, `:678`; import block `:36-44`)
- Modify: `api/openapi.yaml` (remove `x-cyoda-status: planned` from `listScheduledTasks`)
- Test: `internal/api/scheduled_tasks_route_test.go` (new)

**Interfaces:**
- Consumes: `scheduledtask.NewHandler` (Q-3); a working `Query` on every
  backend (BM, BQ, BP).
- Produces: `internalapi.Server.ScheduledTasks *scheduledtask.Handler`.

- [ ] **Step 1: Write the failing test** — `internal/api/scheduled_tasks_route_test.go`:

```go
package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	internalapi "github.com/cyoda-platform/cyoda-go/internal/api"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/scheduledtask"
)

type routeStore struct {
	spi.ScheduledTaskStore
	calls int
	query spi.ScheduledTaskQuery
}

func (s *routeStore) Query(_ context.Context, _ spi.TenantID, q spi.ScheduledTaskQuery) (spi.ScheduledTaskPage, error) {
	s.calls++
	s.query = q
	return spi.ScheduledTaskPage{}, nil
}

type routeFactory struct {
	spi.StoreFactory
	store spi.ScheduledTaskStore
}

func (f routeFactory) ScheduledTaskStore(context.Context) (spi.ScheduledTaskStore, error) {
	return f.store, nil
}

// TestListScheduledTasks_RoutedAndBound: the generated router reaches the
// handler through Server, binds repeated and typed parameters, and answers the
// binder's own refusals with 400 BAD_REQUEST without echoing the value.
func TestListScheduledTasks_RoutedAndBound(t *testing.T) {
	st := &routeStore{}
	s := internalapi.NewServer()
	s.ScheduledTasks = scheduledtask.NewHandler(routeFactory{store: st})
	h := genapi.HandlerWithOptions(s, genapi.StdHTTPServerOptions{
		BaseRouter:       internalapi.NewChiMux(),
		ErrorHandlerFunc: internalapi.BindingErrorHandler,
	})
	serve := func(target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r = r.WithContext(spi.WithUserContext(r.Context(),
			&spi.UserContext{UserID: "u1", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: "tenant-a"}}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := serve("/scheduled-tasks?status=WAITING&status=FAILED&modelName=orders&modelVersion=2&limit=5")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	want := spi.ScheduledTaskQuery{
		Statuses:     []spi.ScheduledTaskStatus{spi.ScheduledTaskWaiting, spi.ScheduledTaskFailed},
		ModelName:    "orders",
		ModelVersion: 2,
		Limit:        5,
	}
	if !reflect.DeepEqual(st.query, want) {
		t.Errorf("query = %+v, want %+v", st.query, want)
	}

	for name, tc := range map[string]struct{ target, value string }{
		"limit not an integer":        {"/scheduled-tasks?limit=abc", "abc"},
		"limit empty":                 {"/scheduled-tasks?limit=", ""},
		"limit decimal":               {"/scheduled-tasks?limit=2.5", "2.5"},
		"limit repeated":              {"/scheduled-tasks?limit=1&limit=2", ""},
		"modelVersion not an integer": {"/scheduled-tasks?modelName=m&modelVersion=xyz", "xyz"},
		"entityId not a UUID":         {"/scheduled-tasks?entityId=not-a-uuid", "not-a-uuid"},
	} {
		t.Run(name, func(t *testing.T) {
			before := st.calls
			w := serve(tc.target)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), "BAD_REQUEST")
			if tc.value != "" && strings.Contains(w.Body.String(), tc.value) {
				t.Errorf("the 400 echoes %q: %s", tc.value, w.Body.String())
			}
			if st.calls != before {
				t.Errorf("the store was queried for a request the binder refused")
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./internal/api/ -run TestListScheduledTasks_RoutedAndBound`
Expected: FAIL — build error `s.ScheduledTasks undefined (type *api.Server has no field or method ScheduledTasks)`.

- [ ] **Step 3: Implement**

`internal/api/server.go` — import
`"github.com/cyoda-platform/cyoda-go/internal/domain/scheduledtask"`; add the
field after `Account *account.Handler`:

```go
	ScheduledTasks *scheduledtask.Handler
```

and, after the audit delegation block:

```go
// ---------------------------------------------------------------------------
// Scheduled task delegation (1 method)
// ---------------------------------------------------------------------------

func (s *Server) ListScheduledTasks(w http.ResponseWriter, r *http.Request, params genapi.ListScheduledTasksParams) {
	if s.ScheduledTasks != nil {
		s.ScheduledTasks.ListScheduledTasks(w, r, params)
		return
	}
	s.Unimplemented.ListScheduledTasks(w, r, params)
}
```

`app/app.go` — import
`"github.com/cyoda-platform/cyoda-go/internal/domain/scheduledtask"`; after
`server.Audit = audit.New(a.storeFactory)`:

```go
	server.ScheduledTasks = scheduledtask.NewHandler(a.storeFactory)
```

`api/openapi.yaml` — delete the line `      x-cyoda-status: planned` under
`operationId: listScheduledTasks`. Then `go generate ./api && make check-codegen`
(the extension is not in the generated code; the check confirms it).

- [ ] **Step 4: Run to verify GREEN**

Run: `go build ./... && go test ./internal/api/... ./internal/domain/scheduledtask/... ./api/...`
Expected: PASS.

Run: `make preflight && go test ./internal/e2e/ -run 'TestScheduledTasks'`
Expected: PASS — every Q-4 test.

Run: `go test ./internal/e2e/`
Expected: PASS, including `TestOpenAPIConformanceReport` (the operation is now
unmarked and exercised; every response matches the schema) and
`TestZZZErrorCodeMatrix` (the `listScheduledTasks` 400 cell is produced).

Run each backend's scenarios:
```
go test -count=1 ./e2e/parity/memory/   -run 'TestParity/ScheduledTasksQuery'
go test -count=1 ./e2e/parity/sqlite/   -run 'TestParity/ScheduledTasksQuery'
go test -count=1 ./e2e/parity/postgres/ -run 'TestParity/ScheduledTasksQuery'
```
Expected: PASS on all three.

`go vet ./internal/api/ ./app/` is clean.

- [ ] **Step 5: Commit**

```
git add internal/api/server.go internal/api/scheduled_tasks_route_test.go app/app.go api/openapi.yaml
git commit -m "feat(api): serve GET /scheduled-tasks (#598)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task Q-7: Help topic `scheduled-tasks`; the openapi topic's path count

**Spec:** §12 "New help topic `scheduled-tasks`, shaped like `audit.md`, added
to `topLevelTopicsV061`". Gate 6: `openapi.md:87` states 83 paths; the spec has
71 before this stream and 72 after. README C-D1: `config/scheduler.md` (R-10's)
gains `scheduled-tasks` in its SEE ALSO here, because the topic exists only
from this task on.

**Files:**
- Create: `cmd/cyoda/help/content/scheduled-tasks.md`
- Modify: `cmd/cyoda/help/help_test.go` (`topLevelTopicsV061`, `:423-427`)
- Modify: `cmd/cyoda/help/content/openapi.md` (`:87` count; tag list `:89-98`)
- Modify: `cmd/cyoda/help/content/config/scheduler.md` (front matter `see_also` `:5-9`; SEE ALSO `:28-33`)
- Test: `cmd/cyoda/help/scheduled_tasks_help_test.go` (new)

**Interfaces:**
- Consumes: the Q-1 contract through `genapi.GetSwagger()`; SPI status and
  reason constants (stream S).
- Produces: the help topic other topics may reference as `scheduled-tasks`
  (stream D links it from `workflows.md` SCHEDULED TRANSITIONS).

- [ ] **Step 1: Write the failing tests**

`cmd/cyoda/help/help_test.go` — `topLevelTopicsV061` becomes:

```go
var topLevelTopicsV061 = []string{
	"cli", "config", "errors", "crud", "search", "analytics",
	"models", "workflows", "run", "helm", "telemetry",
	"openapi", "grpc", "quickstart", "admin", "cluster",
	"scheduled-tasks",
}
```

`cmd/cyoda/help/scheduled_tasks_help_test.go`:

```go
package help

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TestScheduledTasksTopic_CoversTheContract: the topic names every parameter,
// every DTO field, every status and failure reason, and every error code of
// GET /scheduled-tasks, read from the published contract so a later field
// cannot be added without documenting it.
func TestScheduledTasksTopic_CoversTheContract(t *testing.T) {
	topic := DefaultTree.Find([]string{"scheduled-tasks"})
	if topic == nil {
		t.Fatal("help topic scheduled-tasks is missing")
	}
	body := string(topic.Body)
	doc, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	op := doc.Paths.Find("/scheduled-tasks").Get
	var names []string
	for _, p := range op.Parameters {
		names = append(names, p.Value.Name)
	}
	for name := range doc.Components.Schemas["ScheduledTaskDto"].Value.Properties {
		names = append(names, name)
	}
	for _, s := range []spi.ScheduledTaskStatus{spi.ScheduledTaskWaiting, spi.ScheduledTaskRunning, spi.ScheduledTaskFailed} {
		names = append(names, string(s))
	}
	for _, r := range []spi.ScheduledTaskFailureReason{spi.FailureUnsafeWorkNotCompleted, spi.FailureOwnerLostRepeatedly,
		spi.FailureExpiredAfterFailedAttempts, spi.FailureRunPanicked, spi.FailureStoppedAfterPartialCommit} {
		names = append(names, string(r))
	}
	for _, name := range names {
		if !strings.Contains(body, "`"+name+"`") {
			t.Errorf("topic does not name `%s`", name)
		}
	}
	for _, code := range []string{"errors.BAD_REQUEST", "errors.UNAUTHORIZED", "errors.SERVER_ERROR", "errors.STORAGE_UNAVAILABLE"} {
		if !strings.Contains(body, code) {
			t.Errorf("topic ERRORS does not name %s", code)
		}
	}
	if !strings.Contains(body, "GET  /api/scheduled-tasks") {
		t.Errorf("topic SYNOPSIS does not show the endpoint")
	}
}

var openapiPathCount = regexp.MustCompile(`The spec declares (\d+) paths`)

// TestOpenAPITopic_PathCountMatchesSpec keeps the openapi topic's path count
// equal to the embedded spec's.
func TestOpenAPITopic_PathCountMatchesSpec(t *testing.T) {
	topic := DefaultTree.Find([]string{"openapi"})
	if topic == nil {
		t.Fatal("help topic openapi is missing")
	}
	m := openapiPathCount.FindStringSubmatch(string(topic.Body))
	if m == nil {
		t.Fatal(`openapi topic has no "The spec declares N paths" sentence`)
	}
	doc, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	if got, _ := strconv.Atoi(m[1]); got != doc.Paths.Len() {
		t.Errorf("openapi topic says %d paths; the spec declares %d", got, doc.Paths.Len())
	}
}

// TestSchedulerConfigTopic_SeesScheduledTasks: config.scheduler points the
// reader at the list of the tasks the scheduler runs.
func TestSchedulerConfigTopic_SeesScheduledTasks(t *testing.T) {
	topic := DefaultTree.Find([]string{"config", "scheduler"})
	if topic == nil {
		t.Fatal("help topic config.scheduler is missing")
	}
	found := false
	for _, s := range topic.SeeAlso {
		if s == "scheduled-tasks" {
			found = true
		}
	}
	if !found {
		t.Errorf("config.scheduler see_also = %v, want it to list scheduled-tasks", topic.SeeAlso)
	}
	if !strings.Contains(string(topic.Body), "- scheduled-tasks") {
		t.Error("config.scheduler SEE ALSO does not list scheduled-tasks")
	}
}
```

- [ ] **Step 2: Run to verify RED**

Run: `go test ./cmd/cyoda/help/ -run 'TestAllTopLevelTopicsPresent|TestScheduledTasksTopic_CoversTheContract|TestOpenAPITopic_PathCountMatchesSpec|TestSchedulerConfigTopic_SeesScheduledTasks'`
Expected: FAIL — `top-level topic "scheduled-tasks" missing from embedded content`;
`help topic scheduled-tasks is missing`; `openapi topic says 83 paths; the spec declares 72`;
`config.scheduler see_also = [config config.cluster config.grpc run], want it to list scheduled-tasks`.

- [ ] **Step 3: Implement**

`cmd/cyoda/help/content/scheduled-tasks.md`:

~~~markdown
---
topic: scheduled-tasks
title: "scheduled-tasks — the tenant's scheduled-transition tasks"
stability: stable
version_added: 0.9.0
see_also:
  - workflows
  - audit
  - openapi
  - errors.BAD_REQUEST
  - errors.UNAUTHORIZED
  - errors.SERVER_ERROR
  - errors.STORAGE_UNAVAILABLE
---

# scheduled-tasks

## NAME

scheduled-tasks — list the scheduled-transition tasks of the caller's tenant: waiting, running and failed.

## SYNOPSIS

```
GET  /api/scheduled-tasks
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`). Requires `Authorization: Bearer <token>` except when `CYODA_IAM_MODE=mock`. Any authenticated user of the tenant may call it; no role is required. The tenant is the token's. HTTP only: there is no gRPC equivalent.

## DESCRIPTION

A transition with a `schedule` arms a task when the entity enters the transition's source state. The task says "fire this transition of this entity at this time". When it is due, one node claims it and runs it.

The list shows the tasks that still exist. Status is an open set; accept values not listed here:

- `WAITING` — due at `nextAttemptTime`. A new task is `WAITING`. A failed attempt that is safe to retry puts it back to `WAITING` with a later `nextAttemptTime`.
- `RUNNING` — a node has claimed it and is running it.
- `FAILED` — it will not run again. `failureReason` says why. A FAILED task never moves the entity.

A task that fired, was declined by its criterion, expired or was cancelled is removed, and is not listed. Its outcome is in the entity's audit trail (`cyoda help audit`).

A FAILED task ends in one of these ways:

- the entity is written in the task's source state — the write arms the task again, and `attempts`, `lostOwners` and the errors start again from zero;
- the entity leaves the source state, or is deleted — the task is removed;
- the transition stops being scheduled — the task is removed at the next write of the entity or the next workflow import of its model.

Failure reasons are an open set; accept values not listed here:

- `UNSAFE_WORK_NOT_COMPLETED` — a processor not declared `idempotent` was handed to a compute node and the run did not commit. cyoda does not repeat it, because the processor may already have acted.
- `OWNER_LOST_REPEATEDLY` — the node running the task was lost too many times.
- `EXPIRED_AFTER_FAILED_ATTEMPTS` — the schedule's `timeoutMs` passed after a failed attempt or a lost node.
- `RUN_PANICKED` — the run failed with an internal error. The server log carries a ticket.
- `STOPPED_AFTER_PARTIAL_COMMIT` — the run committed the entity into another state and then stopped.

## PARAMETERS

All query parameters are optional. Filters combine with AND.

- `status`: repeatable — `WAITING`, `RUNNING`, `FAILED`. Returns tasks in any of the given statuses. Any other value answers `400`.
- `modelName`: 1 to 256 characters of valid UTF-8, without NUL.
- `modelVersion`: integer, at least 1. Only together with `modelName`.
- `entityId`: UUID.
- `cursor`: opaque, at most 256 characters. Pass `nextCursor` from the previous response; omit it for the first page. A cursor that cannot be read answers `400`, and its value is not echoed.
- `limit`: integer from 1 to 1000, default 20. A value outside the range answers `400`; it is not clamped.

An unknown model or entity, or one of another tenant, returns an empty list, not `404`.

## ORDER AND PAGING

Tasks are sorted by `scheduledTime` ascending, then `taskId` ascending. The cursor is a position in that order, not an offset. Each page is read when it is requested, not from a snapshot of the whole walk: a task armed, armed again or removed between two pages can be missed, or be seen at both its old and its new position.

## RESPONSE

`200 OK`, `application/json` — `ScheduledTaskPageDto`:

```json
{
  "items": [
    {
      "taskId": "3c9d0f6e2b1a4d5c8e7f6a5b4c3d2e1f",
      "entityId": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
      "modelName": "orders",
      "modelVersion": 1,
      "sourceState": "AWAITING_PAYMENT",
      "transition": "CancelUnpaid",
      "status": "FAILED",
      "scheduledTime": "2026-09-24T10:00:00Z",
      "armedTime": "2026-09-23T10:00:00Z",
      "expiresTime": "2026-09-24T11:00:00Z",
      "attempts": 1,
      "lostOwners": 0,
      "lastAttemptTime": "2026-09-24T10:00:01.250Z",
      "lastError": "PROCESSOR_ERROR: payment gateway refused the refund",
      "failureReason": "UNSAFE_WORK_NOT_COMPLETED",
      "failedTime": "2026-09-24T10:00:01.250Z",
      "armedBy": {"id": "alice", "kind": "user"}
    }
  ],
  "pagination": {"hasNext": false}
}
```

Fields of each item:

- `taskId`: opaque, stable id. The same entity, source state and transition keep the same id when the task is armed again.
- `entityId`, `modelName`, `modelVersion`: the entity and its model.
- `sourceState`: the state the entity must be in for the transition to fire.
- `transition`: the transition the task fires.
- `status`: see DESCRIPTION.
- `scheduledTime`: when the transition is due.
- `armedTime`: when the task was last armed.
- `expiresTime`: present when the schedule sets `timeoutMs` — `scheduledTime` plus `timeoutMs`.
- `attempts`: failed attempts since the task was last armed.
- `lostOwners`: times the node running the task was lost since the task was last armed.
- `nextAttemptTime`: present when `status` is `WAITING` — the earliest time the next attempt may start.
- `lastAttemptTime`, `lastError`: present after a failed attempt. `lastError` is client-safe text: a `CODE: detail` message, a compute node's own message, or `internal error [ticket: <uuid>]`.
- `failureReason`, `failedTime`: present when `status` is `FAILED`.
- `armedBy`: `{id, kind}` of the principal whose write armed the task, when known.

Claim tokens, arm tokens and node identities are never returned.

`pagination.hasNext` is true when another page exists; `pagination.nextCursor` is then present.

## ERRORS

- `errors.BAD_REQUEST` — `400` — unknown `status`; `modelVersion` without `modelName`; `modelVersion` not an integer of at least 1; `entityId` not a UUID; `modelName` empty, too long, not valid UTF-8 or containing NUL; `limit` not an integer or outside 1 to 1000; a `cursor` that cannot be read
- `errors.UNAUTHORIZED` — `401` — no token, or an invalid one
- `errors.SERVER_ERROR` — `500` — internal failure; a generic message with a ticket
- `errors.STORAGE_UNAVAILABLE` — `503` — storage unavailable; retryable

## EXAMPLES

**List the failed tasks:**

```
curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?status=FAILED"
```

**List one entity's tasks:**

```
curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?entityId=$ENTITY_ID"
```

**Walk every waiting task of a model, 100 at a time:**

```
NEXT=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?status=WAITING&modelName=orders&limit=100" \
  | jq -r '.pagination.nextCursor // empty')

curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?status=WAITING&modelName=orders&limit=100&cursor=$NEXT"
```

## SEE ALSO

- workflows
- audit
- openapi
- errors.BAD_REQUEST
- errors.UNAUTHORIZED
- errors.SERVER_ERROR
- errors.STORAGE_UNAVAILABLE
~~~

`cmd/cyoda/help/content/openapi.md` — change `The spec declares 83 paths` to
the value `grep -c '^  /' api/openapi.yaml` prints (72 on this branch after
Q-1), and add after the `**Entity, Audit**` bullet (`:94`):

```
- **Scheduled Tasks** — the tenant's scheduled-transition tasks under `/scheduled-tasks`
```

`cmd/cyoda/help/content/config/scheduler.md` — add `scheduled-tasks` as the
last entry of the front matter `see_also` (after `  - run`, `:9`) and of SEE
ALSO (after `- run`, `:33`):

```
  - scheduled-tasks
```

```
- scheduled-tasks
```

- [ ] **Step 4: Run to verify GREEN**

Run: `go test ./cmd/cyoda/help/...`
Expected: PASS — including `TestAllTopLevelTopicsPresent`,
`TestSeeAlsoResolution`, `TestContentMarkdownSubsetLinter`,
`TestHelpContent_NoIssueIDs`, `TestHelpContent_CrossReferencesUseAWorkingInvocation`,
`TestRunHelp_NoDuplicateSeeAlso` and the three new tests.

- [ ] **Step 5: Commit**

```
git add cmd/cyoda/help/content/scheduled-tasks.md cmd/cyoda/help/content/openapi.md cmd/cyoda/help/content/config/scheduler.md \
  cmd/cyoda/help/help_test.go cmd/cyoda/help/scheduled_tasks_help_test.go
git commit -m "docs(help): scheduled-tasks topic; openapi topic states the real path count (#598)

config.scheduler lists scheduled-tasks in its SEE ALSO.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Coverage carried forward (spec §13, `GET /scheduled-tasks`)

| Row | U | S | E | P |
|---|---|---|---|---|
| 200, no filter, several pages | Q-3 `TestList_Defaults`, `TestList_NextCursorLeadsToTheNextPage`; Q-2 `TestCursor_*` | stream S | Q-4 `TestScheduledTasks_Paging_200` | Q-5 `ScheduledTasksQueryPaging` |
| 200, each filter (status one and several, model name, name and version, entity) | Q-3 `TestList_PassesFiltersToTheStore`, `TestList_BoundaryValuesAccepted`; Q-6 `TestListScheduledTasks_RoutedAndBound` | stream S | Q-4 `TestScheduledTasks_Filters_200` | Q-5 `ScheduledTasksQueryFilters` |
| 200 empty: unknown model, unknown entity, another tenant's entity | — | — | Q-4 `TestScheduledTasks_EmptyList_200` | — |
| 200, a FAILED item with reason, error, times, attempts | Q-3 `TestList_ItemFields` | — | Q-4 `TestScheduledTasks_Filters_200/FAILED_item` | Open point 2 |
| 400 unknown `status` | Q-3 `TestList_InvalidParameters_400` | — | Q-4 `TestScheduledTasks_InvalidParameters_400` | — |
| 400 `modelVersion` without `modelName` | Q-3 (same) | — | Q-4 (same) | — |
| 400 `modelVersion` not an integer ≥ 1 | Q-3 (zero, negative); Q-6 (not an integer) | — | Q-4 (zero, negative, `abc`, `1.5`) | — |
| 400 `entityId` not a UUID | Q-6 | — | Q-4 | — |
| 400 `modelName` empty or too long | Q-3 (also invalid UTF-8, NUL) | — | Q-4 (same four) | — |
| 400 `limit` 0, 1001, or not an integer | Q-3 (0, 1001, −1); Q-6 (`abc`, empty, `2.5`, repeated) | — | Q-4 (0, 1001, `abc`, empty) | — |
| 400 invalid cursor, value not echoed | Q-2 `TestCursor_Rejects`; Q-3 | — | Q-4 (not base64url, wrong version, over 256) | — |
| 401 no token; 401 invalid token | — | — | Q-4 `TestScheduledTasks_Unauthorized_401` | — |
| 500 `SERVER_ERROR` with a ticket | Q-3 `TestList_Failure_500WithTicket` (store double) | — | Q-4 `TestScheduledTasks_StorageFailure_500` (terminated session) | — |
| 503 `STORAGE_UNAVAILABLE` | Q-3 `TestList_StorageUnavailable_503` (store double) | — | Q-4 `TestScheduledTasks_TornConnection_503` (torn socket) | — |
| another tenant's tasks never returned, under any filter | — | stream S | Q-4 `TestScheduledTasks_TenantIsolation` (also a foreign cursor) | Q-5 `ScheduledTasksQueryTenantIsolation` |

**gRPC:** waived (spec §8, §13). Recorded in Q-1.

Existing tests this stream extends: `TestAllTopLevelTopicsPresent` (list +1),
`TestZZZErrorCodeMatrix` (row +1), `TestParityScenarioCount` (+3),
`TestOpenAPIConformanceReport` (one more operation, unmarked from Q-6).

## Stream interface summary

**Q produces:**

```go
// package api (generated, Q-1)
type ListScheduledTasksParams struct { Status *[]ListScheduledTasksParamsStatus; ModelName *string; ModelVersion *int32; EntityId *openapi_types.UUID; Cursor *string; Limit *int32 }
type ScheduledTaskPageDto, ScheduledTaskDto, ScheduledTaskArmedByDto // fields in Q-1
// ServerInterface: ListScheduledTasks(w, r, params ListScheduledTasksParams)

// package scheduledtask (Q-3)
func NewHandler(factory spi.StoreFactory) *Handler
func (h *Handler) ListScheduledTasks(w http.ResponseWriter, r *http.Request, params genapi.ListScheduledTasksParams)

// package internal/api (Q-6)
Server.ScheduledTasks *scheduledtask.Handler

// package e2e/parity/client (Q-5)
func (c *Client) ListScheduledTasks(t *testing.T, query url.Values) (ScheduledTaskPage, error)
type ScheduledTask, ScheduledTaskArmedBy, ScheduledTaskPage
```

- OpenAPI: `operationId: listScheduledTasks`, path `GET /scheduled-tasks`, tag
  `Scheduled Tasks`; cursor `base64url({"v":1,"t":<ms>,"i":"<task id>"})`.
- OpenAPI: `SCHEDULED_TRANSITION_FAIL` in the `StateMachineAuditEventDto.eventType`
  enum (Q-1, wave 1), so every later e2e test can read a FAIL event through
  the validated audit API.
- Help topic `scheduled-tasks` (Q-7).

**Q consumes:**
- **S:** `spi.ScheduledTaskStatus` and its three constants, the five
  `ScheduledTaskFailureReason` constants, `ScheduledTask` (new fields),
  `TaskClaim`, `ScheduledTaskQuery`, `ScheduledTaskPage`,
  `ScheduledTaskCursor`, `ScheduledTaskStore.Query`. The `spitest` cases for
  `Query` (order, paging, each filter, tenant scoping, `Next` nil on the last
  page — including when the last page is exactly full) are S's; Q-4 and Q-5
  assume exact page sizes (`[2 2 1]`, `[1 1]`).
- **BM, BQ, BP:** `Query` on every backend. BP additionally: the column names
  of spec §10.2 (Q-4's `markFailed` writes them), and `Query` on the main pool
  through the classifying querier, so a torn socket is marked and a 57P01 is
  not.
- **D:** CHANGELOG `### Breaking` "the query" line, `docs/cloud-parity/
  scheduled-transitions.md` (Gate 7), and a link to `scheduled-tasks` from
  `help/workflows.md` SCHEDULED TRANSITIONS. Q changes neither file.
- **T:** see Open point 2.

**Shared files (merge by hand, no semantic overlap):** `api/openapi.yaml` and
`api/generated.go` (W adds 409 cells — regenerate after merging), `internal/e2e/zzz_errorcode_matrix_test.go`, `e2e/parity/registry.go`
and `registry_count_test.go` (each stream adds its own delta),
`cmd/cyoda/help/help_test.go`, `app/app.go` (R rewires the scheduler around
`:586-657`; Q adds one line at `:678`).

## Open points

1. **`modelName` also rejects invalid UTF-8 and NUL.** Spec §8 says "1–256"
   and the error table says "empty or too long". Without the extra rule,
   PostgreSQL rejects such text inside `Query` and the request answers 500 for
   a client mistake. Same status and code (400 `BAD_REQUEST`). Suggest the
   spec's error cell read "empty, too long, not valid UTF-8, or containing NUL".
2. **The P cell "200, a FAILED item".** A FAILED task on a parity backend
   needs a real run (streams E and R). Proposal: stream T's parity scenario
   for "unsafe processor fails → FAILED `UNSAFE_WORK_NOT_COMPLETED`" (the
   compute-test-client's `inject-error` processor, not declared `idempotent`,
   on a short `delayMs`) reads the task with `client.ListScheduledTasks`
   and asserts `status`, `failureReason`, `lastError` containing
   `deliberate failure`, `failedTime` present and `nextAttemptTime` absent.
   Q's E cell seeds the row by SQL instead and needs no run. If the lead
   prefers Q to own it, it becomes Q-8, after R merges.
3. **Seeding by SQL couples Q-4 to BP's column names.** They are the names
   spec §10.2 fixes; if BP chooses different ones, `markFailed` changes with
   them.
4. **`X-Tx-Token` is not declared** on this operation, following the
   `TxToken` parameter's own rule (`api/openapi.yaml:12248-12251`): it is not
   a callback target. The join middleware still runs on every route
   (`internal/httpmw/txjoin_mw.go:40-47`), so a request that does send a token
   can be refused with a status this operation does not declare — the same as
   for the model and workflow operations today.
5. **New tag `Scheduled Tasks`** rather than reusing `Entity, Audit`. It
   slugifies to `scheduled-tasks`, unique among tag slugs
   (`TestOpenAPISpecSlugsAreUnique`). Say if the lead prefers the audit tag.
6. **The cursor's task id is checked only as non-empty.** The SPI calls the
   id opaque; the engine's format (32 hex characters, `arm.go:26-29`) is not
   enforced in the cursor, so a store with another id format still pages. A
   forged id only positions the walk inside the caller's own tenant.
7. **Audit's `limit` clamps, this one rejects.** Both follow their own
   contract (`audit.md` documents the clamp). Not changed here.
8. **Error-matrix row lists only 400.** 401 and 500 are exempt as
   cross-cutting (`zzz_errorcode_matrix_test.go:185-189`); the 503 is
   produced on a harness outside the validator, so listing it would make the
   matrix demand a triple the shared stack never records.
9. **Commit subjects carry `(#598)`**, as the precedent plan's did; no issue
   number appears in code, comments, help or OpenAPI.
