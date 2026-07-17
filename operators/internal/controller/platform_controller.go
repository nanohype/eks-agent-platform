/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// PlatformReconciler reconciles Platform CRs. It owns:
//   - the per-Platform tenant Namespace (with Pod Security Standards label),
//   - ResourceQuota + LimitRange + default-deny NetworkPolicy in that ns,
//   - the ArgoCD AppProject scoped to that namespace + Platform source repos,
//   - the per-Platform tenant IAM role (baseline attachment + the
//     bedrock-model-scoping inline policy reconciled from spec.identity),
//     KMS grant, and S3 bucket policy.
//
// The k8s-side reconciliation runs first and unconditionally; AWS-side
// state is reconciled behind interface-injected clients (IAM/KMS/S3) that
// can be nil in tests, so the reconciler stays unit-testable.
type PlatformReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Concurrency int

	// NetworkEngine ("cilium"|"kubernetes") selects whether tenant egress is a
	// CiliumNetworkPolicy (required to allow the host-entity Pod Identity creds
	// endpoint) or a vanilla NetworkPolicy. Wired by main.go from the chart.
	NetworkEngine string

	// AWS — wired by main.go from operatorconfig + awsclients. May be nil
	// in unit-test paths that exercise only k8s-side reconciliation.
	IAM awsclients.IAM
	EKS awsclients.EKS
	KMS awsclients.KMS
	S3  awsclients.S3

	// IAMCfg carries the SSM-resolved values the IAM step needs:
	// TenantIAMPath, TenantBaselinePolicyARN, ClusterName, Environment.
	IAMCfg IAMConfig
	// AWSCfg carries the SSM-resolved values the KMS + S3 steps need:
	// DataKMSKeyARN, ArtifactsBucketName, Environment.
	AWSCfg PlatformAWSConfig

	// VCluster builds the per-Platform virtual-cluster client for the vcluster
	// isolation tier. nil in the namespace tier and in k8s-only test paths; the
	// vcluster tier fails closed when it's nil (never silently downgrades). See
	// docs/adr/0009-vcluster-isolation-tier.md.
	VCluster VClusterClientFactory
	// VClusterCfg pins the vcluster chart coordinates + in-vcluster init-charts.
	VClusterCfg VClusterConfig

	// bucketPolicyMu serializes the read-modify-write of the SHARED artifacts
	// bucket policy across concurrent reconciles. That policy is one document
	// holding a statement per tenant; with MaxConcurrentReconciles > 1 two
	// Platform reconciles could otherwise interleave Get→mutate→Put and
	// silently drop a peer tenant's statement. The operator runs as a single
	// leader (leader election), so a process-local mutex is sufficient.
	bucketPolicyMu sync.Mutex
}

// +kubebuilder:rbac:groups=platform.nanohype.dev,resources=platforms,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=platform.nanohype.dev,resources=platforms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.nanohype.dev,resources=platforms/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=argoproj.io,resources=appprojects;applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=users,verbs=impersonate
// vcluster tier: read the vcluster kubeconfig Secret + write the ArgoCD cluster-
// registration Secret; discover the syncer-created host ServiceAccount + drain-
// check synced Pods; create the tenant ServiceAccount on the host tier.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile drives a Platform CR toward its desired state.
func (r *PlatformReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("platform", req.NamespacedName)

	platform, err := r.fetchPlatform(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if platform == nil {
		// Deleted while we were queued; nothing to do.
		return ctrl.Result{}, nil
	}

	// Finalizer-driven cleanup. When DeletionTimestamp is set we tear down
	// the tenant namespace + AppProject + IAM role (resources the kube GC
	// can't reap via OwnerReferences from a namespaced parent), then drop
	// the finalizer.
	if !platform.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(platform, finalizerName) {
			// vcluster tier: tear the virtual cluster down first (reverse of
			// provisioning) and gate the finalizer on it being fully drained, so a
			// deleted Platform can orphan neither the vcluster nor the host objects
			// its syncer created (nor the synced-SA Pod Identity association on the
			// AWS side, which a namespace delete alone will not reap).
			if platform.Spec.Isolation == isolationVCluster {
				if err := r.cleanupVClusterResources(ctx, platform, r.IAMCfg); err != nil {
					logger.Error(err, "vcluster cleanup incomplete; will retry")
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
			}
			if err := r.cleanupTenantResources(ctx, platform); err != nil {
				logger.Error(err, "k8s cleanup failed; will retry")
				return ctrl.Result{}, err
			}
			if err := r.revokeKmsGrant(ctx, platform, r.AWSCfg); err != nil {
				logger.Error(err, "KMS grant revocation failed; will retry")
				return ctrl.Result{}, err
			}
			if err := r.removeBucketPolicyStatements(ctx, platform, r.AWSCfg); err != nil {
				logger.Error(err, "bucket policy cleanup failed; will retry")
				return ctrl.Result{}, err
			}
			if err := r.deleteIamRole(ctx, platform, r.IAMCfg); err != nil {
				logger.Error(err, "IAM role cleanup failed; will retry")
				return ctrl.Result{}, err
			}
			// Attribution resources (no-ops when attribution was never enabled).
			if err := r.deleteSessionRole(ctx, platform, r.IAMCfg.ClusterName); err != nil {
				logger.Error(err, "session role cleanup failed; will retry")
				return ctrl.Result{}, err
			}
			if err := r.deleteOperatorImpersonateRBAC(ctx, platform); err != nil {
				logger.Error(err, "impersonate RBAC cleanup failed; will retry")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(platform, finalizerName)
			if err := r.Update(ctx, platform); err != nil {
				return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before doing any provisioning so we never
	// orphan AWS state on a fast-create-then-delete.
	if !controllerutil.ContainsFinalizer(platform, finalizerName) {
		controllerutil.AddFinalizer(platform, finalizerName)
		if err := r.Update(ctx, platform); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond * 100}, nil
	}

	// Mark Provisioning at the start of a generation transition.
	if platform.Status.ObservedGeneration != platform.Generation {
		platform.Status.Phase = phaseProvisioning
		if err := r.Status().Update(ctx, platform); err != nil {
			logger.Error(err, "status update to Provisioning")
		}
	}

	// k8s-side reconcile. Ordered: namespace must exist before
	// quota/limits/networkpolicy can land in it.
	steps := []struct {
		name string
		fn   func(context.Context, *platformv1alpha1.Platform) error
	}{
		{"ensureNamespace", r.ensureNamespace},
		{"ensureQuota", r.ensureQuota},
		{"ensureLimitRange", r.ensureLimitRange},
		{"ensureNetworkPolicy", r.ensureNetworkPolicy},
		{"ensureTenantCiliumEgress", r.ensureTenantCiliumEgress},
		{"ensureAppProject", r.ensureAppProject},
	}
	for _, s := range steps {
		if err := s.fn(ctx, platform); err != nil {
			// AppProject CRD may not be installed on every cluster; tolerate.
			if s.name == "ensureAppProject" && isNoKindMatch(err) {
				logger.Info("AppProject CRD not installed; skipping (ArgoCD not on this cluster)")
				continue
			}
			logger.Error(err, "reconcile step failed", "step", s.name)
			return ctrl.Result{}, err
		}
	}

	// vcluster isolation tier: layer the per-Platform virtual cluster on top of
	// the host-side provisioning above (which stays intact and load-bearing as the
	// containment layer). Fails closed and requeues while converging, so the IAM
	// binding below targets the syncer-created host ServiceAccount only once it
	// actually exists. See docs/adr/0009-vcluster-isolation-tier.md.
	if platform.Spec.Isolation == isolationVCluster {
		ready, verr := r.reconcileVClusterTier(ctx, platform)
		switch {
		case errors.Is(verr, errArgoCDRequired):
			logger.Error(verr, "vcluster tier requires ArgoCD; failing closed (no downgrade to namespace isolation)")
			platform.Status.Phase = phaseFailed
			setVClusterReady(platform, metav1.ConditionFalse, "ArgoCDRequired",
				"isolation: vcluster requires ArgoCD; AppProject/Application CRDs are not installed on this cluster")
			if serr := r.Status().Update(ctx, platform); serr != nil {
				return ctrl.Result{}, serr
			}
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		case verr != nil:
			logger.Error(verr, "vcluster tier reconcile failed")
			setVClusterReady(platform, metav1.ConditionFalse, "VClusterError", verr.Error())
			if serr := r.Status().Update(ctx, platform); serr != nil {
				logger.Error(serr, "status update after vcluster error")
			}
			return ctrl.Result{}, verr
		case !ready:
			logger.Info("vcluster tier converging; requeueing")
			platform.Status.Phase = phaseProvisioning
			setVClusterReady(platform, metav1.ConditionFalse, "Provisioning",
				"virtual cluster installing; waiting on kubeconfig and the synced tenant ServiceAccount")
			if serr := r.Status().Update(ctx, platform); serr != nil {
				return ctrl.Result{}, serr
			}
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		// ready — VClusterReady=True is set in the Ready status block below.
	}

	// AWS-side: tenant role. If r.IAM is nil (unit-test path), the
	// helper returns ({}, nil) and we leave status.IamRoleArn empty.
	susp, err := r.ensureIamRole(ctx, platform, r.IAMCfg)
	if err != nil {
		logger.Error(err, "ensureIamRole failed")
		return ctrl.Result{}, err
	}

	// AWS-side: KMS grant + S3 bucket policy. Skip when suspended — the
	// tenant role has had its baseline detached and we don't want to
	// hand it data-key access while the kill-switch is in effect.
	if !susp.Suspended {
		if err := r.ensureKmsGrant(ctx, platform, susp.RoleARN, r.AWSCfg); err != nil {
			logger.Error(err, "ensureKmsGrant failed")
			return ctrl.Result{}, err
		}
		if err := r.ensureBucketPolicy(ctx, platform, susp.RoleARN, r.AWSCfg); err != nil {
			logger.Error(err, "ensureBucketPolicy failed")
			return ctrl.Result{}, err
		}
	}

	// Update status. status.phase = Suspended when the kill-switch tag is
	// set; otherwise Ready.
	platform.Status.Namespace = PlatformNamespace(platform)
	// Only overwrite IamRoleArn when the IAM client returned one. The
	// envtest / no-IAM path returns empty string; preserve any value
	// previously written by a prior reconcile rather than clobbering.
	if susp.RoleARN != "" {
		platform.Status.IamRoleArn = susp.RoleARN
	}
	platform.Status.ObservedGeneration = platform.Generation

	// Create the tenant-runtime ServiceAccount (no role-arn annotation — its IAM
	// role is bound by the Pod Identity association ensureIamRole created). It is a
	// Platform-level identity primitive: the association targets exactly
	// system:serviceaccount:<ns>:tenant-runtime, so the
	// SA must exist whenever the Platform is Ready — independent of whether the
	// tenant has an AgentFleet yet. The AgentFleet/AgentSandbox reconcilers also
	// ensure it; CreateOrUpdate is idempotent so the duplicate call is harmless.
	//
	// vcluster tier: the tenant SA lives INSIDE the virtual cluster (created by
	// reconcileVClusterTier's bootstrap, then synced to the host under a
	// translated name that the Pod Identity association targets). It is not
	// created directly on the host — doing so would leave an unbound host
	// tenant-runtime SA that no synced pod runs under.
	if platform.Spec.Isolation != isolationVCluster {
		if err := ensureTenantServiceAccount(ctx, r.Client, platform); err != nil {
			logger.Error(err, "ensureTenantServiceAccount failed")
			return ctrl.Result{}, err
		}
	}

	// Per-session human attribution (optional). Provision the session role
	// (assumable by the tenant role with the operator as STS
	// SourceIdentity) + the apiserver impersonate RBAC. Reconciles in both
	// directions: removing spec.attribution tears the pair back down. The
	// session role honors the kill-switch via the susp.Suspended flag (baseline
	// detached when suspended, like the tenant role).
	if platform.Spec.Attribution != nil {
		if susp.RoleARN != "" {
			sessionARN, err := r.ensureSessionRole(ctx, platform, susp.RoleARN, susp.Suspended, r.IAMCfg)
			if err != nil {
				logger.Error(err, "ensureSessionRole failed")
				return ctrl.Result{}, err
			}
			if sessionARN != "" {
				platform.Status.SessionRoleArn = sessionARN
			}
		}
		if err := r.ensureOperatorImpersonateRBAC(ctx, platform); err != nil {
			logger.Error(err, "ensureOperatorImpersonateRBAC failed")
			return ctrl.Result{}, err
		}
	} else if platform.Status.SessionRoleArn != "" {
		// Attribution was enabled and is now removed — tear the pair down.
		if err := r.deleteSessionRole(ctx, platform, r.IAMCfg.ClusterName); err != nil {
			logger.Error(err, "deleteSessionRole (attribution removed) failed")
			return ctrl.Result{}, err
		}
		if err := r.deleteOperatorImpersonateRBAC(ctx, platform); err != nil {
			logger.Error(err, "deleteOperatorImpersonateRBAC (attribution removed) failed")
			return ctrl.Result{}, err
		}
		platform.Status.SessionRoleArn = ""
	}

	if susp.Suspended {
		platform.Status.Phase = phaseSuspended
		if platform.Status.SuspendedAt == nil {
			now := metav1.Now()
			platform.Status.SuspendedAt = &now
		}
		platform.Status.SuspendedReason = susp.Reason
		upsertCondition(&platform.Status.Conditions, metav1.Condition{
			Type:               "Suspended",
			Status:             metav1.ConditionTrue,
			Reason:             "KillSwitchActive",
			Message:            fmt.Sprintf("tenant role tagged suspended (%s); baseline policy not reattached", susp.Reason),
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: platform.Generation,
		})
	} else {
		platform.Status.Phase = phaseReady
		platform.Status.SuspendedAt = nil
		platform.Status.SuspendedReason = ""
		// Surface the reconciled Bedrock model boundary. Only written when the
		// IAM client is wired — otherwise no bedrock-model-scoping policy
		// exists on any role and the condition would overclaim.
		if r.IAM != nil {
			upsertCondition(&platform.Status.Conditions, metav1.Condition{
				Type:               "ModelAccessScoped",
				Status:             metav1.ConditionTrue,
				Reason:             modelScopeConditionReason(platform.Spec.Identity),
				Message:            modelScopeConditionMessage(platform.Spec.Identity),
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: platform.Generation,
			})
		}
		upsertCondition(&platform.Status.Conditions, metav1.Condition{
			Type:               "NamespaceReady",
			Status:             metav1.ConditionTrue,
			Reason:             "Provisioned",
			Message:            "tenant namespace and quota/limits/networkpolicy installed",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: platform.Generation,
		})
		// vcluster tier reached Ready ⇒ the virtual cluster is up, the tenant SA
		// has synced to the host, and Pod Identity binds the synced name.
		if platform.Spec.Isolation == isolationVCluster {
			setVClusterReady(platform, metav1.ConditionTrue, "Provisioned",
				"virtual cluster installed; tenant ServiceAccount synced and bound via Pod Identity")
		}
		// Clear any prior Suspended condition that lingered.
		upsertCondition(&platform.Status.Conditions, metav1.Condition{
			Type:               "Suspended",
			Status:             metav1.ConditionFalse,
			Reason:             "NotSuspended",
			Message:            "kill-switch tag not set on the tenant role",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: platform.Generation,
		})
	}
	if err := r.Status().Update(ctx, platform); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}
	logger.Info("reconcile complete", "phase", platform.Status.Phase, "namespace", platform.Status.Namespace, "suspended", susp.Suspended)
	// Periodic re-queue so we pick up out-of-band kill-switch tag changes
	// (no Platform CR write to trigger us otherwise). Skip when r.IAM is
	// nil — envtest/dev path has nothing to detect, and the unit-test
	// drivers treat RequeueAfter == 0 as the convergence signal.
	if r.IAM != nil {
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// setVClusterReady upserts the VClusterReady condition on a Platform. A single
// helper so every vcluster-tier code path (converging, failed-closed, ready)
// writes the condition the same way — and a PrometheusRule can alert on it.
func setVClusterReady(p *platformv1alpha1.Platform, status metav1.ConditionStatus, reason, message string) {
	upsertCondition(&p.Status.Conditions, metav1.Condition{
		Type:               conditionVClusterReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: p.Generation,
	})
}

// upsertCondition adds or replaces a Condition by Type, preserving
// LastTransitionTime when Status hasn't changed (standard k8s pattern).
func upsertCondition(conditions *[]metav1.Condition, cond metav1.Condition) {
	for i, existing := range *conditions {
		if existing.Type == cond.Type {
			if existing.Status == cond.Status {
				cond.LastTransitionTime = existing.LastTransitionTime
			}
			(*conditions)[i] = cond
			return
		}
	}
	*conditions = append(*conditions, cond)
}

// SetupWithManager registers the reconciler with the controller manager.
// Owns() on the namespaced children gives free re-reconcile-on-drift; the
// Namespace itself is cluster-scoped so we can't Own it from a namespaced
// parent — drift on it is handled by the periodic resync.
func (r *PlatformReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c := r.Concurrency
	if c <= 0 {
		c = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Platform{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&corev1.LimitRange{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("platform").
		WithOptions(ctrlruntime.Options{MaxConcurrentReconciles: c}).
		Complete(r)
}
