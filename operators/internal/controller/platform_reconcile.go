/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
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
func (r *PlatformReconciler) labelsForPlatform(p *platformv1alpha1.Platform) map[string]string {
	l := map[string]string{
		// resource-tagging required_by_surface.k8s — the four dimensions every
		// object on the stack carries. managed-by and component name the
		// lifecycle owner and the workload role; environment and team are what
		// a cost or ownership rollup groups on, and a rollup cannot recover a
		// dimension that was never stamped.
		"app.kubernetes.io/managed-by": "eks-agent-platform",
		"app.kubernetes.io/component":  "tenant",
		labelEnvironment:               r.IAMCfg.Environment,
		labelTeam:                      p.Spec.Tenant,

		"app.kubernetes.io/part-of": "eks-agent-platform",
		LabelPlatform:               p.Name,
		LabelTenant:                 p.Spec.Tenant,
		LabelPersona:                p.Spec.Persona,
	}
	// A label VALUE may not be empty on the API server's rules, and the
	// operator runs without --environment on a dev cluster. Drop the key rather
	// than stamp "" — an absent dimension is recoverable by re-labelling, and a
	// rejected object is a Platform that does not reconcile at all.
	for k, v := range l {
		if v == "" {
			delete(l, k)
		}
	}
	return l
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
		for k, v := range r.labelsForPlatform(p) {
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
		q.Labels = r.labelsForPlatform(p)
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
		lr.Labels = r.labelsForPlatform(p)
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
	// On cilium the tenant egress policy is a CiliumNetworkPolicy
	// (ensureTenantCiliumEgress) — a vanilla NetworkPolicy can't allow egress to
	// the EKS Pod Identity creds endpoint (the reserved host entity). Emit this
	// portable NetworkPolicy only on non-cilium clusters.
	if r.NetworkEngine == NetworkEngineCilium {
		return nil
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-egress",
			Namespace: PlatformNamespace(p),
		},
	}
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)
	otlpGRPC := intstr.FromInt(otlpGRPCPort)
	otlpHTTP := intstr.FromInt(otlpHTTPPort)
	gatewayPort := intstr.FromInt(8080)
	credsPort := intstr.FromInt(80)
	// The tenant's own declared datastores. Built before the spec so the rule
	// can be appended below only when the Platform asked for one.
	var datastoreRule []networkingv1.NetworkPolicyEgressRule
	if ports := datastoreEgressPorts(p); len(ports) > 0 {
		npPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
		for _, prt := range ports {
			pp := intstr.FromInt(prt)
			npPorts = append(npPorts, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &pp})
		}
		// No `To`, which in a NetworkPolicy means any destination on these
		// ports. Aurora and ElastiCache get their addresses from the substrate
		// at provision time and the operator does not know them; the port is
		// the bound that holds without guessing a CIDR. Mirrors the cilium
		// twin's toEntities "all" so the two engines enforce the same thing.
		datastoreRule = append(datastoreRule, networkingv1.NetworkPolicyEgressRule{Ports: npPorts})
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = r.labelsForPlatform(p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // all pods in the tenant namespace
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// EKS Pod Identity credential endpoint (169.254.170.23:80).
					// On cilium this is the reserved host entity and needs a
					// CiliumNetworkPolicy (ensureTenantCiliumEgress); on other
					// CNIs the link-local /32 ipBlock works here.
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "169.254.170.23/32"},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &credsPort},
					},
				},
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
					// The Platform's model gateway, whose Envoy runs in this
					// same namespace. A peer with no NamespaceSelector means
					// "this namespace" — and default-deny egress applies to
					// same-namespace traffic too, so without this rule the
					// tenant cannot reach its own gateway at all.
					To: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app.kubernetes.io/name":       "envoy",
								"app.kubernetes.io/component":  "proxy",
								"app.kubernetes.io/managed-by": "envoy-gateway",
							},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &gatewayPort},
					},
				},
				{
					// The OTel collector gateway, on :4317 + :4318. The
					// namespace is `monitoring` — that is where the catalog
					// deploys the collector and where the stable
					// `telemetry.monitoring.svc` Service every tenant chart
					// wires against lives.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": collectorNamespace},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &otlpGRPC},
						{Protocol: &tcp, Port: &otlpHTTP},
					},
				},
			},
		}
		np.Spec.Egress = append(np.Spec.Egress, datastoreRule...)
		return nil
	})
	return err
}

// ensureTenantIngressPolicy closes the other direction of the tenant boundary.
//
// ensureNetworkPolicy selects every pod in the namespace but declares
// PolicyTypes: [Egress] only, and a policy that names no Ingress type does not
// restrict ingress at all — it is not a deny, it is an absence. AgentFleet,
// AgentSandbox and SandboxPool pods each carry their own deny-all ingress, so
// the gap was never the agent workloads: it was the tenant's own chart-deployed
// application pods, which are reachable from any pod on the cluster.
//
// A separate policy rather than another field on the shared one, for the reason
// ensureGatewayEgressPolicy already states: policies are additive, and the
// tenant chart ships its own NetworkPolicy declaring whatever ingress its
// application actually serves. Adding rules here would put the operator in the
// business of guessing that.
//
// Two exceptions, and they are the two that break silently if omitted:
//
//   - Same-namespace to the gateway. The tenant egress rule lets an application
//     pod SEND to its own Envoy, but a deny-all ingress on the Envoy refuses the
//     delivery, and the failure surfaces as model calls timing out against a
//     Gateway that reports healthy.
//   - The monitoring namespace. A metrics scrape is ingress; without it the
//     tenant's own series stop arriving, which reads as a quiet workload rather
//     than as a blocked one.
//
// Vanilla NetworkPolicy on both engines, with no cilium twin: the tenant EGRESS
// policy needs one only because the Pod Identity endpoint is a cilium host
// entity that a vanilla ipBlock cannot express. Nothing here names an entity
// outside the standard API, and Cilium enforces standard NetworkPolicy, so one
// object covers both.
func (r *PlatformReconciler) ensureTenantIngressPolicy(ctx context.Context, p *platformv1alpha1.Platform) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-ingress",
			Namespace: PlatformNamespace(p),
		},
	}
	tcp := corev1.ProtocolTCP
	gatewayPort := intstr.FromInt(8080)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = r.labelsForPlatform(p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // all pods in the tenant namespace
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// The gateway, from this namespace only. A peer with no
					// NamespaceSelector means "this namespace".
					From: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &gatewayPort},
					},
				},
				{
					// Metrics scrape from the collector's namespace.
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": collectorNamespace},
						},
					}},
				},
			},
		}
		return nil
	})
	return err
}

// ensureGatewayEgressPolicy grants the model gateway's Envoy the one thing no
// other tenant pod gets: outbound TLS, which is how it reaches Bedrock.
//
// The tenant egress policy selects every pod in the namespace and allows DNS,
// the gateway, the OTel collector and the Pod Identity endpoint — nothing on
// 443. The gateway lives in that namespace, so without this second policy it
// is denied the only call it exists to make, and every model request fails
// while the Gateway reports itself healthy.
//
// A separate policy rather than another rule on the shared one: network
// policies are additive, so selecting just the Envoy pods gives the gateway
// outbound TLS and leaves application pods without it. That is what makes the
// gateway the only route to a model — enforced by the network, not by asking
// applications to prefer it. Widening the tenant list instead would hand every
// pod a direct path to Bedrock.
//
// Emitted on non-cilium clusters only; ensureGatewayCiliumEgress is the twin.
func (r *PlatformReconciler) ensureGatewayEgressPolicy(ctx context.Context, p *platformv1alpha1.Platform) error {
	if r.NetworkEngine == NetworkEngineCilium {
		return nil
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-egress",
			Namespace: PlatformNamespace(p),
		},
	}
	tcp := corev1.ProtocolTCP
	tlsPort := intstr.FromInt(443)
	otlpGRPC := intstr.FromInt(otlpGRPCPort)
	otlpHTTP := intstr.FromInt(otlpHTTPPort)
	xdsPort := intstr.FromInt(envoyGatewayXDSPort)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = r.labelsForPlatform(p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name":       "envoy",
				"app.kubernetes.io/component":  "proxy",
				"app.kubernetes.io/managed-by": "envoy-gateway",
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// No peer: any destination, on 443 only. Scoped to the port
					// rather than a Bedrock address because on PrivateLink the
					// endpoint resolves to an in-VPC IP that varies per cluster.
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &tlsPort}},
				},
				{
					// xDS, to Envoy Gateway's control plane. Not covered by
					// the 443 rule above: xDS is plaintext gRPC on 18000, to a
					// namespace the tenant otherwise has no route to. Without
					// it the proxy never receives a listener and the Gateway
					// never reports Programmed — see the constant's comment.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": envoyGatewayNamespace},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &xdsPort}},
				},
				{
					// The gateway's own telemetry. It records the routing
					// decision — route name, resolved model, tokens, guardrail
					// applied, and requests it refused before Bedrock ever saw
					// them — none of which appears in Bedrock's invocation log.
					// Without this rule the extproc exports into a closed
					// egress and the ledger is empty, with nothing unhealthy
					// to notice.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": collectorNamespace},
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

// ensureTenantCiliumEgress emits the tenant egress CiliumNetworkPolicy on
// cilium clusters (the default). It carries the same allow-list as the k8s NP
// PLUS egress to the EKS Pod Identity creds endpoint (169.254.170.23:80, the
// reserved host entity a vanilla NetworkPolicy can't match) — without it every
// tenant-runtime pod silently fails to obtain AWS credentials. No-op on the
// kubernetes engine, where ensureNetworkPolicy's k8s NetworkPolicy applies.
func (r *PlatformReconciler) ensureTenantCiliumEgress(ctx context.Context, p *platformv1alpha1.Platform) error {
	if r.NetworkEngine != NetworkEngineCilium {
		return nil
	}
	return ensureCiliumEgress(ctx, r.Client, PlatformNamespace(p), "tenant-egress", map[string]interface{}{}, r.labelsForPlatform(p), false, datastoreEgressPorts(p), datastoreFQDNs(p, r.IAMCfg.Region))
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
		labels := r.labelsForPlatform(p)
		ap.SetLabels(labels)
		sourceRepos := []interface{}{
			// Allow every nanohype org repo so a tenant Application can pull
			// its own chart + values (github.com/nanohype/<app>.git) through
			// this per-Platform AppProject, plus the operator's own charts.
			"https://github.com/nanohype/*",
			"oci://ghcr.io/nanohype/eks-agent-platform/charts/*",
		}
		destinations := []interface{}{
			map[string]interface{}{
				"namespace": PlatformNamespace(p),
				"server":    "https://kubernetes.default.svc",
			},
		}
		// The namespace tier grants no cluster-scoped resource rights — tenant apps
		// are namespaced-only.
		clusterResourceWhitelist := []interface{}{}
		// vcluster tier: the operator-declared vcluster Application pulls the
		// upstream vcluster chart, so its repo must be allow-listed here; and the
		// tenant's own app targets the registered vcluster destination, scoped so
		// this AppProject can deploy only into its own virtual cluster.
		if p.Spec.Isolation == isolationVCluster && r.VClusterCfg.ChartRepoURL != "" {
			sourceRepos = append(sourceRepos, r.VClusterCfg.ChartRepoURL)
			destinations = append(destinations, map[string]interface{}{
				"namespace": "*",
				"server":    vclusterInClusterServer(p),
			})
			// The vcluster chart's only cluster-scoped resources are the syncer's
			// ClusterRole + ClusterRoleBinding; the AppProject must permit exactly
			// those two kinds or ArgoCD refuses the sync ("not permitted in
			// project"). Deliberately narrow — no CRDs, no other cluster-scoped
			// kinds — so the tier grants the minimum the vcluster install needs.
			clusterResourceWhitelist = []interface{}{
				map[string]interface{}{"group": "rbac.authorization.k8s.io", "kind": "ClusterRole"},
				map[string]interface{}{"group": "rbac.authorization.k8s.io", "kind": "ClusterRoleBinding"},
			}
		}
		spec := map[string]interface{}{
			"description":                fmt.Sprintf("AppProject for Platform %s (tenant %s)", p.Name, p.Spec.Tenant),
			"sourceRepos":                sourceRepos,
			"destinations":               destinations,
			"clusterResourceWhitelist":   clusterResourceWhitelist,
			"namespaceResourceWhitelist": []interface{}{map[string]interface{}{"group": "*", "kind": "*"}},
			"namespaceResourceBlacklist": tenantDeniedNamespacedKinds(),
		}
		// Rollout holds. This write replaces the whole spec — SetNestedField
		// assigns wholesale at the leaf — so any syncWindow written here by a
		// second controller would be erased on the next tick. Rather than have
		// two writers contend for one field, the AppProject stays single-writer
		// and the hold is rendered from desired state: every SLOPolicy in this
		// Platform's namespace whose burn-rate hold is engaged contributes a deny
		// window. Idempotent by construction, and reversible — clearing
		// status.holdEngagedAt drops the key on the next reconcile, which the
		// SLOPolicy watch triggers immediately.
		windows, err := r.sloHoldWindows(ctx, p)
		if err != nil {
			return err
		}
		if len(windows) > 0 {
			spec["syncWindows"] = windows
		}
		return unstructured.SetNestedField(ap.Object, spec, "spec")
	})
	return err
}

// tenantDeniedNamespacedKinds is the deny half of the tenant's namespaced
// surface. The whitelist beside it stays {*, *} on purpose: a tenant deploys
// arbitrary application resources and nobody holds the list of what they ship,
// so an allow-list of kinds would fail at admission naming a kind rather than a
// policy, for workloads that were never the concern.
//
// The concern is the gateway data plane. A tenant declares a ModelGateway CR and
// the operator renders the Gateway, its AIGatewayRoute, the AIServiceBackends
// and the policies; nothing a tenant ships creates those kinds directly. With
// {*, *} alone a tenant's Application could create an HTTPRoute or
// AIGatewayRoute in its OWN namespace naming another tenant's Gateway as
// parentRef — Gateway API permits a cross-namespace parent, and the destination
// scoping does not stop it because the object never leaves the tenant's
// namespace. The listener's allowedRoutes: Same closes that today, and this
// closes the step before it: with no route kind creatable, there is nothing to
// attach.
//
// Denied by group rather than by kind. Every kind in these three groups is
// operator-owned — the data plane, its backends and its traffic policies — so
// the group is the honest boundary, and it does not have to be revisited when
// Gateway API adds a route type.
//
// This constrains ArgoCD Applications in the tenant's project. The operator
// writes these objects through its own ClusterRole and is unaffected.
func tenantDeniedNamespacedKinds() []interface{} {
	return []interface{}{
		// Gateway API itself: HTTPRoute, GRPCRoute, TCPRoute, TLSRoute,
		// UDPRoute, Gateway, ReferenceGrant, BackendTLSPolicy.
		map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "*"},
		// Envoy AI Gateway: AIGatewayRoute, AIServiceBackend,
		// BackendSecurityPolicy.
		map[string]interface{}{"group": "aigateway.envoyproxy.io", "kind": "*"},
		// Envoy Gateway extensions: EnvoyProxy, Backend, ClientTrafficPolicy,
		// BackendTrafficPolicy. EnvoyProxy is the one that would otherwise let a
		// tenant restyle the proxy its own Gateway runs, and BackendTrafficPolicy
		// the one that would let it rewrite a rate limit.
		map[string]interface{}{"group": "gateway.envoyproxy.io", "kind": "*"},
	}
}

// sloHoldWindows renders the deny syncWindows this Platform's AppProject should
// carry: one per SLOPolicy in the Platform's namespace that points at this
// Platform and has an engaged burn-rate hold.
//
// The SLO reconciler decides the hold and records it on its own status; this
// renders it. Keeping the decision and the write in different controllers is
// what lets the AppProject stay single-writer while the control loop still acts
// — see the file comment in slo_hold.go for why a second writer cannot work
// here. The SLO reconciler reads the window back to confirm the effect landed.
func (r *PlatformReconciler) sloHoldWindows(ctx context.Context, p *platformv1alpha1.Platform) ([]interface{}, error) {
	var policies governancev1alpha1.SLOPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(p.Namespace)); err != nil {
		return nil, fmt.Errorf("list slopolicies in %s: %w", p.Namespace, err)
	}
	windows := make([]interface{}, 0, len(policies.Items))
	for i := range policies.Items {
		s := &policies.Items[i]
		if s.Spec.PlatformRef.Name != p.Name || s.Status.HoldEngagedAt == nil {
			continue
		}
		windows = append(windows, sloDenyWindow())
		// One window denies everything in the project, so a second engaged
		// policy would add nothing but noise to the spec.
		break
	}
	return windows, nil
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
	if err == nil {
		return false
	}
	// apimachinery's typed predicate catches the missing-REST-mapping case
	// (NoKindMatchError / NoResourceMatchError); the string fallback covers
	// the rarer pre-mapping text some client paths surface.
	if apimeta.IsNoMatchError(err) {
		return true
	}
	return strings.Contains(err.Error(), "no matches for kind") ||
		strings.Contains(err.Error(), "no kind \"")
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
