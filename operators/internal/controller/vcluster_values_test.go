/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

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
		{ChartName: "keda", RepoURL: "https://kedacore.github.io/charts", Version: "2.20.1", ReleaseName: "keda", Namespace: "keda", Values: "replicas: 1"},
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

// TestCoreDNSKeepsNetBindService pins the one capability CoreDNS needs to exec
// at all.
//
// Found by running it: in the kx workspace a per-Platform vcluster's synced
// CoreDNS had been in CrashLoopBackOff for eighteen days with 4530 restarts and
// one line of output, `exec /coredns: operation not permitted`, while its ArgoCD
// Application reported Synced/Healthy and the Platform reported Ready. Every
// vcluster-isolated tenant had no cluster DNS the whole time.
//
// The cause is not the uid and not a privileged port. The coredns binary carries
// file capabilities, and the kernel refuses to exec such a file when the
// capability bounding set is empty — execve returns EPERM before CoreDNS runs a
// line of its own code. Isolated with a three-way container experiment: uid
// 12345 + drop ALL fails, uid 12345 with capabilities kept succeeds, and root +
// drop ALL fails too.
//
// PSS restricted permits adding back NET_BIND_SERVICE and nothing else, so the
// fix stays inside the profile the tenant namespace enforces.
func TestCoreDNSKeepsNetBindService(t *testing.T) {
	values := buildVClusterValues(&platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
	}, VClusterConfig{})

	caps := nestedMap(t, values, "controlPlane", "coredns", "security", "containerSecurityContext", "capabilities")

	drop, _ := caps["drop"].([]interface{})
	if len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("coredns capabilities.drop = %v, want [ALL] — PSS restricted requires dropping all", drop)
	}
	add, _ := caps["add"].([]interface{})
	if len(add) != 1 || add[0] != "NET_BIND_SERVICE" {
		t.Errorf("coredns capabilities.add = %v, want [NET_BIND_SERVICE]; without it the container cannot exec its own binary", add)
	}

	// The control plane's own container has no file capabilities, so it keeps the
	// stricter form. If this ever needs the same treatment it should be a
	// deliberate change, not a copy of the CoreDNS block.
	spCaps := nestedMap(t, values, "controlPlane", "statefulSet", "security", "containerSecurityContext", "capabilities")
	if _, hasAdd := spCaps["add"]; hasAdd {
		t.Error("the control-plane container added a capability back; only coredns needs one")
	}
}

// TestExportVClusterValues is not an assertion — it is the export seam that
// scripts/check-vcluster-chart.py renders against the pinned chart.
//
// Everything else in this file asserts what the operator EMITS. Nothing asserted
// that the chart ACCEPTS it, and the two are different questions: the values are
// a map[string]interface{} the compiler never checks against a schema, so a chart
// bump that renamed a values path would keep every test in this package green and
// break the isolation tier at sync, on every tenant.
//
// It goes through buildVClusterValues rather than restating the values, so the
// thing rendered is the thing shipped. Without the env var it is a no-op, so a
// normal `go test ./...` neither writes files nor needs helm.
func TestExportVClusterValues(t *testing.T) {
	out := os.Getenv("VCLUSTER_VALUES_OUT")
	if out == "" {
		t.Skip("VCLUSTER_VALUES_OUT unset — export seam for scripts/check-vcluster-chart.py")
	}
	// Init-charts included: they render into experimental.deploy.vcluster.helm,
	// which is a values path like any other and would otherwise go unrendered.
	values := buildVClusterValues(vclusterPlatform(), VClusterConfig{
		InitCharts: []VClusterInitChart{{
			ChartName:   "keda",
			RepoURL:     "https://kedacore.github.io/charts",
			Version:     "2.20.2",
			ReleaseName: "keda",
			Namespace:   "keda",
			Values:      "replicas: 1",
		}},
	})
	blob, err := yaml.Marshal(values)
	if err != nil {
		t.Fatalf("marshal vcluster values: %v", err)
	}
	if err := os.WriteFile(out, blob, 0o600); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
}

func nestedMap(t *testing.T, m map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	cur := m
	for i, key := range path {
		next, ok := cur[key].(map[string]interface{})
		if !ok {
			t.Fatalf("values path %v is absent at %q", path[:i+1], key)
		}
		cur = next
	}
	return cur
}
