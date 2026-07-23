/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"fmt"
	"os"
	"strings"
)

// floors is the operator's coverage gate.
//
// The org testing rubric (nanohype/standards/testing-rubric.json) sets a global
// floor of 75% for lines/functions/statements. Go coverage is statement-based,
// so the gate enforces statement coverage, applied per package (the rubric's
// per-package-floors rule) rather than as one blurred aggregate:
//
//   - internal/controller and internal/operatorconfig — the substantive
//     reconcile + config-loading logic — sit at the org floor.
//   - internal/agentctl carries a lower, explicitly-documented floor: it is the
//     agentctl CLI package, and much of its surface is cobra command wiring and
//     controller-runtime cluster calls (tenant list/get/status) that only run
//     against a live cluster. The scaffold logic it owns — persona defaults and
//     the byte-for-byte `platform new` renderer — is covered by fixtures.
//   - internal/awsclients is thin, generated-shaped SDK-client wiring (interface
//     declarations + a constructor); there is no branching logic to exercise
//     without a live AWS endpoint, so it carries a floor matching that reality.
//
// On top of the package floors, four security-critical files carry a per-file
// 100% override (the rubric's security-critical-100 rule): the tenant/session
// IAM role reconcilers, the KMS-grant + bucket-policy reconciler, and the
// datastore-access policy generator. These mint the IAM roles, KMS grants, S3
// policies, and per-datastore grants that are the tenant isolation boundary; a
// single uncovered branch there is an unproven security control.
var floors = config{
	packageFloors: map[string]float64{
		"internal/controller":     75,
		"internal/operatorconfig": 75,
		"internal/agentctl":       30,
		"internal/awsclients":     30,
	},
	fileFloors: map[string]float64{
		"internal/controller/platform_iam.go":              100,
		"internal/controller/platform_session_iam.go":      100,
		"internal/controller/platform_kms_s3.go":           100,
		"internal/controller/platform_datastore_policy.go": 100,
	},
}

func main() {
	profile := "cover.out"
	if len(os.Args) > 1 {
		profile = os.Args[1]
	}

	blocks, err := parseProfile(profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fileCov, err := coverageByFile(blocks, ignoredLines)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	vios, pkgCov := check(fileCov, floors)

	var b strings.Builder
	report(&b, pkgCov, fileCov, floors, vios)
	fmt.Print(b.String())

	if len(vios) > 0 {
		os.Exit(1)
	}
}
