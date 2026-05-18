/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// agentctl — non-technical-persona CLI for the eks-agent-platform.
//
// Wraps `kubectl apply`-shaped tenant onboarding in plain-language
// subcommands tailored to sales-ops / support / finance / ops / founder
// / eng / marketing / legal personas. Reads the same CRDs the operator
// reconciles; emits YAML you can pipe to kubectl or apply directly.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nanohype/eks-agent-platform/operators/internal/agentctl"
)

func main() {
	root := &cobra.Command{
		Use:           "agentctl",
		Short:         "Onboard + manage tenants on the eks-agent-platform",
		Long:          "agentctl wraps the eks-agent-platform CRDs in plain-language commands tailored to each persona (sales-ops, support, finance, ops, founder, eng, marketing, legal).",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildVersion,
	}

	root.AddCommand(
		agentctl.NewTenantCmd(),
		agentctl.NewStatusCmd(),
		agentctl.NewPersonaCmd(),
		agentctl.NewVersionCmd(buildVersion),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// buildVersion is overridden at link time via -ldflags="-X main.buildVersion=...".
var buildVersion = "dev"
