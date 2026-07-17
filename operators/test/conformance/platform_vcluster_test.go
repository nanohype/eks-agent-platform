/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

// fakeVClusterFactory is the faked vcluster-client seam (the awsclients-fake
// idiom, one tier up): it returns an in-memory client standing in for the virtual
// cluster's API, so the target-client swap is exercised in envtest without a real
// vcluster. Readiness is driven not by this factory but by whether the test has
// seeded the synced host ServiceAccount (simulating vcluster's syncer) — which is
// exactly what the operator's discovery gates on.
type fakeVClusterFactory struct {
	vc client.Client
}

func (f *fakeVClusterFactory) ClientFor(_ context.Context, _ *platformv1alpha1.Platform) (client.Client, error) {
	return f.vc, nil
}
func (f *fakeVClusterFactory) Invalidate(_ *platformv1alpha1.Platform) {}

// minimal parseable kubeconfig for the vc-<name> Secret so ensureVClusterClusterSecret
// (clientcmd) can build the ArgoCD cluster registration.
const fakeVClusterKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: vc
  cluster:
    server: https://vcluster.example.svc
    insecure-skip-tls-verify: true
contexts:
- name: vc
  context: {cluster: vc, user: vc}
current-context: vc
users:
- name: vc
  user:
    token: fake-token
`

func newVClusterPlatformReconciler(vc client.Client) *controller.PlatformReconciler {
	return &controller.PlatformReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
		VCluster:    &fakeVClusterFactory{vc: vc},
		VClusterCfg: controller.VClusterConfig{
			ChartRepoURL: "https://charts.loft.sh",
			ChartVersion: "0.35.2",
		},
	}
}

// newFakeVCluster builds the in-memory stand-in for a Platform's virtual cluster,
// on the same scheme as the host so corev1 objects (namespace, ServiceAccount)
// round-trip.
func newFakeVCluster() client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

// seedSyncedSA simulates vcluster's syncer landing the translated tenant-runtime
// SA on the host, so the operator's discovery + Pod Identity binding can proceed.
func seedSyncedSA(ctx context.Context, t *testing.T, p *platformv1alpha1.Platform) {
	t.Helper()
	ns := controller.PlatformNamespace(p)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.SyncedHostSAName(ns),
			Namespace: ns,
			Labels:    map[string]string{"vcluster.loft.sh/managed-by": "vcluster"},
			Annotations: map[string]string{
				"vcluster.loft.sh/object-name":      "tenant-runtime",
				"vcluster.loft.sh/object-namespace": ns,
			},
		},
	}
	if err := k8sClient.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("seed synced SA: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sa) })
}

// seedVClusterKubeconfig seeds the vc-<name> kubeconfig Secret the operator reads
// to register the vcluster as an ArgoCD destination.
func seedVClusterKubeconfig(ctx context.Context, t *testing.T, p *platformv1alpha1.Platform) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vc-vcluster", Namespace: controller.PlatformNamespace(p)},
		Data:       map[string][]byte{"config": []byte(fakeVClusterKubeconfig)},
	}
	if err := k8sClient.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("seed vcluster kubeconfig: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })
}

func reconcileVCluster(ctx context.Context, t *testing.T, r *controller.PlatformReconciler, p *platformv1alpha1.Platform) ctrl.Result {
	t.Helper()
	var res ctrl.Result
	var err error
	for i := 0; i < 6; i++ {
		res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}})
		if err != nil {
			t.Fatalf("vcluster reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return res
		}
	}
	return res
}

// TestPlatformReconciler_VClusterTier_ObservablyChangesReconciliation is the core
// acceptance test: isolation: vcluster must change what the operator provisions.
// It asserts the operator declares the vcluster as an ArgoCD Application, drives
// the tenant ServiceAccount into the virtual cluster (not the host), registers
// the ArgoCD destination, keeps every host containment primitive on the host, and
// surfaces VClusterReady.
func TestPlatformReconciler_VClusterTier_ObservablyChangesReconciliation(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "vc"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "ops", Tenant: "acme",
			Budget:    platformv1alpha1.BudgetRef{Name: "x"},
			Identity:  platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Isolation: "vcluster",
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })

	// Pre-create the tenant namespace so the seeded Secret/SA have somewhere to
	// live (the reconciler's ensureNamespace is idempotent over it).
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("pre-create tenant namespace: %v", err)
	}

	// Simulate the vcluster being up (kubeconfig published) and the syncer having
	// landed the translated tenant-runtime SA on the host.
	seedVClusterKubeconfig(ctx, t, p)
	seedSyncedSA(ctx, t, p)

	vc := newFakeVCluster()
	r := newVClusterPlatformReconciler(vc)
	reconcileVCluster(ctx, t, r, p)

	// 1. The vcluster is declared as an ArgoCD Application with the right source.
	// Looked up by the platform label (its name is hash-truncated for long names).
	appList := &unstructured.UnstructuredList{}
	appList.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ApplicationList"})
	if err := k8sClient.List(ctx, appList, client.InNamespace("argocd"), client.MatchingLabels{"agents.nanohype.dev/platform": p.Name}); err != nil {
		t.Fatalf("list vcluster Applications: %v", err)
	}
	if len(appList.Items) != 1 {
		t.Fatalf("expected exactly one vcluster ArgoCD Application for the platform, got %d", len(appList.Items))
	}
	app := &appList.Items[0]
	chart, _, _ := unstructured.NestedString(app.Object, "spec", "source", "chart")
	if chart != "vcluster" {
		t.Errorf("Application source chart: got %q want vcluster", chart)
	}
	releaseName, _, _ := unstructured.NestedString(app.Object, "spec", "source", "helm", "releaseName")
	if releaseName != "vcluster" {
		t.Errorf("Application helm releaseName: got %q want vcluster (must equal the syncer suffix)", releaseName)
	}
	saSyncOn, _, _ := unstructured.NestedBool(app.Object, "spec", "source", "helm", "valuesObject", "sync", "toHost", "serviceAccounts", "enabled")
	if !saSyncOn {
		t.Error("vcluster values must set sync.toHost.serviceAccounts.enabled=true (the Pod Identity mechanism)")
	}
	destNS, _, _ := unstructured.NestedString(app.Object, "spec", "destination", "namespace")
	if destNS != tenantNS {
		t.Errorf("Application destination namespace: got %q want %q", destNS, tenantNS)
	}

	// 2. The tenant ServiceAccount landed in the VIRTUAL cluster (target-client
	//    swap), NOT on the host — the whole point of the tier.
	var vcSA corev1.ServiceAccount
	if err := vc.Get(ctx, types.NamespacedName{Name: "tenant-runtime", Namespace: tenantNS}, &vcSA); err != nil {
		t.Errorf("tenant-runtime SA should exist in the virtual cluster: %v", err)
	}
	var hostSA corev1.ServiceAccount
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-runtime", Namespace: tenantNS}, &hostSA)
	if err == nil {
		t.Error("tenant-runtime SA must NOT be created directly on the host in the vcluster tier (it syncs from the vcluster under a translated name)")
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error getting host tenant-runtime SA: %v", err)
	}

	// 3. Host containment primitives still exist on the host (unchanged, and now
	//    load-bearing as the layer that bounds the vcluster's pods from outside).
	var q corev1.ResourceQuota
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-default", Namespace: tenantNS}, &q); err != nil {
		t.Errorf("host ResourceQuota missing in vcluster tier: %v", err)
	}
	var np networkingv1.NetworkPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-egress", Namespace: tenantNS}, &np); err != nil {
		t.Errorf("host default-deny/egress NetworkPolicy missing in vcluster tier: %v", err)
	}
	var tenantNSObj corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: tenantNS}, &tenantNSObj); err != nil {
		t.Fatalf("host tenant namespace missing: %v", err)
	}
	if tenantNSObj.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Error("host tenant namespace must still enforce PSS restricted")
	}

	// 4. The vcluster is registered as an ArgoCD cluster destination.
	var secrets corev1.SecretList
	if err := k8sClient.List(ctx, &secrets, client.InNamespace("argocd"), client.MatchingLabels{
		"argocd.argoproj.io/secret-type": "cluster",
		"agents.nanohype.dev/platform":   p.Name,
	}); err != nil {
		t.Fatalf("list vcluster cluster Secrets: %v", err)
	}
	if len(secrets.Items) != 1 {
		t.Errorf("expected exactly one ArgoCD cluster Secret registering the vcluster, got %d", len(secrets.Items))
	} else if string(secrets.Items[0].Data["server"]) == "" {
		t.Error("cluster Secret missing server endpoint")
	}

	// 5. VClusterReady=True + Ready phase.
	var got platformv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: p.Namespace}, &got); err != nil {
		t.Fatalf("get platform: %v", err)
	}
	if got.Status.Phase != phaseReady {
		t.Errorf("status.phase: got %q want Ready", got.Status.Phase)
	}
	if !hasConditionTrue(got.Status.Conditions, "VClusterReady") {
		t.Errorf("expected VClusterReady=True; conditions: %+v", got.Status.Conditions)
	}
}

// TestPlatformReconciler_VClusterTier_RequeuesUntilSyncedNoDowngrade proves the
// fail-closed property: before the syncer has landed the tenant SA on the host,
// the vcluster-tier Platform stays Provisioning with VClusterReady=False and does
// NOT create a host-side tenant-runtime SA — it never silently downgrades to
// namespace isolation.
func TestPlatformReconciler_VClusterTier_RequeuesUntilSyncedNoDowngrade(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "vc"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "ops", Tenant: "acme",
			Budget:    platformv1alpha1.BudgetRef{Name: "x"},
			Identity:  platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Isolation: "vcluster",
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })

	// Pre-create the tenant namespace so the seeded Secret/SA have somewhere to
	// live (the reconciler's ensureNamespace is idempotent over it).
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("pre-create tenant namespace: %v", err)
	}

	seedVClusterKubeconfig(ctx, t, p)
	// deliberately DO NOT seed the synced SA — the syncer hasn't run yet.

	vc := newFakeVCluster()
	r := newVClusterPlatformReconciler(vc)
	res := reconcileVCluster(ctx, t, r, p)
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while the vcluster tier is still converging")
	}

	var got platformv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: p.Namespace}, &got); err != nil {
		t.Fatalf("get platform: %v", err)
	}
	if got.Status.Phase == phaseReady {
		t.Errorf("vcluster-tier Platform must not be Ready before the SA syncs; got phase %q", got.Status.Phase)
	}
	if hasConditionTrue(got.Status.Conditions, "VClusterReady") {
		t.Error("VClusterReady must not be True before the vcluster is fully up")
	}
	// No silent downgrade: the host tenant-runtime SA must not be created.
	var hostSA corev1.ServiceAccount
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tenant-runtime", Namespace: tenantNS}, &hostSA); err == nil {
		t.Error("host tenant-runtime SA created while converging — that is a silent downgrade to namespace isolation")
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPlatformReconciler_VClusterTier_FinalizerTearsDownInOrder proves the
// finalizer-gated teardown: on delete the operator removes the ArgoCD cluster
// Secret and the vcluster Application, and only completes (drops the finalizer,
// deletes the tenant namespace) once no vcluster-managed host objects linger.
func TestPlatformReconciler_VClusterTier_FinalizerTearsDownInOrder(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "vcdel"), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona: "ops", Tenant: "acme",
			Budget:    platformv1alpha1.BudgetRef{Name: "x"},
			Identity:  platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Isolation: "vcluster",
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("pre-create tenant namespace: %v", err)
	}
	seedVClusterKubeconfig(ctx, t, p)
	seedSyncedSA(ctx, t, p)

	vc := newFakeVCluster()
	r := newVClusterPlatformReconciler(vc)
	reconcileVCluster(ctx, t, r, p)

	// Delete the Platform — the finalizer must gate teardown.
	if err := k8sClient.Delete(ctx, p); err != nil {
		t.Fatalf("delete platform: %v", err)
	}

	// First finalizer pass: the seeded synced SA still lingers, so the drain gate
	// must hold the finalizer (requeue, not complete).
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}})
	if err != nil {
		t.Fatalf("finalizer reconcile (drain pending): %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("teardown must not complete while vcluster-managed host objects linger")
	}
	var stillThere platformv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: p.Namespace}, &stillThere); err != nil {
		t.Fatalf("platform should still exist while drain is pending: %v", err)
	}

	// Simulate ArgoCD uninstalling the vcluster: the synced host SA is drained.
	syncedSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: controller.SyncedHostSAName(tenantNS), Namespace: tenantNS}}
	if err := k8sClient.Delete(ctx, syncedSA); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("drain synced SA: %v", err)
	}

	// Second finalizer pass: drain gate passes, teardown completes.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}}); err != nil {
		t.Fatalf("finalizer reconcile (drain clear): %v", err)
	}

	// The vcluster Application and the ArgoCD cluster Secret are gone.
	appList := &unstructured.UnstructuredList{}
	appList.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ApplicationList"})
	if err := k8sClient.List(ctx, appList, client.InNamespace("argocd"), client.MatchingLabels{"agents.nanohype.dev/platform": p.Name}); err != nil {
		t.Fatalf("list Applications post-teardown: %v", err)
	}
	if len(appList.Items) != 0 {
		t.Errorf("vcluster Application not deleted on teardown: %d remain", len(appList.Items))
	}
	var secrets corev1.SecretList
	if err := k8sClient.List(ctx, &secrets, client.InNamespace("argocd"), client.MatchingLabels{"agents.nanohype.dev/platform": p.Name}); err != nil {
		t.Fatalf("list cluster Secrets post-teardown: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("vcluster ArgoCD cluster Secret not deleted on teardown: %d remain", len(secrets.Items))
	}
	// The Platform finalizer dropped (object gone).
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: p.Namespace}, &platformv1alpha1.Platform{}); err == nil {
		t.Error("Platform should be gone after finalizer completes")
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error getting deleted platform: %v", err)
	}
}

func hasConditionTrue(conds []metav1.Condition, condType string) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
