package common

const (
	ErrCodeModelNotFound        = "MODEL_NOT_FOUND"
	ErrCodeModelNotLocked       = "MODEL_NOT_LOCKED"
	ErrCodeModelAlreadyLocked   = "MODEL_ALREADY_LOCKED"
	ErrCodeModelAlreadyUnlocked = "MODEL_ALREADY_UNLOCKED"
	ErrCodeModelHasEntities     = "MODEL_HAS_ENTITIES"
	ErrCodeEntityModified       = "ENTITY_MODIFIED"
	ErrCodeEntityNotFound       = "ENTITY_NOT_FOUND"
	ErrCodeValidationFailed     = "VALIDATION_FAILED"
	ErrCodeTransitionNotFound   = "TRANSITION_NOT_FOUND"
	ErrCodeWorkflowNotFound     = "WORKFLOW_NOT_FOUND"
	ErrCodeWorkflowFailed       = "WORKFLOW_FAILED"
	ErrCodeConflict             = "CONFLICT"
	ErrCodeEpochMismatch        = "EPOCH_MISMATCH"
	ErrCodeBadRequest           = "BAD_REQUEST"
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
	// ErrCodeConditionTypeMismatch is returned when a simple condition's value
	// type does not match the field's locked DataType (e.g. "abc" against a
	// DOUBLE field). Equivalent to Cloud's InvalidTypesInClientConditionException.
	ErrCodeConditionTypeMismatch = "CONDITION_TYPE_MISMATCH"
)

// Help subsystem
const (
	ErrCodeHelpTopicNotFound = "HELP_TOPIC_NOT_FOUND"
)

// OIDC provider management
const (
	ErrCodeOIDCProviderDuplicate = "OIDC_PROVIDER_DUPLICATE"
	ErrCodeOIDCProviderNotFound  = "OIDC_PROVIDER_NOT_FOUND"
	ErrCodeOIDCProviderInactive  = "OIDC_PROVIDER_INACTIVE"
	ErrCodeOIDCSSRFBlocked       = "OIDC_SSRF_BLOCKED"
)
