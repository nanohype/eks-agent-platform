/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// defaultAgentSandboxTTL is the fallback retention (seconds) for an
// AgentSandbox after its session pod terminates, used when the spec leaves
// TTLSecondsAfterFinished unset.
const defaultAgentSandboxTTL int32 = 3600

// resolveAgentSandboxPlatform fetches the AgentSandbox's referenced Platform.
func (r *AgentSandboxReconciler) resolveAgentSandboxPlatform(ctx context.Context, box *agentsv1alpha1.AgentSandbox) (*platformv1alpha1.Platform, error) {
	var p platformv1alpha1.Platform
	key := types.NamespacedName{Namespace: box.Namespace, Name: box.Spec.PlatformRef.Name}
	if err := r.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errPlatformNotFound
		}
		return nil, fmt.Errorf("get platform %s: %w", key, err)
	}
	return &p, nil
}

// agentSandboxResourceName names the per-AgentSandbox session pod and its
// NetworkPolicy.
func agentSandboxResourceName(box *agentsv1alpha1.AgentSandbox) string {
	return "session-" + box.Name
}

// agentSandboxLabels are stamped on the session pod and matched by the
// NetworkPolicy podSelector. The `agentsandbox` label is the selector.
func agentSandboxLabels(box *agentsv1alpha1.AgentSandbox, p *platformv1alpha1.Platform) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":    "eks-agent-platform",
		"app.kubernetes.io/component":     "agent-sandbox",
		"eks-agent-platform/platform":     p.Name,
		"eks-agent-platform/agentsandbox": box.Name,
	}
}

// ensureSessionPod creates the hardened, single-use session pod. The pod is
// create-only: a session runs once and a pod spec is immutable, so once it
// exists the operator never rewrites it.
func (r *AgentSandboxReconciler) ensureSessionPod(ctx context.Context, box *agentsv1alpha1.AgentSandbox, p *platformv1alpha1.Platform) error {
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: agentSandboxResourceName(box)}
	var existing corev1.Pod
	if err := r.Get(ctx, key, &existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get session pod: %w", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentSandboxResourceName(box),
			Namespace: PlatformNamespace(p),
			Labels:    agentSandboxLabels(box, p),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ServiceAccountName:           tenantSAName,
			AutomountServiceAccountToken: ptrTo(false),
			RuntimeClassName:             box.Spec.RuntimeClassName,
			NodeSelector:                 sandboxNodeSelector(),
			Tolerations:                  sandboxTolerations(),
			SecurityContext:              restrictedPodSecurityContext(),
			Containers: []corev1.Container{{
				Name:            "session",
				Image:           box.Spec.Image,
				Command:         box.Spec.Command,
				Args:            box.Spec.Args,
				Env:             box.Spec.Env,
				Resources:       box.Spec.Resources,
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts:    []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "workspace",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create session pod: %w", err)
	}
	return nil
}

// ensureAgentSandboxNetworkPolicy installs the default-deny NetworkPolicy
// for the session pod: ingress denied entirely; egress only to kube-dns and
// outbound HTTPS (Bedrock inference + the AWS STS endpoint for IRSA), with
// the cloud instance-metadata endpoint excluded.
func (r *AgentSandboxReconciler) ensureAgentSandboxNetworkPolicy(ctx context.Context, box *agentsv1alpha1.AgentSandbox, p *platformv1alpha1.Platform) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: agentSandboxResourceName(box), Namespace: PlatformNamespace(p)},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = agentSandboxLabels(box, p)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"eks-agent-platform/agentsandbox": box.Name},
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

// deleteSessionPod removes the session pod — used by the finalizer and the
// Platform-Suspended kill-switch branch.
func (r *AgentSandboxReconciler) deleteSessionPod(ctx context.Context, box *agentsv1alpha1.AgentSandbox, p *platformv1alpha1.Platform) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: agentSandboxResourceName(box), Namespace: PlatformNamespace(p)},
	}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete session pod: %w", err)
	}
	return nil
}

// cleanupAgentSandbox is the finalizer counterpart: removes the session pod
// and the NetworkPolicy.
func (r *AgentSandboxReconciler) cleanupAgentSandbox(ctx context.Context, box *agentsv1alpha1.AgentSandbox, p *platformv1alpha1.Platform) error {
	if err := r.deleteSessionPod(ctx, box, p); err != nil {
		return err
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: agentSandboxResourceName(box), Namespace: PlatformNamespace(p)},
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete network policy: %w", err)
	}
	return nil
}

// reconcileAgentSandboxSelf resolves the Platform, gates on readiness, and
// reconciles the session pod + NetworkPolicy. Returns (phase, pod, error);
// pod is nil when no pod exists yet (Pending) or it was torn down
// (Suspended).
func (r *AgentSandboxReconciler) reconcileAgentSandboxSelf(ctx context.Context, box *agentsv1alpha1.AgentSandbox) (string, *corev1.Pod, error) {
	platform, err := r.resolveAgentSandboxPlatform(ctx, box)
	if err != nil {
		if errors.Is(err, errPlatformNotFound) {
			return phasePending, nil, nil
		}
		return "", nil, err
	}
	// Platform Suspended: tear the session pod down until the kill-switch
	// clears. The NetworkPolicy stays in place.
	if platform.Status.Phase == phaseSuspended {
		if err := r.deleteSessionPod(ctx, box, platform); err != nil {
			return "", nil, fmt.Errorf("suspend cleanup: %w", err)
		}
		return phaseSuspended, nil, nil
	}
	if platform.Status.Phase != phaseReady {
		return phasePending, nil, nil
	}

	if err := ensureTenantServiceAccount(ctx, r.Client, platform); err != nil {
		return "", nil, fmt.Errorf("ensure ServiceAccount: %w", err)
	}
	if err := r.ensureAgentSandboxNetworkPolicy(ctx, box, platform); err != nil {
		return "", nil, err
	}
	if err := r.ensureSessionPod(ctx, box, platform); err != nil {
		return "", nil, err
	}

	var pod corev1.Pod
	key := types.NamespacedName{Namespace: PlatformNamespace(platform), Name: agentSandboxResourceName(box)}
	if err := r.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return phasePending, nil, nil
		}
		return "", nil, fmt.Errorf("get session pod: %w", err)
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return phaseSucceeded, &pod, nil
	case corev1.PodFailed:
		return phaseFailed, &pod, nil
	case corev1.PodRunning:
		return phaseRunning, &pod, nil
	default: // Pending, Unknown
		return phasePending, &pod, nil
	}
}

// reconcileTTL garbage-collects a finished AgentSandbox: once its session
// pod has been terminal for spec.ttlSecondsAfterFinished, the AgentSandbox
// is deleted, which cascades through the finalizer cleanup. It returns a
// non-zero duration when the caller should requeue to delete later.
func (r *AgentSandboxReconciler) reconcileTTL(ctx context.Context, box *agentsv1alpha1.AgentSandbox) (time.Duration, error) {
	if box.Status.CompletedAt == nil {
		return 0, nil
	}
	ttl := defaultAgentSandboxTTL
	if box.Spec.TTLSecondsAfterFinished != nil {
		ttl = *box.Spec.TTLSecondsAfterFinished
	}
	remaining := time.Until(box.Status.CompletedAt.Add(time.Duration(ttl) * time.Second))
	if remaining > 0 {
		return remaining, nil
	}
	if err := r.Delete(ctx, box); err != nil && !apierrors.IsNotFound(err) {
		return 0, fmt.Errorf("ttl garbage-collect: %w", err)
	}
	return 0, nil
}

// applyAgentSandboxStatus writes phase, the pod fields, and the condition.
func (r *AgentSandboxReconciler) applyAgentSandboxStatus(ctx context.Context, box *agentsv1alpha1.AgentSandbox, phase string, pod *corev1.Pod) error {
	box.Status.Phase = phase
	box.Status.PodName = ""
	box.Status.PodPhase = ""
	if pod != nil {
		box.Status.PodName = pod.Name
		box.Status.PodPhase = string(pod.Status.Phase)
	}
	box.Status.ObservedGeneration = box.Generation
	if (phase == phaseSucceeded || phase == phaseFailed) && box.Status.CompletedAt == nil {
		now := metav1.Now()
		box.Status.CompletedAt = &now
	}
	cond := metav1.Condition{
		Type:               "SessionReconciled",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("session pod %s", phase),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: box.Generation,
	}
	switch phase {
	case phaseRunning, phaseSucceeded:
		// healthy — condition stays True
	case phaseFailed:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "SessionFailed"
		cond.Message = "session pod failed"
	case phaseSuspended:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonPlatformSuspended
		cond.Message = "Platform kill-switch fired; session pod torn down"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = phasePending
		cond.Message = "waiting on Platform readiness or pod scheduling"
	}
	upsertCondition(&box.Status.Conditions, cond)
	return r.Status().Update(ctx, box)
}
