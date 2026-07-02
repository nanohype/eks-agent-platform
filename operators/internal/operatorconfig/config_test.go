/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package operatorconfig

import "testing"

// The operator creates tenant Pod Identity associations against ClusterName,
// which it reads from the SSM key cluster/name. Guard the decode so a renamed
// key fails the build rather than silently leaving ClusterName empty.
func TestAssign_DecodesClusterName(t *testing.T) {
	var c Config
	c.assign("cluster/name", "production-eks")
	if c.ClusterName != "production-eks" {
		t.Errorf("ClusterName: got %q want production-eks", c.ClusterName)
	}
}

func TestAssign_DecodesKnownKeyAndIgnoresUnknown(t *testing.T) {
	var c Config
	c.assign("agent-iam/operator_role_arn", "arn:aws:iam::123:role/op")
	c.assign("unrecognized/key", "ignored")
	if c.OperatorRoleARN != "arn:aws:iam::123:role/op" {
		t.Errorf("OperatorRoleARN not decoded: %q", c.OperatorRoleARN)
	}
}

func completeConfig() Config {
	return Config{
		OperatorRoleARN:              "arn:aws:iam::123:role/op",
		TenantIAMPath:                "/agents/",
		TenantBaselinePolicyARN:      "arn:aws:iam::123:policy/baseline",
		TenantPermissionsBoundaryARN: "arn:aws:iam::123:policy/boundary",
		ArtifactsBucketName:          "artifacts",
	}
}

func TestValidate_CompleteConfigIsClean(t *testing.T) {
	c := completeConfig()
	if missing := c.Validate(); len(missing) != 0 {
		t.Errorf("complete config reported missing fields: %v", missing)
	}
}

// The permissions boundary is what caps every tenant role the operator
// mints. Validate must report its absence so startup fails closed instead
// of silently creating unbounded roles when the SSM parameter is missing.
func TestValidate_ReportsMissingPermissionsBoundary(t *testing.T) {
	c := completeConfig()
	c.TenantPermissionsBoundaryARN = ""
	missing := c.Validate()
	found := false
	for _, m := range missing {
		if m == "TenantPermissionsBoundaryARN" {
			found = true
		}
	}
	if !found {
		t.Errorf("Validate() = %v; want TenantPermissionsBoundaryARN reported", missing)
	}
}
