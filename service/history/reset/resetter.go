// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

//go:generate mockgen -copyright_file ../../../LICENSE -package $GOPACKAGE -source $GOFILE -destination resetter_mock.go

package reset

import (
	context "context"
	ctx "context"
	"fmt"

	"github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/collection"
	"github.com/uber/cadence/common/definition"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/service/history/execution"
	"github.com/uber/cadence/service/history/shard"
)

type (
	// WorkflowResetter is the new NDC compatible workflow reset component
	WorkflowResetter interface {
		ResetWorkflow(
			ctx ctx.Context,
			domainID string,
			workflowID string,
			baseRunID string,
			baseBranchToken []byte,
			baseRebuildLastEventID int64,
			baseRebuildLastEventVersion int64,
			baseNextEventID int64,
			resetRunID string,
			resetRequestID string,
			currentWorkflow execution.Workflow,
			resetReason string,
			additionalReapplyEvents []*shared.HistoryEvent,
		) error
	}

	workflowResetterImpl struct {
		shard             shard.Context
		domainCache       cache.DomainCache
		clusterMetadata   cluster.Metadata
		historyV2Mgr      persistence.HistoryManager
		executionCache    *execution.Cache
		newStateRebuilder nDCStateRebuilderProvider
		logger            log.Logger
	}

	nDCStateRebuilderProvider func() execution.StateRebuilder
)

var _ WorkflowResetter = (*workflowResetterImpl)(nil)

// NewWorkflowResetter creates a workflow resetter
func NewWorkflowResetter(
	shard shard.Context,
	executionCache *execution.Cache,
	logger log.Logger,
) WorkflowResetter {
	return &workflowResetterImpl{
		shard:           shard,
		domainCache:     shard.GetDomainCache(),
		clusterMetadata: shard.GetClusterMetadata(),
		historyV2Mgr:    shard.GetHistoryManager(),
		executionCache:  executionCache,
		newStateRebuilder: func() execution.StateRebuilder {
			return execution.NewStateRebuilder(shard, logger)
		},
		logger: logger,
	}
}

func (r *workflowResetterImpl) ResetWorkflow(
	ctx ctx.Context,
	domainID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildLastEventID int64,
	baseRebuildLastEventVersion int64,
	baseNextEventID int64,
	resetRunID string,
	resetRequestID string,
	currentWorkflow execution.Workflow,
	resetReason string,
	additionalReapplyEvents []*shared.HistoryEvent,
) (retError error) {

	domainEntry, err := r.domainCache.GetDomainByID(domainID)
	if err != nil {
		return err
	}
	resetWorkflowVersion := domainEntry.GetFailoverVersion()

	currentMutableState := currentWorkflow.GetMutableState()
	currentWorkflowTerminated := false
	if currentMutableState.IsWorkflowExecutionRunning() {
		if err := r.terminateWorkflow(
			currentMutableState,
			resetReason,
		); err != nil {
			return err
		}
		resetWorkflowVersion = currentMutableState.GetCurrentVersion()
		currentWorkflowTerminated = true
	}

	resetWorkflow, err := r.prepareResetWorkflow(
		ctx,
		domainID,
		workflowID,
		baseRunID,
		baseBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		baseNextEventID,
		resetRunID,
		resetRequestID,
		resetWorkflowVersion,
		resetReason,
		additionalReapplyEvents,
	)
	if err != nil {
		return err
	}
	defer resetWorkflow.GetReleaseFn()(retError)

	return r.persistToDB(
		ctx,
		currentWorkflowTerminated,
		currentWorkflow,
		resetWorkflow,
	)
}

func (r *workflowResetterImpl) prepareResetWorkflow(
	ctx ctx.Context,
	domainID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildLastEventID int64,
	baseRebuildLastEventVersion int64,
	baseNextEventID int64,
	resetRunID string,
	resetRequestID string,
	resetWorkflowVersion int64,
	resetReason string,
	additionalReapplyEvents []*shared.HistoryEvent,
) (execution.Workflow, error) {

	resetWorkflow, err := r.replayResetWorkflow(
		ctx,
		domainID,
		workflowID,
		baseRunID,
		baseBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		resetRunID,
		resetRequestID,
	)
	if err != nil {
		return nil, err
	}

	resetMutableState := resetWorkflow.GetMutableState()

	baseLastEventVersion := resetMutableState.GetCurrentVersion()
	if baseLastEventVersion > resetWorkflowVersion {
		return nil, &shared.InternalServiceError{
			Message: "workflowResetter encounter version mismatch.",
		}
	}
	if err := resetMutableState.UpdateCurrentVersion(
		resetWorkflowVersion,
		false,
	); err != nil {
		return nil, err
	}

	// TODO add checking of reset until event ID == decision task started ID + 1
	decision, ok := resetMutableState.GetInFlightDecision()
	if !ok || decision.StartedID+1 != resetMutableState.GetNextEventID() {
		return nil, &shared.BadRequestError{
			Message: fmt.Sprintf("Can only reset workflow to DecisionTaskStarted + 1: %v", baseRebuildLastEventID+1),
		}
	}
	if len(resetMutableState.GetPendingChildExecutionInfos()) > 0 {
		return nil, &shared.BadRequestError{
			Message: fmt.Sprintf("Can only reset workflow with pending child workflows"),
		}
	}

	_, err = resetMutableState.AddDecisionTaskFailedEvent(
		decision.ScheduleID,
		decision.StartedID, shared.DecisionTaskFailedCauseResetWorkflow,
		nil,
		execution.IdentityHistoryService,
		resetReason,
		"",
		baseRunID,
		resetRunID,
		baseLastEventVersion,
	)
	if err != nil {
		return nil, err
	}

	if err := r.failInflightActivity(resetMutableState, resetReason); err != nil {
		return nil, err
	}

	if err := r.reapplyContinueAsNewWorkflowEvents(
		ctx,
		resetMutableState,
		domainID,
		workflowID,
		baseRunID,
		baseBranchToken,
		baseRebuildLastEventID+1,
		baseNextEventID,
	); err != nil {
		return nil, err
	}

	if err := r.reapplyEvents(resetMutableState, additionalReapplyEvents); err != nil {
		return nil, err
	}

	if err := execution.ScheduleDecision(resetMutableState); err != nil {
		return nil, err
	}

	return resetWorkflow, nil
}

func (r *workflowResetterImpl) persistToDB(
	ctx context.Context,
	currentWorkflowTerminated bool,
	currentWorkflow execution.Workflow,
	resetWorkflow execution.Workflow,
) error {

	if currentWorkflowTerminated {
		return currentWorkflow.GetContext().UpdateWorkflowExecutionWithNewAsActive(
			ctx,
			r.shard.GetTimeSource().Now(),
			resetWorkflow.GetContext(),
			resetWorkflow.GetMutableState(),
		)
	}

	currentMutableState := currentWorkflow.GetMutableState()
	currentRunID := currentMutableState.GetExecutionInfo().RunID
	currentLastWriteVersion, err := currentMutableState.GetLastWriteVersion()
	if err != nil {
		return err
	}

	now := r.shard.GetTimeSource().Now()
	resetWorkflowSnapshot, resetWorkflowEventsSeq, err := resetWorkflow.GetMutableState().CloseTransactionAsSnapshot(
		now,
		execution.TransactionPolicyActive,
	)
	if err != nil {
		return err
	}
	if len(resetWorkflowEventsSeq) != 1 {
		return &shared.InternalServiceError{
			Message: "there should be EXACTLY one batch of events for reset",
		}
	}
	resetHistorySize, err := resetWorkflow.GetContext().PersistNonFirstWorkflowEvents(ctx, resetWorkflowEventsSeq[0])
	if err != nil {
		return err
	}

	return resetWorkflow.GetContext().CreateWorkflowExecution(
		ctx,
		resetWorkflowSnapshot,
		resetHistorySize,
		now,
		persistence.CreateWorkflowModeContinueAsNew,
		currentRunID,
		currentLastWriteVersion,
	)
}

func (r *workflowResetterImpl) replayResetWorkflow(
	ctx ctx.Context,
	domainID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildLastEventID int64,
	baseRebuildLastEventVersion int64,
	resetRunID string,
	resetRequestID string,
) (execution.Workflow, error) {

	resetBranchToken, err := r.forkAndGenerateBranchToken(
		ctx,
		domainID,
		workflowID,
		baseBranchToken,
		baseRebuildLastEventID+1,
		resetRunID,
	)
	if err != nil {
		return nil, err
	}

	resetContext := execution.NewContext(
		domainID,
		shared.WorkflowExecution{
			WorkflowId: common.StringPtr(workflowID),
			RunId:      common.StringPtr(resetRunID),
		},
		r.shard,
		r.shard.GetExecutionManager(),
		r.logger,
	)
	resetMutableState, resetHistorySize, err := r.newStateRebuilder().Rebuild(
		ctx,
		r.shard.GetTimeSource().Now(),
		definition.NewWorkflowIdentifier(
			domainID,
			workflowID,
			baseRunID,
		),
		baseBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		definition.NewWorkflowIdentifier(
			domainID,
			workflowID,
			resetRunID,
		),
		resetBranchToken,
		resetRequestID,
	)
	if err != nil {
		return nil, err
	}

	resetContext.SetHistorySize(resetHistorySize)
	return execution.NewWorkflow(
		ctx,
		r.domainCache,
		r.clusterMetadata,
		resetContext,
		resetMutableState,
		execution.NoopReleaseFn,
	), nil
}

func (r *workflowResetterImpl) failInflightActivity(
	mutableState execution.MutableState,
	terminateReason string,
) error {

	for _, ai := range mutableState.GetPendingActivityInfos() {
		switch ai.StartedID {
		case common.EmptyEventID:
			// activity not started, noop
		case common.TransientEventID:
			// activity is started (with retry policy)
			// should not encounter this case when rebuilding mutable state
			return &shared.InternalServiceError{
				Message: "workflowResetter encounter transient activity",
			}
		default:
			if _, err := mutableState.AddActivityTaskFailedEvent(
				ai.ScheduleID,
				ai.StartedID,
				&shared.RespondActivityTaskFailedRequest{
					Reason:   common.StringPtr(terminateReason),
					Details:  ai.Details,
					Identity: common.StringPtr(ai.StartedIdentity),
				},
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *workflowResetterImpl) forkAndGenerateBranchToken(
	ctx context.Context,
	domainID string,
	workflowID string,
	forkBranchToken []byte,
	forkNodeID int64,
	resetRunID string,
) ([]byte, error) {
	// fork a new history branch
	shardID := r.shard.GetShardID()
	resp, err := r.historyV2Mgr.ForkHistoryBranch(ctx, &persistence.ForkHistoryBranchRequest{
		ForkBranchToken: forkBranchToken,
		ForkNodeID:      forkNodeID,
		Info:            persistence.BuildHistoryGarbageCleanupInfo(domainID, workflowID, resetRunID),
		ShardID:         common.IntPtr(shardID),
	})
	if err != nil {
		return nil, err
	}

	return resp.NewBranchToken, nil
}

func (r *workflowResetterImpl) terminateWorkflow(
	mutableState execution.MutableState,
	terminateReason string,
) error {

	eventBatchFirstEventID := mutableState.GetNextEventID()
	return execution.TerminateWorkflow(
		mutableState,
		eventBatchFirstEventID,
		terminateReason,
		nil,
		execution.IdentityHistoryService,
	)
}

func (r *workflowResetterImpl) reapplyContinueAsNewWorkflowEvents(
	ctx ctx.Context,
	resetMutableState execution.MutableState,
	domainID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseRebuildNextEventID int64,
	baseNextEventID int64,
) error {

	// TODO change this logic to fetching all workflow [baseWorkflow, currentWorkflow]
	//  from visibility for better coverage of events eligible for re-application.

	var nextRunID string
	var err error

	// first special handling the remaining events for base workflow
	if nextRunID, err = r.reapplyWorkflowEvents(
		ctx,
		resetMutableState,
		baseRebuildNextEventID,
		baseNextEventID,
		baseBranchToken,
	); err != nil {
		return err
	}

	getNextEventIDBranchToken := func(runID string) (nextEventID int64, branchToken []byte, retError error) {
		context, release, err := r.executionCache.GetOrCreateWorkflowExecution(
			ctx,
			domainID,
			shared.WorkflowExecution{
				WorkflowId: common.StringPtr(workflowID),
				RunId:      common.StringPtr(runID),
			},
		)
		if err != nil {
			return 0, nil, err
		}
		defer func() { release(retError) }()

		mutableState, err := context.LoadWorkflowExecution(ctx)
		if err != nil {
			// no matter what error happen, we need to retry
			return 0, nil, err
		}

		nextEventID = mutableState.GetNextEventID()
		branchToken, err = mutableState.GetCurrentBranchToken()
		if err != nil {
			return 0, nil, err
		}
		return nextEventID, branchToken, nil
	}

	// second for remaining continue as new workflow, reapply eligible events
	for len(nextRunID) != 0 {
		nextWorkflowNextEventID, nextWorkflowBranchToken, err := getNextEventIDBranchToken(nextRunID)
		if err != nil {
			return err
		}

		if nextRunID, err = r.reapplyWorkflowEvents(
			ctx,
			resetMutableState,
			common.FirstEventID,
			nextWorkflowNextEventID,
			nextWorkflowBranchToken,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *workflowResetterImpl) reapplyWorkflowEvents(
	ctx context.Context,
	mutableState execution.MutableState,
	firstEventID int64,
	nextEventID int64,
	branchToken []byte,
) (string, error) {

	// TODO change this logic to fetching all workflow [baseWorkflow, currentWorkflow]
	//  from visibility for better coverage of events eligible for re-application.
	//  after the above change, this API do not have to return the continue as new run ID

	iter := collection.NewPagingIterator(r.getPaginationFn(
		ctx,
		firstEventID,
		nextEventID,
		branchToken,
	))

	var nextRunID string
	var lastEvents []*shared.HistoryEvent

	for iter.HasNext() {
		batch, err := iter.Next()
		if err != nil {
			return "", err
		}
		lastEvents = batch.(*shared.History).Events
		if err := r.reapplyEvents(mutableState, lastEvents); err != nil {
			return "", err
		}
	}

	if len(lastEvents) > 0 {
		lastEvent := lastEvents[len(lastEvents)-1]
		if lastEvent.GetEventType() == shared.EventTypeWorkflowExecutionContinuedAsNew {
			nextRunID = lastEvent.GetWorkflowExecutionContinuedAsNewEventAttributes().GetNewExecutionRunId()
		}
	}
	return nextRunID, nil
}

func (r *workflowResetterImpl) reapplyEvents(
	mutableState execution.MutableState,
	events []*shared.HistoryEvent,
) error {

	for _, event := range events {
		switch event.GetEventType() {
		case shared.EventTypeWorkflowExecutionSignaled:
			attr := event.GetWorkflowExecutionSignaledEventAttributes()
			if _, err := mutableState.AddWorkflowExecutionSignaled(
				attr.GetSignalName(),
				attr.GetInput(),
				attr.GetIdentity(),
			); err != nil {
				return err
			}
		default:
			// events other than signal will be ignored
		}
	}
	return nil
}

func (r *workflowResetterImpl) getPaginationFn(
	ctx context.Context,
	firstEventID int64,
	nextEventID int64,
	branchToken []byte,
) collection.PaginationFn {

	return func(paginationToken []byte) ([]interface{}, []byte, error) {

		_, historyBatches, token, _, err := persistence.PaginateHistory(
			ctx,
			r.historyV2Mgr,
			true,
			branchToken,
			firstEventID,
			nextEventID,
			paginationToken,
			execution.NDCDefaultPageSize,
			common.IntPtr(r.shard.GetShardID()),
		)
		if err != nil {
			return nil, nil, err
		}

		var paginateItems []interface{}
		for _, history := range historyBatches {
			paginateItems = append(paginateItems, history)
		}
		return paginateItems, token, nil
	}
}
