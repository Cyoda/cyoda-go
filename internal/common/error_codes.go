package common

const (
	ErrCodeModelNotFound                    = "MODEL_NOT_FOUND"
	ErrCodeModelNotLocked                   = "MODEL_NOT_LOCKED"
	ErrCodeModelAlreadyLocked               = "MODEL_ALREADY_LOCKED"
	ErrCodeModelAlreadyUnlocked             = "MODEL_ALREADY_UNLOCKED"
	ErrCodeModelHasEntities                 = "MODEL_HAS_ENTITIES"
	ErrCodeEntityModified                   = "ENTITY_MODIFIED"
	ErrCodeEntityNotFound                   = "ENTITY_NOT_FOUND"
	ErrCodeValidationFailed                 = "VALIDATION_FAILED"
	ErrCodeTransitionNotFound               = "TRANSITION_NOT_FOUND"
	ErrCodeWorkflowNotFound                 = "WORKFLOW_NOT_FOUND"
	ErrCodeWorkflowFailed                   = "WORKFLOW_FAILED"
	ErrCodeWorkflowSchemaVersionUnsupported = "WORKFLOW_SCHEMA_VERSION_UNSUPPORTED"
	ErrCodeConflict                         = "CONFLICT"
	ErrCodeEpochMismatch                    = "EPOCH_MISMATCH"
	ErrCodeBadRequest                       = "BAD_REQUEST"
	// ErrCodeIncompatibleType is returned when an entity payload's leaf
	// value type is not assignable to the schema's declared DataType for
	// that field (e.g. submitting "abc" or 13.111 against an INTEGER
	// field). Equivalent to Cloud's
	// FoundIncompatibleTypeWithEntityModelException. Distinct from
	// ErrCodeConditionTypeMismatch which is the search-side equivalent
	// for a condition's literal-vs-field mismatch.
	ErrCodeIncompatibleType          = "INCOMPATIBLE_TYPE"
	ErrCodeInvalidChangeLevel        = "INVALID_CHANGE_LEVEL"
	ErrCodeInvalidFieldPath          = "INVALID_FIELD_PATH"
	ErrCodePolymorphicSlot           = "POLYMORPHIC_SLOT"
	ErrCodeUnauthorized              = "UNAUTHORIZED"
	ErrCodeForbidden                 = "FORBIDDEN"
	ErrCodeFeatureDisabled           = "FEATURE_DISABLED"
	ErrCodeKeyOwnedByDifferentTenant = "KEY_OWNED_BY_DIFFERENT_TENANT"
	ErrCodeKeypairNotFound           = "KEYPAIR_NOT_FOUND"
	ErrCodeTrustedKeyCapReached      = "TRUSTED_KEY_CAP_REACHED"
	ErrCodeTrustedKeyNotFound        = "TRUSTED_KEY_NOT_FOUND"
	ErrCodeM2MClientNotFound         = "M2M_CLIENT_NOT_FOUND"
	ErrCodeUnsupportedAlgorithm      = "UNSUPPORTED_ALGORITHM"
	ErrCodeUnsupportedKeyType        = "UNSUPPORTED_KEY_TYPE"
	ErrCodeServerError               = "SERVER_ERROR"
	ErrCodeNotImplemented            = "NOT_IMPLEMENTED"
	ErrCodeNotFound                  = "NOT_FOUND"
	ErrCodePreconditionRequired      = "PRECONDITION_REQUIRED"
	ErrCodeUnsupportedMediaType      = "UNSUPPORTED_MEDIA_TYPE"
)

const (
	ErrCodeTransactionNodeUnavailable = "TRANSACTION_NODE_UNAVAILABLE"
	ErrCodeTransactionExpired         = "TRANSACTION_EXPIRED"
	ErrCodeIdempotencyConflict        = "IDEMPOTENCY_CONFLICT"
	ErrCodeClusterNodeNotRegistered   = "CLUSTER_NODE_NOT_REGISTERED"
	ErrCodeTransactionNotFound        = "TRANSACTION_NOT_FOUND"
)

const (
	ErrCodeNoComputeMemberForTag     = "NO_COMPUTE_MEMBER_FOR_TAG"
	ErrCodeDispatchForwardFailed     = "DISPATCH_FORWARD_FAILED"
	ErrCodeDispatchTimeout           = "DISPATCH_TIMEOUT"
	ErrCodeComputeMemberDisconnected = "COMPUTE_MEMBER_DISCONNECTED"
)

const (
	ErrCodeTxRequired                 = "TX_REQUIRED"
	ErrCodeTxConflict                 = "TX_CONFLICT"
	ErrCodeTxCoordinatorNotConfigured = "TX_COORDINATOR_NOT_CONFIGURED"
	ErrCodeTxNoState                  = "TX_NO_STATE"
)

const (
	ErrCodeSearchJobNotFound        = "SEARCH_JOB_NOT_FOUND"
	ErrCodeSearchJobAlreadyTerminal = "SEARCH_JOB_ALREADY_TERMINAL"
	ErrCodeSearchShardTimeout       = "SEARCH_SHARD_TIMEOUT"
	ErrCodeSearchResultLimit        = "SEARCH_RESULT_LIMIT"
	// ErrCodeScanBudgetExhausted is returned when a residual (non-pushdown)
	// search or streaming aggregation examined more rows than the backend's
	// configured scan budget before completing. Non-retryable: the client
	// must narrow the query or add an indexable predicate.
	ErrCodeScanBudgetExhausted = "SCAN_BUDGET_EXHAUSTED"
	// ErrCodeConditionTypeMismatch is returned when a simple condition's value
	// type does not match the field's locked DataType (e.g. "abc" against a
	// DOUBLE field). Equivalent to Cloud's InvalidTypesInClientConditionException.
	ErrCodeConditionTypeMismatch = "CONDITION_TYPE_MISMATCH"
	// ErrCodeInvalidCondition is returned when a request body condition
	// (AbstractConditionDto) cannot be parsed. Non-retryable: the client
	// must fix the malformed condition.
	ErrCodeInvalidCondition = "INVALID_CONDITION"
)

// Composite unique-key errors
const (
	// ErrCodeUniqueViolation is returned when a save would duplicate a
	// declared composite unique key. Non-retryable: the client must change
	// the entity's key fields before retrying.
	ErrCodeUniqueViolation = "UNIQUE_VIOLATION"
	// ErrCodeInvalidUniqueKey is returned when the engine cannot compute a
	// complete unique claim because a required key field is null or
	// otherwise unusable. Non-retryable: the client must supply valid
	// values for all declared key fields.
	ErrCodeInvalidUniqueKey = "INVALID_UNIQUE_KEY"
	// ErrCodeCompositeKeyUnsupported is returned when a request references
	// a composite unique-key operation that the active storage backend does
	// not support.
	ErrCodeCompositeKeyUnsupported = "COMPOSITE_KEY_UNSUPPORTED"
	// ErrCodeInvalidUniqueKeyDefinition is returned when a model's
	// UniqueKey definition is structurally invalid (e.g. empty field list,
	// unknown field path, or duplicate key name).
	ErrCodeInvalidUniqueKeyDefinition = "INVALID_UNIQUE_KEY_DEFINITION"
)

// Help subsystem
const (
	ErrCodeHelpTopicNotFound = "HELP_TOPIC_NOT_FOUND"
)

// Scheduled transition Function callouts
const (
	// ErrCodeScheduleFunctionInvalidResult is returned when a scheduled
	// transition's arm-time Function callout completes but its result
	// cannot be interpreted as a Schedule (wrong resultKind, or a
	// malformed/ambiguous fireAt/fireAfterMs/expireAt/expireAfterMs
	// payload). Non-retryable: the compute node's implementation must be
	// fixed. Internal (500 + ticket) because the caller supplied a valid
	// transition; the failure is in the compute node's response.
	ErrCodeScheduleFunctionInvalidResult = "SCHEDULE_FUNCTION_INVALID_RESULT"
)

// OIDC provider management
const (
	// ErrCodeOidcInvalidTenant is returned when an OIDC provider registration
	// is attempted from a tenant context whose ID is not a valid UUID.
	// OIDC provider ownership requires UUID-shaped legal entity identifiers
	// (matching the cyoda data model). Bootstrap deployments using the literal
	// "default-tenant" string must migrate to real tenant UUIDs before
	// registering OIDC providers.
	ErrCodeOidcInvalidTenant     = "OIDC_INVALID_TENANT"
	ErrCodeOIDCProviderDuplicate = "OIDC_PROVIDER_DUPLICATE"
	ErrCodeOIDCProviderNotFound  = "OIDC_PROVIDER_NOT_FOUND"
	ErrCodeOIDCProviderInactive  = "OIDC_PROVIDER_INACTIVE"
	ErrCodeOIDCSSRFBlocked       = "OIDC_SSRF_BLOCKED"
)

// Token-validation failures (audience mismatch, claims invalid, iat
// pre-transition, KID unknown, JWKS unavailable during key resolution) carry
// no precise OIDC_* code. The bearer-auth middleware uniformly returns a
// problem-detail body with code UNAUTHORIZED and no per-cause distinction; a
// precise code would enumerate IdP / audience / kid / claim-shape recognition
// to an unauthenticated caller.
//
// Discovery failures at registration time also carry no precise code.
// Registry warm-up is non-fatal: the provider stays registered, discovery
// errors log internally, and tokens 401 until the IdP becomes reachable.
//
// The per-cause diagnostic path is the server-side log stream — see the
// auth.oidc help topic.
