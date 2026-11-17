/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// Envoy Gateway's marks on a proxy ServiceAccount it owns: the labels its
// infrastructure provider renders (EnvoyAppLabel, the GatewayNamespace-mode
// gateway-name label and the owning-gateway labels — proxy/resource.go and
// gatewayapi/translator.go at v1.10.0) and the owner reference to the Gateway
// (or GatewayClass) it sets on everything it renders. The label values are the
// ones Envoy Gateway writes, so a label of the same key carrying anything else
// is left alone.
var envoyGatewayOwnedLabels = map[string]string{
	"app.kubernetes.io/name":       "envoy",
	"app.kubernetes.io/component":  "proxy",
	"app.kubernetes.io/managed-by": "envoy-gateway",
}

// envoyGatewayIdentityLabels are the keys whose presence alone is Envoy
// Gateway's: their values are the owning Gateway's name and namespace.
var envoyGatewayIdentityLabels = []string{
	"gateway.envoyproxy.io/owning-gateway-name",
	"gateway.envoyproxy.io/owning-gateway-namespace",
	"gateway.envoyproxy.io/owning-gatewayclass",
	"gateway.networking.k8s.io/gateway-name",
}

// gatewayAPIGroup is the API group of the Gateway and GatewayClass an Envoy
// Gateway owner reference points at.
const gatewayAPIGroup = "gateway.networking.k8s.io"

// releaseFromEnvoyGateway strips what Envoy Gateway leaves on a ServiceAccount
// it adopted, and reports whether there was anything to strip.
//
// Operator releases before the gateway got its own ServiceAccount pointed the
// EnvoyProxy at tenant-runtime. Envoy Gateway 1.8.x applied its own rendering
// of that account unconditionally in GatewayNamespace mode; 1.10.x does so for
// an account carrying its ownership labels, which is how a hand-labelled
// workaround reached the same state. Either way tenant-runtime may carry:
//
//   - an owner reference to the Gateway, through which garbage collection
//     deletes tenant-runtime when the ModelGateway's Gateway is deleted;
//   - Envoy Gateway's labels, through which its DeleteAllExcept — run after it
//     applies a proxy ServiceAccount of another name, which is the first thing
//     it does once the EnvoyProxy names <platform>-gateway — and its
//     deleteServiceAccount on Gateway deletion both select and delete it;
//   - automountServiceAccountToken: false, which the operator never sets and
//     which changes what every tenant pod is given.
//
// tenant-runtime is every tenant workload's identity, so all three go. The
// automount setting is reset only when one of the ownership marks shows Envoy
// Gateway wrote it; a value set any other way is not this function's to undo.
func releaseFromEnvoyGateway(sa *corev1.ServiceAccount) bool {
	adopted := false

	kept := sa.OwnerReferences[:0:0]
	for _, ref := range sa.OwnerReferences {
		if isGatewayOwnerRef(ref) {
			adopted = true
			continue
		}
		kept = append(kept, ref)
	}
	if adopted {
		sa.OwnerReferences = kept
		if len(sa.OwnerReferences) == 0 {
			sa.OwnerReferences = nil
		}
	}

	for k, v := range envoyGatewayOwnedLabels {
		if got, ok := sa.Labels[k]; ok && got == v {
			delete(sa.Labels, k)
			adopted = true
		}
	}
	for _, k := range envoyGatewayIdentityLabels {
		if _, ok := sa.Labels[k]; ok {
			delete(sa.Labels, k)
			adopted = true
		}
	}

	if adopted && sa.AutomountServiceAccountToken != nil && !*sa.AutomountServiceAccountToken {
		sa.AutomountServiceAccountToken = nil
	}
	return adopted
}

func isGatewayOwnerRef(ref metav1.OwnerReference) bool {
	group, _, _ := strings.Cut(ref.APIVersion, "/")
	return group == gatewayAPIGroup && (ref.Kind == "Gateway" || ref.Kind == "GatewayClass")
}

// releaseTenantServiceAccount releases the host tenant-runtime from Envoy
// Gateway (releaseFromEnvoyGateway) and reports whether it had to. The
// ModelGateway reconciler calls it before it renders the EnvoyProxy and before
// it deletes the Gateway, the two moments Envoy Gateway would otherwise delete
// an adopted tenant-runtime; the Platform reconciler's ensureTenantServiceAccount
// releases it too, on its own schedule.
//
// It touches nothing on the vcluster tier. There tenant-runtime lives in the
// virtual cluster and its host copy has a translated name, so a host
// tenant-runtime in the tenant namespace can only be one Envoy Gateway created
// for an earlier EnvoyProxy — its own to delete.
func releaseTenantServiceAccount(ctx context.Context, c client.Client, p *platformv1alpha1.Platform) (bool, error) {
	if p.Spec.Isolation == isolationVCluster {
		return false, nil
	}
	sa := &corev1.ServiceAccount{}
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: tenantSAName}
	if err := c.Get(ctx, key, sa); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read ServiceAccount %s: %w", key, err)
	}
	if !releaseFromEnvoyGateway(sa) {
		return false, nil
	}
	if err := c.Update(ctx, sa); err != nil {
		return true, fmt.Errorf("release ServiceAccount %s from Envoy Gateway: %w", key, err)
	}
	return true, nil
}
