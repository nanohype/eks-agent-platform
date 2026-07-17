/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// conditionVClusterReady is the status condition surfacing the health of a
// vcluster-tier Platform's virtual cluster. False while installing, on a missing
// ArgoCD, or on a synced-SA naming mismatch; True once the vcluster is up, the
// tenant ServiceAccount has synced to the host, and the Pod Identity binding
// targets it. A PrometheusRule alerts on it (charts/operator/files/slo).
const conditionVClusterReady = "VClusterReady"

// errArgoCDRequired is returned when a Platform requests the vcluster tier on a
// cluster without ArgoCD. The tier depends on ArgoCD to install the vcluster and
// register its destination; there is no silent downgrade to namespace isolation.
var errArgoCDRequired = errors.New("isolation: vcluster requires ArgoCD (AppProject/Application CRDs absent)")

// VClusterConfig carries the operator-chart-configured coordinates for the
// vcluster Helm chart the operator declares per Platform, plus the in-vcluster
// init-charts that bootstrap the tenant's fleet control plane (kagent) and
// autoscaler (KEDA) inside the isolation boundary.
type VClusterConfig struct {
	// ChartRepoURL is the vcluster chart repository (https://charts.loft.sh, or
	// the OCI mirror oci://ghcr.io/loft-sh/charts/vcluster). Also added to the
	// Platform AppProject's sourceRepos allow-list for the vcluster tier.
	ChartRepoURL string
	// ChartVersion pins the vcluster chart's targetRevision (e.g. 0.35.2).
	// Renovate proposes bumps; a human reviews — the operator never auto-upgrades.
	ChartVersion string
	// InitCharts are deployed INSIDE each per-Platform vcluster on startup via the
	// chart's experimental.deploy.vcluster.helm. The recommended placement for
	// kagent + KEDA so the tenant's control+data plane live entirely inside the
	// virtual cluster. Empty ships a containment-only vcluster.
	InitCharts []VClusterInitChart
}

// VClusterInitChart is one entry in experimental.deploy.vcluster.helm. JSON tags
// let the operator chart render the structured vcluster.initCharts values into
// the --vcluster-init-charts flag, so chart versions stay Renovate-visible in
// values.yaml rather than buried in flags.
type VClusterInitChart struct {
	ChartName   string `json:"chartName"`
	RepoURL     string `json:"repoURL"`
	Version     string `json:"version"`
	ReleaseName string `json:"releaseName"`
	Namespace   string `json:"namespace"`
	// Values is a YAML string handed to the init-chart verbatim (optional).
	Values string `json:"values,omitempty"`
}

// vclusterAppName is the ArgoCD Application name the operator declares for a
// Platform's virtual cluster. Distinct from the AppProject (named after the
// Platform) so both can coexist in the argocd namespace.
func vclusterAppName(p *platformv1alpha1.Platform) string {
	return safeConcatName(p.Name, "vcluster")
}

// vclusterInClusterServer is the in-cluster API endpoint of a Platform's virtual
// cluster — the vcluster Service in the tenant host namespace. Consumed both by
// exportKubeConfig.server (so the published kubeconfig is reachable from other
// pods) and by the ArgoCD cluster-Secret registration.
func vclusterInClusterServer(p *platformv1alpha1.Platform) string {
	return fmt.Sprintf("https://%s.%s.svc", vclusterInstanceName, PlatformNamespace(p))
}

// vclusterClusterSecretName is the ArgoCD cluster-Secret name registering a
// Platform's virtual cluster as a deploy destination. Keyed on the (globally
// unique) tenant host namespace so two same-named Platforms in different
// control-plane namespaces don't collide in the shared argocd namespace.
func vclusterClusterSecretName(p *platformv1alpha1.Platform) string {
	return safeConcatName(PlatformNamespace(p), "vcluster")
}

// buildVClusterValues renders the per-Platform vcluster.yaml the operator hands
// to the vcluster chart through the Application's helm.valuesObject. It sets the
// load-bearing bits ADR 0009 mandates:
//   - sync.toHost.serviceAccounts.enabled: true — so the tenant tenant-runtime SA
//     materializes on the host under a syncer-generated name, the target of the
//     Pod Identity association.
//   - exportKubeConfig.server — the in-cluster Service endpoint, so the published
//     kubeconfig is reachable by the operator and by in-cluster ArgoCD.
//   - experimental.deploy.vcluster.helm — kagent + KEDA bootstrapped inside the
//     virtual cluster (when configured), the recommended fleet placement.
//
// It deliberately does NOT enable multi-namespace sync: single-namespace mode is
// the vcluster default and this design mandates it, keeping the syncer's host
// RBAC scoped to the one tenant namespace. The syncer's own ServiceAccount gets
// no Pod Identity association (see the finalizer + threat model), so it has no
// AWS reach.
func buildVClusterValues(p *platformv1alpha1.Platform, cfg VClusterConfig) map[string]interface{} {
	// The tenant namespace enforces Pod Security Standards `restricted`, and this
	// design relies on that admission bounding the vcluster control-plane pod and
	// its syncer from outside. But the vcluster chart's control plane defaults to
	// runAsUser=0 and does not drop capabilities, so PSS-restricted rejects its
	// pod outright. Set a restricted-compliant securityContext on the control-plane
	// StatefulSet (non-root, all capabilities dropped, no privilege escalation,
	// RuntimeDefault seccomp) so the vcluster pod is admitted by — and stays inside
	// — the same policy that bounds every tenant pod. fsGroup lets the non-root
	// control plane own its data volume.
	// int64 (not int): these values flow into an unstructured Application via
	// SetNestedField, whose deep-copy accepts only JSON-native scalar types.
	const vclusterUID int64 = 12345
	// A fresh copy per consumer — the same map value must not be aliased into two
	// places in the values tree (unstructured deep-copy + Helm would share it).
	containerSecurity := func() map[string]interface{} {
		return map[string]interface{}{
			"allowPrivilegeEscalation": false,
			"runAsNonRoot":             true,
			"runAsUser":                vclusterUID,
			"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
			"seccompProfile":           map[string]interface{}{"type": "RuntimeDefault"},
		}
	}
	podSecurity := func() map[string]interface{} {
		return map[string]interface{}{
			"runAsNonRoot":   true,
			"runAsUser":      vclusterUID,
			"runAsGroup":     vclusterUID,
			"fsGroup":        vclusterUID,
			"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
		}
	}
	statefulSetSecurity := func() map[string]interface{} {
		return map[string]interface{}{
			"podSecurityContext":       podSecurity(),
			"containerSecurityContext": containerSecurity(),
		}
	}
	values := map[string]interface{}{
		"sync": map[string]interface{}{
			"toHost": map[string]interface{}{
				"serviceAccounts": map[string]interface{}{
					"enabled": true,
				},
			},
		},
		"exportKubeConfig": map[string]interface{}{
			"server": vclusterInClusterServer(p),
		},
		"controlPlane": map[string]interface{}{
			// The operator's target client + the ArgoCD cluster registration reach
			// the vcluster at its in-cluster Service FQDN (exportKubeConfig.server),
			// but the vcluster proxy cert does not include the `.svc` form by
			// default — so sign it for both the `.svc` and fully-qualified names, or
			// the operator's client fails TLS verification.
			"proxy": map[string]interface{}{
				"extraSANs": []interface{}{
					fmt.Sprintf("%s.%s.svc", vclusterInstanceName, PlatformNamespace(p)),
					fmt.Sprintf("%s.%s.svc.cluster.local", vclusterInstanceName, PlatformNamespace(p)),
				},
			},
			// The control plane runs two containers in the StatefulSet pod: the
			// distro ("kubernetes") and the syncer. Both need a restricted-
			// compliant securityContext or the tenant namespace's PSS-restricted
			// admission rejects the pod. The syncer is covered by statefulSet
			// .security.containerSecurityContext; the distro container has its own
			// path under distro.k8s.securityContext.
			"distro": map[string]interface{}{
				"k8s": map[string]interface{}{
					"securityContext": containerSecurity(),
				},
			},
			"statefulSet": map[string]interface{}{
				"security": statefulSetSecurity(),
			},
			"coredns": map[string]interface{}{
				"security": statefulSetSecurity(),
			},
		},
	}
	if len(cfg.InitCharts) > 0 {
		helm := make([]interface{}, 0, len(cfg.InitCharts))
		for _, ic := range cfg.InitCharts {
			entry := map[string]interface{}{
				"chart": map[string]interface{}{
					"name":    ic.ChartName,
					"repo":    ic.RepoURL,
					"version": ic.Version,
				},
				"release": map[string]interface{}{
					"name":      ic.ReleaseName,
					"namespace": ic.Namespace,
				},
			}
			if ic.Values != "" {
				entry["values"] = ic.Values
			}
			helm = append(helm, entry)
		}
		values["experimental"] = map[string]interface{}{
			"deploy": map[string]interface{}{
				"vcluster": map[string]interface{}{
					"helm": helm,
				},
			},
		}
	}
	return values
}

// argoApplicationGVK is the ArgoCD Application kind the operator declares the
// vcluster as — the same unstructured mechanism it uses for the AppProject, so
// the operator gains no Helm code and no vcluster Go types.
var argoApplicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// ensureVClusterApplication declares (idempotently) the ArgoCD Application whose
// source is the upstream vcluster chart pinned to cfg.ChartVersion, destination
// the tenant host namespace, project the Platform's AppProject. ArgoCD performs
// the Helm install; the operator never shells out to Helm. Returns a NoKindMatch
// error when ArgoCD's CRDs are absent — the caller maps that to errArgoCDRequired
// (fail closed, never a silent downgrade).
func (r *PlatformReconciler) ensureVClusterApplication(ctx context.Context, p *platformv1alpha1.Platform, cfg VClusterConfig) error {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argoApplicationGVK)
	app.SetName(vclusterAppName(p))
	app.SetNamespace(argoCDNamespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, app, func() error {
		app.SetLabels(labelsForPlatform(p))
		spec := map[string]interface{}{
			"project": p.Name,
			"source": map[string]interface{}{
				"repoURL":        cfg.ChartRepoURL,
				"chart":          "vcluster",
				"targetRevision": cfg.ChartVersion,
				"helm": map[string]interface{}{
					// Release name = the vcluster instance name; the syncer uses it
					// as the host-name suffix, so it must equal vclusterInstanceName.
					"releaseName":  vclusterInstanceName,
					"valuesObject": buildVClusterValues(p, cfg),
				},
			},
			"destination": map[string]interface{}{
				"namespace": PlatformNamespace(p),
				"server":    "https://kubernetes.default.svc",
			},
			"syncPolicy": map[string]interface{}{
				"automated": map[string]interface{}{
					"prune":    true,
					"selfHeal": true,
				},
				"syncOptions": []interface{}{"CreateNamespace=false"},
			},
		}
		return unstructured.SetNestedField(app.Object, spec, "spec")
	})
	return err
}

// vclusterControlPlaneSelector matches the vcluster control-plane StatefulSet
// pod (the chart labels it app=vcluster, release=<instance>). Used to scope the
// kube-apiserver egress allowance to that one pod.
func vclusterControlPlaneSelector() map[string]string {
	return map[string]string{"app": "vcluster", "release": vclusterInstanceName}
}

// ensureVClusterControlPlaneEgress opens, for the vcluster control-plane pod
// only, egress to the HOST kube-apiserver (and DNS). The tenant namespace's
// default-deny egress policy exists precisely to keep tenant pods off the host —
// but the vcluster syncer MUST reach the host API to read its config and sync
// objects, or its pod crash-loops on `dial tcp <apiserver>:443: i/o timeout`.
// Scoping the allowance to the vcluster pod's own labels (not the namespace-wide
// selector) keeps the host API unreachable from every tenant workload pod, which
// is the whole point of the tier — only the trusted syncer gets through.
//
// The rule is additive to the namespace-wide tenant egress under cilium (per-
// endpoint allow-lists union), so the vcluster pod keeps DNS/agentgateway/OTel
// and gains the apiserver. Runs on the host client.
func (r *PlatformReconciler) ensureVClusterControlPlaneEgress(ctx context.Context, p *platformv1alpha1.Platform) error {
	ns := PlatformNamespace(p)
	labels := labelsForPlatform(p)
	if r.NetworkEngine == NetworkEngineCilium {
		cnp := &unstructured.Unstructured{}
		cnp.SetGroupVersionKind(ciliumNetworkPolicyGVK)
		cnp.SetName("vcluster-control-plane")
		cnp.SetNamespace(ns)
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cnp, func() error {
			cnp.SetLabels(labels)
			spec := map[string]interface{}{
				"endpointSelector": map[string]interface{}{"matchLabels": map[string]interface{}{
					"app":     "vcluster",
					"release": vclusterInstanceName,
				}},
				"egress": []interface{}{
					// Host kube-apiserver (reserved entity — an ipBlock cannot match
					// it under cilium, the same reason the operator's own policy uses
					// this entity).
					map[string]interface{}{"toEntities": []interface{}{"kube-apiserver"}},
					// DNS.
					map[string]interface{}{
						"toEndpoints": []interface{}{map[string]interface{}{"matchLabels": map[string]interface{}{
							"k8s:io.kubernetes.pod.namespace": "kube-system",
							"k8s:k8s-app":                     "kube-dns",
						}}},
						"toPorts": []interface{}{map[string]interface{}{"ports": []interface{}{
							map[string]interface{}{"port": "53", "protocol": "UDP"},
							map[string]interface{}{"port": "53", "protocol": "TCP"},
						}}},
					},
				},
			}
			return unstructured.SetNestedField(cnp.Object, spec, "spec")
		})
		if isNoKindMatch(err) {
			return nil
		}
		return err
	}
	// Vanilla NetworkPolicy fallback (non-cilium): the vcluster pod reaches the
	// apiserver on 443/6443. Scoped to the vcluster pod, so tenant workload pods
	// keep their default-deny.
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "vcluster-control-plane", Namespace: ns}}
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	apiPort := intstr.FromInt(443)
	apiPortAlt := intstr.FromInt(6443)
	dnsPort := intstr.FromInt(53)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = labels
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: vclusterControlPlaneSelector()},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &apiPort}, {Protocol: &tcp, Port: &apiPortAlt}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}, {Protocol: &tcp, Port: &dnsPort}},
				},
			},
		}
		return nil
	})
	return err
}

// ensureVClusterInternalBootstrap creates, inside the virtual cluster, the tenant
// workload namespace and the tenant-runtime ServiceAccount. The SA is what
// vcluster's syncer copies down to the host under a translated name — the whole
// reason SA sync is enabled. Writes go through the vcluster client, never the
// host client.
func (r *PlatformReconciler) ensureVClusterInternalBootstrap(ctx context.Context, vc client.Client, p *platformv1alpha1.Platform) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace(p)}}
	if _, err := controllerutil.CreateOrUpdate(ctx, vc, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		for k, v := range labelsForPlatform(p) {
			ns.Labels[k] = v
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensure vcluster-internal namespace: %w", err)
	}
	if err := ensureTenantServiceAccount(ctx, vc, p); err != nil {
		return fmt.Errorf("ensure vcluster-internal tenant ServiceAccount: %w", err)
	}
	return nil
}

// discoverSyncedHostSA finds the host ServiceAccount vcluster's syncer minted for
// the virtual tenant-runtime SA — discovery-first (robust to vcluster changing
// its naming across versions), then cross-checked against the operator's replica
// of vcluster's own algorithm. Returns errVClusterNotReady while the SA has not
// synced yet (caller requeues). Returns a hard error if the discovered name does
// not match the computed one — a signal that vcluster's naming changed on an
// upgrade; the operator refuses to bind Pod Identity to the wrong name rather
// than silently strand the tenant's credentials.
func (r *PlatformReconciler) discoverSyncedHostSA(ctx context.Context, p *platformv1alpha1.Platform) (string, error) {
	virtualNs := PlatformNamespace(p)
	var list corev1.ServiceAccountList
	if err := r.List(ctx, &list,
		client.InNamespace(virtualNs),
		client.MatchingLabels{vclusterManagedByLabel: vclusterInstanceName},
	); err != nil {
		return "", fmt.Errorf("list synced ServiceAccounts: %w", err)
	}
	for i := range list.Items {
		sa := &list.Items[i]
		ann := sa.Annotations
		if ann[vclusterObjectNameAnnotation] != tenantSAName || ann[vclusterObjectNamespaceAnnotation] != virtualNs {
			continue
		}
		computed := syncedHostSAName(virtualNs, vclusterInstanceName)
		if sa.Name != computed {
			return "", fmt.Errorf(
				"vcluster synced ServiceAccount name %q does not match computed %q: vcluster's naming algorithm may have changed on an upgrade — refusing to bind Pod Identity to the wrong name",
				sa.Name, computed)
		}
		return sa.Name, nil
	}
	return "", errVClusterNotReady
}

// ensureVClusterClusterSecret registers the virtual cluster as an ArgoCD cluster
// Secret (label argocd.argoproj.io/secret-type: cluster), so the tenant's
// ApplicationSet entry can target it as a destination. Server + credentials come
// from the vcluster kubeconfig Secret the operator already reads. Scoped by the
// Platform AppProject's destinations (ensureAppProject) so the tenant can deploy
// only into its own vcluster.
func (r *PlatformReconciler) ensureVClusterClusterSecret(ctx context.Context, p *platformv1alpha1.Platform) error {
	var kube corev1.Secret
	kubeKey := client.ObjectKey{Namespace: PlatformNamespace(p), Name: vclusterKubeconfigSecretName()}
	if err := r.Get(ctx, kubeKey, &kube); err != nil {
		if apierrors.IsNotFound(err) {
			return errVClusterNotReady
		}
		return fmt.Errorf("get vcluster kubeconfig for ArgoCD registration: %w", err)
	}
	raw := kube.Data["config"]
	if len(raw) == 0 {
		raw = kube.Data["value"]
	}
	if len(raw) == 0 {
		return fmt.Errorf("vcluster kubeconfig Secret %s empty", kubeKey)
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return fmt.Errorf("parse vcluster kubeconfig for ArgoCD registration: %w", err)
	}
	// ArgoCD cluster config JSON. []byte fields marshal as base64.
	type tlsClientConfig struct {
		Insecure bool   `json:"insecure,omitempty"`
		CAData   []byte `json:"caData,omitempty"`
		CertData []byte `json:"certData,omitempty"`
		KeyData  []byte `json:"keyData,omitempty"`
	}
	cfgJSON, err := json.Marshal(struct {
		TLSClientConfig tlsClientConfig `json:"tlsClientConfig"`
	}{TLSClientConfig: tlsClientConfig{
		CAData:   restCfg.CAData,
		CertData: restCfg.CertData,
		KeyData:  restCfg.KeyData,
	}})
	if err != nil {
		return fmt.Errorf("marshal ArgoCD cluster config: %w", err)
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      vclusterClusterSecretName(p),
		Namespace: argoCDNamespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		labels := labelsForPlatform(p)
		labels["argocd.argoproj.io/secret-type"] = "cluster"
		secret.Labels = labels
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			"name":   []byte(PlatformNamespace(p)),
			"server": []byte(vclusterInClusterServer(p)),
			"config": cfgJSON,
		}
		return nil
	})
	return err
}

// hostHasVClusterManagedObjects reports whether any host object the syncer
// created still lingers in the tenant namespace — the finalizer's drain gate. It
// checks the two kinds the syncer lands per this design (ServiceAccounts, Pods);
// a non-empty result means the vcluster teardown has not finished and the
// finalizer must not drop yet.
func (r *PlatformReconciler) hostHasVClusterManagedObjects(ctx context.Context, p *platformv1alpha1.Platform) (bool, error) {
	sel := client.MatchingLabels{vclusterManagedByLabel: vclusterInstanceName}
	inNs := client.InNamespace(PlatformNamespace(p))

	var sas corev1.ServiceAccountList
	if err := r.List(ctx, &sas, inNs, sel); err != nil {
		if isNoKindMatch(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("list synced ServiceAccounts (drain check): %w", err)
	}
	if len(sas.Items) > 0 {
		return true, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, inNs, sel); err != nil {
		if isNoKindMatch(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("list synced Pods (drain check): %w", err)
	}
	return len(pods.Items) > 0, nil
}

// deleteVClusterApplication removes the operator-declared vcluster Application;
// ArgoCD uninstalls the Helm release (control-plane StatefulSet, syncer, synced
// host objects). Tolerates NoKindMatch/NotFound so finalizer re-runs are safe.
func (r *PlatformReconciler) deleteVClusterApplication(ctx context.Context, p *platformv1alpha1.Platform) error {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argoApplicationGVK)
	app.SetName(vclusterAppName(p))
	app.SetNamespace(argoCDNamespace)
	if err := r.Delete(ctx, app); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
		return fmt.Errorf("delete vcluster Application: %w", err)
	}
	return nil
}

// deleteVClusterClusterSecret removes the ArgoCD cluster-Secret registration.
func (r *PlatformReconciler) deleteVClusterClusterSecret(ctx context.Context, p *platformv1alpha1.Platform) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      vclusterClusterSecretName(p),
		Namespace: argoCDNamespace,
	}}
	if err := r.Delete(ctx, secret, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete vcluster ArgoCD cluster Secret: %w", err)
	}
	return nil
}

// deleteTenantAppApplications best-effort removes any ArgoCD Applications the
// tenant's ApplicationSet may have created that target this Platform's vcluster,
// scoped by the platform label — step 1 of the teardown order. The
// ApplicationSet is the authoritative owner; this is a belt-and-braces sweep so
// a torn-down Platform never leaves an Application pointing at a vanishing
// destination. Tolerates NoKindMatch/none.
func (r *PlatformReconciler) deleteTenantAppApplications(ctx context.Context, p *platformv1alpha1.Platform) error {
	var apps unstructured.UnstructuredList
	apps.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ApplicationList"})
	if err := r.List(ctx, &apps, client.InNamespace(argoCDNamespace), client.MatchingLabels{LabelPlatform: p.Name}); err != nil {
		if isNoKindMatch(err) || apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list tenant Applications: %w", err)
	}
	for i := range apps.Items {
		app := &apps.Items[i]
		// Don't delete the operator's own vcluster Application here — it's removed
		// explicitly, in order, by deleteVClusterApplication.
		if app.GetName() == vclusterAppName(p) {
			continue
		}
		if err := r.Delete(ctx, app); err != nil && !apierrors.IsNotFound(err) && !isNoKindMatch(err) {
			return fmt.Errorf("delete tenant Application %s: %w", app.GetName(), err)
		}
	}
	return nil
}

// reconcileVClusterTier stands up (idempotently) the virtual cluster and its
// containment plumbing for a vcluster-tier Platform, returning ready=true only
// once the vcluster is installed, the tenant SA has synced to the host, and the
// synced name matches the computed cross-check. It fails closed: ArgoCD absent →
// errArgoCDRequired; a naming mismatch → hard error; still converging →
// (false, nil) so the caller requeues without erroring.
func (r *PlatformReconciler) reconcileVClusterTier(ctx context.Context, p *platformv1alpha1.Platform) (bool, error) {
	if r.VCluster == nil {
		return false, errVClusterFactoryUnset
	}
	// 1. Declare the vcluster Application (ArgoCD installs it). ArgoCD is a hard
	//    prerequisite — no silent downgrade to namespace isolation.
	if err := r.ensureVClusterApplication(ctx, p, r.VClusterCfg); err != nil {
		if isNoKindMatch(err) {
			return false, errArgoCDRequired
		}
		return false, fmt.Errorf("ensure vcluster Application: %w", err)
	}
	// 1a. Open the vcluster control-plane pod's egress to the host apiserver
	//     BEFORE it starts, so its syncer doesn't crash-loop on the tenant
	//     namespace's default-deny egress. Scoped to the vcluster pod only.
	if err := r.ensureVClusterControlPlaneEgress(ctx, p); err != nil {
		return false, fmt.Errorf("ensure vcluster control-plane egress: %w", err)
	}
	// 2. Is the vcluster up (kubeconfig published)?
	vc, err := r.VCluster.ClientFor(ctx, p)
	if err != nil {
		if errors.Is(err, errVClusterNotReady) {
			return false, nil // still installing — requeue
		}
		return false, fmt.Errorf("build vcluster client: %w", err)
	}
	// 3. Create the tenant SA inside the vcluster so the syncer copies it to host.
	if err := r.ensureVClusterInternalBootstrap(ctx, vc, p); err != nil {
		return false, err
	}
	// 4. Register the vcluster as an ArgoCD destination (tolerate not-ready +
	//    NoKindMatch — the AppProject destination scoping in ensureAppProject is
	//    the other half and always runs).
	if err := r.ensureVClusterClusterSecret(ctx, p); err != nil {
		if errors.Is(err, errVClusterNotReady) {
			return false, nil
		}
		if !isNoKindMatch(err) {
			return false, err
		}
	}
	// 5. Discover + cross-check the synced host SA (requeue until it appears).
	if _, err := r.discoverSyncedHostSA(ctx, p); err != nil {
		if errors.Is(err, errVClusterNotReady) {
			return false, nil
		}
		return false, err // naming mismatch — fail loud
	}
	return true, nil
}

// cleanupVClusterResources tears down a vcluster-tier Platform in the reverse of
// provisioning, finalizer-gated (ADR 0009 teardown order):
//  1. delete tenant app Applications targeting the vcluster,
//  2. delete the ArgoCD cluster-Secret registration,
//  3. delete the vcluster Application (ArgoCD uninstalls the release),
//  4. delete the synced-SA Pod Identity association (AWS-side; a namespace delete
//     will NOT reap it),
//  5. gate: refuse to return clean until no vcluster-managed host objects remain.
//
// Steps 1–4 are idempotent and NotFound/NoKindMatch-tolerant so finalizer
// re-runs are safe. The host cleanup (namespace, IAM role, KMS, bucket policy)
// runs after this in the finalizer flow, unchanged.
func (r *PlatformReconciler) cleanupVClusterResources(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig) error {
	if r.VCluster != nil {
		r.VCluster.Invalidate(p)
	}
	if err := r.deleteTenantAppApplications(ctx, p); err != nil {
		return err
	}
	if err := r.deleteVClusterClusterSecret(ctx, p); err != nil {
		return err
	}
	if err := r.deleteVClusterApplication(ctx, p); err != nil {
		return err
	}
	// The synced host SA name is deterministic, so we can delete its Pod Identity
	// association without discovering the (possibly already-gone) SA.
	syncedSA := syncedHostSAName(PlatformNamespace(p), vclusterInstanceName)
	if err := r.deletePodIdentityAssociation(ctx, cfg, PlatformNamespace(p), syncedSA); err != nil {
		return fmt.Errorf("delete synced-SA Pod Identity association: %w", err)
	}
	// Drain gate: the finalizer must not release until the vcluster and every host
	// object it synced are gone.
	lingering, err := r.hostHasVClusterManagedObjects(ctx, p)
	if err != nil {
		return err
	}
	if lingering {
		return fmt.Errorf("vcluster teardown in progress: host objects labelled %s=%s still present in %s",
			vclusterManagedByLabel, vclusterInstanceName, PlatformNamespace(p))
	}
	return nil
}
