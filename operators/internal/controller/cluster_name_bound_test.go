/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roleNamesFor is every role name the operator derives for a Platform on a
// cluster, by kind.
func roleNamesFor(cluster, platform string) map[string]string {
	p := newPlatform(platform, "acme")
	return map[string]string{
		"tenant":           tenantRoleName(cluster, p),
		"session":          sessionRoleName(cluster, p),
		"eval":             evalRoleName(cluster, p),
		"scheduler-invoke": schedulerInvokeRoleName(cluster, p),
	}
}

// TestClusterNameBoundIsExact: at MaxClusterNameLen every role name fits IAM's
// limit, even for a Platform name long enough to be cut, and one character
// more puts the longest past it. The bound is safe and no tighter than IAM.
func TestClusterNameBoundIsExact(t *testing.T) {
	longPlatform := strings.Repeat("p", 63)
	atBound := strings.Repeat("c", MaxClusterNameLen)
	for kind, name := range roleNamesFor(atBound, longPlatform) {
		if len(name) > iamRoleNameMax {
			t.Errorf("%s role %q is %d chars at the bound, over IAM's %d", kind, name, len(name), iamRoleNameMax)
		}
		if !strings.HasPrefix(name, atBound+"-") {
			t.Errorf("%s role %q does not carry the cluster name whole", kind, name)
		}
	}
	over := 0
	for _, name := range roleNamesFor(atBound+"c", longPlatform) {
		if len(name) > iamRoleNameMax {
			over++
		}
	}
	if over == 0 {
		t.Errorf("one character past MaxClusterNameLen=%d every role name still fits; the bound is tighter than IAM's limit requires", MaxClusterNameLen)
	}
}

func TestValidateClusterName(t *testing.T) {
	for _, ok := range []string{"", "dev", strings.Repeat("c", MaxClusterNameLen)} {
		if err := ValidateClusterName(ok); err != nil {
			t.Errorf("ValidateClusterName(%d chars) = %v, want nil", len(ok), err)
		}
	}
	tooLong := strings.Repeat("c", MaxClusterNameLen+1)
	err := ValidateClusterName(tooLong)
	if err == nil {
		t.Fatalf("ValidateClusterName(%d chars) = nil, want a refusal", len(tooLong))
	}
	for _, want := range []string{fmt.Sprintf("is %d characters", len(tooLong)), fmt.Sprintf("at most %d", MaxClusterNameLen), "-scheduler-invoke"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

// TestChartCapsClusterNameAtTheOperatorsBound: the chart refuses at render
// time the cluster names the operator refuses at startup on the AWS path, and
// no others.
func TestChartCapsClusterNameAtTheOperatorsBound(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "operator", "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Config struct {
				Properties struct {
					ClusterName struct {
						MaxLength *int `json:"maxLength"`
					} `json:"clusterName"`
				} `json:"properties"`
			} `json:"config"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	got := schema.Properties.Config.Properties.ClusterName.MaxLength
	if got == nil || *got != MaxClusterNameLen {
		t.Errorf("values.schema.json config.clusterName.maxLength = %v, want MaxClusterNameLen (%d)", got, MaxClusterNameLen)
	}
}
