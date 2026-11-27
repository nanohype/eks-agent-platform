/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

var _ awsclients.Athena = (*Athena)(nil)

// Athena is an in-memory awsclients.Athena that drives the query lifecycle:
// StartQueryExecution, then a scripted sequence of GetQueryExecution states,
// then GetQueryResults.
//
// It honours StartQueryExecution's idempotency the way Athena does: a call
// repeating an earlier ClientRequestToken starts nothing and returns the first
// call's QueryExecutionId, and one repeating the token with a different query
// string gets InvalidRequestException. A call with no token starts a new
// execution every time. The real SDK client fills an absent token once per
// StartQueryExecution invocation, before its retries, so a fake that receives
// none is being called the way the SDK's caller calls it.
type Athena struct {
	sync.Mutex

	// States are returned in order across GetQueryExecution calls, the last
	// repeating once the sequence runs out.
	States []athenatypes.QueryExecutionState
	// StateIdx is the position in States the next GetQueryExecution returns.
	StateIdx int
	// Results is what GetQueryResults returns. Nil returns an empty result
	// set.
	Results *athena.GetQueryResultsOutput
	// Failure is the StateChangeReason every GetQueryExecution reports.
	Failure string
	// StartErr, GetErr and ResultErr fail StartQueryExecution,
	// GetQueryExecution and GetQueryResults.
	StartErr  error
	GetErr    error
	ResultErr error

	// LastQuery is the query string of the last execution started.
	LastQuery string
	// StartCalls records every StartQueryExecution call past StartErr,
	// repeated tokens and refused ones included.
	StartCalls []athena.StartQueryExecutionInput
	// Started holds the id of every execution started, in order. A call
	// answered from an earlier token starts none.
	Started []string
	// Stopped reports whether StopQueryExecution was called, and StopCalls
	// records each call.
	Stopped   bool
	StopCalls []athena.StopQueryExecutionInput

	// tokens maps each ClientRequestToken seen to the execution it started.
	tokens map[string]startedQuery
}

// startedQuery is the execution a ClientRequestToken started.
type startedQuery struct {
	id, query string
}

// NewAthena returns an Athena that answers GetQueryExecution with states, in
// order.
func NewAthena(states ...athenatypes.QueryExecutionState) *Athena {
	return &Athena{States: states}
}

// StartQueryExecution starts an execution, or returns the one an earlier call
// with the same ClientRequestToken started. Ids run qid-1, qid-2, ...
func (f *Athena) StartQueryExecution(_ context.Context, in *athena.StartQueryExecutionInput, _ ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	f.Lock()
	defer f.Unlock()
	if f.StartErr != nil {
		return nil, f.StartErr
	}
	f.StartCalls = append(f.StartCalls, *in)
	query := aws.ToString(in.QueryString)
	token := aws.ToString(in.ClientRequestToken)
	if in.ClientRequestToken != nil {
		if prior, ok := f.tokens[token]; ok {
			if prior.query != query {
				return nil, &athenatypes.InvalidRequestException{
					Message: aws.String("Idempotent parameters do not match: ClientRequestToken " + token + " was used with a different query"),
				}
			}
			return &athena.StartQueryExecutionOutput{QueryExecutionId: aws.String(prior.id)}, nil
		}
	}
	id := fmt.Sprintf("qid-%d", len(f.Started)+1)
	f.Started = append(f.Started, id)
	f.LastQuery = query
	if in.ClientRequestToken != nil {
		if f.tokens == nil {
			f.tokens = map[string]startedQuery{}
		}
		f.tokens[token] = startedQuery{id: id, query: query}
	}
	return &athena.StartQueryExecutionOutput{QueryExecutionId: aws.String(id)}, nil
}

// GetQueryExecution returns the next scripted state for the execution.
func (f *Athena) GetQueryExecution(_ context.Context, in *athena.GetQueryExecutionInput, _ ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	f.Lock()
	defer f.Unlock()
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	if len(f.States) == 0 {
		return nil, errors.New("awsfake.Athena: GetQueryExecution called with no States scripted")
	}
	state := f.States[f.StateIdx]
	if f.StateIdx < len(f.States)-1 {
		f.StateIdx++
	}
	return &athena.GetQueryExecutionOutput{QueryExecution: &athenatypes.QueryExecution{
		QueryExecutionId: in.QueryExecutionId,
		Status:           &athenatypes.QueryExecutionStatus{State: state, StateChangeReason: aws.String(f.Failure)},
	}}, nil
}

// GetQueryResults returns Results.
func (f *Athena) GetQueryResults(_ context.Context, _ *athena.GetQueryResultsInput, _ ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	f.Lock()
	defer f.Unlock()
	if f.ResultErr != nil {
		return nil, f.ResultErr
	}
	if f.Results == nil {
		return &athena.GetQueryResultsOutput{ResultSet: &athenatypes.ResultSet{}}, nil
	}
	return f.Results, nil
}

// StopQueryExecution records the stop.
func (f *Athena) StopQueryExecution(_ context.Context, in *athena.StopQueryExecutionInput, _ ...func(*athena.Options)) (*athena.StopQueryExecutionOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.Stopped = true
	f.StopCalls = append(f.StopCalls, *in)
	return &athena.StopQueryExecutionOutput{}, nil
}
