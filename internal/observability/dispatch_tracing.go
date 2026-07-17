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
)

// TracingExternalProcessingService wraps an ExternalProcessingService with OTel spans and metrics.
type TracingExternalProcessingService struct {
	inner            contract.ExternalProcessingService
	tracer           trace.Tracer
	dispatchDuration metric.Float64Histogram
	dispatchTotal    metric.Int64Counter
	typeProcessor    metric.MeasurementOption
	typeCriteria     metric.MeasurementOption
}

// NewTracingExternalProcessingService returns a TracingExternalProcessingService that decorates
// inner with OTel tracing spans and metrics for processor and criteria dispatches.
func NewTracingExternalProcessingService(inner contract.ExternalProcessingService, meter metric.Meter) *TracingExternalProcessingService {
	tracer := Tracer()

	duration, err := meter.Float64Histogram("cyoda.dispatch.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Processor/criteria dispatch duration"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	instrErr("cyoda.dispatch.duration", err)
	total, err := meter.Int64Counter("cyoda.dispatch.count",
		metric.WithDescription("Total dispatches"))
	instrErr("cyoda.dispatch.count", err)

	return &TracingExternalProcessingService{
		inner:            inner,
		tracer:           tracer,
		dispatchDuration: duration,
		dispatchTotal:    total,
		typeProcessor:    metric.WithAttributes(AttrDispatchType.String(kindProcessor)),
		typeCriteria:     metric.WithAttributes(AttrDispatchType.String(kindCriteria)),
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

	opt := t.measurementOption(kind)
	start := time.Now()
	err := fn(ctx, span)
	elapsed := time.Since(start).Seconds()

	t.dispatchDuration.Record(ctx, elapsed, opt)
	t.dispatchTotal.Add(ctx, 1, opt)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// measurementOption returns the cached MeasurementOption for kind, falling back to a
// freshly-built one for kinds not pre-cached in NewTracingExternalProcessingService.
func (t *TracingExternalProcessingService) measurementOption(kind string) metric.MeasurementOption {
	switch kind {
	case kindProcessor:
		return t.typeProcessor
	case kindCriteria:
		return t.typeCriteria
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
