/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/nanohype/eks-agent-platform/operators/internal/operatorconfig"
)

// WHAT THE REQUIRED SET IS FOR
//
// A value is required when its absence makes the operator do something other
// than what the substrate specified, silently — no boundary on a tenant role, no
// baseline policy attached, no bucket policy written, a role created at a path
// this code picked. Absence is refused for those because the smaller grant would
// never announce itself.
//
// What that is NOT is "the substrate publishes it". A required value that
// reaches no consumer is an operator refusing to reconcile any tenant over
// something that reconciles nothing — and after the substrate wait landed, that
// is a pod that never becomes ready, reporting a key it does not use. The
// operator's own role ARN was in the set on those terms: published beside the
// others, decoded, and read by nothing in this binary.
//
// So the set is held to behaviour. Every required key is driven from a parameter
// store, through the same Load the operator runs at startup, through the same
// mapping that builds a reconciler's config — and the value has to come out the
// other end. A key that does not reach a consumer fails here.
//
// WHERE THIS STOPS, AND WHY
//
// The reconcilers do not import operatorconfig; that decoupling is deliberate
// and stated where IAMConfig is declared. So this asserts the route up to the
// config a reconciler is handed, and the controller package's own tests assert
// what happens after — TestEnsureIamRole_SetsPermissionsBoundaryOnCreate is the
// far side of the same route. Neither spans the boundary alone.
//
// WHAT NEITHER SIDE CHECKS, STATED RATHER THAN IMPLIED
//
// Nothing here checks the SHAPE of a value. A malformed ARN reaches AWS verbatim
// and AWS refuses it, per Platform, at reconcile. What a shape check would not
// catch is the case that actually costs something: a well-formed ARN naming the
// wrong policy, or a real bucket that is the wrong bucket. Both are accepted by
// AWS and by any pattern. A guard here would close the class AWS already closes
// and leave the class nothing closes, so there is no guard here.

const sentinel = "SENTINEL-VALUE"

// singleKeySSM serves one parameter under the cluster prefix.
type singleKeySSM struct{ params map[string]string }

func (s singleKeySSM) GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	panic("GetParameter is not on the startup path")
}

func (s singleKeySSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	prefix := aws.ToString(in.Path)
	out := &ssm.GetParametersByPathOutput{}
	for key, value := range s.params {
		out.Parameters = append(out.Parameters, ssmtypes.Parameter{
			Name: aws.String(prefix + key), Value: aws.String(value),
		})
	}
	return out, nil
}

func loadWith(t *testing.T, params map[string]string) *operatorconfig.Config {
	t.Helper()
	cfg, err := operatorconfig.Load(
		context.Background(), singleKeySSM{params: params}, "dev-analytics", "dev", "us-west-2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// reconcilerConfigs is everything a reconciler is handed that came from the
// substrate, flattened so a value can be looked for without naming the field it
// lands in — naming the field would make this a check that the mapping matches
// the mapping.
func reconcilerConfigs(cfg *operatorconfig.Config) []string {
	iam := iamConfigFrom(cfg, orgTags{Environment: "dev"})
	s3 := platformAWSConfigFrom(cfg, "dev")
	var out []string
	for _, v := range []any{iam, s3} {
		value := reflect.ValueOf(v)
		for i := 0; i < value.NumField(); i++ {
			if field := value.Field(i); field.Kind() == reflect.String {
				out = append(out, field.String())
			}
		}
	}
	return out
}

// TestEveryRequiredValueReachesAReconciler is the property.
func TestEveryRequiredValueReachesAReconciler(t *testing.T) {
	keys := operatorconfig.RequiredKeys()
	if len(keys) == 0 {
		t.Fatal("no key is required, so this asserts nothing and nothing stops the operator " +
			"reconciling with an empty substrate")
	}
	for _, key := range keys {
		cfg := loadWith(t, map[string]string{key: sentinel})
		carried := reconcilerConfigs(cfg)
		found := false
		for _, v := range carried {
			if v == sentinel {
				found = true
				break
			}
		}
		if !found {
			// Two things produce this, and the message names the observation
			// rather than picking one: the key has no consumer at all, or it has
			// one and the mapping above stopped carrying it. Either way the
			// operator refuses to reconcile any tenant without a value that then
			// changes nothing it does, so a substrate publishing everything else
			// leaves the pod not ready over it.
			t.Errorf("%s is required and its value reaches no reconciler config. Either nothing "+
				"consumes it — in which case take it out of the required set and record where it "+
				"is actually read — or a consumer exists and iamConfigFrom / platformAWSConfigFrom "+
				"no longer carries it across.", key)
		}
	}
}

// TestEveryRequiredValueIsMissedWhenAbsent is the other direction: the set has
// to be complete about what it refuses, not merely correct about what it names.
func TestEveryRequiredValueIsMissedWhenAbsent(t *testing.T) {
	full := map[string]string{}
	for _, key := range operatorconfig.RequiredKeys() {
		full[key] = sentinel
	}
	if missing := loadWith(t, full).Validate(); len(missing) != 0 {
		t.Fatalf("a config carrying every required key does not validate: %v", missing)
	}
	for _, key := range operatorconfig.RequiredKeys() {
		partial := map[string]string{}
		for k, v := range full {
			if k != key {
				partial[k] = v
			}
		}
		cfg := loadWith(t, partial)
		missing, keys := cfg.Validate(), cfg.MissingKeys()
		if len(missing) != 1 {
			t.Errorf("dropping %s leaves Validate reporting %v, want exactly one field", key, missing)
		}
		if len(keys) != 1 || keys[0] != key {
			t.Errorf("dropping %s leaves MissingKeys reporting %v, want exactly that key", key, keys)
		}
	}
}

// TestValidateNamesFieldsAndMissingKeysNamesKeys keeps the two answers about one
// set addressed at their own readers: a person greps SSM for the key and reads
// the field name in a struct.
func TestValidateNamesFieldsAndMissingKeysNamesKeys(t *testing.T) {
	empty := loadWith(t, map[string]string{})
	fields, keys := empty.Validate(), empty.MissingKeys()
	if len(fields) != len(keys) || len(fields) == 0 {
		t.Fatalf("Validate reports %d and MissingKeys reports %d", len(fields), len(keys))
	}
	for _, f := range fields {
		if strings.Contains(f, "/") {
			t.Errorf("Validate reported %q, which is an SSM key rather than a field name", f)
		}
	}
	for _, k := range keys {
		if !strings.Contains(k, "/") {
			t.Errorf("MissingKeys reported %q, which is a field name rather than an SSM key", k)
		}
	}
}
