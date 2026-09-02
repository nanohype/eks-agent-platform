/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"os"
	"path/filepath"
	"testing"
)

// The CLI's client is the one place in this module where a rest.Config timeout
// is the right instrument, because these commands open no watch: they issue
// one-shot reads and writes and hold no cache. Inside the operator the same
// setting would cut watches, which is why the host client is bounded per
// reconcile instead.
//
// Read from the config the CLI would actually build rather than from the
// constant, because the constant is bounded whatever the code does with it. A
// config that reaches the API server with no timeout does not return an error
// on an unreachable cluster — it returns nothing, and the person who typed the
// command is left with a cursor.
func TestTheCLIClientCarriesARequestTimeout(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
  - name: probe
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: probe
    context:
      cluster: probe
      user: probe
current-context: probe
users:
  - name: probe
    user:
      token: probe
`), 0o600); err != nil {
		t.Fatalf("write the probe kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	cfg, err := clusterRESTConfig()
	if err != nil {
		t.Fatalf("clusterRESTConfig: %v", err)
	}
	if cfg.Timeout <= 0 {
		t.Fatalf("the CLI's rest.Config carries timeout %v; a command against an unreachable API server "+
			"then hangs rather than failing, and nothing else in this process ends the wait", cfg.Timeout)
	}
	if cfg.Timeout != clusterRequestTimeout {
		t.Errorf("rest.Config timeout is %v and clusterRequestTimeout is %v; the constant is what the "+
			"reasoning is written against", cfg.Timeout, clusterRequestTimeout)
	}
}
