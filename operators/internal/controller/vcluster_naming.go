/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// vcluster hard-isolation tier constants. See docs/adr/0009-vcluster-isolation-tier.md.
const (
	// isolationVCluster is the Platform.spec.isolation value that layers a
	// per-Platform virtual cluster on top of the host-side tenant provisioning.
	// The default tier ("namespace") is the empty/absent case everywhere the code
	// checks `!= isolationVCluster`, so only this value needs a constant.
	isolationVCluster = "vcluster"

	// vclusterInstanceName is the fixed Helm release name / instance name of the
	// per-Platform virtual cluster. vcluster's syncer uses this string as the
	// trailing suffix when it translates a synced object's virtual name to its
	// host name (SingleNamespaceHostName's suffix argument), so it is load-bearing
	// for the synced-SA name the Pod Identity association targets. Fixed and short:
	// each vcluster lives in its own tenants-<platform> host namespace, so the
	// release name need not encode the platform, and a short constant keeps the
	// synced host name in the un-truncated, human-legible regime for typical
	// platform names.
	vclusterInstanceName = "vcluster"

	// vclusterKubeconfigSecretPrefix is the prefix of the Secret vcluster
	// publishes its admin kubeconfig into, in the vcluster's host namespace. The
	// full name is vc-<instance-name>.
	vclusterKubeconfigSecretPrefix = "vc-"

	// vclusterManagedByLabel is stamped by the syncer on every host object it
	// creates, valued with the vcluster instance name. The operator selects on it
	// to discover synced objects and to assert the host namespace is drained of
	// synced state before the finalizer drops.
	vclusterManagedByLabel = "vcluster.loft.sh/managed-by"
	// vclusterObjectNameAnnotation carries the synced object's virtual name.
	vclusterObjectNameAnnotation = "vcluster.loft.sh/object-name"
	// vclusterObjectNamespaceAnnotation carries the synced object's virtual namespace.
	vclusterObjectNamespaceAnnotation = "vcluster.loft.sh/object-namespace"
)

// vclusterKubeconfigSecretName returns the Secret name vcluster writes its
// kubeconfig to in the vcluster's host namespace (vc-<instance-name>).
func vclusterKubeconfigSecretName() string {
	return vclusterKubeconfigSecretPrefix + vclusterInstanceName
}

// safeConcatName replicates vcluster's pkg/util/translate.SafeConcatName exactly
// (v0.35.x). It joins the parts with '-' and, when the result exceeds the 63-char
// DNS-label limit, keeps the first 52 characters, appends '-' plus the first 10
// hex characters of the SHA-256 digest of the full joined string, then collapses
// any ".-" sequence to "-" (vcluster's own cleanup for a prefix that ends on a
// dot). 52 + 1 + 10 = 63 = the limit.
//
// This MUST stay byte-identical to upstream: vcluster is the writer of the synced
// host ServiceAccount name, so the operator's computed name is only a valid
// cross-check if it reproduces vcluster's algorithm precisely — SHA-256, prefix
// 52, hash 10, the ".-" collapse. It deliberately does NOT reuse the operator's
// own fnv1a64/tenantRoleName truncation (FNV-1a, prefix budget, 8 hex): that
// would compute a different string and the Pod Identity association would bind
// nothing. Same discipline (concat, then hash-truncate), a different and
// non-negotiable algorithm. Covered by TestSafeConcatName_ByteIdenticalToUpstream.
func safeConcatName(parts ...string) string {
	full := strings.Join(parts, "-")
	if len(full) > 63 {
		digest := sha256.Sum256([]byte(full))
		return strings.ReplaceAll(full[0:52]+"-"+hex.EncodeToString(digest[:])[0:10], ".-", "-")
	}
	return full
}

// SyncedHostSAName returns the host ServiceAccount name the vcluster tier binds
// its Pod Identity association to for a given tenant namespace — the syncer-
// translated name of the virtual tenant-runtime SA, using the operator's fixed
// vcluster instance name. Exported for tooling and tests that must match the
// operator's computed name (e.g. correlating an
// `aws eks list-pod-identity-associations` entry back to a Platform).
func SyncedHostSAName(virtualNamespace string) string {
	return syncedHostSAName(virtualNamespace, vclusterInstanceName)
}

// syncedHostSAName returns the host ServiceAccount name vcluster's syncer mints
// for the virtual tenant-runtime ServiceAccount, given the virtual namespace it
// lives in and the vcluster instance name. It mirrors vcluster's
// SingleNamespaceHostName(name, namespace, suffix) = SafeConcatName(name, "x",
// namespace, "x", suffix) — order name-x-namespace-x-suffix.
//
// The Pod Identity association for a vcluster-tier Platform targets THIS name,
// not the virtual "tenant-runtime", because EKS Pod Identity resolves credentials
// by the pod's host (namespace, serviceAccountName) and the syncer rewrites
// serviceAccountName to this translated host name on every synced pod.
//
// vclusterName is kept an explicit parameter (rather than closing over the
// package constant) to mirror vcluster's own SingleNamespaceHostName(name, ns,
// suffix) signature and to let the byte-identity tests vary it.
//
//nolint:unparam // suffix is a first-class input to vcluster's naming algorithm
func syncedHostSAName(virtualNamespace, vclusterName string) string {
	return safeConcatName(tenantSAName, "x", virtualNamespace, "x", vclusterName)
}
