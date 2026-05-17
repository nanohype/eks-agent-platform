/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import "testing"

func TestAwsRegionFromQueueURL(t *testing.T) {
	cases := []struct {
		name, url, want string
	}{
		{"standard", "https://sqs.us-west-2.amazonaws.com/123456789012/work", "us-west-2"},
		{"us-east-1", "https://sqs.us-east-1.amazonaws.com/123456789012/q", "us-east-1"},
		{"eu-central-1", "https://sqs.eu-central-1.amazonaws.com/123456789012/q", "eu-central-1"},
		{"fifo", "https://sqs.us-west-2.amazonaws.com/123456789012/work.fifo", "us-west-2"},
		{"empty", "", "us-west-2"},
		{"missing_prefix", "sqs.us-west-2.amazonaws.com/123/q", "us-west-2"},
		{"malformed_no_dot", "https://sqs/", "us-west-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := awsRegionFromQueueURL(c.url)
			if got != c.want {
				t.Errorf("awsRegionFromQueueURL(%q) = %q; want %q", c.url, got, c.want)
			}
		})
	}
}
