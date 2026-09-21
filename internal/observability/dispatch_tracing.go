package observability

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// Dispatch kind labels for the "type" metric attribute. Values are preserved exactly
// as before the kind-labeled refactor so existing dashboards/alerts keep working.
const (
	kindProcessor = "processor"
	kindCriteria  = "criteria"
	kindFunction  = "function"
)

// dispatchDurationBuckets reach past the longest a callout may take (tries ×
// answer limit + patience + hand-over allowance: 155 s at the defaults, 275 s at
// the upper bound of the answer limit), so a slow callout lands in a bucket
// rather than in +Inf.
var dispatchDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// TracingExternalProcessingService wraps an ExternalProcessingService with OTel spans and metrics.
type TracingExternalProcessingService struct {
	inner            contract.ExternalProcessingService
	tracer           trace.Tracer
	dispatchDuration metric.Float64Histogram
	dispatchTotal    metric.Int64Counter
	calloutTries     metric.Int64Counter
	calloutWait      metric.Float64Histogram
	typeProcessor    metric.MeasurementOption
	typeCriteria     metric.MeasurementOption
	typeFunction     metric.MeasurementOption
}

// NewTracingExternalProcessingService returns a TracingExternalProcessingService that decorates
// inner with OTel tracing spans and metrics for processor and criteria dispatches.
func NewTracingExternalProcessingService(inner contract.ExternalProcessingService, meter metric.Meter) *TracingExternalProcessingService {
	tracer := Tracer()

	duration, err := meter.Float64Histogram("cyoda.dispatch.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one whole processor/criteria/function callout, all its tries, waits and hand-overs included"),
		metric.WithExplicitBucketBoundaries(dispatchDurationBuckets...))
	instrErr("cyoda.dispatch.duration", err)
	total, err := meter.Int64Counter("cyoda.dispatch.count",
		metric.WithDescription("Processor/criteria/function callouts, however many tries each took"))
	instrErr("cyoda.dispatch.count", err)
	// Hand-overs are counted by the peer router, which sees every one of them,
	// including a peer that was never asked; there is no second counter here.
	tries, err := meter.Int64Counter("cyoda.callout.tries",
		metric.WithDescription("Tries made for processor/criteria/function callouts, by outcome"))
	instrErr("cyoda.callout.tries", err)
	wait, err := meter.Float64Histogram("cyoda.callout.wait.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time a callout waited for a compute member to exist; recorded only for callouts that waited"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60))
	instrErr("cyoda.callout.wait.duration", err)

	return &TracingExternalProcessingService{
		inner:            inner,
		tracer:           tracer,
		dispatchDuration: duration,
		dispatchTotal:    total,
		calloutTries:     tries,
		calloutWait:      wait,
		typeProcessor:    metric.WithAttributes(AttrDispatchType.String(kindProcessor)),
		typeCriteria:     metric.WithAttributes(AttrDispatchType.String(kindCriteria)),
		typeFunction:     metric.WithAttributes(AttrDispatchType.String(kindFunction)),
	}
}

// record opens a span named spanName with spanAttrs, runs fn, and records the elapsed
// duration and count against the metric attribute set for kind. It centralizes the
// span/metric bookkeeping shared by every dispatch kind (processor, criteria, and
// future kinds such as "function") so each DispatchXxx method only supplies what
// differs: the span name/attributes and the inner call itself.
func (t *TracingExternalProcessingService) record(
	ctx context.Context, kind, spanName string, spanAttrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	ctx, span := t.tracer.Start(ctx, spanName, trace.WithAttributes(spanAttrs...))
	defer span.End()

	// Ask the owner's loop for its account of the callout; an inner that is
	// not the owner's loop leaves it empty. The loop fills it on this goroutine
	// before fn returns, so reading it afterwards needs no synchronisation.
	ctx, stats := contract.WithCalloutStats(ctx)

	opt := t.measurementOption(kind)
	start := time.Now()
	err := fn(ctx, span)
	elapsed := time.Since(start).Seconds()

	t.dispatchDuration.Record(ctx, elapsed, opt)
	t.dispatchTotal.Add(ctx, 1, opt)
	t.recordCallout(ctx, span, kind, stats)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// recordCallout puts the owner's account of one callout on its span — how many
// tries, whether it was handed over to another node, how long it waited — and
// counts each try by outcome. Outcomes are a closed set
// (contract.CalloutOutcome* and contract.CalloutFailureKind), so the label
// cardinality is bounded; nothing tenant-specific goes on a metric.
func (t *TracingExternalProcessingService) recordCallout(ctx context.Context, span trace.Span, kind string, stats *contract.CalloutStats) {
	span.SetAttributes(
		AttrCalloutTries.Int(len(stats.Tries)),
		AttrCalloutHandOver.Bool(len(stats.HandOvers) > 0),
		AttrCalloutWaitedMs.Int64(stats.Waited.Milliseconds()),
	)
	for _, outcome := range stats.Tries {
		t.calloutTries.Add(ctx, 1, metric.WithAttributes(AttrDispatchType.String(kind), AttrCalloutOutcome.String(outcome)))
	}
	if stats.Waited > 0 {
		t.calloutWait.Record(ctx, stats.Waited.Seconds(), t.measurementOption(kind))
	}
}

// measurementOption returns the cached MeasurementOption for kind, falling back to a
// freshly-built one for kinds not pre-cached in NewTracingExternalProcessingService.
func (t *TracingExternalProcessingService) measurementOption(kind string) metric.MeasurementOption {
	switch kind {
	case kindProcessor:
		return t.typeProcessor
	case kindCriteria:
		return t.typeCriteria
	case kindFunction:
		return t.typeFunction
	default:
		return metric.WithAttributes(AttrDispatchType.String(kind))
	}
}

func (t *TracingExternalProcessingService) DispatchProcessor(
	ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition,
	workflowName, transitionName, txID string,
) (*spi.Entity, error) {
	var result *spi.Entity
	err := t.record(ctx, kindProcessor, "dispatch.processor", []attribute.KeyValue{
		AttrProcessorName.String(processor.Name),
		AttrProcessorMode.String(processor.ExecutionMode),
		AttrProcessorTags.String(processor.Config.CalculationNodesTags),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	}, func(ctx context.Context, span trace.Span) error {
		var err error
		result, err = t.inner.DispatchProcessor(ctx, entity, processor, workflowName, transitionName, txID)
		return err
	})
	return result, err
}

func (t *TracingExternalProcessingService) DispatchCriteria(
	ctx context.Context, entity *spi.Entity, criterion json.RawMessage,
	target, workflowName, transitionName, processorName, txID string,
) (bool, string, error) {
	var matches bool
	var reason string
	err := t.record(ctx, kindCriteria, "dispatch.criteria", []attribute.KeyValue{
		AttrCriterionTarget.String(target),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	}, func(ctx context.Context, span trace.Span) error {
		var err error
		matches, reason, err = t.inner.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
		span.SetAttributes(AttrCriteriaMatches.Bool(matches))
		return err
	})
	return matches, reason, err
}

func (t *TracingExternalProcessingService) DispatchFunction(
	ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction,
	workflowName, transitionName, txID string,
) (contract.FunctionResult, error) {
	var result contract.FunctionResult
	err := t.record(ctx, kindFunction, "dispatch.function", []attribute.KeyValue{
		AttrFunctionName.String(fn.Name),
		AttrFunctionTags.String(fn.CalculationNodesTags),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	}, func(ctx context.Context, span trace.Span) error {
		var err error
		result, err = t.inner.DispatchFunction(ctx, entity, fn, workflowName, transitionName, txID)
		return err
	})
	return result, err
}
