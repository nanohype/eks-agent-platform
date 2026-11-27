/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

func metricQuery(id string, returnData *bool) cloudwatchtypes.MetricDataQuery {
	return cloudwatchtypes.MetricDataQuery{
		Id: aws.String(id),
		MetricStat: &cloudwatchtypes.MetricStat{
			Metric: &cloudwatchtypes.Metric{
				Namespace:  aws.String("agents/Bedrock"),
				MetricName: aws.String("EstimatedInvocationCostUsd"),
				Dimensions: []cloudwatchtypes.Dimension{{Name: aws.String("PlatformId"), Value: aws.String("c-acme")}},
			},
			Period: aws.Int32(60),
			Stat:   aws.String("Sum"),
		},
		ReturnData: returnData,
	}
}

// TestCloudWatchScriptsResults: each query that returns data is answered under
// its own Id with the scripted values, Empty answers nothing, Err fails the
// call, and every call and the metric it named are recorded.
func TestCloudWatchScriptsResults(t *testing.T) {
	ctx := context.Background()
	in := &cloudwatch.GetMetricDataInput{MetricDataQueries: []cloudwatchtypes.MetricDataQuery{
		metricQuery("inflight", nil), metricQuery("hidden", aws.Bool(false)), metricQuery("shown", aws.Bool(true)),
	}}

	f := awsfake.NewCloudWatch(0.25, 0.5)
	out, err := f.GetMetricData(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.MetricDataResults) != 2 || aws.ToString(out.MetricDataResults[0].Id) != "inflight" ||
		aws.ToString(out.MetricDataResults[1].Id) != "shown" || len(out.MetricDataResults[0].Values) != 2 {
		t.Errorf("results = %+v, want inflight and shown with the scripted values", out.MetricDataResults)
	}
	if f.Namespace != "agents/Bedrock" || f.MetricName != "EstimatedInvocationCostUsd" || f.Dimension("PlatformId") != "c-acme" || f.Dimension("Other") != "" {
		t.Errorf("recorded metric %s/%s %v", f.Namespace, f.MetricName, f.Dimensions)
	}

	empty := &awsfake.CloudWatch{Empty: true, Values: []float64{1}}
	if out, err := empty.GetMetricData(ctx, in); err != nil || len(out.MetricDataResults) != 0 {
		t.Errorf("Empty = (%+v, %v), want no results", out, err)
	}
	boom := errors.New("throttled")
	failing := &awsfake.CloudWatch{Err: boom}
	if _, err := failing.GetMetricData(ctx, in); !errors.Is(err, boom) || len(failing.Calls) != 1 {
		t.Errorf("Err = %v with %d calls recorded, want the injected error and the call recorded", err, len(failing.Calls))
	}
}
