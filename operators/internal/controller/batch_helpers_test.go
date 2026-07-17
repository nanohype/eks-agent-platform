/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

func batchJob(name, platform string) *agentsv1alpha1.BatchJob {
	return &agentsv1alpha1.BatchJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenants-acme", Generation: 3},
		Spec: agentsv1alpha1.BatchJobSpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: platform},
			InputS3Uri:  "s3://in/data.jsonl",
		},
	}
}

func TestMapBedrockStatus(t *testing.T) {
	cases := map[string]string{
		"Submitted":          phaseProvisioning,
		"Validating":         phaseProvisioning,
		"Scheduled":          phaseProvisioning,
		"InProgress":         phaseRunning,
		"Stopping":           phaseRunning,
		"Completed":          phaseSucceeded,
		"PartiallyCompleted": phaseSucceeded,
		"Failed":             phaseFailed,
		"Expired":            phaseFailed,
		"Stopped":            phaseBatchStopped,
		"SomethingNew":       phaseProvisioning, // unknown ⇒ keep polling
	}
	for in, want := range cases {
		if got := mapBedrockStatus(in); got != want {
			t.Errorf("mapBedrockStatus(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestIsTerminalBatchPhase(t *testing.T) {
	for _, p := range []string{phaseSucceeded, phaseFailed, phaseBatchStopped} {
		if !isTerminalBatchPhase(p) {
			t.Errorf("%q should be terminal", p)
		}
	}
	for _, p := range []string{phaseProvisioning, phaseRunning, ""} {
		if isTerminalBatchPhase(p) {
			t.Errorf("%q should not be terminal", p)
		}
	}
}

func TestS3OutputURI(t *testing.T) {
	cfg := &bedrocktypes.ModelInvocationJobOutputDataConfigMemberS3OutputDataConfig{
		Value: bedrocktypes.ModelInvocationJobS3OutputDataConfig{S3Uri: aws.String("s3://out/results/")},
	}
	if got := s3OutputURI(cfg); got != "s3://out/results/" {
		t.Errorf("s3OutputURI: got %q", got)
	}
	// A nil/other union member yields the empty string, not a panic.
	if got := s3OutputURI(nil); got != "" {
		t.Errorf("s3OutputURI(nil): got %q want empty", got)
	}
}

func TestBatchJobName(t *testing.T) {
	short := batchJobName(batchJob("nightly", "acme"))
	if short != "acme-nightly" {
		t.Errorf("short name: got %q want acme-nightly", short)
	}
	long := batchJobName(batchJob(strings.Repeat("z", 80), "platform"))
	if len(long) > batchJobNameMaxLen {
		t.Errorf("long name over Bedrock's %d-char limit: %d", batchJobNameMaxLen, len(long))
	}
}

func TestSanitizeBedrockName(t *testing.T) {
	cases := map[string]string{
		"already-ok.9":  "already-ok.9",
		"has spaces!":   "has-spaces-",
		"9leadingdigit": "9leadingdigit", // digit is a valid first char
		"_underscore":   "b-underscore",  // invalid leading char gets a 'b' prefix
	}
	for in, want := range cases {
		if got := sanitizeBedrockName(in); got != want {
			t.Errorf("sanitizeBedrockName(%q) = %q; want %q", in, got, want)
		}
	}
	if got := sanitizeBedrockName(""); got != "batch" {
		t.Errorf("empty input must default to batch, got %q", got)
	}
}

func TestIsASCIIAlnum(t *testing.T) {
	for _, c := range []byte("aZ9") {
		if !isASCIIAlnum(c) {
			t.Errorf("%c should be alnum", c)
		}
	}
	for _, c := range []byte("-._ ") {
		if isASCIIAlnum(c) {
			t.Errorf("%c should not be alnum", c)
		}
	}
}

func TestBatchClientTokenIsStableAndIdentitySensitive(t *testing.T) {
	a := batchClientToken(batchJob("j", "acme"))
	b := batchClientToken(batchJob("j", "acme"))
	if a != b {
		t.Error("client token must be stable for the same spec (idempotency)")
	}
	c := batchClientToken(batchJob("other", "acme"))
	if a == c {
		t.Error("client token must differ for a different job identity")
	}
}

func TestBatchTimeoutHoursAndInvocationTypeDefaults(t *testing.T) {
	bj := batchJob("j", "acme")
	if batchTimeoutHours(bj) != 24 {
		t.Errorf("default timeout: got %d want 24", batchTimeoutHours(bj))
	}
	if batchInvocationType(bj) != "InvokeModel" {
		t.Errorf("default invocation type: got %q want InvokeModel", batchInvocationType(bj))
	}
	bj.Spec.TimeoutHours = 6
	bj.Spec.ModelInvocationType = "Converse"
	if batchTimeoutHours(bj) != 6 || batchInvocationType(bj) != "Converse" {
		t.Error("explicit timeout/invocation-type must win over the defaults")
	}
}

func TestPtrTimeNow(t *testing.T) {
	if ptrTimeNow() == nil {
		t.Error("ptrTimeNow must return a non-nil time")
	}
}
