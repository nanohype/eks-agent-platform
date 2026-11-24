/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func TestEnsureOperatorImpersonateRBAC_WithoutAttributionCreatesNothing(t *testing.T) {
	cl := rbacTestClient(t)
	r := &PlatformReconciler{Client: cl}
	p := attributedPlatform("acme", "reliability", []string{"alice@acme.com"}, nil)
	p.Spec.Attribution = nil

	if err := r.ensureOperatorImpersonateRBAC(context.Background(), p); err != nil {
		t.Fatalf("ensureOperatorImpersonateRBAC: %v", err)
	}
	var crs rbacv1.ClusterRoleList
	if err := cl.List(context.Background(), &crs); err != nil {
		t.Fatalf("list ClusterRoles: %v", err)
	}
	var crbs rbacv1.ClusterRoleBindingList
	if err := cl.List(context.Background(), &crbs); err != nil {
		t.Fatalf("list ClusterRoleBindings: %v", err)
	}
	if len(crs.Items) != 0 || len(crbs.Items) != 0 {
		t.Errorf("a Platform without attribution got %d ClusterRoles and %d ClusterRoleBindings, want none",
			len(crs.Items), len(crbs.Items))
	}
}

// rbacFailingClient fails every write of the named kind with a server error,
// and passes every other call through.
func rbacFailingClient(t *testing.T, failKind string, seed ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add rbac scheme: %v", err)
	}
	fail := func(obj client.Object) error {
		kind := ""
		switch obj.(type) {
		case *rbacv1.ClusterRole:
			kind = "ClusterRole"
		case *rbacv1.ClusterRoleBinding:
			kind = "ClusterRoleBinding"
		}
		if kind == failKind {
			return apierrors.NewInternalError(errors.New("injected " + kind + " write failure"))
		}
		return nil
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := fail(obj); err != nil {
					return err
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := fail(obj); err != nil {
					return err
				}
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := fail(obj); err != nil {
					return err
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
}

func TestEnsureOperatorImpersonateRBAC_ReturnsWriteFailures(t *testing.T) {
	for _, kind := range []string{"ClusterRole", "ClusterRoleBinding"} {
		t.Run(kind, func(t *testing.T) {
			r := &PlatformReconciler{Client: rbacFailingClient(t, kind)}
			p := attributedPlatform("acme", "reliability", []string{"alice@acme.com"}, nil)

			err := r.ensureOperatorImpersonateRBAC(context.Background(), p)
			if err == nil {
				t.Fatalf("a failed %s write returned nil, so the reconcile would report the grant in place", kind)
			}
			if !strings.Contains(err.Error(), "ensure impersonate "+kind+" ") {
				t.Errorf("error %q does not name the %s write that failed", err, kind)
			}
		})
	}
}

func TestDeleteOperatorImpersonateRBAC_ReturnsDeleteFailures(t *testing.T) {
	p := attributedPlatform("acme", "reliability", []string{"alice@acme.com"}, nil)
	name := impersonateResourceName(p)
	seed := func() []client.Object {
		return []client.Object{
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}},
			&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
		}
	}
	for _, kind := range []string{"ClusterRoleBinding", "ClusterRole"} {
		t.Run(kind, func(t *testing.T) {
			r := &PlatformReconciler{Client: rbacFailingClient(t, kind, seed()...)}

			err := r.deleteOperatorImpersonateRBAC(context.Background(), p)
			if err == nil {
				t.Fatalf("a failed %s delete returned nil, so the finalizer would release with the grant still in place", kind)
			}
			if !strings.Contains(err.Error(), "delete impersonate "+kind+" ") {
				t.Errorf("error %q does not name the %s delete that failed", err, kind)
			}
		})
	}
}
