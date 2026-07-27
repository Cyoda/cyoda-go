package observability

import "go.opentelemetry.io/otel/attribute"

// Attribute keys used across observability instrumentation.
// Using attribute.Key constants avoids typos and enables IDE navigation.
const (
	AttrEntityID    = attribute.Key("entity.id")
	AttrEntityModel = attribute.Key("entity.model")
	AttrEntityState = attribute.Key("entity.state")

	AttrTxID = attribute.Key("tx.id")
	AttrTxOp = attribute.Key("op")

	AttrWorkflowName   = attribute.Key("workflow.name")
	AttrTransitionName = attribute.Key("transition.name")
	AttrStateFrom      = attribute.Key("state.from")
	AttrStateTo        = attribute.Key("state.to")
	AttrCascadeDepth   = attribute.Key("cascade.depth")

	AttrProcessorName = attribute.Key("processor.name")
	AttrProcessorMode = attribute.Key("processor.execution_mode")
	AttrProcessorTags = attribute.Key("processor.tags")

	AttrCriterionTarget = attribute.Key("criterion.target")
	AttrCriteriaMatches = attribute.Key("criteria.matches")

	AttrFunctionName = attribute.Key("function.name")
	AttrFunctionTags = attribute.Key("function.tags")

	AttrDispatchType = attribute.Key("type")

	AttrEntityCount = attribute.Key("entity.count")
	AttrCQLName     = attribute.Key("cql.name")
	AttrCQLOp       = attribute.Key("cql.op")
	AttrBatchSize   = attribute.Key("batch.size")
	AttrBatchType   = attribute.Key("batch.type")

	AttrVersionCheckReason = attribute.Key("version_check.reason")
)
