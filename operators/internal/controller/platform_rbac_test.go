/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func rbacTestClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add rbac scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func TestEnsureOperatorImpersonateRBAC(t *testing.T) {
	cl := rbacTestClient(t)
	r := &PlatformReconciler{Client: cl}
	p := attributedPlatform("acme", "reliability", []string{"alice@acme.com", "bob@acme.com"}, nil)

	if err := r.ensureOperatorImpersonateRBAC(context.Background(), p); err != nil {
		t.Fatalf("ensureOperatorImpersonateRBAC: %v", err)
	}
	name := impersonateResourceName(p)

	var cr rbacv1.ClusterRole
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &cr); err != nil {
		t.Fatalf("get ClusterRole: %v", err)
	}
	if len(cr.Rules) != 1 {
		t.Fatalf("rules: got %d want 1", len(cr.Rules))
	}
	rule := cr.Rules[0]
	if len(rule.Verbs) != 1 || rule.Verbs[0] != "impersonate" {
		t.Errorf("verbs: got %v want [impersonate]", rule.Verbs)
	}
	if len(rule.Resources) != 1 || rule.Resources[0] != "users" {
		t.Errorf("resources: got %v want [users]", rule.Resources)
	}
	wantOps := map[string]bool{"alice@acme.com": true, "bob@acme.com": true}
	if len(rule.ResourceNames) != len(wantOps) {
		t.Fatalf("resourceNames: got %v want %v", rule.ResourceNames, wantOps)
	}
	for _, op := range rule.ResourceNames {
		if !wantOps[op] {
			t.Errorf("unexpected resourceName %q (impersonation must be scoped to the named operators)", op)
		}
	}

	var crb rbacv1.ClusterRoleBinding
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &crb); err != nil {
		t.Fatalf("get ClusterRoleBinding: %v", err)
	}
	if crb.RoleRef.Name != name || crb.RoleRef.Kind != "ClusterRole" {
		t.Errorf("roleRef: got %+v", crb.RoleRef)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("subjects: got %d want 1", len(crb.Subjects))
	}
	sub := crb.Subjects[0]
	if sub.Kind != "ServiceAccount" || sub.Name != tenantSAName || sub.Namespace != PlatformNamespace(p) {
		t.Errorf("subject: got %+v want ServiceAccount %s/%s", sub, PlatformNamespace(p), tenantSAName)
	}
}

func TestEnsureOperatorImpersonateRBAC_UpdatesOperators(t *testing.T) {
	cl := rbacTestClient(t)
	r := &PlatformReconciler{Client: cl}
	p := attributedPlatform("acme", "reliability", []string{"alice@acme.com"}, nil)
	if err := r.ensureOperatorImpersonateRBAC(context.Background(), p); err != nil {
		t.Fatalf("first ensure: %v", err)
	}

	p.Spec.Attribution.Operators = []string{"carol@acme.com"}
	if err := r.ensureOperatorImpersonateRBAC(context.Background(), p); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	var cr rbacv1.ClusterRole
	if err := cl.Get(context.Background(), types.NamespacedName{Name: impersonateResourceName(p)}, &cr); err != nil {
		t.Fatalf("get ClusterRole: %v", err)
	}
	got := cr.Rules[0].ResourceNames
	if len(got) != 1 || got[0] != "carol@acme.com" {
		t.Errorf("resourceNames after operator change: got %v want [carol@acme.com]", got)
	}
}

func TestDeleteOperatorImpersonateRBAC(t *testing.T) {
	cl := rbacTestClient(t)
	r := &PlatformReconciler{Client: cl}
	p := attributedPlatform("acme", "reliability", []string{"alice@acme.com"}, nil)
	if err := r.ensureOperatorImpersonateRBAC(context.Background(), p); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if err := r.deleteOperatorImpersonateRBAC(context.Background(), p); err != nil {
		t.Fatalf("delete: %v", err)
	}
	name := impersonateResourceName(p)
	var cr rbacv1.ClusterRole
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name}, &cr); !apierrors.IsNotFound(err) {
		t.Errorf("ClusterRole should be gone: err=%v", err)
	}
	// Deleting again is a tolerated no-op.
	if err := r.deleteOperatorImpersonateRBAC(context.Background(), p); err != nil {
		t.Errorf("second delete should be a no-op: %v", err)
	}
}
