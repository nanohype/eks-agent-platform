/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// modelScopingPolicyName is the inline policy the operator reconciles onto
// every tenant (and attribution session) role. It narrows the tenant
// baseline's broad bedrock:InvokeModel* grant (landing-zone agent-iam attaches
// the baseline with Resource "*") to exactly the models the Platform's
// spec.identity declares, via an explicit Deny on everything outside the
// allowed set. Deny-only by design:
//
//   - explicit Deny overrides the baseline Allow, so the effective invoke
//     surface is spec.identity's expansion — never the wildcard;
//   - an empty spec degrades to Deny-on-everything (deny-by-default);
//   - the kill-switch lever is untouched: detaching the baseline managed
//     policy still zeroes ALL invoke instantly, because this policy grants
//     nothing on its own.
const modelScopingPolicyName = "bedrock-model-scoping"

// modelInvokeActions are the Bedrock model data-plane actions the scoping
// policy clamps. Deliberately excludes bedrock:ApplyGuardrail /
// bedrock:GetGuardrail — those authorize against guardrail ARNs, which are
// never in the allowed-model set and must not become collateral of the
// NotResource deny.
var modelInvokeActions = []string{
	"bedrock:Converse",
	"bedrock:ConverseStream",
	"bedrock:InvokeModel",
	"bedrock:InvokeModelWithResponseStream",
}

// modelFamilyExpansion maps one spec.identity.allowedModelFamilies value to
// the Bedrock model-ID prefixes it implies. FoundationModelPrefix matches
// under foundation-model/; InferenceProfilePrefix matches under
// inference-profile/ and is empty for families Bedrock publishes no
// cross-region (`us.`) inference profiles for.
type modelFamilyExpansion struct {
	FoundationModelPrefix  string
	InferenceProfilePrefix string
}

// modelFamilyExpansions is the authoritative family → ARN-prefix table. Keys
// mirror the ModelFamily vocabulary shared with @eks-agent/core (schemas.ts)
// and the CRD enum on IdentitySpec.AllowedModelFamilies. The ARN shapes match
// how landing-zone's *-platform IRSA components write them: foundation-model
// ARNs region-wildcarded with no account (cross-region inference profiles fan
// out to foundation models in sibling regions), inference-profile ARNs
// region+account scoped.
var modelFamilyExpansions = map[string]modelFamilyExpansion{
	"anthropic":    {FoundationModelPrefix: "anthropic.", InferenceProfilePrefix: "us.anthropic."},
	"amazon-nova":  {FoundationModelPrefix: "amazon.nova-", InferenceProfilePrefix: "us.amazon.nova-"},
	"amazon-titan": {FoundationModelPrefix: "amazon.titan-"},
	"meta":         {FoundationModelPrefix: "meta.", InferenceProfilePrefix: "us.meta."},
	"mistral":      {FoundationModelPrefix: "mistral.", InferenceProfilePrefix: "us.mistral."},
	"cohere":       {FoundationModelPrefix: "cohere."},
	"stability":    {FoundationModelPrefix: "stability."},
}

// inferenceProfileGeoPrefixes are the geo prefixes Bedrock uses for
// cross-region inference-profile IDs. An allowedModels entry starting with
// one of these is a profile ID (e.g. us.anthropic.claude-sonnet-4-6-v1:0);
// anything else is a foundation-model ID.
var inferenceProfileGeoPrefixes = []string{"us.", "eu.", "apac.", "us-gov.", "jp.", "au.", "global."}

// arnScope carries the partition/region/account triple the expansion needs to
// mint concrete ARN patterns. Zero values wildcard the corresponding field so
// dev/test paths (no real role ARN, no configured region) stay functional.
type arnScope struct {
	Partition string
	Region    string
	AccountID string
}

func (s arnScope) partition() string { return orDefault(s.Partition, "aws") }
func (s arnScope) region() string    { return orDefault(s.Region, "*") }
func (s arnScope) account() string   { return orDefault(s.AccountID, "*") }

// foundationModelARN returns the foundation-model ARN pattern for a model-ID
// prefix. Region is wildcarded (cross-region inference profiles authorize
// against foundation-model ARNs in sibling regions) and the account field is
// empty — foundation models are AWS-owned resources.
func (s arnScope) foundationModelARN(prefix string) string {
	return fmt.Sprintf("arn:%s:bedrock:*::foundation-model/%s", s.partition(), wildcardSuffix(prefix))
}

// inferenceProfileARN returns the inference-profile ARN pattern for a
// profile-ID prefix. Profiles live in the caller's account + region.
func (s arnScope) inferenceProfileARN(prefix string) string {
	return fmt.Sprintf("arn:%s:bedrock:%s:%s:inference-profile/%s", s.partition(), s.region(), s.account(), wildcardSuffix(prefix))
}

// wildcardSuffix appends a trailing * so an ID pattern covers version
// suffixes (":0", "-v1:0") and dated snapshots, unless the caller already
// wrote a wildcard.
func wildcardSuffix(id string) string {
	if strings.HasSuffix(id, "*") {
		return id
	}
	return id + "*"
}

// arnScopeFromRole derives partition + account from an IAM role ARN
// (arn:<partition>:iam::<account>:role/...) and takes the region from the
// operator's config. Any part that can't be resolved wildcards.
func arnScopeFromRole(roleARN, region string) arnScope {
	scope := arnScope{Region: region}
	parts := strings.Split(roleARN, ":")
	if len(parts) >= 5 {
		scope.Partition = parts[1]
		scope.AccountID = parts[4]
	}
	return scope
}

// expandModelResources turns spec.identity's model declaration into the
// Bedrock ARN patterns the tenant role may invoke:
//
//   - AllowedModels entries expand individually. A geo-prefixed entry
//     (us.…, eu.…, apac.…) is a cross-region inference-profile ID and yields
//     its profile ARN plus the underlying foundation-model ARN (the profile
//     fans invocations out to foundation models, so both are required). A
//     bare model ID yields its foundation-model ARN plus the `us.` profile
//     ARN it implies.
//   - AllowedModelFamilies entries expand through modelFamilyExpansions:
//     every family yields its foundation-model prefix ARN, and families with
//     cross-region profiles also yield the `us.` profile prefix ARN.
//
// AllowedModels and AllowedModelFamilies are mutually exclusive at admission
// (CEL rule on IdentitySpec); explicit models therefore always scope tighter
// than a family. An empty spec returns nil — the caller renders that as a
// deny-everything policy. Unknown families are an error, not a silent skip:
// silently dropping a family would widen nothing but would let a typo ship a
// deny-by-default Platform whose owner believes models were granted.
func expandModelResources(identity platformv1alpha1.IdentitySpec, scope arnScope) ([]string, error) {
	set := map[string]struct{}{}

	if len(identity.AllowedModels) > 0 {
		for _, m := range identity.AllowedModels {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			if geo := geoPrefix(m); geo != "" {
				set[scope.inferenceProfileARN(m)] = struct{}{}
				set[scope.foundationModelARN(strings.TrimPrefix(m, geo))] = struct{}{}
				continue
			}
			set[scope.foundationModelARN(m)] = struct{}{}
			set[scope.inferenceProfileARN("us."+m)] = struct{}{}
		}
	} else {
		for _, f := range identity.AllowedModelFamilies {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			exp, ok := modelFamilyExpansions[f]
			if !ok {
				return nil, fmt.Errorf("unknown model family %q; known families: %s", f, strings.Join(knownModelFamilies(), ", "))
			}
			set[scope.foundationModelARN(exp.FoundationModelPrefix)] = struct{}{}
			if exp.InferenceProfilePrefix != "" {
				set[scope.inferenceProfileARN(exp.InferenceProfilePrefix)] = struct{}{}
			}
		}
	}

	if len(set) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(set))
	for arn := range set {
		out = append(out, arn)
	}
	sort.Strings(out)
	return out, nil
}

// geoPrefix returns the matched cross-region geo prefix ("us.", "eu.", …) of
// an allowedModels entry, or "" when the entry is a plain model ID.
func geoPrefix(id string) string {
	for _, p := range inferenceProfileGeoPrefixes {
		if strings.HasPrefix(id, p) {
			return p
		}
	}
	return ""
}

// knownModelFamilies lists the family vocabulary sorted, for error messages.
func knownModelFamilies() []string {
	out := make([]string, 0, len(modelFamilyExpansions))
	for f := range modelFamilyExpansions {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// policyStatement / policyDocument are minimal ordered types so the rendered
// JSON is deterministic — the idempotency compare in
// ensureModelScopingPolicy depends on stable serialization.
type policyStatement struct {
	Sid         string   `json:"Sid"`
	Effect      string   `json:"Effect"`
	Action      []string `json:"Action"`
	Resource    []string `json:"Resource,omitempty"`
	NotResource []string `json:"NotResource,omitempty"`
}

type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

// modelScopingPolicyDoc renders the scoping document for an expanded
// resource set. Non-empty: deny the invoke actions on everything OUTSIDE the
// set (the baseline's wildcard Allow is thereby narrowed to exactly the
// set). Empty: deny the invoke actions everywhere — a Platform that declares
// no models can invoke none, regardless of what the baseline grants.
func modelScopingPolicyDoc(resources []string) (string, error) {
	stmt := policyStatement{
		Sid:      "DenyAllBedrockInvoke",
		Effect:   "Deny",
		Action:   modelInvokeActions,
		Resource: []string{"*"},
	}
	if len(resources) > 0 {
		stmt = policyStatement{
			Sid:         "DenyUnscopedBedrockInvoke",
			Effect:      "Deny",
			Action:      modelInvokeActions,
			NotResource: resources,
		}
	}
	b, err := json.Marshal(policyDocument{Version: "2012-10-17", Statement: []policyStatement{stmt}})
	if err != nil {
		return "", fmt.Errorf("marshal model scoping policy: %w", err)
	}
	return string(b), nil
}

// ensureModelScopingPolicy reconciles the bedrock-model-scoping inline policy
// on a role. Idempotent: reads the current document via GetRolePolicy and
// writes only on drift, so a converged Platform costs one read per reconcile.
// Callers MUST NOT invoke this on a suspended role — ensureIamRole's
// suspension short-circuit returns before reaching it, keeping the operator
// observe-only while the kill-switch marker is set.
func (r *PlatformReconciler) ensureModelScopingPolicy(ctx context.Context, roleName, roleARN string, identity platformv1alpha1.IdentitySpec, cfg IAMConfig) error {
	if r.IAM == nil {
		return nil
	}
	resources, err := expandModelResources(identity, arnScopeFromRole(roleARN, cfg.Region))
	if err != nil {
		return fmt.Errorf("expand allowed models for role %s: %w", roleName, err)
	}
	desired, err := modelScopingPolicyDoc(resources)
	if err != nil {
		return err
	}

	getOut, getErr := r.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
		RoleName:   aws.String(roleName),
		PolicyName: aws.String(modelScopingPolicyName),
	})
	if getErr == nil && getOut != nil && policyDocEqual(aws.ToString(getOut.PolicyDocument), desired) {
		return nil
	}
	if getErr != nil && !isIAMNotFound(getErr) {
		return fmt.Errorf("iam GetRolePolicy %s/%s: %w", roleName, modelScopingPolicyName, getErr)
	}

	if _, err := r.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(modelScopingPolicyName),
		PolicyDocument: aws.String(desired),
	}); err != nil {
		return fmt.Errorf("iam PutRolePolicy %s/%s: %w", roleName, modelScopingPolicyName, err)
	}
	return nil
}

// policyDocEqual compares two policy documents structurally. IAM returns
// GetRolePolicy documents URL-encoded; decode, then compare the unmarshalled
// forms so key order and whitespace never force a spurious PutRolePolicy.
func policyDocEqual(current, desired string) bool {
	if decoded, err := url.QueryUnescape(current); err == nil {
		current = decoded
	}
	var a, b any
	if err := json.Unmarshal([]byte(current), &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(desired), &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// deleteInlinePolicies removes every inline policy from a role — IAM refuses
// DeleteRole while inline policies remain, so the finalizer path runs this
// between detaching managed policies and deleting the role. Tolerates
// NotFound at every step (role already gone, policy raced away) so re-runs
// are safe no-ops.
func (r *PlatformReconciler) deleteInlinePolicies(ctx context.Context, roleName string) error {
	var marker *string
	for {
		listOut, err := r.IAM.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{
			RoleName: aws.String(roleName),
			Marker:   marker,
		})
		if isIAMNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("iam ListRolePolicies %s: %w", roleName, err)
		}
		for _, name := range listOut.PolicyNames {
			if _, err := r.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
				RoleName:   aws.String(roleName),
				PolicyName: aws.String(name),
			}); err != nil && !isIAMNotFound(err) {
				return fmt.Errorf("iam DeleteRolePolicy %s/%s: %w", roleName, name, err)
			}
		}
		if !listOut.IsTruncated || listOut.Marker == nil {
			break
		}
		marker = listOut.Marker
	}
	return nil
}

// modelScopeConditionReason returns the machine-readable reason for the
// ModelAccessScoped status condition.
func modelScopeConditionReason(identity platformv1alpha1.IdentitySpec) string {
	if len(identity.AllowedModels) > 0 || len(identity.AllowedModelFamilies) > 0 {
		return "Scoped"
	}
	return "DenyByDefault"
}

// modelScopeConditionMessage renders the human-readable summary the
// ModelAccessScoped status condition carries.
func modelScopeConditionMessage(identity platformv1alpha1.IdentitySpec) string {
	if len(identity.AllowedModels) > 0 {
		return fmt.Sprintf("bedrock invoke scoped to models [%s]", strings.Join(identity.AllowedModels, ", "))
	}
	if len(identity.AllowedModelFamilies) > 0 {
		return fmt.Sprintf("bedrock invoke scoped to model families [%s]", strings.Join(identity.AllowedModelFamilies, ", "))
	}
	return "no allowed models declared; all bedrock invoke denied"
}
