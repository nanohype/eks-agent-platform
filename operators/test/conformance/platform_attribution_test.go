/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

const attributedOperator = "alice@example.com"

// attributedScopingPlatform creates a Platform that declares attribution for
// one operator, and registers cleanup of its tenant namespace and of the
// cluster-scoped impersonation objects named after it.
func attributedScopingPlatform(ctx context.Context, t *testing.T) *platformv1alpha1.Platform {
	t.Helper()
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: shortScopingName(t), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:     "generic",
			Tenant:      "acme",
			Budget:      platformv1alpha1.BudgetRef{Name: "x"},
			Identity:    platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Attribution: &platformv1alpha1.AttributionSpec{Operators: []string{attributedOperator}},
		},
	}
	mustCreate(ctx, t, p)
	name := impersonateName(p)
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controller.PlatformNamespace(p)}})
		_ = k8sClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
		_ = k8sClient.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return p
}

// impersonateName restates the name the ClusterRole and ClusterRoleBinding
// share: the tenant namespace with an -impersonate suffix.
func impersonateName(p *platformv1alpha1.Platform) string {
	return controller.PlatformNamespace(p) + "-impersonate"
}

// roleNameFromARN returns the last path segment of an IAM role ARN.
func roleNameFromARN(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

// TestPlatformReconciler_AttributionGrantIsRevokedWhenRemoved drives one
// Platform through attribution declared, then removed, against the API server:
// the grant names the declared operator and binds tenant-runtime only, and
// removing the declaration revokes the grant and deletes the session role.
func TestPlatformReconciler_AttributionGrantIsRevokedWhenRemoved(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	iamFake := newMemIAM()
	r := newScopingReconciler(iamFake)
	p := attributedScopingPlatform(ctx, t)
	reconcileIAM(ctx, t, r, p)

	name := impersonateName(p)
	var cr rbacv1.ClusterRole
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &cr); err != nil {
		t.Fatalf("impersonate ClusterRole %s: %v", name, err)
	}
	if len(cr.Rules) != 1 || len(cr.Rules[0].ResourceNames) != 1 || cr.Rules[0].ResourceNames[0] != attributedOperator ||
		len(cr.Rules[0].Verbs) != 1 || cr.Rules[0].Verbs[0] != "impersonate" {
		t.Fatalf("ClusterRole rules = %+v, want impersonate on exactly %s", cr.Rules, attributedOperator)
	}
	var crb rbacv1.ClusterRoleBinding
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &crb); err != nil {
		t.Fatalf("impersonate ClusterRoleBinding %s: %v", name, err)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Name != "tenant-runtime" || crb.Subjects[0].Namespace != controller.PlatformNamespace(p) {
		t.Fatalf("ClusterRoleBinding subjects = %+v, want only %s/tenant-runtime", crb.Subjects, controller.PlatformNamespace(p))
	}
	sessionARN := getPlatform(ctx, t, p.Namespace, p.Name).Status.SessionRoleArn
	if sessionARN == "" {
		t.Fatal("status.sessionRoleArn is empty after a pass that declared attribution")
	}
	if !iamFake.hasRole(roleNameFromARN(sessionARN)) {
		t.Fatalf("session role %s was not minted", sessionARN)
	}

	stored := getPlatform(ctx, t, p.Namespace, p.Name)
	stored.Spec.Attribution = nil
	if err := k8sClient.Update(ctx, stored); err != nil {
		t.Fatalf("remove spec.attribution: %v", err)
	}
	reconcileIAM(ctx, t, r, p)

	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("ClusterRoleBinding %s survived the removal of attribution (err=%v), so tenant-runtime can still impersonate %s", name, err, attributedOperator)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &rbacv1.ClusterRole{}); !apierrors.IsNotFound(err) {
		t.Errorf("ClusterRole %s survived the removal of attribution (err=%v)", name, err)
	}
	if got := getPlatform(ctx, t, p.Namespace, p.Name).Status.SessionRoleArn; got != "" {
		t.Errorf("status.sessionRoleArn = %q after attribution was removed, want empty", got)
	}
	if iamFake.hasRole(roleNameFromARN(sessionARN)) {
		t.Errorf("session role %s survived the removal of attribution", sessionARN)
	}
}

// TestPlatformReconciler_AttributionGrantIsRevokedWithoutASessionRole drives
// attribution declared, then removed, through a reconciler with no IAM client.
// No session role is minted, so the grant's revocation cannot wait on one.
func TestPlatformReconciler_AttributionGrantIsRevokedWithoutASessionRole(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := attributedScopingPlatform(ctx, t)
	reconcileOnce(ctx, t, p)

	name := impersonateName(p)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &rbacv1.ClusterRoleBinding{}); err != nil {
		t.Fatalf("impersonate ClusterRoleBinding %s: %v", name, err)
	}
	if got := getPlatform(ctx, t, p.Namespace, p.Name).Status.SessionRoleArn; got != "" {
		t.Fatalf("status.sessionRoleArn = %q with no IAM client, want empty", got)
	}

	stored := getPlatform(ctx, t, p.Namespace, p.Name)
	stored.Spec.Attribution = nil
	if err := k8sClient.Update(ctx, stored); err != nil {
		t.Fatalf("remove spec.attribution: %v", err)
	}
	reconcileOnce(ctx, t, p)

	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("ClusterRoleBinding %s survived the removal of attribution (err=%v), so tenant-runtime can still impersonate %s", name, err, attributedOperator)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &rbacv1.ClusterRole{}); !apierrors.IsNotFound(err) {
		t.Errorf("ClusterRole %s survived the removal of attribution (err=%v)", name, err)
	}
}
