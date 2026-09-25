# Binding interfaces

Every section uses these names, types and signatures exactly. A section that needs
something not listed here adds it to its own `## Stream interface summary` and
names it in `## Open points` for the lead to fold in here.

## SPI (`github.com/cyoda-platform/cyoda-go-spi`, stream S)

```go
// types.go
type ScheduledTaskStatus string

const (
	ScheduledTaskWaiting ScheduledTaskStatus = "WAITING"
	ScheduledTaskRunning ScheduledTaskStatus = "RUNNING"
	ScheduledTaskFailed  ScheduledTaskStatus = "FAILED"
)

type ScheduledTaskFailureReason string

const (
	FailureUnsafeWorkNotCompleted     ScheduledTaskFailureReason = "UNSAFE_WORK_NOT_COMPLETED"
	FailureOwnerLostRepeatedly        ScheduledTaskFailureReason = "OWNER_LOST_REPEATEDLY"
	FailureExpiredAfterFailedAttempts ScheduledTaskFailureReason = "EXPIRED_AFTER_FAILED_ATTEMPTS"
	FailureRunPanicked                ScheduledTaskFailureReason = "RUN_PANICKED"
	FailureStoppedAfterPartialCommit  ScheduledTaskFailureReason = "STOPPED_AFTER_PARTIAL_COMMIT"
)

type TaskClaim struct {
	Token uuid.UUID `json:"token"`
	Owner uuid.UUID `json:"owner"`
}

type ScheduledTask struct {
	ID            string            `json:"id"`
	TenantID      TenantID          `json:"tenantId"`
	Type          ScheduledTaskType `json:"type"`
	ScheduledTime int64             `json:"scheduledTime"`
	TimeoutMs     *int64            `json:"timeoutMs,omitempty"`
	EntityID      string            `json:"entityId,omitempty"`
	ModelName     string            `json:"modelName,omitempty"`
	ModelVersion  int               `json:"modelVersion,omitempty"`
	Transition    string            `json:"transition,omitempty"`
	SourceState   string            `json:"sourceState,omitempty"`
	ArmedAt       int64             `json:"armedAt,omitempty"`
	ArmedBy       Principal         `json:"armedBy,omitempty"`

	Status          ScheduledTaskStatus        `json:"status"`
	ArmToken        uuid.UUID                  `json:"armToken"`        // drawn by the store on every arm
	NextAttemptTime int64                      `json:"nextAttemptTime"` // unix ms; = ScheduledTime on arm
	Attempts        int                        `json:"attempts"`
	LostOwners      int                        `json:"lostOwners"`
	LastAttemptTime *int64                     `json:"lastAttemptTime,omitempty"`
	LastError       string                     `json:"lastError,omitempty"`
	FailureReason   ScheduledTaskFailureReason `json:"failureReason,omitempty"`
	FailedTime      *int64                     `json:"failedTime,omitempty"`
	PartialCommit   bool                       `json:"partialCommit"`
	Claim           *TaskClaim                 `json:"claim,omitempty"` // RUNNING only
	UnsafeMarked    bool                       `json:"unsafeMarked"`    // read-only: a mark exists for this life
	ClaimedFromLostOwner bool                  `json:"-"`               // read-only; ClaimDue results only (README C-S1)
}
// Removed: RedispatchAfter, AttemptCount.

// TaskRef names one claim of one life. Every fenced method takes it.
type TaskRef struct {
	TenantID   TenantID
	ID         string
	ArmToken   uuid.UUID
	ClaimToken uuid.UUID
}

type ClaimRequest struct {
	Owner            uuid.UUID
	NowMs            int64 // pnode clock; compared with NextAttemptTime
	StaleAfter       time.Duration
	Limit            int
	PerTenantLimit   int
	TenantInProgress map[TenantID]int
	AllowLostOwner   bool
}

type Attempt struct {
	Error           string // already sanitised (≤1024 bytes, valid UTF-8, no NUL)
	AtMs            int64
	NextAttemptTime int64
	NotCounted      bool
	ClearOwnMark    bool
}

type Failure struct {
	Reason ScheduledTaskFailureReason
	Error  string
	AtMs   int64
}

type ScheduledTaskCursor struct {
	ScheduledTime int64
	ID            string
}

type ScheduledTaskQuery struct {
	Statuses     []ScheduledTaskStatus // empty = all
	ModelName    string                // "" = any
	ModelVersion int                   // 0 = any; only with ModelName
	EntityID     string                // "" = any
	After        *ScheduledTaskCursor  // exclusive
	Limit        int                   // 1..1000, validated by the caller
}

type ScheduledTaskPage struct {
	Items []ScheduledTask
	Next  *ScheduledTaskCursor // nil when there is no further page
}

// persistence.go — ReconcileRequest keeps its fields; its semantics change:
// arm req.Arm (each a new life), remove every OTHER task of the entity, and
// remove req.Cancel. Returns the removed tasks (Cancel-driven ones reported
// distinctly, as today).
type ScheduledTaskStore interface {
	ReconcileForEntity(ctx context.Context, req ReconcileRequest) (removed []ScheduledTask, err error)
	RemoveLife(ctx context.Context, tenant TenantID, id string, armToken uuid.UUID) error
	StampSegment(ctx context.Context, ref TaskRef, partial bool) error
	DeleteForEntities(ctx context.Context, tenant TenantID, entityIDs []string) error
	DeleteForModel(ctx context.Context, tenant TenantID, modelName string, modelVersion int,
		keep func(sourceState, transition string) bool) error
	Get(ctx context.Context, tenant TenantID, id string) (*ScheduledTask, bool, error)
	Query(ctx context.Context, tenant TenantID, q ScheduledTaskQuery) (ScheduledTaskPage, error)
	ClaimDue(ctx context.Context, req ClaimRequest) ([]ScheduledTask, error)
	Heartbeat(ctx context.Context, owner uuid.UUID) error
	RetireOwner(ctx context.Context, owner uuid.UUID) error
	SweepOwners(ctx context.Context, deadFor time.Duration) error
	GiveBackIdle(ctx context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error)
	MarkUnsafe(ctx context.Context, ref TaskRef) error
	RecordAttempt(ctx context.Context, ref TaskRef, a Attempt) error
	Fail(ctx context.Context, ref TaskRef, f Failure) error
	SweepMarks(ctx context.Context) error
}
// Joining methods: ReconcileForEntity, RemoveLife, StampSegment, DeleteForEntities,
// DeleteForModel, Fail (Get may join). All others never join a transaction on ctx.
// A joining method whose tenant is not the tenant of the transaction on ctx
// returns ErrTxTenantMismatch (README C-S5), on every backend.

// errors.go
var ErrMarkedByAnotherClaim = errors.New("scheduled task: marked by another claim of this life")
var ErrTaskBusy             = errors.New("row is being written by an open transaction") // also AsyncSearchStore.Heartbeat, ClaimDue
var ErrStoreRejected        = errors.New("store rejected the write deterministically")
// ErrStaleClaim (exists) — doc comment widened to both stores.
// A store wraps a deterministic rejection so that errors.Is(err, ErrStoreRejected) holds.

// types.go — audit event
const SMEventScheduledTransitionFailed StateMachineEventType = "SCHEDULED_TRANSITION_FAIL"

// spitest — new suite entry point, registered in spitest.Run like AsyncSearch:
func runScheduledTasksSuite(t *testing.T, h Harness, tracker *skipTracker) // README C-S2
// Removed: RunScheduledTaskStoreConformance (root package).
// spitest Audit group gains RolledBackEventNotKept (README C-S4); the
// ScheduledTasks group has Claim/LostOwnerFlagged (C-S1) and
// Tenant/JoiningWriteOtherTenantRefused (C-S5).

// scheduled_task_helpers.go (S-3a, README C-P2) — shared backend helpers;
// memory (BM-2, BM-5) and SQLite (BQ-2, BQ-6) call them; PostgreSQL meets the
// same rules in SQL.
const MaxTaskErrorBytes = 1024
func SelectClaims(cands []ScheduledTask, req ClaimRequest) []ScheduledTask // one per entity; per-tenant limits; tenants take turns
func ValidateTaskErrorText(s string) error                                 // > MaxTaskErrorBytes, invalid UTF-8, NUL → ErrStoreRejected
func ValidateFailureReason(r ScheduledTaskFailureReason) error             // not one of the five → ErrStoreRejected
func ValidateArm(req ReconcileRequest) error                               // an Arm task without an id → ErrStoreRejected
```

## Callout proof (`internal/contract`, `internal/grpc`, `internal/callout`, stream K)

```go
// internal/contract
// NoHandOffProof is attached only by the callout coordinator, only when no try
// had member.Send return nil and no hand-over got past StageNotConnected without
// a no_handoff answer. A peer's no_handoff answer counts as proof only for a
// callout that is not repeat-safe (README C-K1). The coordinator also attaches
// it on the criterion parse failure. Absence means "may have been handed off".
type NoHandOffProof struct{ Err error }
func (p *NoHandOffProof) Error() string
func (p *NoHandOffProof) Unwrap() error
func ProvesNoHandOff(err error) bool // errors.As(err, **NoHandOffProof)

// internal/grpc
type LocalResult struct { /* existing fields */ HandedOff bool }
```

## Engine (`internal/domain/workflow`, stream E)

```go
type RunGuard struct {
	Ref         spi.TaskRef
	Store       spi.ScheduledTaskStore
	Done        <-chan struct{} // the run's cancellation; closed on self-cancel, panic latch, shutdown step 3
	NoNewUnsafe <-chan struct{} // closed at shutdown step 1; nil = never
	Unsafe      *UnsafeFlight   // brackets each unsafe dispatch (README C-G1)
}
type UnsafeFlight struct{ /* mutex, count, oldest start */ }
func (f *UnsafeFlight) Begin()
func (f *UnsafeFlight) End()
func (f *UnsafeFlight) Since() (time.Time, bool)
func WithRunGuard(ctx context.Context, g *RunGuard) context.Context
func RunGuardFrom(ctx context.Context) *RunGuard // exported (README C-G1)
func (g *RunGuard) UnsafeInFlight() bool        // _, ok := g.Unsafe.Since(); ok

type RunReport struct {
	Outcome        ScheduledOutcome // fired | declined | expired | cancelled | superseded | failed
	Err            error            // the run's failure; nil when it committed or was superseded
	MarkHeld       bool             // a MarkUnsafe was accepted in this run
	UnsafeReached  bool             // the in-memory fact of spec §5.5
	MarkErrored    bool             // the last MarkUnsafe failed with a non-refusal error
	PartialCommit  bool             // a stamp with partial=true committed in this run
	FailReason     spi.ScheduledTaskFailureReason // set when the engine itself decided FAILED before running (§5.1), or when MarkUnsafe answers ErrMarkedByAnotherClaim
}

const (
	OutcomeFired      ScheduledOutcome = "fired"
	OutcomeDeclined   ScheduledOutcome = "declined"
	OutcomeExpired    ScheduledOutcome = "expired"
	OutcomeCancelled  ScheduledOutcome = "cancelled"
	OutcomeSuperseded ScheduledOutcome = "superseded"
	OutcomeFailed     ScheduledOutcome = "failed"      // the run did not commit; bookkeeping decides (§5.6)
)
// OutcomeDropped is removed.

// The scheduler's only door. task carries Claim and ArmToken from ClaimDue.
// The engine performs §5.1 steps 1-4, and the removals and audits of the
// endings that commit (fired, declined, expired, cancelled). It never writes
// RecordAttempt / Fail — those are the scheduler's (§5.6).
func (e *Engine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) RunReport

// The one arm rule (README C-P7), arm.go, added by E-2. Arm, fire, the
// model-level flag and the workflow import's keep all use it.
func armsOnSchedule(tr *spi.TransitionDefinition) bool // Schedule != nil && !Manual && !Disabled

// Model-level flag (spec §7): any workflow of the model, active or not, has a
// transition armsOnSchedule arms.
func modelHasSchedule(wfs []spi.WorkflowDefinition) bool
```

## Scheduler (`internal/scheduler`, stream R)

```go
type Config struct {
	Enabled           bool
	ScanInterval      time.Duration
	MaxRuns           int
	MaxRunsPerTenant  int
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
	MaxLostOwners     int
	RetryDelay        time.Duration
	RetryDelayMax     time.Duration
	ShutdownDrain     time.Duration
}

type Firer interface {
	FireScheduledTransition(ctx context.Context, task spi.ScheduledTask, maxLostOwners int, retryDelay time.Duration) workflow.RunReport
}

func New(cfg Config, deps Deps) *Service
func (s *Service) Start(ctx context.Context) error   // returns after the first heartbeat attempt is scheduled
func (s *Service) Drain(ctx context.Context)         // shutdown steps 1-5 (spec §6.4)
func (s *Service) Stop()                              // idempotent; best effort after servers (server-failure path)

// Bookkeeping decision (spec §5.6) — pure function, table-tested.
type Bookkeeping struct {
	Kind    BookkeepingKind // RecordAttemptKind | FailKind | NoneKind
	Attempt spi.Attempt
	Failure spi.Failure
}
func decideBookkeeping(r workflow.RunReport, task spi.ScheduledTask, cutByShutdown, panicked bool,
	nowMs int64, cfg Config, errText string) Bookkeeping

// lastError (spec §5.8) — pure function.
func recordedError(err error) (text string, ticket uuid.UUID, warnOnly bool)
func sanitiseErrorText(s string) string // valid UTF-8, no NUL, ≤1024 bytes at a rune boundary
```

App config: `app.SchedulerConfig` gains the fields of `scheduler.Config` above with
the same names; `Distribution`, `Coordinator`, `BatchSize`, `RedispatchBackoff`,
`ExpiryGrace` removed; `cluster.Config.DispatchForwardTimeout` removed.

Postgres plugin config: `config.SchedulerConns int32` from `CYODA_POSTGRES_SCHEDULER_CONNS` (default 10, ≥ 2).

## Entity writes (`internal/domain/entity`, `internal/domain/workflow`, stream W)

```go
// internal/common (README C-W1)
const TaskConflictRetries = 3
func RetryOnTaskConflict(ctx context.Context, owned bool, op func() error) error // owned=false → op once
```

## Query (`internal/domain/scheduledtask`, stream Q)

```go
package scheduledtask
func NewHandler(factory spi.StoreFactory) *Handler
func (h *Handler) ListScheduledTasks(w http.ResponseWriter, r *http.Request, params genapi.ListScheduledTasksParams)
// OpenAPI operationId: listScheduledTasks; path GET /scheduled-tasks
// cursor: base64url JSON {"v":1,"t":<scheduledTime ms>,"i":"<task id>"}, strict decode
```

## Metrics (`internal/scheduler`, stream R)

`cyoda.scheduler.runs` (counter, attr `outcome`), `cyoda.scheduler.run.duration`
(histogram s, attr `outcome`), `cyoda.scheduler.runs.in_progress` (up-down),
`cyoda.scheduler.claims` (counter, attr `reason` ∈ due|owner_lost),
`cyoda.scheduler.heartbeat.failures`, `cyoda.scheduler.bookkeeping.retries` (counters).
Startup log (README C-R2): INFO `scheduler started` with `incarnation=<uuid>`.
Pool gauge (README C-R6): `cyoda.storage.pool.connections{pool=main|scheduler|heartbeat}`.
Outcome values: fired, declined, expired, cancelled, attempt_failed, failed,
superseded, self_cancelled, shutdown_cancelled, panicked.
