/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// adoptedTenantSA is tenant-runtime as Envoy Gateway leaves it after adopting
// it as a GatewayNamespace-mode proxy ServiceAccount: its rendering of the
// object (proxy/resource_provider.go ServiceAccount at v1.8.3 and v1.10.0) —
// EnvoyAppLabel, the gateway-name and owning-gateway labels, an owner reference
// to the Gateway, and automount off — over the operator's own labels, plus an
// owner reference nobody here wrote, which must survive.
func adoptedTenantSA(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantSAName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":                         "envoy",
				"app.kubernetes.io/component":                    "proxy",
				"app.kubernetes.io/managed-by":                   "envoy-gateway",
				"gateway.networking.k8s.io/gateway-name":         ctrlTestPlatform + "-gateway",
				"gateway.envoyproxy.io/owning-gateway-name":      ctrlTestPlatform + "-gateway",
				"gateway.envoyproxy.io/owning-gateway-namespace": namespace,
				LabelPlatform: ctrlTestPlatform,
			},
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway", Name: ctrlTestPlatform + "-gateway", UID: "gw-uid"},
				{APIVersion: "example.com/v1", Kind: "Keeper", Name: "k", UID: "keeper-uid"},
			},
		},
		AutomountServiceAccountToken: new(false),
	}
}

// envoyGatewayMarks lists what on sa would let Envoy Gateway or the garbage
// collector take it: any of its labels, a Gateway owner reference.
func envoyGatewayMarks(sa *corev1.ServiceAccount) []string {
	var marks []string
	for k, v := range envoyGatewayOwnedLabels {
		if sa.Labels[k] == v {
			marks = append(marks, k+"="+v)
		}
	}
	for _, k := range envoyGatewayIdentityLabels {
		if _, ok := sa.Labels[k]; ok {
			marks = append(marks, k)
		}
	}
	for _, ref := range sa.OwnerReferences {
		if isGatewayOwnerRef(ref) {
			marks = append(marks, "ownerReference "+ref.Kind+"/"+ref.Name)
		}
	}
	return marks
}

func TestReleaseFromEnvoyGateway(t *testing.T) {
	t.Run("an adopted account loses every mark and keeps the rest", func(t *testing.T) {
		sa := adoptedTenantSA("tenants-acme")
		if !releaseFromEnvoyGateway(sa) {
			t.Fatal("an adopted account reported nothing to release")
		}
		if m := envoyGatewayMarks(sa); len(m) != 0 {
			t.Errorf("Envoy Gateway marks left behind: %v", m)
		}
		if sa.AutomountServiceAccountToken != nil {
			t.Errorf("automount Envoy Gateway turned off was kept: %v", *sa.AutomountServiceAccountToken)
		}
		if sa.Labels[LabelPlatform] != ctrlTestPlatform {
			t.Errorf("the operator's own label was dropped: %v", sa.Labels)
		}
		if len(sa.OwnerReferences) != 1 || sa.OwnerReferences[0].Kind != "Keeper" {
			t.Errorf("an unrelated owner reference was dropped: %+v", sa.OwnerReferences)
		}
	})

	t.Run("a GatewayClass owner reference is Envoy Gateway's too", func(t *testing.T) {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "gateway.networking.k8s.io/v1beta1", Kind: "GatewayClass", Name: "c"}},
		}}
		if !releaseFromEnvoyGateway(sa) || sa.OwnerReferences != nil {
			t.Errorf("GatewayClass owner reference kept: %+v", sa.OwnerReferences)
		}
	})

	t.Run("an account Envoy Gateway never touched is left alone", func(t *testing.T) {
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					// Same keys as Envoy Gateway's, someone else's values.
					"app.kubernetes.io/name":       "envoy-sidecar",
					"app.kubernetes.io/managed-by": "eks-agent-platform",
				},
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "example.com/v1", Kind: "Gateway", Name: "not-gateway-api"},
				},
			},
			// Off for a reason of its own: not this function's to undo.
			AutomountServiceAccountToken: new(false),
		}
		before := sa.DeepCopy()
		if releaseFromEnvoyGateway(sa) {
			t.Error("reported a release on an account with no Envoy Gateway marks")
		}
		if !reflect.DeepEqual(before, sa) {
			t.Errorf("mutated an account with no Envoy Gateway marks:\nbefore %+v\nafter  %+v", before, sa)
		}
	})

	t.Run("automount left on is not touched", func(t *testing.T) {
		sa := adoptedTenantSA("tenants-acme")
		sa.AutomountServiceAccountToken = new(true)
		releaseFromEnvoyGateway(sa)
		if sa.AutomountServiceAccountToken == nil || !*sa.AutomountServiceAccountToken {
			t.Errorf("automount: got %v want true", sa.AutomountServiceAccountToken)
		}
	})
}

// TestEnsureTenantServiceAccountReleasesAnAdoptedAccount: the Platform
// reconciler's own pass over tenant-runtime strips what an earlier Envoy
// Gateway adoption left. Replacing the label map was already the operator's
// behaviour; the owner reference and the automount setting were not, and the
// owner reference is the one that deletes tenant-runtime with the Gateway.
func TestEnsureTenantServiceAccountReleasesAnAdoptedAccount(t *testing.T) {
	s := mgwScheme(t)
	p := readyTestPlatform()
	ns := PlatformNamespace(p)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(adoptedTenantSA(ns)).Build()

	if err := ensureTenantServiceAccount(context.Background(), cl, p); err != nil {
		t.Fatalf("ensureTenantServiceAccount: %v", err)
	}
	got := &corev1.ServiceAccount{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: tenantSAName}, got); err != nil {
		t.Fatal(err)
	}
	if m := envoyGatewayMarks(got); len(m) != 0 {
		t.Errorf("tenant-runtime still carries Envoy Gateway marks after the Platform pass: %v", m)
	}
	if got.AutomountServiceAccountToken != nil {
		t.Errorf("automount: got %v want unset", *got.AutomountServiceAccountToken)
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "eks-agent-platform" {
		t.Errorf("labels: %v", got.Labels)
	}
}

// TestModelGatewayRendersTheRenameOnlyOverAReleasedTenantSA is the upgrade
// path from an operator that pointed the EnvoyProxy at tenant-runtime. The pass
// that finds tenant-runtime adopted releases it and renders nothing; the
// EnvoyProxy naming <platform>-gateway — after which Envoy Gateway deletes every
// other proxy ServiceAccount its labels select — is rendered only by a pass
// that found tenant-runtime already released.
func TestModelGatewayRendersTheRenameOnlyOverAReleasedTenantSA(t *testing.T) {
	s := mgwScheme(t)
	ctx := context.Background()
	p := readyTestPlatform()
	ns := PlatformNamespace(p)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, adoptedTenantSA(ns)).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}

	res, err := r.reconcileSelf(ctx, twoRouteGateway())
	if err != nil {
		t.Fatalf("reconcileSelf: %v", err)
	}
	if res.phase != phasePending || res.waitingOn != tenantSAReleaseMessage {
		t.Fatalf("an adopted tenant-runtime: got phase %q waiting on %q, want Pending on the release", res.phase, res.waitingOn)
	}
	proxy := &unstructured.Unstructured{}
	proxy.SetGroupVersionKind(envoyGatewayGV.WithKind("EnvoyProxy"))
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: ctrlTestPlatform + "-gateway"}, proxy); !apierrors.IsNotFound(err) {
		t.Fatalf("EnvoyProxy rendered on the pass that released tenant-runtime (get err %v)", err)
	}
	sa := &corev1.ServiceAccount{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: tenantSAName}, sa); err != nil {
		t.Fatal(err)
	}
	if m := envoyGatewayMarks(sa); len(m) != 0 {
		t.Fatalf("tenant-runtime not released: %v", m)
	}

	res, err = r.reconcileSelf(ctx, twoRouteGateway())
	if err != nil {
		t.Fatalf("reconcileSelf (released): %v", err)
	}
	if res.phase != phaseProvisioning {
		t.Fatalf("a released tenant-runtime: got phase %q want Provisioning", res.phase)
	}
	proxy = getRendered(t, cl, envoyGatewayGV, "EnvoyProxy", ctrlTestPlatform+"-gateway")
	name, _, _ := unstructured.NestedString(proxy.Object, "spec", "provider", "kubernetes", "envoyServiceAccount", "name")
	if name != gatewayServiceAccountName(p) {
		t.Errorf("envoyServiceAccount: got %q want %q", name, gatewayServiceAccountName(p))
	}
}

// TestModelGatewayCleanupReleasesTenantRuntimeBeforeTheGateway: deleting the
// Gateway garbage-collects whatever holds an owner reference to it. An adopted
// tenant-runtime must already be released by the time that delete is sent.
func TestModelGatewayCleanupReleasesTenantRuntimeBeforeTheGateway(t *testing.T) {
	s := mgwScheme(t)
	ctx := context.Background()
	p := readyTestPlatform()
	ns := PlatformNamespace(p)
	var marksAtGatewayDelete []string
	gatewayDeleted := false
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, adoptedTenantSA(ns)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Gateway" {
					gatewayDeleted = true
					sa := &corev1.ServiceAccount{}
					if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: tenantSAName}, sa); err != nil {
						t.Errorf("read tenant-runtime at Gateway delete: %v", err)
					}
					marksAtGatewayDelete = envoyGatewayMarks(sa)
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}

	if err := r.cleanupGatewayResources(ctx, twoRouteGateway()); err != nil {
		t.Fatalf("cleanupGatewayResources: %v", err)
	}
	if !gatewayDeleted {
		t.Fatal("cleanup never deleted the Gateway, so the ordering was not observed")
	}
	if len(marksAtGatewayDelete) != 0 {
		t.Errorf("the Gateway was deleted while tenant-runtime still carried %v", marksAtGatewayDelete)
	}
}

// TestReleaseTenantServiceAccount covers the paths around the release itself.
func TestReleaseTenantServiceAccount(t *testing.T) {
	s := mgwScheme(t)
	ctx := context.Background()

	t.Run("vcluster tier: a host tenant-runtime is Envoy Gateway's own", func(t *testing.T) {
		p := readyTestPlatform()
		p.Spec.Isolation = isolationVCluster
		host := adoptedTenantSA(PlatformNamespace(p))
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(host).Build()
		released, err := releaseTenantServiceAccount(ctx, cl, p)
		if err != nil || released {
			t.Fatalf("got (%v, %v) want (false, nil)", released, err)
		}
		got := &corev1.ServiceAccount{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(host), got); err != nil {
			t.Fatal(err)
		}
		if len(envoyGatewayMarks(got)) == 0 {
			t.Error("released a host ServiceAccount the vcluster tier never ran tenants under")
		}
	})

	t.Run("no tenant-runtime yet", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		if released, err := releaseTenantServiceAccount(ctx, cl, readyTestPlatform()); err != nil || released {
			t.Fatalf("got (%v, %v) want (false, nil)", released, err)
		}
	})

	t.Run("a clean tenant-runtime is not written", func(t *testing.T) {
		p := readyTestPlatform()
		clean := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: tenantSAName, Namespace: PlatformNamespace(p)}}
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(clean).
			WithInterceptorFuncs(interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					t.Error("updated a tenant-runtime with nothing to release")
					return nil
				},
			}).Build()
		if released, err := releaseTenantServiceAccount(ctx, cl, p); err != nil || released {
			t.Fatalf("got (%v, %v) want (false, nil)", released, err)
		}
	})

	t.Run("an unreadable tenant-runtime is an error, not a clean one", func(t *testing.T) {
		boom := errors.New("apiserver unavailable")
		cl := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).Build()
		if _, err := releaseTenantServiceAccount(ctx, cl, readyTestPlatform()); !errors.Is(err, boom) {
			t.Fatalf("got %v want %v", err, boom)
		}
	})

	t.Run("a refused release stops the reconcile before anything renders", func(t *testing.T) {
		p := readyTestPlatform()
		boom := apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, tenantSAName, errors.New("denied"))
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p, adoptedTenantSA(PlatformNamespace(p))).
			WithInterceptorFuncs(interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					return boom
				},
			}).Build()
		r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
		if _, err := r.reconcileSelf(ctx, twoRouteGateway()); !errors.Is(err, boom) || !strings.Contains(err.Error(), "release ServiceAccount") {
			t.Fatalf("reconcileSelf: got %v want the refused release", err)
		}
		if err := r.cleanupGatewayResources(ctx, twoRouteGateway()); !errors.Is(err, boom) {
			t.Fatalf("cleanupGatewayResources: got %v want the refused release, before any delete", err)
		}
	})
}
