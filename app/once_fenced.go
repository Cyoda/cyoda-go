package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// onceFenced makes every dispatch a callout of exactly one try as far as the
// fence is concerned: it begins the callout, advances it to its first number,
// mints the one pass of that try, and ends the callout when the dispatch
// returns. It is TEMPORARY: the owner's loop (internal/callout.Coordinator)
// begins, advances and ends callouts itself, and deletes this type when it
// becomes the ExternalProcessingService.
type onceFenced struct {
	inner      contract.ExternalProcessingService
	fence      *fence.Fence
	signer     *token.Signer
	selfNodeID string
	passTTL    time.Duration
	uuids      spi.UUIDGenerator
}

func newOnceFenced(inner contract.ExternalProcessingService, f *fence.Fence, signer *token.Signer, selfNodeID string, passTTL time.Duration, uuids spi.UUIDGenerator) *onceFenced {
	return &onceFenced{inner: inner, fence: f, signer: signer, selfNodeID: selfNodeID, passTTL: passTTL, uuids: uuids}
}

// begin opens the callout and returns the context the dispatch runs under, the
// func that ends the callout, and the func that turns the dispatch's error into
// CALLOUT_SUPERSEDED when the fence released it.
func (d *onceFenced) begin(ctx context.Context, txID string) (context.Context, func(), func(error) error, error) {
	calloutID := uuid.UUID(d.uuids.NewTimeUUID()).String()
	outer := fence.Pairs(ctx)
	cctx, end := d.fence.Begin(ctx, calloutID, txID, outer)
	d.fence.Advance(calloutID, 1)
	if txID != "" {
		pass, err := d.signer.Issue(token.Claims{
			NodeID:    d.selfNodeID,
			TxRef:     txID,
			ExpiresAt: time.Now().Add(d.passTTL).Unix(),
			Callout:   calloutID,
			Major:     1,
			Outer:     outer,
		})
		if err != nil {
			end()
			return nil, nil, nil, common.Internal("failed to mint the callout's pass", fmt.Errorf("failed to mint pass: %w", err))
		}
		cctx = internalgrpc.WithTxToken(cctx, pass)
	}
	outcome := func(err error) error {
		if err != nil && errors.Is(context.Cause(cctx), fence.ErrSuperseded) {
			return fence.NewSupersededError()
		}
		return err
	}
	return cctx, end, outcome, nil
}

func (d *onceFenced) DispatchProcessor(ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition, workflowName, transitionName, txID string) (*spi.Entity, error) {
	cctx, end, outcome, err := d.begin(ctx, txID)
	if err != nil {
		return nil, err
	}
	defer end()
	res, err := d.inner.DispatchProcessor(cctx, entity, processor, workflowName, transitionName, txID)
	if err = outcome(err); err != nil {
		return nil, err
	}
	return res, nil
}

func (d *onceFenced) DispatchCriteria(ctx context.Context, entity *spi.Entity, criterion json.RawMessage, target, workflowName, transitionName, processorName, txID string) (bool, string, error) {
	cctx, end, outcome, err := d.begin(ctx, txID)
	if err != nil {
		return false, "", err
	}
	defer end()
	matches, reason, err := d.inner.DispatchCriteria(cctx, entity, criterion, target, workflowName, transitionName, processorName, txID)
	if err = outcome(err); err != nil {
		return false, "", err
	}
	return matches, reason, nil
}

func (d *onceFenced) DispatchFunction(ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction, workflowName, transitionName, txID string) (contract.FunctionResult, error) {
	cctx, end, outcome, err := d.begin(ctx, txID)
	if err != nil {
		return contract.FunctionResult{}, err
	}
	defer end()
	res, err := d.inner.DispatchFunction(cctx, entity, fn, workflowName, transitionName, txID)
	if err = outcome(err); err != nil {
		return contract.FunctionResult{}, err
	}
	return res, nil
}
