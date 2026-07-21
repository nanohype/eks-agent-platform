/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func vclusterHostScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return scheme
}

// syncedSAObject simulates what vcluster's syncer lands on the host: a
// ServiceAccount named per SafeConcatName, labelled managed-by=<instance> and
// annotated with the virtual object's name/namespace.
func syncedSAObject(p *platformv1alpha1.Platform) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      syncedHostSAName(PlatformNamespace(p), vclusterInstanceName),
			Namespace: PlatformNamespace(p),
			Labels:    map[string]string{vclusterManagedByLabel: vclusterInstanceName},
			Annotations: map[string]string{
				vclusterObjectNameAnnotation:      tenantSAName,
				vclusterObjectNamespaceAnnotation: PlatformNamespace(p),
			},
		},
	}
}

func vclusterPlatform() *platformv1alpha1.Platform {
	p := newPlatform("demo", "reliability")
	p.Spec.Isolation = isolationVCluster
	return p
}

// TestEnsureIamRole_VClusterTier_BindsSyncedSA proves the load-bearing Pod
// Identity behavior for the vcluster tier: the association targets the SYNCED
// host ServiceAccount name (SafeConcatName), never the virtual "tenant-runtime"
// — because EKS Pod Identity resolves by the pod's host (namespace, SA), and the
// syncer rewrites synced pods to the translated name. Binding tenant-runtime
// would resolve nothing.
func TestEnsureIamRole_VClusterTier_BindsSyncedSA(t *testing.T) {
	f := newFakeIAM()
	fe := newFakeEKS()
	platform := vclusterPlatform()

	host := fake.NewClientBuilder().
		WithScheme(vclusterHostScheme(t)).
		WithObjects(syncedSAObject(platform)).
		Build()
	r := &PlatformReconciler{Client: host, IAM: f, EKS: fe}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
	}

	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("ensureIamRole (vcluster tier): %v", err)
	}

	if len(fe.createCalls) != 1 {
		t.Fatalf("expected exactly one Pod Identity association, got %d", len(fe.createCalls))
	}
	got := aws.ToString(fe.createCalls[0].ServiceAccount)
	wantSynced := syncedHostSAName(PlatformNamespace(platform), vclusterInstanceName)
	if got != wantSynced {
		t.Errorf("association SA: got %q want synced host name %q", got, wantSynced)
	}
	if got == tenantSAName {
		t.Errorf("association bound the virtual tenant-runtime SA %q — Pod Identity would resolve nothing", got)
	}
}

// TestEnsureIamRole_VClusterTier_NeverBindsSyncerSA is the positive posture
// assertion from the threat model: the syncer's own host ServiceAccount (named
// after the vcluster instance) must NEVER receive a Pod Identity association, so
// a compromised syncer has no AWS credential path. The operator only ever binds
// the tenant's synced SA; here we prove no association targets the syncer/control-
// plane SA name or any SA other than the tenant's synced one.
func TestEnsureIamRole_VClusterTier_NeverBindsSyncerSA(t *testing.T) {
	f := newFakeIAM()
	fe := newFakeEKS()
	platform := vclusterPlatform()
	host := fake.NewClientBuilder().
		WithScheme(vclusterHostScheme(t)).
		WithObjects(syncedSAObject(platform)).
		Build()
	r := &PlatformReconciler{Client: host, IAM: f, EKS: fe}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("ensureIamRole (vcluster tier): %v", err)
	}

	wantSynced := syncedHostSAName(PlatformNamespace(platform), vclusterInstanceName)
	for _, c := range fe.createCalls {
		sa := aws.ToString(c.ServiceAccount)
		switch sa {
		case vclusterInstanceName:
			t.Errorf("a Pod Identity association was created for the syncer/control-plane SA %q — it must have zero AWS reach", sa)
		case tenantSAName:
			t.Errorf("a Pod Identity association was created for the virtual tenant-runtime SA %q", sa)
		case wantSynced:
			// the one legitimate binding
		default:
			t.Errorf("unexpected Pod Identity association for SA %q (only the synced tenant SA should be bound)", sa)
		}
	}
}

// TestDiscoverSyncedHostSA_RequeuesUntilSynced proves the two-phase reconcile:
// before the syncer has materialized the host SA, discovery returns
// errVClusterNotReady (the caller requeues) rather than binding a wrong name.
func TestDiscoverSyncedHostSA_RequeuesUntilSynced(t *testing.T) {
	platform := vclusterPlatform()
	host := fake.NewClientBuilder().WithScheme(vclusterHostScheme(t)).Build()
	r := &PlatformReconciler{Client: host}
	if _, err := r.discoverSyncedHostSA(context.Background(), platform); err != errVClusterNotReady {
		t.Fatalf("want errVClusterNotReady before the SA syncs, got %v", err)
	}
}

// TestDiscoverSyncedHostSA_FailsLoudOnNamingMismatch proves the cross-check: if
// vcluster ever writes a synced SA name that does not match the operator's
// replica of its algorithm (an upstream naming change on upgrade), discovery
// fails loud instead of binding Pod Identity to the wrong name.
func TestDiscoverSyncedHostSA_FailsLoudOnNamingMismatch(t *testing.T) {
	platform := vclusterPlatform()
	// Same provenance annotations, but a name that does NOT match the computed one.
	rogue := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-runtime-wrongly-translated",
			Namespace: PlatformNamespace(platform),
			Labels:    map[string]string{vclusterManagedByLabel: vclusterInstanceName},
			Annotations: map[string]string{
				vclusterObjectNameAnnotation:      tenantSAName,
				vclusterObjectNamespaceAnnotation: PlatformNamespace(platform),
			},
		},
	}
	host := fake.NewClientBuilder().WithScheme(vclusterHostScheme(t)).WithObjects(rogue).Build()
	r := &PlatformReconciler{Client: host}
	_, err := r.discoverSyncedHostSA(context.Background(), platform)
	if err == nil || err == errVClusterNotReady {
		t.Fatalf("want a hard naming-mismatch error, got %v", err)
	}
}

// TestVClusterTeardown_DeletesSyncedSAAssociation proves the teardown targets the
// synced-SA association (AWS-side state a namespace delete won't reap), computed
// deterministically from the platform + instance name so it works even after the
// SA itself is gone. The full finalizer ordering is covered in the conformance
// suite (where the ArgoCD CRDs are installed).
func TestVClusterTeardown_DeletesSyncedSAAssociation(t *testing.T) {
	fe := newFakeEKS()
	platform := vclusterPlatform()
	cfg := IAMConfig{ClusterName: "production-cluster"}
	syncedSA := syncedHostSAName(PlatformNamespace(platform), vclusterInstanceName)
	fe.associations[PlatformNamespace(platform)+"/"+syncedSA] = "a-1"
	r := &PlatformReconciler{EKS: fe}

	if err := r.deletePodIdentityAssociation(context.Background(), cfg, PlatformNamespace(platform), syncedSA); err != nil {
		t.Fatalf("delete synced-SA association: %v", err)
	}
	if len(fe.deleteCalls) != 1 {
		t.Fatalf("expected the synced-SA Pod Identity association to be deleted once, got %d deletes", len(fe.deleteCalls))
	}
}

// ensure the fake host client satisfies client.Client (compile guard).
var _ client.Client = fake.NewClientBuilder().Build()
