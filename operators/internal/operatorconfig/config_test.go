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
