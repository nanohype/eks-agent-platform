/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// impersonateResourceName is the cluster-scoped name shared by the ClusterRole
// and ClusterRoleBinding that let a Platform's tenant ServiceAccount
// impersonate its named operators. Keyed off the (already unique, length-safe)
// tenant namespace so two Platforms never collide on a cluster-global name.
func impersonateResourceName(p *platformv1alpha1.Platform) string {
	return PlatformNamespace(p) + "-impersonate"
}

// ensureOperatorImpersonateRBAC provisions the apiserver half of attribution:
// a ClusterRole granting `impersonate` on exactly the Platform's operator
// users, bound to the tenant-runtime ServiceAccount. fab's session kubeconfig
// authenticates with that SA's token while impersonating the operator, so the
// apiserver audit log records impersonatedUser=<operator>.
//
// Scoped to the named users only (never `impersonate *`), so the SA can act as
// the listed humans and no one else. Cluster-scoped resources can't be GC'd via
// an OwnerReference from the namespaced Platform, so cleanup runs through
// deleteOperatorImpersonateRBAC in the finalizer (same pattern as the tenant
// namespace).
func (r *PlatformReconciler) ensureOperatorImpersonateRBAC(ctx context.Context, p *platformv1alpha1.Platform) error {
	if p.Spec.Attribution == nil {
		return nil
	}
	name := impersonateResourceName(p)
	operators := p.Spec.Attribution.Operators

	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cr, func() error {
		cr.Labels = labelsForPlatform(p)
		cr.Rules = []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"users"},
			Verbs:         []string{"impersonate"},
			ResourceNames: operators,
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("ensure impersonate ClusterRole %s: %w", name, err)
	}

	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.Labels = labelsForPlatform(p)
		crb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     name,
		}
		crb.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      tenantSAName,
			Namespace: PlatformNamespace(p),
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("ensure impersonate ClusterRoleBinding %s: %w", name, err)
	}
	return nil
}

// deleteOperatorImpersonateRBAC removes the impersonate ClusterRole +
// ClusterRoleBinding. Tolerates NotFound so non-attribution Platforms and
// re-runs are safe no-ops.
func (r *PlatformReconciler) deleteOperatorImpersonateRBAC(ctx context.Context, p *platformv1alpha1.Platform) error {
	name := impersonateResourceName(p)
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := r.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete impersonate ClusterRoleBinding %s: %w", name, err)
	}
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := r.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete impersonate ClusterRole %s: %w", name, err)
	}
	return nil
}
