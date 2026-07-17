/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The vcluster-tier target-client seam (target_client.go, vcluster.go) was
// exercised end-to-end on a real kind cluster but its error branches — a nil
// factory, a still-installing vcluster (missing kubeconfig Secret), an empty or
// malformed kubeconfig, and a syncer that renamed the synced SA on an upgrade —
// had no automated coverage. These tests drive every one against a fake host
// client so a regression in the fail-closed behavior surfaces in CI, not on a
// live cluster.

// a minimal, parseable kubeconfig; the server never gets dialed (client.New is
// lazy) so the fake token/insecure-verify are inert.
const targetTestKubeconfig = `apiVersion: v1
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

func vclusterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func uidVClusterPlatform() *platformv1alpha1.Platform {
	p := newPlatform("acme", "team")
	p.UID = types.UID("uid-acme")
	p.Spec.Isolation = isolationVCluster
	return p
}

func kubeconfigSecret(p *platformv1alpha1.Platform, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: vclusterKubeconfigSecretName(), Namespace: PlatformNamespace(p)},
		Data:       map[string][]byte{key: []byte(targetTestKubeconfig)},
	}
}

func TestTargetClient_SelectsHostVsVCluster(t *testing.T) {
	host := fake.NewClientBuilder().WithScheme(vclusterScheme(t)).Build()

	t.Run("namespace tier writes through the host client", func(t *testing.T) {
		got, err := targetClient(context.Background(), host, nil, newPlatform("acme", "t"))
		if err != nil {
			t.Fatalf("targetClient (namespace): %v", err)
		}
		if got != host {
			t.Error("namespace tier must return the host client unchanged")
		}
	})

	t.Run("vcluster tier without a factory fails closed", func(t *testing.T) {
		_, err := targetClient(context.Background(), host, nil, uidVClusterPlatform())
		if !errors.Is(err, errVClusterFactoryUnset) {
			t.Fatalf("expected errVClusterFactoryUnset, got %v", err)
		}
	})

	t.Run("vcluster tier delegates to the factory", func(t *testing.T) {
		vc := fake.NewClientBuilder().WithScheme(vclusterScheme(t)).Build()
		got, err := targetClient(context.Background(), host, &fakeVClusterFactoryUnit{vc: vc}, uidVClusterPlatform())
		if err != nil {
			t.Fatalf("targetClient (vcluster): %v", err)
		}
		if got != vc {
			t.Error("vcluster tier must return the factory's client")
		}
	})
}

// fakeVClusterFactoryUnit is a unit-test stand-in for VClusterClientFactory (the
// conformance suite has its own; this keeps the unit tests self-contained).
type fakeVClusterFactoryUnit struct{ vc client.Client }

func (f *fakeVClusterFactoryUnit) ClientFor(context.Context, *platformv1alpha1.Platform) (client.Client, error) {
	return f.vc, nil
}
func (f *fakeVClusterFactoryUnit) Invalidate(*platformv1alpha1.Platform) {}

func TestCachedVClusterClientFactory_ClientFor(t *testing.T) {
	scheme := vclusterScheme(t)
	p := uidVClusterPlatform()

	t.Run("missing kubeconfig Secret reads as not-ready", func(t *testing.T) {
		host := fake.NewClientBuilder().WithScheme(scheme).Build()
		f := NewVClusterClientFactory(host, scheme)
		if _, err := f.ClientFor(context.Background(), p); !errors.Is(err, errVClusterNotReady) {
			t.Fatalf("a still-installing vcluster must read as not-ready, got %v", err)
		}
	})

	t.Run("a non-NotFound get error is wrapped", func(t *testing.T) {
		host := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("apiserver unavailable")
				},
			}).Build()
		f := NewVClusterClientFactory(host, scheme)
		_, err := f.ClientFor(context.Background(), p)
		if err == nil || errors.Is(err, errVClusterNotReady) {
			t.Fatalf("a hard Get error must surface (not not-ready), got %v", err)
		}
	})

	t.Run("empty kubeconfig Secret errors", func(t *testing.T) {
		empty := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: vclusterKubeconfigSecretName(), Namespace: PlatformNamespace(p)},
			Data:       map[string][]byte{"other": []byte("x")},
		}
		host := fake.NewClientBuilder().WithScheme(scheme).WithObjects(empty).Build()
		f := NewVClusterClientFactory(host, scheme)
		if _, err := f.ClientFor(context.Background(), p); err == nil {
			t.Fatal("a Secret with neither config nor value must error")
		}
	})

	t.Run("malformed kubeconfig errors", func(t *testing.T) {
		bad := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: vclusterKubeconfigSecretName(), Namespace: PlatformNamespace(p)},
			Data:       map[string][]byte{"config": []byte("not a kubeconfig")},
		}
		host := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bad).Build()
		f := NewVClusterClientFactory(host, scheme)
		if _, err := f.ClientFor(context.Background(), p); err == nil {
			t.Fatal("a malformed kubeconfig must error")
		}
	})

	t.Run("builds, caches, rebuilds on rotation, and invalidates", func(t *testing.T) {
		secret := kubeconfigSecret(p, "config")
		host := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		f := NewVClusterClientFactory(host, scheme)

		c1, err := f.ClientFor(context.Background(), p)
		if err != nil {
			t.Fatalf("build client: %v", err)
		}
		c2, err := f.ClientFor(context.Background(), p)
		if err != nil {
			t.Fatalf("second build: %v", err)
		}
		if c1 != c2 {
			t.Error("an unchanged kubeconfig Secret must serve the cached client")
		}

		// Rotate the Secret (a vcluster restart re-issues certs, bumping the
		// resourceVersion) — the factory must rebuild.
		var cur corev1.Secret
		if err := host.Get(context.Background(), types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, &cur); err != nil {
			t.Fatal(err)
		}
		cur.Data["config"] = []byte(targetTestKubeconfig + "\n") // any change
		if err := host.Update(context.Background(), &cur); err != nil {
			t.Fatal(err)
		}
		c3, err := f.ClientFor(context.Background(), p)
		if err != nil {
			t.Fatalf("rebuild after rotation: %v", err)
		}
		if c3 == c1 {
			t.Error("a rotated kubeconfig must rebuild the client, not serve the stale cache")
		}

		// Invalidate drops the cache; the next call rebuilds again.
		f.Invalidate(p)
		if c4, err := f.ClientFor(context.Background(), p); err != nil || c4 == c3 {
			t.Errorf("Invalidate must force a rebuild: c4==c3? %v err=%v", c4 == c3, err)
		}
		// Invalidate on an unknown Platform is safe.
		f.Invalidate(newPlatform("never-cached", "t"))
	})

	t.Run("accepts the value key when config is absent", func(t *testing.T) {
		secret := kubeconfigSecret(p, "value")
		host := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		f := NewVClusterClientFactory(host, scheme)
		if _, err := f.ClientFor(context.Background(), p); err != nil {
			t.Fatalf("a kubeconfig under the value key must be accepted: %v", err)
		}
	})
}

func TestDiscoverSyncedHostSA_NamingMismatchFailsLoud(t *testing.T) {
	scheme := vclusterScheme(t)
	p := uidVClusterPlatform()
	ns := PlatformNamespace(p)
	// A synced SA carrying the right labels/annotations but a name that does NOT
	// match the operator's computed cross-check — the signal that vcluster's
	// naming algorithm changed on an upgrade. The operator must refuse rather
	// than bind Pod Identity to the wrong name.
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-translated-name",
			Namespace: ns,
			Labels:    map[string]string{vclusterManagedByLabel: vclusterInstanceName},
			Annotations: map[string]string{
				vclusterObjectNameAnnotation:      tenantSAName,
				vclusterObjectNamespaceAnnotation: ns,
			},
		},
	}
	r := &PlatformReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sa).Build(), Scheme: scheme}
	_, err := r.discoverSyncedHostSA(context.Background(), p)
	if err == nil || errors.Is(err, errVClusterNotReady) {
		t.Fatalf("a synced-SA name mismatch must fail loud (not not-ready), got %v", err)
	}
}
