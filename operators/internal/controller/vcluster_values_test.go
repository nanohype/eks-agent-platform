/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func TestBuildVClusterValues_LoadBearingKeys(t *testing.T) {
	p := vclusterPlatform()

	// Without init-charts: containment-only vcluster, no experimental.deploy.
	base := buildVClusterValues(p, VClusterConfig{})
	sync := base["sync"].(map[string]interface{})["toHost"].(map[string]interface{})["serviceAccounts"].(map[string]interface{})
	if sync["enabled"] != true {
		t.Error("sync.toHost.serviceAccounts.enabled must be true (the Pod Identity mechanism)")
	}
	exp := base["exportKubeConfig"].(map[string]interface{})
	if exp["server"] == "" {
		t.Error("exportKubeConfig.server must be set to the in-cluster Service endpoint")
	}
	// PSS-restricted securityContext on the control plane.
	cp := base["controlPlane"].(map[string]interface{})
	ss := cp["statefulSet"].(map[string]interface{})["security"].(map[string]interface{})
	csc := ss["containerSecurityContext"].(map[string]interface{})
	if csc["runAsNonRoot"] != true || csc["allowPrivilegeEscalation"] != false {
		t.Errorf("control-plane container securityContext not PSS-restricted: %v", csc)
	}
	if _, hasExperimental := base["experimental"]; hasExperimental {
		t.Error("a containment-only vcluster must not declare experimental.deploy")
	}

	// With init-charts: the experimental.deploy.vcluster.helm list is rendered.
	withCharts := buildVClusterValues(p, VClusterConfig{InitCharts: []VClusterInitChart{
		{ChartName: "kagent", RepoURL: "https://kagent.dev/charts", Version: "0.3.0", ReleaseName: "kagent", Namespace: "kagent", Values: "replicas: 1"},
		{ChartName: "keda", RepoURL: "https://kedacore.github.io/charts", Version: "2.14.0", ReleaseName: "keda", Namespace: "keda"},
	}})
	helm := withCharts["experimental"].(map[string]interface{})["deploy"].(map[string]interface{})["vcluster"].(map[string]interface{})["helm"].([]interface{})
	if len(helm) != 2 {
		t.Fatalf("expected two init-charts rendered, got %d", len(helm))
	}
	first := helm[0].(map[string]interface{})
	if first["values"] != "replicas: 1" {
		t.Errorf("init-chart values not carried through: %v", first["values"])
	}
	// The second init-chart carries no values, so the values key is omitted.
	if _, hasValues := helm[1].(map[string]interface{})["values"]; hasValues {
		t.Error("an init-chart without values must omit the values key")
	}
}

func TestVClusterNamingHelpers(t *testing.T) {
	p := vclusterPlatform()
	if got := vclusterAppName(p); got == "" {
		t.Error("vclusterAppName must be non-empty")
	}
	if got := vclusterClusterSecretName(p); got == "" {
		t.Error("vclusterClusterSecretName must be non-empty")
	}
	if got := vclusterInClusterServer(p); got == "" {
		t.Error("vclusterInClusterServer must be non-empty")
	}
	if sel := vclusterControlPlaneSelector(); sel["app"] != "vcluster" {
		t.Errorf("control-plane selector: %v", sel)
	}
}

func TestHostHasVClusterManagedObjects(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	p := vclusterPlatform()
	ns := PlatformNamespace(p)

	// Empty host namespace ⇒ drained.
	r := &PlatformReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	if lingering, err := r.hostHasVClusterManagedObjects(context.Background(), p); err != nil || lingering {
		t.Fatalf("drained namespace: got (%v, %v) want (false, nil)", lingering, err)
	}

	// A lingering synced SA ⇒ not drained.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: "synced", Namespace: ns, Labels: map[string]string{vclusterManagedByLabel: vclusterInstanceName},
	}}
	r2 := &PlatformReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(sa).Build(), Scheme: s}
	if lingering, err := r2.hostHasVClusterManagedObjects(context.Background(), p); err != nil || !lingering {
		t.Fatalf("lingering synced SA: got (%v, %v) want (true, nil)", lingering, err)
	}
}
