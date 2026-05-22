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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
)

const sandboxPoolFinalizer = "agents.stxkxs.io/sandboxpool-finalizer"

// defaultSandboxWorkerImage is used when SandboxPool.spec.image is empty.
const defaultSandboxWorkerImage = "ghcr.io/nanohype/eks-agent-platform/sandbox-worker:latest"

// sandboxNodeLabel keys both the label and the NoSchedule taint on the
// dedicated Karpenter sandbox node pool (eks-gitops karpenter-resources).
// Worker pods carry the matching nodeSelector + toleration.
const sandboxNodeLabel = "agents.stxkxs.io/sandbox"

// metadataServiceCIDR is the cloud instance-metadata endpoint. Agent tool
// calls must never reach it, so the worker NetworkPolicy excludes it from
// the egress allow range.
const metadataServiceCIDR = "169.254.169.254/32"

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

// sandboxResourceName names the per-pool Deployment and NetworkPolicy.
func sandboxResourceName(pool *agentsv1alpha1.SandboxPool) string {
	return "sandbox-" + pool.Name
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

// ensureWorkerDeployment reconciles the Deployment of `ant beta:worker`
// pods. Worker pods carry the toleration + nodeSelector for the dedicated
// sandbox node pool. Replicas are set on create only — once the autoscaler
// owns the Deployment it must not be fought on subsequent reconciles.
func (r *SandboxPoolReconciler) ensureWorkerDeployment(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxResourceName(pool), Namespace: PlatformNamespace(p)},
	}
	labels := sandboxPodLabels(pool, p)
	selector := map[string]string{"eks-agent-platform/sandboxpool": pool.Name}
	var minReplicas int32 = 1
	if pool.Spec.Scaling.MinReplicas != nil {
		minReplicas = *pool.Spec.Scaling.MinReplicas
	}
	envKeyRef := pool.Spec.EnvironmentKeySecret
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		// Set replicas only on create; the autoscaler owns it afterward.
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Replicas = ptrTo(minReplicas)
		}
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptrTo(false),
				NodeSelector:                 map[string]string{sandboxNodeLabel: "true"},
				Tolerations: []corev1.Toleration{{
					Key:      sandboxNodeLabel,
					Operator: corev1.TolerationOpEqual,
					Value:    "true",
					Effect:   corev1.TaintEffectNoSchedule,
				}},
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   ptrTo(true),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:  "worker",
					Image: workerImage(pool),
					Env: []corev1.EnvVar{
						{Name: "ANTHROPIC_ENVIRONMENT_ID", Value: pool.Spec.EnvironmentID},
						{Name: "ANTHROPIC_ENVIRONMENT_KEY", ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &envKeyRef,
						}},
					},
					Resources: pool.Spec.Resources,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptrTo(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
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
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)
	httpsPort := intstr.FromInt(443)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = sandboxPodLabels(pool, p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"eks-agent-platform/sandboxpool": pool.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}, {Protocol: &tcp, Port: &dnsPort}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{metadataServiceCIDR}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &httpsPort}},
				},
			},
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

// cleanupSandboxResources is the finalizer counterpart: removes the
// worker Deployment and the NetworkPolicy.
func (r *SandboxPoolReconciler) cleanupSandboxResources(ctx context.Context, pool *agentsv1alpha1.SandboxPool, p *agentsv1alpha1.Platform) error {
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
// reconciles the worker Deployment + NetworkPolicy. Returns (phase,
// readyWorkers, error).
func (r *SandboxPoolReconciler) reconcileSandboxPoolSelf(ctx context.Context, pool *agentsv1alpha1.SandboxPool) (string, int32, error) {
	platform, err := r.resolveSandboxPlatform(ctx, pool)
	if err != nil {
		if errors.Is(err, errPlatformNotFound) {
			return phasePending, 0, nil
		}
		return "", 0, err
	}
	// Platform Suspended: tear down the workers so no sandbox runs until
	// the kill-switch is cleared. The NetworkPolicy stays in place.
	if platform.Status.Phase == phaseSuspended {
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
