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

	appsv1 "k8s.io/api/apps/v1"
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

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

const agentFleetFinalizer = "agents.nanohype.dev/agentfleet-finalizer"

// The one external CRD group the AgentFleet reconciler emits into. KEDA's
// absence is tolerated: scaling is optional, and the Deployment runs at its
// static replica count without it. The agents themselves are core Deployments,
// so they need no addon at all.
var kedaGV = schema.GroupVersion{Group: "keda.sh", Version: "v1alpha1"}

// tenantSAName is the ServiceAccount tenant pods run under; matches the
// Pod Identity association ensureIamRole creates in platform_iam.go, which
// binds tenants-<p>:tenant-runtime to the tenant IAM role.
const tenantSAName = "tenant-runtime"

// resolvePlatform fetches the AgentFleet's referenced Platform.
func (r *AgentFleetReconciler) resolvePlatform(ctx context.Context, fleet *agentsv1alpha1.AgentFleet) (*platformv1alpha1.Platform, error) {
	return getReferencedPlatform(ctx, r.Client, fleet.Namespace, fleet.Spec.PlatformRef.Name, errPlatformNotFound)
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
// agents.nanohype.dev/fleet=<name>). Egress narrows to: kube-dns, the
// Platform's model gateway, observability OTel. Ingress is denied entirely — no one
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
	otlpGRPC := intstr.FromInt(otlpGRPCPort)
	otlpHTTP := intstr.FromInt(otlpHTTPPort)
	gatewayPort := intstr.FromInt(8080)
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
					// The Platform's gateway, in this same namespace. A peer with
					// no NamespaceSelector means "this namespace"; default-deny
					// egress covers same-namespace traffic, so without this rule
					// fleet pods cannot reach their own gateway.
					To: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							"app.kubernetes.io/name":       "envoy",
							"app.kubernetes.io/component":  "proxy",
							"app.kubernetes.io/managed-by": "envoy-gateway",
						}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &gatewayPort}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": collectorNamespace}},
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
	return ensureCiliumEgress(ctx, r.Client, PlatformNamespace(p), "fleet-"+fleet.Name, map[string]interface{}{LabelFleet: fleet.Name}, labels, true, datastoreEgressPorts(p), datastoreFQDNs(p, r.Region))
}

// ensureAgentDeployments emits one Deployment per AgentSpec: the tenant's own
// agent image, running in the tenant's namespace under the tenant
// ServiceAccount.
//
// There is no platform-supplied agent runtime and no tool server. The agent
// loop and its tools are code in the tenant's image, executing in that process
// as the tenant — so an action the agent takes appears in the Kubernetes audit
// log under the tenant's identity, and the agent's own account of what it did
// can be checked against it. A tool server executing under a service identity
// of its own is precisely what breaks that: the record names the tool server,
// and no claim can be confirmed or refuted against it.
//
// The model reaches the agent as a base URL, a wire format and a route name,
// never as a credential or a model id. Idempotent.
func (r *AgentFleetReconciler) ensureAgentDeployments(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform, routes map[string]agentsv1alpha1.RouteStatus) error {
	ns := PlatformNamespace(p)
	// The Platform's gateway runs in this same namespace under the same
	// ServiceAccount, so it is a sibling of the agents it fronts.
	gatewayEndpoint := ModelGatewayEndpoint(p)

	for i := range fleet.Spec.Agents {
		agent := &fleet.Spec.Agents[i]
		route, ok := routes[agent.ModelRoute]
		if !ok {
			// Caught before the Deployment is written rather than after. An
			// agent pointed at a route the gateway does not serve starts
			// cleanly, reports Ready, and fails at its first model call — so
			// the fleet has to refuse the pod, not ship one that cannot work.
			return fmt.Errorf("%w: agent %q wants route %q; gateway publishes %s",
				errRouteNotPublished, agent.Name, agent.ModelRoute, strings.Join(routeNames(routes), ", "))
		}
		name := agentDeploymentName(fleet, agent.Name)
		labels := map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  p.Name,
			LabelFleet:                     fleet.Name,
			LabelAgent:                     agent.Name,
		}

		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if _, err := controllerutil.CreateOrUpdate(ctx, tc, deploy, func() error {
			// Replicas are KEDA's once anything has set them. Writing the floor
			// on every reconcile would fight the ScaledObject: the operator
			// would undo each scale-up moments after KEDA made it, and the two
			// controllers would flap against each other for as long as the
			// fleet existed — with the Deployment reporting healthy throughout.
			//
			// Keyed on the field being unset rather than on the object looking
			// new: a creation timestamp is not reliably populated before the
			// write lands, so "is this the first reconcile" is the wrong
			// question. "Does anyone already have an opinion" is the right one.
			if deploy.Spec.Replicas == nil {
				minReplicas, _ := fleetScalingMinMax(fleet, agent)
				deploy.Spec.Replicas = &minReplicas
			}

			deploy.Labels = labels
			deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{
				LabelFleet: fleet.Name,
				LabelAgent: agent.Name,
			}}
			deploy.Spec.Template.Labels = labels
			deploy.Spec.Template.Spec = corev1.PodSpec{
				// The tenant ServiceAccount, bound by a Pod Identity association
				// to the tenant IAM role. This is what makes the agent's actions
				// attributable — and it is the same account the gateway runs
				// under, so the model call and the agent share one identity.
				ServiceAccountName: tenantSAName,
				SecurityContext:    restrictedPodSecurityContext(),
				Containers: []corev1.Container{{
					Name:            "agent",
					Image:           agent.Image,
					SecurityContext: restrictedContainerSecurityContext(),
					Env: withAgentOTelAttrs([]corev1.EnvVar{
						// The agent never holds an AWS credential and never names
						// a model: the gateway resolves the route, applies the
						// guardrail, and signs.
						//
						// MODEL_ROUTE_BASE_URL is what a model client is
						// configured with — the gateway serves each wire format
						// under its own prefix, so MODEL_GATEWAY_ENDPOINT
						// addresses the gateway but is not a usable base for any
						// SDK. Both are published because an agent may want the
						// gateway's address for something other than inference;
						// only one of them is the client's base URL.
						//
						// MODEL_ROUTE_API says which SDK to build. Reading it
						// beats inferring from the route name, which carries no
						// format, or from a model id the agent is never given.
						{Name: "MODEL_GATEWAY_ENDPOINT", Value: gatewayEndpoint},
						{Name: "MODEL_ROUTE_BASE_URL", Value: route.BaseURL},
						{Name: "MODEL_ROUTE_API", Value: string(route.API)},
						{Name: "MODEL_ROUTE", Value: agent.ModelRoute},
						{Name: "AGENT_NAME", Value: agent.Name},
						{Name: "AGENT_SYSTEM_PROMPT", Value: agent.SystemPrompt},
					}, p, fleet.Name, agent.Name),
					Resources: agentResources(agent),
				}},
			}
			return nil
		}); err != nil {
			return fmt.Errorf("agent Deployment %s/%s: %w", ns, name, err)
		}
	}
	return nil
}

// defaultAgentResources is the per-agent container shape when the AgentSpec
// does not override it. Requests and limits are both set: the tenant namespace
// carries a ResourceQuota, and a container with no request is rejected at
// admission rather than scheduled at zero.
func defaultAgentResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

func agentResources(agent *agentsv1alpha1.AgentSpec) corev1.ResourceRequirements {
	if agent.Resources != nil {
		return *agent.Resources
	}
	return defaultAgentResources()
}

// withAgentOTelAttrs adds the fleet and agent to the tenant/platform resource
// attributes every operator-built pod carries.
//
// The agent SDK reports its own `gen_ai.agent.id`, which defaults to a constant
// — so without these the spans from every agent in the fleet are
// indistinguishable, and a claim stream that cannot say which agent made a
// claim cannot be reconciled against a record that names one. Resource
// attributes ride every span the process emits, so setting them here covers
// the whole agent rather than the calls it remembers to annotate.
func withAgentOTelAttrs(env []corev1.EnvVar, p *platformv1alpha1.Platform, fleetName, agentName string) []corev1.EnvVar {
	out := withOTelResourceAttrs(env, p, platformModelFamily(p))
	for i := range out {
		if out[i].Name != otelResourceAttrsEnvName {
			continue
		}
		out[i].Value += ",agents.fleet=" + fleetName + ",agents.agent=" + agentName
	}
	return out
}

var errKEDANotInstalled = errors.New("keda.sh CRDs not installed on this cluster")

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

// agentDeploymentName is the name the operator gives the workload it renders for a
// fleet agent. The operator creates one Deployment per AgentSpec named
// <fleet>-<agent>, and a KEDA ScaledObject's scaleTargetRef must resolve to
// this exact name; anything else points at a Deployment nothing creates and
// autoscaling silently never fires.
func agentDeploymentName(fleet *agentsv1alpha1.AgentFleet, agentName string) string {
	return fleet.Name + "-" + agentName
}

// fleetScalingMinMax resolves the (min, max) replica bounds for one agent.
// min is the agent's own floor (AgentSpec.Replicas) when set, else the fleet
// scaling minimum (default 1); max is the fleet ceiling (default 10) raised to
// min when a per-agent floor exceeds it so KEDA never sees max < min.
func fleetScalingMinMax(fleet *agentsv1alpha1.AgentFleet, agent *agentsv1alpha1.AgentSpec) (int32, int32) {
	var minR, maxR int32 = 1, 10
	if fleet.Spec.Scaling.Min != nil {
		minR = *fleet.Spec.Scaling.Min
	}
	if fleet.Spec.Scaling.Max != nil {
		maxR = *fleet.Spec.Scaling.Max
	}
	if agent.Replicas != nil {
		minR = *agent.Replicas
	}
	if maxR < minR {
		maxR = minR
	}
	return minR, maxR
}

// fleetScalingTriggers builds the KEDA trigger list shared by every agent in
// the fleet: an aws-sqs-queue trigger on the fleet's work queue when
// scaling.queueUrl is set (the production path), else a CPU-utilization
// placeholder so the fleet scales sensibly during onboarding before a queue is
// wired.
func fleetScalingTriggers(fleet *agentsv1alpha1.AgentFleet, queueURL string) []any {
	if queueURL == "" {
		return []any{
			map[string]any{
				"type": "cpu",
				"metadata": map[string]any{
					"type":  "Utilization",
					"value": "70",
				},
			},
		}
	}
	region := awsRegionFromQueueURL(queueURL)
	depth := fleet.Spec.Scaling.QueueDepthTrigger
	if depth <= 0 {
		depth = 10
	}
	return []any{
		map[string]any{
			"type": "aws-sqs-queue",
			"metadata": map[string]any{
				"queueURL":    queueURL,
				"queueLength": fmt.Sprintf("%d", depth),
				"awsRegion":   region,
			},
			"authenticationRef": map[string]any{
				"name": "fleet-" + fleet.Name + "-aws",
			},
		},
	}
}

// ensureKEDAScaledObject emits one KEDA ScaledObject per agent in the fleet,
// each targeting the Deployment the operator renders for that agent
// (agentDeploymentName). There is one Deployment per agent, so a single
// fleet-wide ScaledObject could only ever scale one of them — per-agent
// ScaledObjects scale every agent's runtime. When scaling.queueUrl is set the
// TriggerAuthentication is emitted once up front (all agents reference it);
// without a queue URL each object carries a CPU-utilization placeholder.
func (r *AgentFleetReconciler) ensureKEDAScaledObject(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	if !fleet.Spec.Scaling.Enabled {
		return nil
	}
	queueURL := fleet.Spec.Scaling.QueueURL
	if queueURL != "" {
		// TriggerAuthentication has to land before any ScaledObject
		// references it. KEDA's CreateOrUpdate semantics handle the order on
		// its end, but we explicitly emit the TA first (once per fleet) to
		// avoid a transient ConfigMap-of-secret-not-found state.
		if err := r.ensureKEDATriggerAuth(ctx, tc, fleet, p); err != nil {
			return err
		}
	}
	for i := range fleet.Spec.Agents {
		if err := r.ensureAgentScaledObject(ctx, tc, fleet, &fleet.Spec.Agents[i], p, queueURL); err != nil {
			return err
		}
	}
	return nil
}

// ensureAgentScaledObject emits the ScaledObject for a single agent, targeting
// the agent Deployment (agentDeploymentName) — the workload the operator
// creates for it. Per-agent minReplicaCount honors
// AgentSpec.Replicas; maxReplicaCount is the fleet-wide ceiling.
func (r *AgentFleetReconciler) ensureAgentScaledObject(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, agent *agentsv1alpha1.AgentSpec, p *platformv1alpha1.Platform, queueURL string) error {
	name := agentDeploymentName(fleet, agent.Name)
	minR, maxR := fleetScalingMinMax(fleet, agent)
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "ScaledObject"})
	so.SetName(name)
	so.SetNamespace(PlatformNamespace(p))
	_, err := controllerutil.CreateOrUpdate(ctx, tc, so, func() error {
		so.SetLabels(map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  p.Name,
			LabelFleet:                     fleet.Name,
			LabelAgent:                     agent.Name,
		})
		spec := map[string]any{
			"scaleTargetRef": map[string]any{
				"name": name,
				"kind": "Deployment",
			},
			"minReplicaCount": int64(minR),
			"maxReplicaCount": int64(maxR),
			"triggers":        fleetScalingTriggers(fleet, queueURL),
		}
		return unstructured.SetNestedField(so.Object, spec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return errKEDANotInstalled
		}
		return fmt.Errorf("KEDA ScaledObject %s: %w", name, err)
	}
	return nil
}

// ensureKEDATriggerAuth provisions the KEDA TriggerAuthentication CR the
// aws-sqs-queue trigger references. podIdentity.provider = aws means KEDA
// resolves AWS credentials as the workload does — through the tenant
// ServiceAccount's EKS Pod Identity association — instead of using KEDA's own
// operator IAM identity. Nothing is annotated onto the ServiceAccount; the
// association is an EKS API object the operator reconciles.
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
				// This provider is the whole mechanism: with it, KEDA polls the
				// queue as the tenant ServiceAccount — the one
				// ensureTenantServiceAccount provisions, which EKS Pod Identity
				// binds to the tenant role — rather than with the operator's own
				// credentials. See docs/adr/0006-keda-pod-identity.md; the SQS
				// read the trigger needs is in the agent-iam baseline policy.
				//
				// "aws", not "aws-eks". In KEDA's aws_common.go the provider is
				// checked first and returns before the legacy identityOwner
				// switch is reached, so under this provider that trigger field
				// is never read. The ScaledObject deliberately does not set it.
				"provider": "aws",
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
// agent Deployments, KEDA ScaledObject, and fleet
// NetworkPolicy. Tenant ServiceAccount is owned by Platform finalizer.
func (r *AgentFleetReconciler) cleanupFleetResources(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) error {
	ns := PlatformNamespace(p)
	// Workload + KEDA objects live wherever the fleet reconciled them — the host for
	// the namespace tier, the virtual cluster for the vcluster tier — so they are
	// deleted through the same target client that created them.
	for _, agent := range fleet.Spec.Agents {
		base := agentDeploymentName(fleet, agent.Name)
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: base, Namespace: ns}}
		if err := tc.Delete(ctx, deploy); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete agent Deployment %s: %w", base, err)
		}
		// Per-agent KEDA ScaledObject (named after the Deployment it scales).
		// Delete it before the shared TriggerAuthentication below so KEDA can't
		// try to re-resolve a TA we're about to remove.
		so := &unstructured.Unstructured{}
		so.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "ScaledObject"})
		so.SetName(base)
		so.SetNamespace(ns)
		if err := tc.Delete(ctx, so); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
			return fmt.Errorf("delete ScaledObject %s: %w", base, err)
		}
	}
	// Fleet-wide TriggerAuthentication (one per fleet, referenced by every
	// per-agent SQS ScaledObject above).
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

// errRouteNotPublished means an agent names a route its gateway does not serve.
// Distinct from the gateway-not-ready errors because requeueing cannot fix it.
var errRouteNotPublished = errors.New("route is not published by the Platform's ModelGateway")

// fleetResult is what one reconcile pass concluded.
//
// A struct rather than more positional returns: phase, reason and message are
// all strings, and a transposition among them would report a plausible-reading
// status about the wrong thing — the same trap that turned reconcileSelf on the
// gateway side into a result type.
type fleetResult struct {
	phase       string
	readyAgents int32
	// reason and message describe a failure the fleet's own spec caused, so the
	// status can name the missing route instead of the generic wait-on-Platform
	// text every other non-Ready phase shares.
	reason  string
	message string
}

// reconcileFleetSelf is the orchestration: resolve Platform, gate on
// Ready, run k8s + external steps.
func (r *AgentFleetReconciler) reconcileFleetSelf(ctx context.Context, fleet *agentsv1alpha1.AgentFleet) (fleetResult, error) {
	platform, err := r.resolvePlatform(ctx, fleet)
	if err != nil {
		if errors.Is(err, errPlatformNotFound) {
			return fleetResult{phase: phasePending}, nil
		}
		return fleetResult{}, err
	}
	// Platform Suspended: tear down the fleet's agent Deployments + KEDA
	// scaler so no pods can serve traffic until the kill-switch is
	// cleared. The tenant SA + NetworkPolicy stay in place so a
	// recovery doesn't have to recreate them.
	if platform.Status.Phase == phaseSuspended {
		if err := r.cleanupFleetResources(ctx, r.cleanupTargetClient(ctx, platform), fleet, platform); err != nil {
			return fleetResult{}, fmt.Errorf("suspend cleanup: %w", err)
		}
		return fleetResult{phase: phaseSuspended}, nil
	}
	if platform.Status.Phase != phaseReady {
		return fleetResult{phase: phasePending}, nil
	}

	// The route contract, read before anything is written. A fleet whose agents
	// cannot be given a working base URL should emit no pods at all — a running
	// agent that fails every model call is worse than an absent one, because it
	// reports Ready.
	routes, err := publishedRoutes(ctx, r.Client, platform)
	if err != nil {
		if errors.Is(err, errGatewayNotFound) || errors.Is(err, errGatewayNotPublished) {
			// Ordering, not misconfiguration: the gateway reconciles
			// independently and may not have published yet. Requeue.
			return fleetResult{phase: phasePending}, nil
		}
		if errors.Is(err, errGatewayAmbiguous) {
			return fleetResult{phase: phaseFailed, reason: "GatewayAmbiguous", message: err.Error()}, nil
		}
		return fleetResult{}, fmt.Errorf("read route contract: %w", err)
	}

	// Resolve the target client: the host client for the namespace tier, the
	// Platform's virtual-cluster client for the vcluster tier. Workload + KEDA
	// objects (which produce the fleet's pods) land through this client so the
	// tenant's pods see the vcluster API; the fleet's host containment
	// (NetworkPolicy/Cilium egress) always stays on the host client below.
	tc, err := targetClient(ctx, r.Client, r.VCluster, platform)
	if err != nil {
		if errors.Is(err, errVClusterNotReady) {
			// vcluster still installing — nothing to write into yet; requeue.
			return fleetResult{phase: phasePending}, nil
		}
		return fleetResult{}, fmt.Errorf("resolve target client: %w", err)
	}

	if err := ensureTenantServiceAccount(ctx, tc, platform); err != nil {
		return fleetResult{}, fmt.Errorf("ensure ServiceAccount: %w", err)
	}
	if err := r.ensureFleetNetworkPolicy(ctx, fleet, platform); err != nil {
		return fleetResult{}, fmt.Errorf("ensure NetworkPolicy: %w", err)
	}
	if err := r.ensureFleetCiliumEgress(ctx, fleet, platform); err != nil {
		return fleetResult{}, fmt.Errorf("ensure CiliumNetworkPolicy: %w", err)
	}
	// No missing-CRD branch here: a Deployment is a core Kubernetes object, so
	// unlike the CRD-backed runtime this replaced there is no cluster where the
	// kind is absent and the fleet has to wait for an addon to install.
	if err := r.ensureAgentDeployments(ctx, tc, fleet, platform, routes); err != nil {
		if errors.Is(err, errRouteNotPublished) {
			// A spec error. Surfaced as status rather than returned, because a
			// returned error retries with backoff forever and says nothing to
			// whoever wrote the route name.
			return fleetResult{phase: phaseFailed, reason: "RouteNotPublished", message: err.Error()}, nil
		}
		return fleetResult{}, err
	}
	if err := r.ensureKEDAScaledObject(ctx, tc, fleet, platform); err != nil {
		if errors.Is(err, errKEDANotInstalled) {
			// KEDA absence isn't fatal — scaling is optional. Log and
			// move on; the deployment runs at the static replica count.
			return fleetResult{phase: phaseReady, readyAgents: r.observeReadyAgents(ctx, tc, fleet, platform)}, nil
		}
		return fleetResult{}, err
	}
	return fleetResult{phase: phaseReady, readyAgents: r.observeReadyAgents(ctx, tc, fleet, platform)}, nil
}

// observeReadyAgents counts the agents whose Deployment reports at least one
// ready replica.
//
// It reads the workloads back rather than returning len(spec.agents), which is
// what the field's contract requires and what the previous count did not do:
// applying a Deployment says the API server accepted it, not that anything is
// serving. A fleet whose every pod was in CrashLoopBackOff reported
// readyAgents == len(spec.agents) and phase Ready, and agents_fleet_ready_agents
// carried the same number onto the dashboards that exist to notice exactly that.
//
// A read failure yields the count observed so far rather than an error. This
// runs after the Deployments have been applied successfully, so the fleet is
// reconciled either way, and failing the whole pass on a status read would turn
// a reporting gap into a reconcile loop. An agent whose Deployment cannot be
// read is simply not counted as ready — the direction that under-reports rather
// than over-reports, which is the safe one for a number a pager reads.
func (r *AgentFleetReconciler) observeReadyAgents(ctx context.Context, tc client.Client, fleet *agentsv1alpha1.AgentFleet, p *platformv1alpha1.Platform) int32 {
	ns := PlatformNamespace(p)
	var ready int32
	for _, agent := range fleet.Spec.Agents {
		var dep appsv1.Deployment
		key := types.NamespacedName{Namespace: ns, Name: agentDeploymentName(fleet, agent.Name)}
		if err := tc.Get(ctx, key, &dep); err != nil {
			continue
		}
		if dep.Status.ReadyReplicas > 0 {
			ready++
		}
	}
	return ready
}

//nolint:dupl // status writeback mirrors the other reconcilers by design
func (r *AgentFleetReconciler) applyFleetStatus(ctx context.Context, fleet *agentsv1alpha1.AgentFleet, res fleetResult) error {
	fleet.Status.Phase = res.phase
	fleet.Status.ReadyAgents = res.readyAgents
	fleet.Status.ObservedGeneration = fleet.Generation
	fleetReadyAgents.WithLabelValues(fleet.Namespace, fleet.Spec.PlatformRef.Name, fleet.Name).Set(float64(res.readyAgents))
	cond := metav1.Condition{
		Type:               "AgentsReconciled",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("%d of %d agent(s) ready", res.readyAgents, safeAgentCount(fleet)),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: fleet.Generation,
	}
	switch {
	case res.phase == phaseReady:
		// healthy — condition stays True
	case res.reason != "":
		// A spec error the fleet's own manifest caused. It gets its own reason
		// and the message that names what is wrong, because the generic
		// wait-on-Platform text would send the reader to the wrong object.
		cond.Status = metav1.ConditionFalse
		cond.Reason = res.reason
		cond.Message = res.message
	case res.phase == phaseSuspended:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonPlatformSuspended
		cond.Message = "Platform kill-switch fired; fleet scaled to zero"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = phasePending
		cond.Message = "waiting on Platform readiness"
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
