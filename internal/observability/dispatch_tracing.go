package observability

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
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
		typeProcessor:    metric.WithAttributes(AttrDispatchType.String("processor")),
		typeCriteria:     metric.WithAttributes(AttrDispatchType.String("criteria")),
	}
}

func (t *TracingExternalProcessingService) DispatchProcessor(
	ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition,
	workflowName, transitionName, txID string,
) (*spi.Entity, error) {
	ctx, span := t.tracer.Start(ctx, "dispatch.processor", trace.WithAttributes(
		AttrProcessorName.String(processor.Name),
		AttrProcessorMode.String(processor.ExecutionMode),
		AttrProcessorTags.String(processor.Config.CalculationNodesTags),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	))
	defer span.End()

	start := time.Now()
	result, err := t.inner.DispatchProcessor(ctx, entity, processor, workflowName, transitionName, txID)
	elapsed := time.Since(start).Seconds()

	t.dispatchDuration.Record(ctx, elapsed, t.typeProcessor)
	t.dispatchTotal.Add(ctx, 1, t.typeProcessor)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}

func (t *TracingExternalProcessingService) DispatchCriteria(
	ctx context.Context, entity *spi.Entity, criterion json.RawMessage,
	target, workflowName, transitionName, processorName, txID string,
) (bool, string, error) {
	ctx, span := t.tracer.Start(ctx, "dispatch.criteria", trace.WithAttributes(
		AttrCriterionTarget.String(target),
		AttrWorkflowName.String(workflowName),
		AttrTransitionName.String(transitionName),
	))
	defer span.End()

	start := time.Now()
	matches, reason, err := t.inner.DispatchCriteria(ctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	elapsed := time.Since(start).Seconds()

	t.dispatchDuration.Record(ctx, elapsed, t.typeCriteria)
	t.dispatchTotal.Add(ctx, 1, t.typeCriteria)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.SetAttributes(AttrCriteriaMatches.Bool(matches))
	return matches, reason, err
}
