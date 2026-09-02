/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// Four Platform spec fields are copied into a generated authorization artifact,
// so the apiserver's marker is what stands between a tenant-supplied string and
// a grant:
//
//	spec.identity.allowedModels     -> the NotResource list of a Deny statement
//	                                   (platform_model_scoping.go)
//	spec.identity.extraPolicyArns   -> the PolicyArn of AttachRolePolicy on the
//	                                   tenant role (platform_iam.go)
//	spec.identity.directSecretReads -> the Resource list of the tenant-secrets
//	                                   inline policy (platform_tenant_secrets_policy.go)
//	spec.attribution.operators      -> the resourceNames of an impersonate
//	                                   ClusterRole (platform_rbac.go)
//
// Three of the four expansions re-validate nothing: reconcileManagedPolicies,
// tenantSecretsPolicyDoc and ensureOperatorImpersonateRBAC take what they are
// given. A Go-level test of those functions therefore cannot see the boundary —
// it exercises code that already trusts its input. So these tests read the
// marker out of the GENERATED CRD, which is the artifact the apiserver
// enforces, and assert on what it admits and refuses.
//
// Reading the rendered schema rather than a Go constant is the point. A
// constant compared against itself holds for whatever the constant says; the
// question is whether the marker that shipped rejects the input that inverts
// the control.
//
// allowedModels is the exception, and the tests below cover both halves: the
// expansion re-derives the cross-region prefix and matches the remainder
// against modelIDPattern, because the marker's engine cannot express the whole
// grammar and because an installed CRD can be older than the operator reading
// it. What the marker admits and what the expansion mints are separate
// assertions here for that reason.
//
// Two vocabularies back those markers and are written twice — the geo
// alternation inside the allowedModels pattern against
// inferenceProfileGeoPrefixes, and the allowedModelFamilies enum against
// modelFamilyExpansions. Each pair is asserted equal in both directions; the
// build binds them nowhere else.

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

// shippedModelIDs is the corpus the truncation class is generated from: the
// Bedrock model and inference-profile IDs this repo names, in every geo form a
// Platform is likely to declare. It is data rather than a case list — the tests
// below derive what the marker must refuse by walking prefixes of these, so
// adding a member widens the class instead of adding one case.
var shippedModelIDs = []string{
	"anthropic.claude-sonnet-4-6",
	"anthropic.claude-sonnet-5",
	"anthropic.claude-opus-5",
	"anthropic.claude-opus-4-8",
	"anthropic.claude-opus-4-6-v1",
	"anthropic.claude-haiku-4-5-v1:0",
	"anthropic.claude-haiku-4-5-20251001-v1:0",
	"anthropic.claude-3-5-sonnet-20241022-v2:0",
	"us.anthropic.claude-sonnet-4-6-v1:0",
	"us.anthropic.claude-sonnet-5",
	"us.anthropic.claude-haiku-4-5-20251001-v1:0",
	"eu.anthropic.claude-sonnet-4-6-v1:0",
	"apac.anthropic.claude-sonnet-4-6-v1:0",
	"jp.anthropic.claude-sonnet-4-6",
	"au.anthropic.claude-sonnet-4-6",
	"us-gov.anthropic.claude-sonnet-4-6-v1:0",
	"global.anthropic.claude-opus-4-6-v1",
	"amazon.nova-lite-v1:0",
	"amazon.nova-micro-v1:0",
	"us.amazon.nova-pro-v1:0",
	"amazon.titan-embed-text-v2",
	"amazon.titan-embed-text-v2:0",
	"meta.llama3-70b",
	"meta.llama3-1-70b-instruct-v1:0",
	"mistral.mistral-large-2407-v1:0",
	"cohere.command-r-plus-v1:0",
	"cohere.command-r-v1:0",
	"stability.stable-diffusion-xl-v1",
}

// craftedModelEntries are the shapes an author would write to widen the Deny
// rather than to name a model. Each is a member of a class the corpus prefixes
// do not reach: a metacharacter the expansion would carry into the ARN, and the
// two parses of a cross-region prefix disagreeing.
var craftedModelEntries = []string{
	"anthropic.claude-*",
	"anthropic.claude*",
	"us.anthropic.*",
	"apac.claude-5",
	"global.x-1",
	"us-gov.a-1",
	"a.b",
	"us.a",
	"eu.a",
	"jp.a",
	"au.a",
}

// modelIDStem returns the part of a model ID that precedes its version: the
// vendor and the name tokens up to, but not including, the first version token
// — "anthropic.claude-sonnet" out of "anthropic.claude-sonnet-4-6",
// "amazon.nova-lite" out of "amazon.nova-lite-v1:0".
//
// The stem is where the class boundary sits. The expansion appends a star to
// whatever it is given, so an entry that stops at or before the stem covers
// every model of that name whatever its version — a family prefix written
// without a star. An entry that reaches into the version token is a narrower
// claim about one version lineage, which is what the trailing star is for.
func modelIDStem(t *testing.T, id string) string {
	t.Helper()
	geo := geoPrefix(id)
	body := strings.TrimPrefix(id, geo)
	dot := strings.Index(body, ".")
	if dot < 0 {
		t.Fatalf("corpus entry %q carries no vendor segment", id)
	}
	vendor, name := body[:dot], body[dot+1:]
	tokens := strings.Split(name, "-")
	for i, tok := range tokens {
		if isVersionToken(tok) {
			if i == 0 {
				t.Fatalf("corpus entry %q opens its name with a version token", id)
			}
			return geo + vendor + "." + strings.Join(tokens[:i], "-")
		}
	}
	t.Fatalf("corpus entry %q carries no version token; the corpus holds whole model IDs", id)
	return ""
}

// isVersionToken reports whether a hyphen-separated name token opens a version:
// a digit, or v followed by a digit. It is what separates a model from the
// family prefix above it — "claude-sonnet-4-6" versions at "4", "nova-lite-v1:0"
// at "v1".
func isVersionToken(tok string) bool {
	if i := strings.Index(tok, ":"); i >= 0 {
		tok = tok[:i]
	}
	if tok == "" {
		return false
	}
	if tok[0] >= '0' && tok[0] <= '9' {
		return true
	}
	return len(tok) > 1 && tok[0] == 'v' && tok[1] >= '0' && tok[1] <= '9'
}

func TestAllowedModelsPatternRefusesEveryTruncationBelowAVersion(t *testing.T) {
	re := itemPattern(t, crdSchemaProps(t, "spec", "identity", "allowedModels"), "spec.identity.allowedModels")

	// Refusing the star form covers one member of the class. The expansion
	// appends the star itself, so an entry that stops short of a whole model ID
	// carries no metacharacter and widens the Deny anyway: "anthropic.claude"
	// reaches IAM as anthropic.claude*. The refusals below are generated from
	// the corpus rather than listed, because a list is what covered one member.
	for _, id := range shippedModelIDs {
		if !re.MatchString(id) {
			t.Errorf("allowedModels refuses %q, which is a model id this repo names", id)
			continue
		}
		// Every prefix up to and including the hyphen that opens the version is
		// a family prefix, and the marker must refuse all of them.
		stem := modelIDStem(t, id)
		for k := 1; k <= len(stem)+1; k++ {
			if re.MatchString(id[:k]) {
				t.Errorf("allowedModels admits %q, a truncation of %q that stops at or before its name; the expansion appends a star, so the entry excludes every version of that model from the Deny", id[:k], id)
			}
		}
	}

	// The crafted entries need not all be refused HERE. The marker is compiled
	// by RE2, which has no negative lookahead, so it cannot stop the longer geo
	// tokens satisfying its own vendor class. What must hold is that no crafted
	// entry ever reaches an ARN — refused at admission, or refused by the
	// expansion re-deriving the prefix from one vocabulary.
	for _, in := range craftedModelEntries {
		if !re.MatchString(in) {
			continue
		}
		if _, err := expandModelResources(platformv1alpha1.IdentitySpec{AllowedModels: []string{in}}, prodScope); err == nil {
			t.Errorf("allowedModels admits %q and the expansion accepts it; it names no model and its star reaches models nobody declared", in)
		}
	}
}

// isWholeModelID reports whether an entry parses as a model ID under the one geo
// vocabulary the expansion uses. It is the check expandModelResources runs;
// the tests call it to say which of the crafted entries the CRD marker is
// allowed to admit and the reconciler must still refuse.
func isWholeModelID(entry string) bool {
	return modelIDPattern.MatchString(strings.TrimPrefix(entry, geoPrefix(entry)))
}

func TestEveryModelResourceARNCarriesAWholeModelID(t *testing.T) {
	re := itemPattern(t, crdSchemaProps(t, "spec", "identity", "allowedModels"), "spec.identity.allowedModels")

	// The property that matters is not what admission accepts but what reaches
	// the policy document. Every ARN the expansion mints from an allowedModels
	// entry must carry a whole model ID under its trailing star; an ARN whose ID
	// prefix is a vendor, a family or a single letter excludes from the Deny
	// every model beneath it.
	candidates := append([]string{}, craftedModelEntries...)
	for _, id := range shippedModelIDs {
		for k := 1; k <= len(id); k++ {
			candidates = append(candidates, id[:k])
		}
	}

	for _, entry := range candidates {
		if !re.MatchString(entry) {
			continue // refused at admission; it never reaches the expansion
		}
		got, err := expandModelResources(platformv1alpha1.IdentitySpec{AllowedModels: []string{entry}}, prodScope)
		if err != nil {
			continue // refused at the point of use; nothing reaches the policy
		}
		for _, arn := range got {
			prefix := strings.TrimSuffix(arn[strings.LastIndex(arn, "/")+1:], "*")
			if !isWholeModelID(prefix) {
				t.Errorf("entry %q is admitted and expands to %s; the id prefix %q is not a whole model id, so the trailing star reaches models the entry does not name", entry, arn, prefix)
			}
		}
	}
}

func TestTheAlphabetBehindACrossRegionPrefixCannotEmptyTheDeny(t *testing.T) {
	re := itemPattern(t, crdSchemaProps(t, "spec", "identity", "allowedModels"), "spec.identity.allowedModels")

	// The total inversion the field is bounded against, written without a
	// metacharacter. Each "us.<letter>" is read by the expansion as the
	// cross-region prefix "us." over the model "<letter>", minting
	// foundation-model/<letter>*; the alphabet is 26 entries and MaxItems is 32,
	// so their union excludes every foundation model from the Deny and leaves
	// the tenant baseline's wildcard Allow governing unopposed.
	alphabet := make([]string, 0, 26)
	for c := byte('a'); c <= 'z'; c++ {
		alphabet = append(alphabet, "us."+string(c))
	}
	for _, in := range alphabet {
		if re.MatchString(in) {
			t.Errorf("allowedModels admits %q; twenty-six of these fit inside MaxItems and together exclude every foundation model from the Deny", in)
		}
	}
	if _, err := expandModelResources(platformv1alpha1.IdentitySpec{AllowedModels: alphabet}, prodScope); err == nil {
		t.Error("the single-letter alphabet expanded without error; their union excludes every foundation model from the Deny")
	}
}

func TestGeoPrefixVocabularyIsBoundToTheCRDPattern(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "identity", "allowedModels")
	re := itemPattern(t, prop, "spec.identity.allowedModels")
	pattern, _ := prop["items"].(map[string]any)["pattern"].(string)

	// inferenceProfileGeoPrefixes and the marker's leading alternation are two
	// hand-written copies of one vocabulary. A prefix in the Go copy alone is
	// admitted nowhere; a prefix in the marker alone is admitted and then
	// expanded as a bare model ID, which drops the profile ARN the entry needs.
	// Reading the alternation out of the generated CRD is what binds them.
	alt := regexp.MustCompile(`^\^\(\(([a-z|-]+)\)\\\.\)\?`).FindStringSubmatch(pattern)
	if alt == nil {
		t.Fatalf("the geo alternation cannot be read out of the generated pattern %q; it is the only thing binding it to inferenceProfileGeoPrefixes", pattern)
	}
	fromCRD := map[string]bool{}
	for _, tok := range strings.Split(alt[1], "|") {
		fromCRD[tok+"."] = true
	}
	fromGo := map[string]bool{}
	for _, p := range inferenceProfileGeoPrefixes {
		fromGo[p] = true
	}
	for p := range fromGo {
		if !fromCRD[p] {
			t.Errorf("inferenceProfileGeoPrefixes carries %q and the CRD pattern refuses it; an entry behind that prefix cannot be written", p)
		}
	}
	for p := range fromCRD {
		if !fromGo[p] {
			t.Errorf("the CRD pattern admits the geo prefix %q and inferenceProfileGeoPrefixes does not; an entry behind it expands as a bare model id and never yields its inference-profile ARN", p)
		}
	}

	// Each entry expands to BOTH ARNs: the profile the entry names, and the
	// foundation model beneath it that the profile fans invocations out to.
	// A profile ARN alone denies the invoke the profile performs.
	const model = "anthropic.claude-sonnet-4-6"
	for _, p := range inferenceProfileGeoPrefixes {
		entry := p + model
		if !re.MatchString(entry) {
			t.Errorf("allowedModels refuses %q, a cross-region form of a model this repo names", entry)
			continue
		}
		got, err := expandModelResources(platformv1alpha1.IdentitySpec{AllowedModels: []string{entry}}, prodScope)
		if err != nil {
			t.Errorf("expandModelResources(%q): %v", entry, err)
			continue
		}
		want := map[string]bool{
			"arn:aws:bedrock:us-west-2:123456789012:inference-profile/" + entry + "*": false,
			"arn:aws:bedrock:*::foundation-model/" + model + "*":                      false,
		}
		for _, arn := range got {
			want[arn] = true
		}
		for arn, seen := range want {
			if !seen {
				t.Errorf("%q expands to %v, missing %s", entry, got, arn)
			}
		}
	}
}

func TestModelFamilyExpansionsMatchTheCRDEnum(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "identity", "allowedModelFamilies")
	items, ok := prop["items"].(map[string]any)
	if !ok {
		t.Fatal("spec.identity.allowedModelFamilies declares no items schema")
	}
	raw, ok := items["enum"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatal("spec.identity.allowedModelFamilies carries no items enum — every entry reaches the family expansion table unvalidated")
	}

	// The enum and modelFamilyExpansions are the same vocabulary written twice.
	// A family in the enum alone is admitted and then fails the reconcile; a
	// family in the table alone is unreachable code claiming a grant nobody can
	// declare.
	fromCRD := map[string]bool{}
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("allowedModelFamilies enum holds a non-string %v", v)
		}
		fromCRD[s] = true
	}
	for f := range modelFamilyExpansions {
		if !fromCRD[f] {
			t.Errorf("modelFamilyExpansions expands %q and the CRD enum refuses it; no Platform can declare that family", f)
		}
	}
	for f := range fromCRD {
		if _, ok := modelFamilyExpansions[f]; !ok {
			t.Errorf("the CRD enum admits the family %q and modelFamilyExpansions has no entry for it; a Platform declaring it is admitted and then fails every reconcile", f)
		}
	}
}

func TestExtraPolicyArnsPatternAdmitsOnlyManagedPolicyArns(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "identity", "extraPolicyArns")
	re := itemPattern(t, prop, "spec.identity.extraPolicyArns")
	if _, ok := prop["maxItems"]; !ok {
		t.Error("spec.identity.extraPolicyArns declares no maxItems; the operator attaches the baseline itself and AWS caps managed policies per role at 10")
	}
	if _, ok := prop["items"].(map[string]any)["maxLength"]; !ok {
		t.Error("spec.identity.extraPolicyArns items declare no maxLength")
	}

	// Every entry is handed to AttachRolePolicy against the tenant role, so the
	// marker is what decides which ARNs the operator will attach. The refusals
	// are generated by mutating each segment of a real managed-policy ARN, so
	// the class is the ARN grammar rather than a list of typos someone thought
	// of.
	admit := []string{
		"arn:aws:iam::aws:policy/ReadOnlyAccess",
		"arn:aws:iam::aws:policy/EksAgentBaseline",
		"arn:aws:iam::123456789012:policy/tenant-boundary",
		"arn:aws:iam::123456789012:policy/eks-agent-platform/conformance-tenant-baseline",
		"arn:aws-us-gov:iam::123456789012:policy/tenant-boundary",
	}
	for _, in := range admit {
		if !re.MatchString(in) {
			t.Errorf("extraPolicyArns refuses %q, which is a managed policy ARN the operator must be able to attach", in)
		}
	}
	for _, base := range admit {
		seg := strings.Split(base, ":")
		for i := range seg {
			mutated := append([]string{}, seg...)
			mutated[i] = "*"
			if in := strings.Join(mutated, ":"); re.MatchString(in) {
				t.Errorf("extraPolicyArns admits %q — a wildcard segment reaches AttachRolePolicy", in)
			}
		}
		for _, swap := range []struct{ from, to string }{
			{":iam:", ":s3:"},
			{":iam:", ":sts:"},
			{":policy/", ":role/"},
			{":policy/", ":user/"},
			{":policy/", ":instance-profile/"},
		} {
			if in := strings.Replace(base, swap.from, swap.to, 1); in != base && re.MatchString(in) {
				t.Errorf("extraPolicyArns admits %q — the operator attaches only managed policies, and this names a %s", in, strings.Trim(swap.to, ":/"))
			}
		}
		if in := strings.Replace(base, "iam::", "iam:us-east-1:", 1); re.MatchString(in) {
			t.Errorf("extraPolicyArns admits %q — IAM is global and carries no region", in)
		}
	}
	for _, in := range []string{"", " ", "*", "ReadOnlyAccess", "arn:aws:iam::12345:policy/short-account", "arn:aws:iam:::policy/no-account"} {
		if re.MatchString(in) {
			t.Errorf("extraPolicyArns admits %q, which is not a managed policy ARN", in)
		}
	}
}

func TestDirectSecretReadsPatternCannotEscapeTheTenantPrefix(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "identity", "directSecretReads")
	re := itemPattern(t, prop, "spec.identity.directSecretReads")
	if _, ok := prop["maxItems"]; !ok {
		t.Error("spec.identity.directSecretReads declares no maxItems; every entry renders one Resource ARN into an inline policy IAM caps at 10,240 characters")
	}
	if _, ok := prop["items"].(map[string]any)["maxLength"]; !ok {
		t.Error("spec.identity.directSecretReads items declare no maxLength")
	}

	// Each entry is rendered into
	// arn:<partition>:secretsmanager:<region>:<account>:secret:<platform>/<env>/<entry>-*
	// (platform_tenant_secrets_policy.go tenantSecretsPolicyDoc). The tenant
	// prefix is the isolation boundary, so an entry must not be able to carry a
	// wildcard past it, to close the ARN and open another, or to be empty —
	// which would grant the whole <platform>/<env>/ prefix.
	admit := []string{
		"grafana/oncall-webhook-hmac",
		"app-config",
		"db.password",
		"a",
	}
	for _, in := range admit {
		if !re.MatchString(in) {
			t.Errorf("directSecretReads refuses %q, which is a secret name under the tenant's own prefix", in)
		}
	}
	refuse := []string{
		"",
		" ",
		"*",
		"a*",
		"*/*",
		"a?",
		"/leading-slash",
		"-leading-hyphen",
		".leading-dot",
		"name with spaces",
		"name\twith\ttab",
		"a-*",
		"a:b",
		"arn:aws:secretsmanager:us-west-2:123456789012:secret:other/prod/x",
	}
	for _, in := range refuse {
		if re.MatchString(in) {
			t.Errorf("directSecretReads admits %q — it lands inside a Resource ARN on the tenant-secrets policy", in)
		}
	}
}

func TestModelIDPatternMirrorsTheCRDMarker(t *testing.T) {
	prop := crdSchemaProps(t, "spec", "identity", "allowedModels")
	pattern, _ := prop["items"].(map[string]any)["pattern"].(string)

	// modelIDPattern is the marker's grammar written a second time, in Go,
	// because the expansion must not trust a value the installed CRD may never
	// have checked. Two copies of one grammar drift, and this one drifts
	// silently in the direction that matters: relax the Go copy alone and every
	// test still passes, because the CRD keeps refusing what the Go copy has
	// stopped refusing — until the CRD in some cluster is the older one.
	//
	// The marker is the geo alternation followed by that grammar, so stripping
	// the alternation off the front is what leaves the two comparable.
	tail := regexp.MustCompile(`^\^\(\([a-z|-]+\)\\\.\)\?`).ReplaceAllString(pattern, "^")
	if tail == pattern {
		t.Fatalf("the geo alternation cannot be stripped off %q, so the marker and modelIDPattern cannot be compared", pattern)
	}
	if tail != modelIDPattern.String() {
		t.Errorf("the CRD marker and modelIDPattern have diverged:\n  CRD (geo alternation stripped): %s\n  modelIDPattern:                %s", tail, modelIDPattern.String())
	}
}

func TestTheDeniedActionsAreNamedWhereTheyAreDocumented(t *testing.T) {
	// IdentitySpec's doc comment names the actions the scoping policy denies,
	// and crd-ref-docs republishes it as the field reference a Platform author
	// reads. modelInvokeActions is what the Deny's Action array actually holds.
	// The two drifted once already, in the direction that reads as safe: the
	// prose named four while the policy denied five, so the action the wildcard
	// grant reaches — InvokeModelWithBidirectionalStream — was scoped in the
	// document and absent from every description of it.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", "api", "platform", "v1alpha1", "platform_types.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse platform_types.go: %v", err)
	}
	var doc string
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Doc == nil {
			return true
		}
		for _, sp := range gd.Specs {
			if ts, ok := sp.(*ast.TypeSpec); ok && ts.Name.Name == "IdentitySpec" {
				doc = gd.Doc.Text()
			}
		}
		return true
	})
	if doc == "" {
		t.Fatal("IdentitySpec carries no doc comment — the actions it documents are the only description of what the scoping policy denies")
	}
	for _, action := range modelInvokeActions {
		if !strings.Contains(doc, strings.TrimPrefix(action, "bedrock:")) {
			t.Errorf("the scoping policy denies %s and IdentitySpec's doc comment does not name it; the comment is what a Platform author reads to learn which actions are scoped", action)
		}
	}
}
