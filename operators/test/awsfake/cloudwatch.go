/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

var _ awsclients.CloudWatch = (*CloudWatch)(nil)

// CloudWatch is an in-memory awsclients.CloudWatch answering GetMetricData
// from a script and recording what it was asked for.
type CloudWatch struct {
	sync.Mutex

	// Values are the datapoints of every result GetMetricData returns: one
	// result per query that returns data (ReturnData unset or true), carrying
	// that query's Id.
	Values []float64
	// Empty returns no results at all, the shape of a series with no
	// datapoints in the window.
	Empty bool
	// Err fails every GetMetricData call.
	Err error

	// Calls records every GetMetricData call, ahead of Err.
	Calls []cloudwatch.GetMetricDataInput
	// Dimensions, Namespace and MetricName are the metric the last call's
	// first query named, so a test can assert the series actually queried.
	Dimensions []cloudwatchtypes.Dimension
	Namespace  string
	MetricName string
}

// NewCloudWatch returns a CloudWatch whose every result carries values.
func NewCloudWatch(values ...float64) *CloudWatch {
	return &CloudWatch{Values: values}
}

// GetMetricData answers each query with Values.
func (f *CloudWatch) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.Calls = append(f.Calls, *in)
	if len(in.MetricDataQueries) > 0 && in.MetricDataQueries[0].MetricStat != nil && in.MetricDataQueries[0].MetricStat.Metric != nil {
		m := in.MetricDataQueries[0].MetricStat.Metric
		f.Dimensions = m.Dimensions
		f.Namespace = aws.ToString(m.Namespace)
		f.MetricName = aws.ToString(m.MetricName)
	}
	if f.Err != nil {
		return nil, f.Err
	}
	out := &cloudwatch.GetMetricDataOutput{}
	if f.Empty {
		return out, nil
	}
	for _, q := range in.MetricDataQueries {
		if q.ReturnData != nil && !*q.ReturnData {
			continue
		}
		out.MetricDataResults = append(out.MetricDataResults, cloudwatchtypes.MetricDataResult{
			Id:         q.Id,
			Values:     append([]float64(nil), f.Values...),
			StatusCode: cloudwatchtypes.StatusCodeComplete,
		})
	}
	return out, nil
}

// Dimension is the value of the named dimension on the last metric queried,
// "" when it carried none of that name.
func (f *CloudWatch) Dimension(name string) string {
	f.Lock()
	defer f.Unlock()
	for _, d := range f.Dimensions {
		if aws.ToString(d.Name) == name {
			return aws.ToString(d.Value)
		}
	}
	return ""
}
