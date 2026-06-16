/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

const (
	// finalizerName guards Platform deletion until the operator has cleaned
	// up resources outside the Platform's own namespace (tenant namespace,
	// ArgoCD AppProject) that the kube garbage collector can't reap via
	// OwnerReferences from a namespaced parent.
	finalizerName = "platform.nanohype.dev/platform-finalizer"

	// argoCDNamespace is where Argo CD lives; the AppProject for each
	// Platform is created here. Hardcoded to match the eks-gitops
	// convention rather than threading another config knob.
	argoCDNamespace = "argocd"
)

// PlatformNamespace returns the tenant workload namespace for a Platform.
// Distinct from the management namespace where the Platform CR itself lives
// (typically `eks-agent-platform`).
//
// The namespace name must fit the RFC 1123 subdomain label limit of 63 chars.
// For Platform names longer than what fits with the `tenants-` prefix, we
// truncate the name and append a short FNV-1a hash so it remains unique.
func PlatformNamespace(p *platformv1alpha1.Platform) string {
	const prefix = "tenants-"
	const maxLabel = 63
	full := prefix + p.Name
	if len(full) <= maxLabel {
		return full
	}
	// 8-char hex hash gives ~32 bits of collision resistance. Trim original
	// name to: 63 - len(prefix) - 1(hyphen) - 8(hash) = 46 chars.
	h := fnv1a64(p.Name)
	hashHex := fmt.Sprintf("%08x", h&0xffffffff)
	trimTo := maxLabel - len(prefix) - 1 - 8
	return prefix + p.Name[:trimTo] + "-" + hashHex
}

// fnv1a64 implements FNV-1a 64-bit without importing hash/fnv — keeps the
// reconcile package free of additional stdlib imports for clarity.
func fnv1a64(s string) uint64 {
	const offset = 1469598103934665603
	const prime = 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// labelsForPlatform returns the canonical label set every resource the
// operator creates on a Platform's behalf must carry. Drives:
//   - the NetworkPolicy podSelector for the tenant default-deny + egress
//     allow rules,
//   - the BudgetPolicy controller's tag-based spend attribution
//     (downstream of CUR / cost-pipeline),
//   - dashboard filtering on `agents.platform=<name>`.
func labelsForPlatform(p *platformv1alpha1.Platform) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "eks-agent-platform",
		"app.kubernetes.io/part-of":    "eks-agent-platform",
		LabelPlatform:                  p.Name,
		LabelTenant:                    p.Spec.Tenant,
		LabelPersona:                   p.Spec.Persona,
	}
}

// ensureNamespace creates (or updates labels on) the tenant workload
// namespace. PSS=restricted is enforced at admission so tenant pods can't
// escalate privilege; the namespace is NOT owned by the Platform CR (a
// namespaced parent can't cascade-delete a cluster-scoped child via
// OwnerReferences), so cleanup goes through the finalizer on the Platform.
func (r *PlatformReconciler) ensureNamespace(ctx context.Context, p *platformv1alpha1.Platform) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace(p)}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		for k, v := range labelsForPlatform(p) {
			ns.Labels[k] = v
		}
		// Pod Security Standards — restricted profile enforced at admission.
		// Audit + warn match enforce so escape attempts surface in events.
		const pssLevel = "restricted"
		ns.Labels["pod-security.kubernetes.io/enforce"] = pssLevel
		ns.Labels["pod-security.kubernetes.io/audit"] = pssLevel
		ns.Labels["pod-security.kubernetes.io/warn"] = pssLevel
		return nil
	})
	return err
}

// ensureQuota installs a default ResourceQuota in the tenant namespace.
// Defaults are deliberately conservative; Platform.spec.quotas can override
// per-Platform once that field is wired through the spec.
func (r *PlatformReconciler) ensureQuota(ctx context.Context, p *platformv1alpha1.Platform) error {
	q := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-default",
			Namespace: PlatformNamespace(p),
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, q, func() error {
		q.Labels = labelsForPlatform(p)
		q.Spec.Hard = corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("16Gi"),
			corev1.ResourceLimitsCPU:      resource.MustParse("8"),
			corev1.ResourceLimitsMemory:   resource.MustParse("32Gi"),
			corev1.ResourcePods:           resource.MustParse("50"),
			corev1.ResourceServices:       resource.MustParse("20"),
			corev1.ResourceSecrets:        resource.MustParse("50"),
			corev1.ResourceConfigMaps:     resource.MustParse("50"),
		}
		return nil
	})
	return err
}

// ensureLimitRange sets sensible per-container defaults so pods that omit
// resources don't trip the ResourceQuota.
func (r *PlatformReconciler) ensureLimitRange(ctx context.Context, p *platformv1alpha1.Platform) error {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-default",
			Namespace: PlatformNamespace(p),
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, lr, func() error {
		lr.Labels = labelsForPlatform(p)
		lr.Spec.Limits = []corev1.LimitRangeItem{
			{
				Type: corev1.LimitTypeContainer,
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		}
		return nil
	})
	return err
}

// ensureNetworkPolicy installs the tenant default-deny + selective-allow
// egress policy. Matches the template that the bedrock-egress Helm chart
// publishes via ConfigMap; the operator embeds the template directly so
// reconciliation doesn't depend on a chart-installed ConfigMap.
//
// NetworkPolicy (same destinations, different podSelector). A shared
// helper would obscure the per-namespace vs per-fleet semantic.
//
//nolint:dupl // intentionally similar to agentfleet_reconcile.go's fleet
func (r *PlatformReconciler) ensureNetworkPolicy(ctx context.Context, p *platformv1alpha1.Platform) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-egress",
			Namespace: PlatformNamespace(p),
		},
	}
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)
	otlpGRPC := intstr.FromInt(4317)
	otlpHTTP := intstr.FromInt(4318)
	agentgatewayPort := intstr.FromInt(8080)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = labelsForPlatform(p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // all pods in the tenant namespace
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// DNS: kube-dns in kube-system on UDP/TCP 53.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k8s-app": "kube-dns"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
					// agentgateway service in its own namespace on :8080.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "agentgateway"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &agentgatewayPort},
					},
				},
				{
					// OTel collector in observability namespace on :4317 + :4318.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "observability"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &otlpGRPC},
						{Protocol: &tcp, Port: &otlpHTTP},
					},
				},
			},
		}
		return nil
	})
	return err
}

// ensureAppProject creates an ArgoCD AppProject scoped to the tenant
// namespace so per-Platform ArgoCD Applications inherit the right sourceRepo
// allowlist and destination scope. Uses unstructured.Unstructured to avoid
// pulling the argoproj.io Go types into the operator's dep graph.
func (r *PlatformReconciler) ensureAppProject(ctx context.Context, p *platformv1alpha1.Platform) error {
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "AppProject",
	})
	ap.SetName(p.Name)
	ap.SetNamespace(argoCDNamespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ap, func() error {
		labels := labelsForPlatform(p)
		ap.SetLabels(labels)
		spec := map[string]interface{}{
			"description": fmt.Sprintf("AppProject for Platform %s (tenant %s)", p.Name, p.Spec.Tenant),
			"sourceRepos": []interface{}{
				// Allow every nanohype org repo so a tenant Application can pull
				// its own chart + values (github.com/nanohype/<app>.git) through
				// this per-Platform AppProject, plus the operator's own charts.
				"https://github.com/nanohype/*",
				"oci://ghcr.io/nanohype/eks-agent-platform/charts/*",
			},
			"destinations": []interface{}{
				map[string]interface{}{
					"namespace": PlatformNamespace(p),
					"server":    "https://kubernetes.default.svc",
				},
			},
			"clusterResourceWhitelist":   []interface{}{},
			"namespaceResourceWhitelist": []interface{}{map[string]interface{}{"group": "*", "kind": "*"}},
		}
		return unstructured.SetNestedField(ap.Object, spec, "spec")
	})
	return err
}

// cleanupTenantResources removes resources outside the Platform's own
// namespace that the kube GC can't reap via OwnerReferences. Called from
// the finalizer flow when Platform.DeletionTimestamp is set.
func (r *PlatformReconciler) cleanupTenantResources(ctx context.Context, p *platformv1alpha1.Platform) error {
	// Delete the tenant namespace; cascades to ResourceQuota, LimitRange,
	// NetworkPolicy, and any agent pods.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace(p)}}
	if err := r.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete tenant namespace: %w", err)
	}

	// Delete the AppProject.
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "AppProject",
	})
	ap.SetName(p.Name)
	ap.SetNamespace(argoCDNamespace)
	if err := r.Delete(ctx, ap); err != nil && !apierrors.IsNotFound(err) {
		// AppProject CRD may not be installed (no Argo CD on this cluster).
		// Tolerate NoKindMatch in addition to NotFound.
		if !isNoKindMatch(err) {
			return fmt.Errorf("delete AppProject: %w", err)
		}
	}
	return nil
}

// isNoKindMatch returns true when the error indicates the cluster doesn't
// have the referenced CRD installed (Argo CD AppProject is optional).
func isNoKindMatch(err error) bool {
	// meta.IsNoMatchError isn't available cleanly via the client package,
	// so match by string. Brittle but acceptable for an optional resource.
	return err != nil && (containsString(err.Error(), "no matches for kind") ||
		containsString(err.Error(), "no kind \""))
}

func containsString(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// fetchPlatform is a thin wrapper that returns NotFound vs other errors
// distinctly so the caller can choose between IgnoreNotFound and requeue.
func (r *PlatformReconciler) fetchPlatform(ctx context.Context, key types.NamespacedName) (*platformv1alpha1.Platform, error) {
	var p platformv1alpha1.Platform
	if err := r.Get(ctx, key, &p); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return &p, nil
}
