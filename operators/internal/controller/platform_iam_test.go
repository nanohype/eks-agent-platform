/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

func TestSuspensionFromTags(t *testing.T) {
	cases := []struct {
		name       string
		tags       []iamtypes.Tag
		wantSusp   bool
		wantReason string
	}{
		{name: "empty", tags: nil, wantSusp: false, wantReason: ""},
		{name: "no_marker", tags: []iamtypes.Tag{
			{Key: aws.String("Environment"), Value: aws.String("production")},
			{Key: aws.String("PlatformId"), Value: aws.String("acme")},
		}, wantSusp: false, wantReason: ""},
		{name: "suspended_true", tags: []iamtypes.Tag{
			{Key: aws.String(suspendedTag), Value: aws.String("true")},
			{Key: aws.String(suspendedReasonTag), Value: aws.String("budget-exceeded")},
		}, wantSusp: true, wantReason: "budget-exceeded"},
		{name: "suspended_false_string", tags: []iamtypes.Tag{
			{Key: aws.String(suspendedTag), Value: aws.String("false")},
		}, wantSusp: false, wantReason: ""},
		{name: "suspended_true_no_reason", tags: []iamtypes.Tag{
			{Key: aws.String(suspendedTag), Value: aws.String("true")},
		}, wantSusp: true, wantReason: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotSusp, gotReason := suspensionFromTags(c.tags)
			if gotSusp != c.wantSusp {
				t.Errorf("suspended: got %v want %v", gotSusp, c.wantSusp)
			}
			if gotReason != c.wantReason {
				t.Errorf("reason: got %q want %q", gotReason, c.wantReason)
			}
		})
	}
}
