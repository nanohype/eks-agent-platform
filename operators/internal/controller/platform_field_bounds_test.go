/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

// Two Platform spec fields are copied verbatim into a generated authorization
// decision, so the apiserver's pattern is the only thing standing between a
// tenant-supplied string and a grant:
//
//	spec.identity.allowedModels     -> the NotResource list of a Deny statement
//	                                   (platform_model_scoping.go)
//	spec.attribution.operators      -> the resourceNames of an impersonate
//	                                   ClusterRole (platform_rbac.go)
//
// Neither expansion re-validates: expandModelResources does TrimSpace and an
// empty-skip, ensureOperatorImpersonateRBAC does nothing at all. A Go-level
// test of those functions therefore cannot see the boundary — it exercises code
// that already trusts its input. So these tests read the pattern out of the
// GENERATED CRD, which is the artifact the apiserver actually enforces, and
// assert on what it admits and refuses.
//
// Reading the rendered schema rather than a Go constant is the point. A
// constant compared against itself holds for whatever the constant says; the
// question here is whether the marker that shipped rejects the input that
// inverts the control.

// crdSchemaProps walks the generated Platform CRD to the named spec property.
func crdSchemaProps(t *testing.T, path ...string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "platform.nanohype.dev_platforms.yaml"))
	if err != nil {
		t.Fatalf("read generated Platform CRD: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse generated Platform CRD: %v", err)
	}

	versions, _ := doc["spec"].(map[string]any)["versions"].([]any)
	var node map[string]any
	for _, v := range versions {
		vm, _ := v.(map[string]any)
		if vm["name"] != "v1alpha1" {
			continue
		}
		node, _ = vm["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	}
	if node == nil {
		t.Fatal("generated Platform CRD declares no v1alpha1 openAPIV3Schema")
	}

	for _, key := range path {
		props, ok := node["properties"].(map[string]any)
		if !ok {
			t.Fatalf("no properties at %q while walking to %v", key, path)
		}
		node, ok = props[key].(map[string]any)
		if !ok {
			t.Fatalf("generated CRD has no property %q (walking %v)", key, path)
		}
	}
	return node
}

// itemPattern returns the compiled items.pattern of an array property, failing
// when the marker is absent. An absent pattern is the defect these tests exist
// to catch, so it is a failure rather than a skip.
func itemPattern(t *testing.T, prop map[string]any, field string) *regexp.Regexp {
	t.Helper()
	items, ok := prop["items"].(map[string]any)
	if !ok {
		t.Fatalf("%s declares no items schema", field)
	}
	pat, ok := items["pattern"].(string)
	if !ok || pat == "" {
		t.Fatalf("%s carries no items.pattern — every entry reaches a generated grant unvalidated", field)
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		t.Fatalf("%s items.pattern %q does not compile: %v", field, pat, err)
	}
	return re
}

func TestAllowedModelsPatternCannotWidenTheDeny(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "identity", "allowedModels")
	re := itemPattern(t, prop, "spec.identity.allowedModels")

	// A NotResource entry is an EXCLUSION from the Deny, so anything that
	// matches more than one literal model widens the tenant's reach. "*" is the
	// total inversion: it expands to foundation-model/* plus
	// inference-profile/us.*, which excludes every model from the Deny and
	// leaves the baseline's wildcard Allow governing unopposed.
	refuse := []string{
		"*",
		"?",
		"anthropic.*",
		"anthropic.claude*",
		"us.anthropic.*",
		".",
		"..",
		"",
		" ",
		"anthropic.claude-sonnet-4-6 ",  // trailing space survives no TrimSpace on the ARN side
		"anthropic.claude/../../../etc", // path traversal into the ARN
		"arn:aws:bedrock:*::foundation-model/*",
		"ANTHROPIC.CLAUDE-SONNET-4-6", // case-shifted, would not match the real id
	}
	for _, in := range refuse {
		if re.MatchString(in) {
			t.Errorf("allowedModels admits %q — it reaches NotResource and widens the Deny", in)
		}
	}

	// Every model id the repo actually ships has to keep working, or the marker
	// is a break rather than a bound.
	admit := []string{
		"anthropic.claude-sonnet-4-6",
		"anthropic.claude-sonnet-5",
		"anthropic.claude-opus-5",
		"us.anthropic.claude-sonnet-4-6-v1:0",
		"us.anthropic.claude-haiku-4-5-20251001-v1:0",
		"eu.anthropic.claude-sonnet-4-6-v1:0",
		"apac.anthropic.claude-sonnet-4-6-v1:0",
		"us-gov.anthropic.claude-sonnet-4-6-v1:0",
		"global.anthropic.claude-sonnet-4-6",
		"amazon.nova-lite-v1:0",
		"us.amazon.nova-pro-v1:0",
		"amazon.titan-embed-text-v2:0",
		"meta.llama3-1-70b-instruct-v1:0",
		"mistral.mistral-large-2407-v1:0",
		"cohere.command-r-plus-v1:0",
	}
	for _, in := range admit {
		if !re.MatchString(in) {
			t.Errorf("allowedModels refuses %q, which is a model id this repo ships", in)
		}
	}
}

func TestAllowedModelsIsBoundedByCount(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "identity", "allowedModels")
	if _, ok := prop["maxItems"]; !ok {
		t.Error("spec.identity.allowedModels declares no maxItems; the expansion renders two ARNs per entry into an inline policy that IAM caps at 10,240 characters")
	}
	items, _ := prop["items"].(map[string]any)
	if _, ok := items["maxLength"]; !ok {
		t.Error("spec.identity.allowedModels items declare no maxLength")
	}
}

func TestAttributionOperatorsPatternCannotNameABuiltInPrincipal(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "attribution", "operators")
	re := itemPattern(t, prop, "spec.attribution.operators")

	// Every entry lands in the resourceNames of a ClusterRole granting
	// impersonate on core users, written by an operator that holds unrestricted
	// impersonate — so the apiserver's escalation-prevention check passes on
	// whatever this field carries. Kubernetes' own privileged principals are
	// colon-prefixed, so a character class without ':' cannot name one.
	refuse := []string{
		"system:admin",
		"system:masters",
		"system:kube-controller-manager",
		"system:serviceaccount:kube-system:default",
		"system:anonymous",
		"*",
		"",
		" ",
		"Operator@Example.com", // case-shifted: would not byte-match the RBAC subject
		"operator@example.com ",
		"operator",         // no domain: not an identity this binds to
		"operator@example", // no TLD
		"@example.com",
	}
	for _, in := range refuse {
		if re.MatchString(in) {
			t.Errorf("attribution.operators admits %q — it reaches an impersonate ClusterRole's resourceNames", in)
		}
	}

	admit := []string{
		"operator@example.com",
		"op@example.com",
		"ops@nanohype.dev",
		"first.last+tag@sub.example.co.uk",
		"a1@b2.io",
	}
	for _, in := range admit {
		if !re.MatchString(in) {
			t.Errorf("attribution.operators refuses %q, which is the canonical form the field documents", in)
		}
	}
}

func TestAttributionOperatorsIsBoundedByCount(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "attribution", "operators")
	if _, ok := prop["maxItems"]; !ok {
		t.Error("spec.attribution.operators declares no maxItems; every entry is also an sts:SourceIdentity condition value on a trust policy IAM caps at 2,048 characters")
	}
	items, _ := prop["items"].(map[string]any)
	if _, ok := items["maxLength"]; !ok {
		t.Error("spec.attribution.operators items declare no maxLength")
	}
}
