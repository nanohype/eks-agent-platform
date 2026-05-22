/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"

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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
)

const sandboxPoolFinalizer = "agents.stxkxs.io/sandboxpool-finalizer"

// defaultSandboxWorkerImage is used when SandboxPool.spec.image is empty.
const defaultSandboxWorkerImage = "ghcr.io/nanohype/eks-agent-platform/sandbox-worker:latest"

// metricsShimPort is the port the metrics-shim binary listens on; the KEDA
// bridge Deployment, Service, and NetworkPolicy all reference it.
const metricsShimPort = 8080

// ptrTo returns a pointer to v — for the optional *bool / *int32 fields in
// pod and Deployment specs.
func ptrTo[T any](v T) *T { return &v }

// resolveSandboxPlatform fetches the SandboxPool's referenced Platform.
func (r *SandboxPoolReconciler) resolveSandboxPlatform(ctx context.Context, pool *agentsv1alpha1.SandboxPool) (*agentsv1alpha1.Platform, error) {
	var p agentsv1alpha1.Platform
	key := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Spec.PlatformRef.Name}
	if err := r.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errPlatformNotFound
		}
		return nil, fmt.Errorf("get platform %s: %w", key, err)
	}
	return &p, nil
}

// sandboxResourceName names the per-pool Deployment, NetworkPolicy, and
// KEDA ScaledObject.
func sandboxResourceName(pool *agentsv1alpha1.SandboxPool) string {
	return "sandbox-" + pool.Name
}

// metricsBridgeName names the per-pool KEDA metrics bridge — its
// Deployment, Service, and NetworkPolicy. The 16-character overhead
// ("sandbox-" + "-metrics") against the 63-character name limit caps a
// pool name at 47 characters.
func metricsBridgeName(pool *agentsv1alpha1.SandboxPool) string {
	return sandboxResourceName(pool) + "-metrics"
}

// workerImage returns the configured image or the platform default.
func workerImage(pool *agentsv1alpha1.SandboxPool) string {
	if pool.Spec.Image != "" {
		return pool.Spec.Image
	}
	return defaultSandboxWorkerImage
}

// sandboxPodLabels are stamped on the Deployment, the pod template, and
// the NetworkPolicy podSelector. The `sandboxpool` label is the selector.
func sandboxPodLabels(pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":   "eks-agent-platform",
		"app.kubernetes.io/component":    "sandbox-worker",
		"eks-agent-platform/platform":    p.Name,
		"eks-agent-platform/sandboxpool": pool.Name,
	}
}

// metricsBridgeLabels are stamped on the bridge Deployment, its pod
// template, the Service, and the NetworkPolicy. The `metrics-bridge`
// label is the selector for all three.
func metricsBridgeLabels(pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":      "eks-agent-platform",
		"app.kubernetes.io/component":       "sandbox-metrics-bridge",
		"eks-agent-platform/platform":       p.Name,
		"eks-agent-platform/sandboxpool":    pool.Name,
		"eks-agent-platform/metrics-bridge": pool.Name,
	}
}

// staticWorkerReplicas is the worker count when KEDA is not managing the
// Deployment: the configured minReplicas, floored at 1. minReplicas: 0 is
// meaningful only with the autoscaler — without that floor a pool would be
// created dead at zero.
func staticWorkerReplicas(pool *agentsv1alpha1.SandboxPool) int32 {
	var minReplicas int32 = 1
	if pool.Spec.Scaling.MinReplicas != nil {
		minReplicas = *pool.Spec.Scaling.MinReplicas
	}
	if minReplicas < 1 {
		return 1
	}
	return minReplicas
}

// ensureWorkerDeployment reconciles the Deployment of `ant beta:worker`
// pods. Worker pods carry the toleration + nodeSelector for the dedicated
// sandbox node pool. Replicas are set on create only — once the KEDA
// ScaledObject exists the autoscaler owns the count.
func (r *SandboxPoolReconciler) ensureWorkerDeployment(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxResourceName(pool), Namespace: PlatformNamespace(p)},
	}
	labels := sandboxPodLabels(pool, p)
	selector := map[string]string{"eks-agent-platform/sandboxpool": pool.Name}
	envKeyRef := pool.Spec.EnvironmentKeySecret
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		// Set replicas only on create; once the ScaledObject exists KEDA
		// owns the count. staticWorkerReplicas floors minReplicas at 1 so
		// a minReplicas:0 pool (valid only with autoscaling) is not born
		// dead; teardownAutoscaling reasserts the same floor.
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Replicas = ptrTo(staticWorkerReplicas(pool))
		}
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptrTo(false),
				RuntimeClassName:             pool.Spec.RuntimeClassName,
				NodeSelector:                 sandboxNodeSelector(),
				Tolerations:                  sandboxTolerations(),
				SecurityContext:              restrictedPodSecurityContext(),
				Containers: []corev1.Container{{
					Name:  "worker",
					Image: workerImage(pool),
					Env: []corev1.EnvVar{
						{Name: "ANTHROPIC_ENVIRONMENT_ID", Value: pool.Spec.EnvironmentID},
						{Name: "ANTHROPIC_ENVIRONMENT_KEY", ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &envKeyRef,
						}},
					},
					Resources:       pool.Spec.Resources,
					SecurityContext: restrictedContainerSecurityContext(),
					VolumeMounts:    []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
				}},
				Volumes: []corev1.Volume{{
					Name:         "workspace",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				}},
			},
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("worker deployment: %w", err)
	}
	return nil
}

// ensureSandboxNetworkPolicy installs a NetworkPolicy selecting worker
// pods: ingress is denied entirely; egress narrows to kube-dns and
// outbound HTTPS (the worker's poll loop to api.anthropic.com). A plain
// NetworkPolicy cannot match an FQDN, so the HTTPS rule is any address
// except the cloud instance-metadata endpoint, which agent tool calls
// must never reach.
func (r *SandboxPoolReconciler) ensureSandboxNetworkPolicy(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxResourceName(pool), Namespace: PlatformNamespace(p)},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = sandboxPodLabels(pool, p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"eks-agent-platform/sandboxpool": pool.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress:      sandboxEgressRules(),
			// Ingress nil with PolicyTypes including Ingress = deny-all.
			Ingress: nil,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("network policy: %w", err)
	}
	return nil
}

// workerReadyCount reads the worker Deployment's ready replica count. A
// missing Deployment (not yet created) reports zero, not an error.
func (r *SandboxPoolReconciler) workerReadyCount(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) (int32, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: sandboxResourceName(pool)}
	if err := r.Get(ctx, key, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("get worker deployment: %w", err)
	}
	return dep.Status.ReadyReplicas, nil
}

// deleteWorkerDeployment removes the worker Deployment — used both by the
// finalizer and when the Platform is Suspended (stop the workers, leave
// the NetworkPolicy so a recovery doesn't reopen the namespace).
func (r *SandboxPoolReconciler) deleteWorkerDeployment(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxResourceName(pool), Namespace: PlatformNamespace(p)},
	}
	if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete worker deployment: %w", err)
	}
	return nil
}

// reconcileAutoscaling brings the SandboxPool's KEDA queue-depth
// autoscaling to its desired state. Autoscaling needs an org API key (the
// metrics bridge calls the Managed Agents work-stats endpoint) and the
// operator image to run that bridge. Without either, the pool runs at a
// static replica count and any existing autoscaling machinery is removed.
func (r *SandboxPoolReconciler) reconcileAutoscaling(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	if pool.Spec.APIKeySecret == nil || r.ShimImage == "" {
		if err := r.teardownAutoscaling(ctx, pool, p); err != nil {
			return err
		}
		return r.resetWorkerReplicas(ctx, pool, p)
	}
	if err := r.ensureMetricsBridgeDeployment(ctx, pool, p); err != nil {
		return err
	}
	if err := r.ensureMetricsBridgeService(ctx, pool, p); err != nil {
		return err
	}
	if err := r.ensureMetricsBridgeNetworkPolicy(ctx, pool, p); err != nil {
		return err
	}
	return r.ensureSandboxScaledObject(ctx, pool, p)
}

// ensureMetricsBridgeDeployment reconciles the metrics-shim Deployment — a
// single-replica HTTP bridge that holds the org API key and re-serves the
// Managed Agents work-queue depth for KEDA's metrics-api scaler. It runs
// on ordinary nodes: it is operator infrastructure, not sandbox work.
func (r *SandboxPoolReconciler) ensureMetricsBridgeDeployment(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: metricsBridgeName(pool), Namespace: PlatformNamespace(p)},
	}
	labels := metricsBridgeLabels(pool, p)
	selector := map[string]string{"eks-agent-platform/metrics-bridge": pool.Name}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Replicas = ptrTo(int32(1))
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptrTo(false),
				SecurityContext:              restrictedPodSecurityContext(),
				Containers: []corev1.Container{{
					Name:    "metrics-shim",
					Image:   r.ShimImage,
					Command: []string{"/metrics-shim"},
					Env: []corev1.EnvVar{
						{Name: "ANTHROPIC_ENVIRONMENT_ID", Value: pool.Spec.EnvironmentID},
						{Name: "ANTHROPIC_API_KEY", ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: pool.Spec.APIKeySecret,
						}},
					},
					Ports: []corev1.ContainerPort{{
						Name:          "http",
						ContainerPort: metricsShimPort,
						Protocol:      corev1.ProtocolTCP,
					}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(metricsShimPort)},
						},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(metricsShimPort)},
						},
						InitialDelaySeconds: 10,
						PeriodSeconds:       20,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
					},
					SecurityContext: restrictedContainerSecurityContext(),
				}},
			},
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("metrics bridge deployment: %w", err)
	}
	return nil
}

// ensureMetricsBridgeService exposes the metrics bridge as a ClusterIP
// Service that KEDA's metrics-api scaler resolves by cluster DNS.
func (r *SandboxPoolReconciler) ensureMetricsBridgeService(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: metricsBridgeName(pool), Namespace: PlatformNamespace(p)},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = metricsBridgeLabels(pool, p)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = map[string]string{"eks-agent-platform/metrics-bridge": pool.Name}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromInt(metricsShimPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		return nil
	})
	if err != nil {
		return fmt.Errorf("metrics bridge service: %w", err)
	}
	return nil
}

// ensureMetricsBridgeNetworkPolicy installs a NetworkPolicy selecting the
// metrics bridge pod. The tenant namespace is default-deny, so the bridge
// needs explicit rules: ingress only from the keda namespace (its scaler
// scrapes the bridge), egress only to kube-dns and outbound HTTPS (the
// call to api.anthropic.com), the instance-metadata endpoint excluded.
func (r *SandboxPoolReconciler) ensureMetricsBridgeNetworkPolicy(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: metricsBridgeName(pool), Namespace: PlatformNamespace(p)},
	}
	tcp := corev1.ProtocolTCP
	shimPort := intstr.FromInt(metricsShimPort)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = metricsBridgeLabels(pool, p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"eks-agent-platform/metrics-bridge": pool.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "keda"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &shimPort}},
			}},
			Egress: sandboxEgressRules(),
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("metrics bridge network policy: %w", err)
	}
	return nil
}

// ensureSandboxScaledObject emits the KEDA ScaledObject that scales the
// worker Deployment on Managed Agents work-queue depth. Its metrics-api
// trigger reads `depth` from the in-cluster metrics bridge — KEDA's
// scaler cannot send the three headers the Anthropic endpoint needs, so
// the bridge stands in. A cluster without KEDA installed is not an error.
func (r *SandboxPoolReconciler) ensureSandboxScaledObject(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	var minR, maxR int32 = 1, 10
	if pool.Spec.Scaling.MinReplicas != nil {
		minR = *pool.Spec.Scaling.MinReplicas
	}
	if pool.Spec.Scaling.MaxReplicas != nil {
		maxR = *pool.Spec.Scaling.MaxReplicas
	}
	target := pool.Spec.Scaling.QueueDepthTarget
	if target < 1 {
		target = 5
	}
	bridgeURL := fmt.Sprintf("http://%s.%s.svc.cluster.local/", metricsBridgeName(pool), PlatformNamespace(p))

	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "ScaledObject"})
	so.SetName(sandboxResourceName(pool))
	so.SetNamespace(PlatformNamespace(p))
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, so, func() error {
		so.SetLabels(sandboxPodLabels(pool, p))
		spec := map[string]any{
			"scaleTargetRef": map[string]any{
				"name": sandboxResourceName(pool),
				"kind": "Deployment",
			},
			"minReplicaCount": int64(minR),
			"maxReplicaCount": int64(maxR),
			"triggers": []any{
				map[string]any{
					"type": "metrics-api",
					"metadata": map[string]any{
						"url":           bridgeURL,
						"valueLocation": "depth",
						"targetValue":   fmt.Sprintf("%d", target),
						"format":        "json",
					},
				},
			},
		}
		return unstructured.SetNestedField(so.Object, spec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return errKEDANotInstalled
		}
		return fmt.Errorf("KEDA ScaledObject %s: %w", sandboxResourceName(pool), err)
	}
	return nil
}

// teardownAutoscaling removes the KEDA ScaledObject and the metrics bridge
// (Service, NetworkPolicy, Deployment). The ScaledObject goes first so
// KEDA stops adjusting the worker count before the machinery behind it
// disappears. Missing objects — and a cluster with no KEDA CRDs — are not
// errors. It does not touch the worker Deployment's replica count; the
// disable path pairs it with resetWorkerReplicas.
func (r *SandboxPoolReconciler) teardownAutoscaling(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	ns := PlatformNamespace(p)
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{Group: kedaGV.Group, Version: kedaGV.Version, Kind: "ScaledObject"})
	so.SetName(sandboxResourceName(pool))
	so.SetNamespace(ns)
	if err := r.Delete(ctx, so); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
		return fmt.Errorf("delete ScaledObject: %w", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: metricsBridgeName(pool), Namespace: ns},
	}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete metrics bridge service: %w", err)
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: metricsBridgeName(pool), Namespace: ns},
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete metrics bridge network policy: %w", err)
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: metricsBridgeName(pool), Namespace: ns},
	}
	if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete metrics bridge deployment: %w", err)
	}
	return nil
}

// resetWorkerReplicas reasserts the static worker count after autoscaling
// is disabled. KEDA leaves the Deployment at whatever count it last set,
// so removing the ScaledObject must restore a fixed value or the pool
// strands at a stale — possibly zero — replica count.
func (r *SandboxPoolReconciler) resetWorkerReplicas(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: sandboxResourceName(pool)}
	if err := r.Get(ctx, key, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get worker deployment: %w", err)
	}
	want := staticWorkerReplicas(pool)
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == want {
		return nil
	}
	dep.Spec.Replicas = ptrTo(want)
	if err := r.Update(ctx, &dep); err != nil {
		return fmt.Errorf("reset worker replicas: %w", err)
	}
	return nil
}

// cleanupSandboxResources is the finalizer counterpart: removes the
// autoscaling machinery, the worker Deployment, and the NetworkPolicy.
func (r *SandboxPoolReconciler) cleanupSandboxResources(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	if err := r.teardownAutoscaling(ctx, pool, p); err != nil {
		return err
	}
	if err := r.deleteWorkerDeployment(ctx, pool, p); err != nil {
		return err
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxResourceName(pool), Namespace: PlatformNamespace(p)},
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete network policy: %w", err)
	}
	return nil
}

// reconcileSandboxPoolSelf resolves the Platform, gates on readiness, and
// reconciles the worker Deployment, NetworkPolicy, and KEDA autoscaling.
// Returns (phase, readyWorkers, error).
func (r *SandboxPoolReconciler) reconcileSandboxPoolSelf(ctx context.Context, pool *agentsv1alpha1.SandboxPool) (string, int32, error) {
	platform, err := r.resolveSandboxPlatform(ctx, pool)
	if err != nil {
		if errors.Is(err, errPlatformNotFound) {
			return phasePending, 0, nil
		}
		return "", 0, err
	}
	// Platform Suspended: tear down the workers and the autoscaling
	// machinery so no sandbox runs until the kill-switch is cleared. The
	// NetworkPolicy stays in place.
	if platform.Status.Phase == phaseSuspended {
		if err := r.teardownAutoscaling(ctx, pool, platform); err != nil {
			return "", 0, fmt.Errorf("suspend cleanup: %w", err)
		}
		if err := r.deleteWorkerDeployment(ctx, pool, platform); err != nil {
			return "", 0, fmt.Errorf("suspend cleanup: %w", err)
		}
		return phaseSuspended, 0, nil
	}
	if platform.Status.Phase != phaseReady {
		return phasePending, 0, nil
	}

	if err := r.ensureSandboxNetworkPolicy(ctx, pool, platform); err != nil {
		return "", 0, err
	}
	if err := r.ensureWorkerDeployment(ctx, pool, platform); err != nil {
		return "", 0, err
	}
	// errKEDANotInstalled is non-fatal: the metrics bridge still applied;
	// the worker Deployment just runs at its static replica count.
	if err := r.reconcileAutoscaling(ctx, pool, platform); err != nil && !errors.Is(err, errKEDANotInstalled) {
		return "", 0, err
	}
	ready, err := r.workerReadyCount(ctx, pool, platform)
	if err != nil {
		return "", 0, err
	}
	return phaseReady, ready, nil
}

// applySandboxPoolStatus writes phase, ready count, and the condition.
//
//nolint:dupl // status writeback mirrors the other reconcilers by design
func (r *SandboxPoolReconciler) applySandboxPoolStatus(ctx context.Context, pool *agentsv1alpha1.SandboxPool, phase string, readyWorkers int32) error {
	pool.Status.Phase = phase
	pool.Status.ReadyWorkers = readyWorkers
	pool.Status.ObservedGeneration = pool.Generation
	cond := metav1.Condition{
		Type:               "WorkersReconciled",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("%d worker(s) ready", readyWorkers),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: pool.Generation,
	}
	switch phase {
	case phaseReady:
		// healthy — condition stays True
	case phaseSuspended:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PlatformSuspended"
		cond.Message = "Platform kill-switch fired; sandbox workers torn down"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Pending"
		cond.Message = "waiting on Platform readiness"
	}
	upsertCondition(&pool.Status.Conditions, cond)
	return r.Status().Update(ctx, pool)
}
