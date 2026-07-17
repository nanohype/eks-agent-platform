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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// errVClusterFactoryUnset is returned when a Platform requests the vcluster
// isolation tier but no VClusterClientFactory is wired. The tier fails closed —
// it never silently downgrades to a host-client write — so a misconfigured
// operator degrades loud rather than landing tenant workloads on the host API.
var errVClusterFactoryUnset = errors.New("isolation: vcluster requires a VClusterClientFactory; none wired")

// VClusterClientFactory builds (and caches) a controller-runtime client for a
// Platform's virtual cluster, from the kubeconfig Secret vcluster publishes in
// the tenant host namespace. It is the vcluster-tier analogue of the interface-
// injected AWS clients: nil in the namespace tier and in the k8s-only test
// paths, faked in envtest so the target-client swap is exercised without a real
// virtual cluster, and validated for real on kx.
type VClusterClientFactory interface {
	// ClientFor returns a client whose writes land in the Platform's virtual
	// cluster API. Returns an error (never the host client) when the vcluster's
	// kubeconfig Secret is absent — the caller treats that as "not ready yet" and
	// requeues, so a vcluster still coming up never gets tenant writes on the host.
	ClientFor(ctx context.Context, p *platformv1alpha1.Platform) (client.Client, error)
	// Invalidate drops any cached client for a Platform. Called on teardown and
	// safe to call for a Platform that was never cached.
	Invalidate(p *platformv1alpha1.Platform)
}

// targetClient resolves the client a workload reconciler writes its in-cluster
// objects through: the host client for the namespace tier (unchanged behavior),
// the Platform's virtual-cluster client for the vcluster tier. It is a client
// swap, not a parallel reconcile path — the reconcile logic that decomposes a
// fleet/sandbox/gateway into objects is identical across tiers; only the API it
// writes to moves. Host-side containment (namespace, quota, PSS, NetworkPolicy,
// AppProject, IAM/KMS) always stays on the host client and is never routed here.
func targetClient(ctx context.Context, host client.Client, factory VClusterClientFactory, p *platformv1alpha1.Platform) (client.Client, error) {
	if p.Spec.Isolation != isolationVCluster {
		return host, nil
	}
	if factory == nil {
		return nil, errVClusterFactoryUnset
	}
	return factory.ClientFor(ctx, p)
}

// errVClusterNotReady signals the vcluster's kubeconfig Secret is not present
// yet — the virtual cluster is still installing. Callers requeue rather than
// error hard.
var errVClusterNotReady = errors.New("vcluster kubeconfig Secret not present yet")

// cachedVClusterClientFactory is the production VClusterClientFactory. It builds
// a controller-runtime client from the vc-<name> kubeconfig Secret in the tenant
// host namespace, caches it per Platform keyed on the Secret's resourceVersion,
// and rebuilds on rotation (a vcluster restart re-issues certs and bumps the
// Secret). The operator runs as a single leader (leader election), so a
// process-local cache is sufficient — the same reasoning that lets the shared
// bucket-policy mutex be process-local.
type cachedVClusterClientFactory struct {
	host   client.Client
	scheme *runtime.Scheme

	mu    sync.Mutex
	cache map[types.UID]cachedVClusterClient
}

type cachedVClusterClient struct {
	resourceVersion string
	client          client.Client
}

// NewVClusterClientFactory returns a production factory reading from host and
// building clients on scheme.
func NewVClusterClientFactory(host client.Client, scheme *runtime.Scheme) VClusterClientFactory {
	return &cachedVClusterClientFactory{
		host:   host,
		scheme: scheme,
		cache:  map[types.UID]cachedVClusterClient{},
	}
}

func (f *cachedVClusterClientFactory) ClientFor(ctx context.Context, p *platformv1alpha1.Platform) (client.Client, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: vclusterKubeconfigSecretName()}
	if err := f.host.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errVClusterNotReady
		}
		return nil, fmt.Errorf("get vcluster kubeconfig Secret %s: %w", key, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if cached, ok := f.cache[p.UID]; ok && cached.resourceVersion == secret.ResourceVersion {
		return cached.client, nil
	}

	// vcluster writes the admin kubeconfig under "config"; some releases also
	// mirror it to "value". Accept either so a version bump doesn't strand us.
	raw := secret.Data["config"]
	if len(raw) == 0 {
		raw = secret.Data["value"]
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("vcluster kubeconfig Secret %s has neither 'config' nor 'value' key", key)
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parse vcluster kubeconfig from %s: %w", key, err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: f.scheme})
	if err != nil {
		return nil, fmt.Errorf("build vcluster client for %s: %w", p.Name, err)
	}
	f.cache[p.UID] = cachedVClusterClient{resourceVersion: secret.ResourceVersion, client: c}
	return c, nil
}

func (f *cachedVClusterClientFactory) Invalidate(p *platformv1alpha1.Platform) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cache, p.UID)
}
