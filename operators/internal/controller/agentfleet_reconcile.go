/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

const agentFleetFinalizer = "agents.nanohype.dev/agentfleet-finalizer"

// External CRD groups the AgentFleet reconciler emits into. Each is
// tolerant of missing — clusters without kagent / KEDA installed see
// the missing-CRD as a Pending state, not a reconcile error.
var (
	kagentGV = schema.GroupVersion{Group: "kagent.dev", Version: "v1alpha1"}
	// Agent's storage version is v1alpha2 (declarative wrapper); ModelConfig
	// is emitted at v1alpha1 (its fields convert cleanly to the v1alpha2
	// storage version).
	kagentAgentGV = schema.GroupVersion{Group: "kagent.dev", Version: "v1alpha2"}
	kedaGV        = schema.GroupVersion{Group: "keda.sh", Version: "v1alpha1"}
)

// tenantSAName is the ServiceAccount tenant pods run under; matches the
// Pod Identity association ensureIamRole creates in platform_iam.go, which
// binds tenants-<p>:tenant-runtime to the tenant IAM role.
const tenantSAName = "tenant-runtime"

// resolvePlatform fetches the AgentFleet's referenced Platform.
func (r *AgentFleetReconciler) resolvePlatform(ctx context.Context, fleet *agentsv1alpha1.AgentFleet) (*platformv1alpha1.Platform, error) {
	var p platformv1alpha1.Platform
	key := types.NamespacedName{Namespace: fleet.Namespace, Name: fleet.Spec.PlatformRef.Name}
	if err := r.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errPlatformNotFound
		}
		return nil, fmt.Errorf("get platform %s: %w", key, err)
	}
	return &p, nil
}

// ensureTenantServiceAccount creates the ServiceAccount tenant pods assume —
// both AgentFleet agent pods and AgentSandbox session pods. SA name + namespace
// match the Pod Identity association the operator creates in platform_iam.go
// (tenants-<platform>:tenant-runtime), which binds it to the tenant IAM role.
// The SA carries no role-arn annotation: Pod Identity is the binding.
func ensureTenantServiceAccount(ctx context.Context, c client.Client, p *platformv1alpha1.Platform) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantSAName,
			Namespace: PlatformNamespace(p),
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, sa, func() error {
		sa.Labels = map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  p.Name,
			LabelTenant:                    p.Spec.Tenant,
		}
		return nil
	})
	return err
}

// ensureFleetNetworkPolicy installs an Egress NetworkPolicy in the
// tenant namespace selecting fleet pods (label
// agents.nanohype.dev/fleet=<name>). Egress narrows to: kube-dns,
// agentgateway, observability OTel. Ingress is denied entirely — no one
// reaches a fleet pod from outside the tenant namespace.
//
// policy (same destinations, different podSelector); a shared helper here
// would obscure the per-fleet vs per-namespace semantic.
//
//nolint:dupl // intentionally similar to platform_reconcile.go's tenant-egress
func (r *AgentFleetReconciler) ensureFleetNetworkPolicy(ctx context.Context, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	// On cilium the fleet egress policy is a CiliumNetworkPolicy
	// (ensureFleetCiliumEgress); emit this portable NetworkPolicy only on
	// non-cilium clusters (see ensureNetworkPolicy for the rationale).
	if r.NetworkEngine == NetworkEngineCilium {
		return nil
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fleet-" + fleet.Name,
			Namespace: PlatformNamespace(p),
		},
	}
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)
	otlpGRPC := intstr.FromInt(4317)
	otlpHTTP := intstr.FromInt(4318)
	agentgatewayPort := intstr.FromInt(8080)
	credsPort := intstr.FromInt(80)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  p.Name,
			LabelFleet:                     fleet.Name,
		}
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{LabelFleet: fleet.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// EKS Pod Identity creds endpoint (169.254.170.23:80) — see
					// ensureFleetCiliumEgress for the cilium host-entity variant.
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "169.254.170.23/32"},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &credsPort}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}, {Protocol: &tcp, Port: &dnsPort}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "agentgateway"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &agentgatewayPort}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "observability"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &otlpGRPC}, {Protocol: &tcp, Port: &otlpHTTP}},
				},
			},
			// Ingress: empty list with PolicyTypes including Ingress = deny-all.
			Ingress: nil,
		}
		return nil
	})
	return err
}

// ensureFleetCiliumEgress emits the per-fleet egress CiliumNetworkPolicy on
// cilium clusters — the shared tenant allow-list (including the host-entity Pod
// Identity creds endpoint) plus deny-all ingress, matching the fleet k8s NP
// twin. No-op on the kubernetes engine.
func (r *AgentFleetReconciler) ensureFleetCiliumEgress(ctx context.Context, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	if r.NetworkEngine != NetworkEngineCilium {
		return nil
	}
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "eks-agent-platform",
		LabelPlatform:                  p.Name,
		LabelFleet:                     fleet.Name,
	}
	return ensureCiliumEgress(ctx, r.Client, PlatformNamespace(p), "fleet-"+fleet.Name, map[string]interface{}{LabelFleet: fleet.Name}, labels, true)
}

// ensureKagentAgents emits, per AgentSpec in the fleet, a kagent ModelConfig
// and a kagent Agent bound to it. The ModelConfig is provider=OpenAI pointed
// at the Platform's agentgateway route — agentgateway exposes an
// OpenAI-compatible endpoint and proxies to Bedrock (applying the route's
// guardrail + rate limit), authenticating with its own IRSA. No client API
// key is set: the gateway does the auth, and the operator does not write
// Secrets (tenant credentials flow through ExternalSecrets). Idempotent;
// tolerates absent kagent CRDs (NoKindMatch → Pending).
func (r *AgentFleetReconciler) ensureKagentAgents(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	ns := PlatformNamespace(p)
	gwHost := fmt.Sprintf("%s-gateway.%s.svc.cluster.local:%d", p.Name, agentgatewayNamespace, gatewayListenerPort)
	for _, agent := range fleet.Spec.Agents {
		base := fleet.Name + "-" + agent.Name
		configName := base + "-config"
		labels := map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  p.Name,
			LabelFleet:                     fleet.Name,
			LabelAgent:                     agent.Name,
		}

		// kagent ModelConfig — provider OpenAI pointed at the route's
		// OpenAI-compatible endpoint on the per-Platform Gateway. The
		// agentgateway backend pins the real Bedrock model; the model here
		// is the OpenAI passthrough identifier (set to the resolved model so
		// it stays correct whether or not the gateway overrides it).
		mc := &unstructured.Unstructured{}
		mc.SetGroupVersionKind(schema.GroupVersionKind{Group: kagentGV.Group, Version: kagentGV.Version, Kind: "ModelConfig"})
		mc.SetName(configName)
		mc.SetNamespace(ns)
		if _, err := controllerutil.CreateOrUpdate(ctx, tc, mc, func() error {
			mc.SetLabels(labels)
			spec := map[string]any{
				"provider": "OpenAI",
				"model":    r.resolveRouteModel(ctx, fleet, agent.ModelRoute),
				"openAI": map[string]any{
					"baseUrl": fmt.Sprintf("http://%s/%s-%s/v1", gwHost, p.Name, agent.ModelRoute),
				},
			}
			return unstructured.SetNestedField(mc.Object, spec, "spec")
		}); err != nil {
			if isNoKindMatch(err) {
				return errKagentNotInstalled
			}
			return fmt.Errorf("kagent ModelConfig %s/%s: %w", ns, configName, err)
		}

		// kagent Agent — bound to the ModelConfig. The storage version is
		// kagent.dev/v1alpha2, which nests the agent config in a
		// `declarative` block under spec.type=Declarative; the flat
		// v1alpha1 fields are dropped on conversion, so emit v1alpha2
		// directly. systemMessage is the instruction text (renamed from
		// systemPrompt in the API this operator originally targeted).
		ag := &unstructured.Unstructured{}
		ag.SetGroupVersionKind(schema.GroupVersionKind{Group: kagentAgentGV.Group, Version: kagentAgentGV.Version, Kind: "Agent"})
		ag.SetName(base)
		ag.SetNamespace(ns)
		if _, err := controllerutil.CreateOrUpdate(ctx, tc, ag, func() error {
			ag.SetLabels(labels)
			declarative := map[string]any{
				"modelConfig":   configName,
				"systemMessage": agent.SystemPrompt,
			}
			if len(agent.Tools) > 0 {
				toolRefs := make([]any, 0, len(agent.Tools))
				for _, t := range agent.Tools {
					toolRefs = append(toolRefs, map[string]any{
						"type":      "McpServer",
						"mcpServer": map[string]any{"toolServer": t.Name},
					})
				}
				declarative["tools"] = toolRefs
			}
			spec := map[string]any{
				"type":        "Declarative",
				"declarative": declarative,
				"description": fmt.Sprintf("%s fleet agent %q", fleet.Name, agent.Name),
			}
			return unstructured.SetNestedField(ag.Object, spec, "spec")
		}); err != nil {
			if isNoKindMatch(err) {
				return errKagentNotInstalled
			}
			return fmt.Errorf("kagent Agent %s/%s: %w", ns, base, err)
		}
	}
	return nil
}

// resolveRouteModel finds the effective Bedrock model id for a named route
// on the fleet's Platform ModelGateway (cross-region inference profile when
// set, else the bare model id). Falls back to the route name when the
// gateway/route can't be found — the agentgateway backend pins the real
// model regardless, so this is only the OpenAI "model" passthrough.
func (r *AgentFleetReconciler) resolveRouteModel(ctx context.Context, fleet *agentsv1alpha1.AgentFleet, routeName string) string {
	var gws agentsv1alpha1.ModelGatewayList
	if err := r.List(ctx, &gws, client.InNamespace(fleet.Namespace)); err == nil {
		for i := range gws.Items {
			mg := &gws.Items[i]
			if mg.Spec.PlatformRef.Name != fleet.Spec.PlatformRef.Name {
				continue
			}
			for _, rt := range mg.Spec.Routes {
				if rt.Name != routeName {
					continue
				}
				if rt.CrossRegionProfile != "" {
					return rt.CrossRegionProfile
				}
				return rt.ModelID
			}
		}
	}
	return routeName
}

var (
	errKagentNotInstalled = errors.New("kagent.dev CRDs not installed on this cluster")
	errKEDANotInstalled   = errors.New("keda.sh CRDs not installed on this cluster")
)

// awsRegionFromQueueURL extracts the region segment from an SQS URL
// (https://sqs.<region>.amazonaws.com/<account>/<queue>). The shape is
// already CRD-validated; defensive defaulting returns "us-west-2" if
// parsing fails so we never emit a malformed trigger.
func awsRegionFromQueueURL(url string) string {
	const prefix = "https://sqs."
	if !strings.HasPrefix(url, prefix) {
		return "us-west-2"
	}
	rest := url[len(prefix):]
	dot := strings.Index(rest, ".")
	if dot <= 0 {
		return "us-west-2"
	}
	return rest[:dot]
}

// ensureKEDAScaledObject emits a KEDA ScaledObject per fleet (not per
// agent) when scaling.enabled. When fleet.spec.scaling.queueUrl is set
// (the production path) we emit an aws-sqs-queue trigger paired with a
// TriggerAuthentication CR that points KEDA at the tenant role.
// Without a queue URL we fall back to a CPU-utilization placeholder so
// the fleet scales sensibly during onboarding before a queue is wired.
func (r *AgentFleetReconciler) ensureKEDAScaledObject(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	if !fleet.Spec.Scaling.Enabled {
		return nil
	}
	var minR, maxR int32 = 1, 10
	if fleet.Spec.Scaling.Min != nil {
		minR = *fleet.Spec.Scaling.Min
	}
	if fleet.Spec.Scaling.Max != nil {
		maxR = *fleet.Spec.Scaling.Max
	}
	queueURL := fleet.Spec.Scaling.QueueURL
	if queueURL != "" {
		// TriggerAuthentication has to land before the ScaledObject
		// references it. KEDA's CreateOrUpdate semantics handle the
		// order on its end, but we explicitly emit the TA first to
		// avoid a transient ConfigMap-of-secret-not-found state.
		if err := r.ensureKEDATriggerAuth(ctx, tc, fleet, p); err != nil {
			return err
		}
	}
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "ScaledObject"})
	so.SetName("fleet-" + fleet.Name)
	so.SetNamespace(PlatformNamespace(p))
	_, err := controllerutil.CreateOrUpdate(ctx, tc, so, func() error {
		so.SetLabels(map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  p.Name,
			LabelFleet:                     fleet.Name,
		})
		var triggers []any
		if queueURL != "" {
			region := awsRegionFromQueueURL(queueURL)
			depth := fleet.Spec.Scaling.QueueDepthTrigger
			if depth <= 0 {
				depth = 10
			}
			triggers = []any{
				map[string]any{
					"type": "aws-sqs-queue",
					"metadata": map[string]any{
						"queueURL":    queueURL,
						"queueLength": fmt.Sprintf("%d", depth),
						"awsRegion":   region,
						// 'pod' identityOwner makes KEDA use the workload's
						// own IRSA token (the tenant SA we provisioned via
						// ensureTenantServiceAccount) rather than KEDA's
						// own operator role — keeps per-tenant IAM clean.
						"identityOwner": "pod",
					},
					"authenticationRef": map[string]any{
						"name": "fleet-" + fleet.Name + "-aws",
					},
				},
			}
		} else {
			triggers = []any{
				map[string]any{
					"type": "cpu",
					"metadata": map[string]any{
						"type":  "Utilization",
						"value": "70",
					},
				},
			}
		}
		spec := map[string]any{
			"scaleTargetRef": map[string]any{
				"name": "fleet-" + fleet.Name,
				"kind": "Deployment",
			},
			"minReplicaCount": int64(minR),
			"maxReplicaCount": int64(maxR),
			"triggers":        triggers,
		}
		return unstructured.SetNestedField(so.Object, spec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return errKEDANotInstalled
		}
		return fmt.Errorf("KEDA ScaledObject %s: %w", fleet.Name, err)
	}
	return nil
}

// ensureKEDATriggerAuth provisions the KEDA TriggerAuthentication CR
// the aws-sqs-queue trigger references for IRSA. podIdentity.provider
// = aws means KEDA uses the workload's existing IRSA token (the
// tenant SA we annotated with the role ARN) instead of KEDA's own
// operator IAM identity.
func (r *AgentFleetReconciler) ensureKEDATriggerAuth(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	ta := &unstructured.Unstructured{}
	ta.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "TriggerAuthentication"})
	ta.SetName("fleet-" + fleet.Name + "-aws")
	ta.SetNamespace(PlatformNamespace(p))
	_, err := controllerutil.CreateOrUpdate(ctx, tc, ta, func() error {
		ta.SetLabels(map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  p.Name,
			LabelFleet:                     fleet.Name,
		})
		spec := map[string]any{
			"podIdentity": map[string]any{
				"provider": "aws",
				// identityOwner is on the trigger itself in the
				// ScaledObject; the TA only declares the provider.
			},
		}
		return unstructured.SetNestedField(ta.Object, spec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return errKEDANotInstalled
		}
		return fmt.Errorf("KEDA TriggerAuthentication %s: %w", fleet.Name, err)
	}
	return nil
}

// cleanupFleetResources is the finalizer counterpart: deletes the
// kagent Agents, ModelConfigs, KEDA ScaledObject, and fleet
// NetworkPolicy. Tenant ServiceAccount is owned by Platform finalizer.
func (r *AgentFleetReconciler) cleanupFleetResources(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	ns := PlatformNamespace(p)
	// kagent + KEDA objects live wherever the fleet reconciled them — the host for
	// the namespace tier, the virtual cluster for the vcluster tier — so they are
	// deleted through the same target client that created them.
	for _, agent := range fleet.Spec.Agents {
		base := fleet.Name + "-" + agent.Name
		for _, kind := range []string{"Agent", "ModelConfig"} {
			suffix := "-config"
			if kind == "Agent" {
				suffix = ""
			}
			o := &unstructured.Unstructured{}
			o.SetGroupVersionKind(schema.GroupVersionKind{Group: kagentGV.Group, Version: kagentGV.Version, Kind: kind})
			o.SetName(base + suffix)
			o.SetNamespace(ns)
			if err := tc.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
				return fmt.Errorf("delete kagent %s %s: %w", kind, o.GetName(), err)
			}
		}
	}
	// KEDA ScaledObject + TriggerAuthentication. Delete the SO first so
	// KEDA can't try to re-resolve a TA we're about to remove.
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "ScaledObject"})
	so.SetName("fleet-" + fleet.Name)
	so.SetNamespace(ns)
	if err := tc.Delete(ctx, so); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
		return fmt.Errorf("delete ScaledObject: %w", err)
	}
	ta := &unstructured.Unstructured{}
	ta.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "TriggerAuthentication"})
	ta.SetName("fleet-" + fleet.Name + "-aws")
	ta.SetNamespace(ns)
	if err := tc.Delete(ctx, ta); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
		return fmt.Errorf("delete TriggerAuthentication: %w", err)
	}
	// NetworkPolicy is host containment — always deleted on the host client.
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "fleet-" + fleet.Name, Namespace: ns}}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete NetworkPolicy: %w", err)
	}
	return nil
}

// cleanupTargetClient resolves the client the fleet's teardown deletes through.
// On the vcluster tier it is the virtual-cluster client; when the vcluster is
// unreachable (already torn down during a Platform delete) it falls back to the
// host client, so the host-side NetworkPolicy delete still runs and the
// vcluster-object deletes NotFound harmlessly against the host.
func (r *AgentFleetReconciler) cleanupTargetClient(ctx context.Context, p *platformv1alpha1.Platform) client.Client {
	tc, err := targetClient(ctx, r.Client, r.VCluster, p)
	if err != nil {
		return r.Client
	}
	return tc
}

// reconcileFleetSelf is the orchestration: resolve Platform, gate on
// Ready, run k8s + external steps. Returns (phase, readyAgents, error).
func (r *AgentFleetReconciler) reconcileFleetSelf(ctx context.Context, fleet *agentsv1alpha1.AgentFleet) (string, int32, error) {
	platform, err := r.resolvePlatform(ctx, fleet)
	if err != nil {
		if errors.Is(err, errPlatformNotFound) {
			return phasePending, 0, nil
		}
		return "", 0, err
	}
	// Platform Suspended: tear down the fleet's kagent Agents + KEDA
	// scaler so no pods can serve traffic until the kill-switch is
	// cleared. The tenant SA + NetworkPolicy stay in place so a
	// recovery doesn't have to recreate them.
	if platform.Status.Phase == phaseSuspended {
		if err := r.cleanupFleetResources(ctx, r.cleanupTargetClient(ctx, platform), fleet, platform); err != nil {
			return "", 0, fmt.Errorf("suspend cleanup: %w", err)
		}
		return phaseSuspended, 0, nil
	}
	if platform.Status.Phase != phaseReady {
		return phasePending, 0, nil
	}

	// Resolve the target client: the host client for the namespace tier, the
	// Platform's virtual-cluster client for the vcluster tier. kagent + KEDA
	// objects (which produce the fleet's pods) land through this client so the
	// tenant's pods see the vcluster API; the fleet's host containment
	// (NetworkPolicy/Cilium egress) always stays on the host client below.
	tc, err := targetClient(ctx, r.Client, r.VCluster, platform)
	if err != nil {
		if errors.Is(err, errVClusterNotReady) {
			// vcluster still installing — nothing to write into yet; requeue.
			return phasePending, 0, nil
		}
		return "", 0, fmt.Errorf("resolve target client: %w", err)
	}

	if err := ensureTenantServiceAccount(ctx, tc, platform); err != nil {
		return "", 0, fmt.Errorf("ensure ServiceAccount: %w", err)
	}
	if err := r.ensureFleetNetworkPolicy(ctx, fleet, platform); err != nil {
		return "", 0, fmt.Errorf("ensure NetworkPolicy: %w", err)
	}
	if err := r.ensureFleetCiliumEgress(ctx, fleet, platform); err != nil {
		return "", 0, fmt.Errorf("ensure CiliumNetworkPolicy: %w", err)
	}
	if err := r.ensureKagentAgents(ctx, tc, fleet, platform); err != nil {
		if errors.Is(err, errKagentNotInstalled) {
			return phasePending, 0, nil
		}
		return "", 0, err
	}
	if err := r.ensureKEDAScaledObject(ctx, tc, fleet, platform); err != nil {
		if errors.Is(err, errKEDANotInstalled) {
			// KEDA absence isn't fatal — scaling is optional. Log and
			// move on; the deployment runs at the static replica count.
			return phaseReady, safeAgentCount(fleet), nil
		}
		return "", 0, err
	}
	return phaseReady, safeAgentCount(fleet), nil
}

//nolint:dupl // status writeback mirrors the other reconcilers by design
func (r *AgentFleetReconciler) applyFleetStatus(ctx context.Context, fleet *agentsv1alpha1.AgentFleet, phase string, readyAgents int32) error {
	fleet.Status.Phase = phase
	fleet.Status.ReadyAgents = readyAgents
	fleet.Status.ObservedGeneration = fleet.Generation
	cond := metav1.Condition{
		Type:               "AgentsReconciled",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("%d agent(s) emitted", readyAgents),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: fleet.Generation,
	}
	switch phase {
	case phaseReady:
		// healthy — condition stays True
	case phaseSuspended:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonPlatformSuspended
		cond.Message = "Platform kill-switch fired; fleet scaled to zero"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = phasePending
		cond.Message = "waiting on Platform readiness or kagent CRDs"
	}
	upsertCondition(&fleet.Status.Conditions, cond)
	return r.Status().Update(ctx, fleet)
}

// safeAgentCount returns len(fleet.Spec.Agents) clamped to int32 max.
// AgentFleet conformance tests cap agents at a handful; this is paranoia
// against a hypothetical multi-million-entry list that would overflow.
func safeAgentCount(fleet *agentsv1alpha1.AgentFleet) int32 {
	n := len(fleet.Spec.Agents)
	if n > 2147483647 {
		return 2147483647
	}
	return int32(n) //nolint:gosec // bounded by the check above
}
