/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Shared hardened-pod construction. Every sandbox workload — the
// SandboxPool worker, the KEDA metrics bridge, and the per-session
// AgentSandbox pod — is built from these helpers, so the isolation
// posture has one source of truth.

// sandboxNodeLabel keys both the label and the NoSchedule taint on the
// dedicated Karpenter sandbox node pool (eks-gitops karpenter-resources).
// Sandbox pods carry the matching nodeSelector + toleration.
const sandboxNodeLabel = "agents.nanohype.dev/sandbox"

// metadataServiceCIDR is the cloud instance-metadata endpoint. Agent tool
// calls must never reach it, so the sandbox egress rules exclude it from
// the outbound HTTPS allow range.
const metadataServiceCIDR = "169.254.169.254/32"

// restrictedPodSecurityContext is the pod-level securityContext for the
// Pod Security "restricted" profile.
func restrictedPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptrTo(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// restrictedContainerSecurityContext is the container-level securityContext
// for the "restricted" profile: no privilege escalation, all capabilities
// dropped.
//
// It does NOT set readOnlyRootFilesystem, and that is the correct default for
// this helper: AgentFleet runs the tenant's own long-running application image,
// which this repo has no contract with. Forcing a read-only root on an arbitrary
// image breaks the ones that write to paths nobody enumerated, and a hardening
// measure that makes a workload fail to start is one an operator turns off
// wholesale.
//
// Sandbox workloads use sandboxContainerSecurityContext below instead. The split
// is the trust model made explicit rather than one posture stretched over two
// different situations.
func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptrTo(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// sandboxContainerSecurityContext hardens a container that runs UNTRUSTED agent
// code, adding a read-only root filesystem to the restricted profile.
//
// Pod Security's "restricted" profile does not require readOnlyRootFilesystem,
// so the profile label alone does not get it — which left the operator's own
// pod hardened further than the pods executing arbitrary tool calls, exactly
// inverted relative to the trust each deserves.
//
// A read-only root is only usable alongside somewhere to write, so it is paired
// with sandboxWritableMounts below and the two are never used apart.
func sandboxContainerSecurityContext() *corev1.SecurityContext {
	sc := restrictedContainerSecurityContext()
	sc.ReadOnlyRootFilesystem = ptrTo(true)
	return sc
}

// sandboxWritableVolumes / sandboxWritableMounts are the paths a sandbox
// container may write with a read-only root.
//
// WHOSE IMAGE DECIDES THE PATHS. /workspace and /tmp are the only two this code
// can name for an arbitrary image: /workspace because the AgentSandbox contract
// already mounted it before the root became read-only, and /tmp because
// approximately every runtime expects it and a tool failing on a missing temp
// dir reports it as its own bug rather than as a mount that is not there.
//
// Anything beyond those depends on a layout only the image knows. The
// SandboxPool worker runs THIS repo's image, so its HOME is knowable and passed
// in by that caller; an AgentSandbox runs whatever image the tenant names, so
// its extra paths come from spec.writablePaths. Deriving one image's layout and
// applying it to the other is how a read-only root turns into a workload that
// cannot start — the first version of this helper mounted /home/worker for both,
// which is correct for the worker and a guess for every tenant image.
//
// emptyDir throughout, so nothing survives the session — which is most of what
// makes a single-use sandbox single-use.
func sandboxWritablePaths(extra ...string) []string {
	paths := []string{"/workspace", "/tmp"}
	seen := map[string]bool{"/workspace": true, "/tmp": true}
	for _, e := range extra {
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		paths = append(paths, e)
	}
	return paths
}

// volumeNameForPath renders an RFC-1123 volume name from a mount path, so a
// tenant-declared path cannot produce a name the API server rejects.
func volumeNameForPath(path string) string {
	b := make([]rune, 0, len(path))
	for _, r := range strings.ToLower(strings.Trim(path, "/")) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, r)
		default:
			b = append(b, '-')
		}
	}
	name := strings.Trim(string(b), "-")
	if name == "" {
		name = "writable"
	}
	if len(name) > 63 {
		name = name[:63]
		name = strings.Trim(name, "-")
	}
	return name
}

func sandboxWritableVolumes(extra ...string) []corev1.Volume {
	paths := sandboxWritablePaths(extra...)
	out := make([]corev1.Volume, 0, len(paths))
	for _, p := range paths {
		out = append(out, corev1.Volume{
			Name:         volumeNameForPath(p),
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return out
}

func sandboxWritableMounts(extra ...string) []corev1.VolumeMount {
	paths := sandboxWritablePaths(extra...)
	out := make([]corev1.VolumeMount, 0, len(paths))
	for _, p := range paths {
		out = append(out, corev1.VolumeMount{Name: volumeNameForPath(p), MountPath: p})
	}
	return out
}

// sandboxNodeSelector pins a pod to the dedicated, tainted sandbox node
// pool — keeping sandbox workloads off the nodes that run operator,
// system, or other tenant pods.
func sandboxNodeSelector() map[string]string {
	return map[string]string{sandboxNodeLabel: "true"}
}

// sandboxTolerations lets a pod schedule onto the NoSchedule-tainted
// sandbox node pool.
func sandboxTolerations() []corev1.Toleration {
	return []corev1.Toleration{{
		Key:      sandboxNodeLabel,
		Operator: corev1.TolerationOpEqual,
		Value:    "true",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
}

// sandboxEgressRules are the NetworkPolicy egress rules shared by sandbox
// workloads: kube-dns for resolution, and outbound HTTPS to any address
// except the cloud instance-metadata endpoint. A plain NetworkPolicy
// cannot match an FQDN, so HTTPS is allowed broadly with that one
// exclusion. Ingress stays deny-all — the caller leaves it nil.
func sandboxEgressRules() []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)
	httpsPort := intstr.FromInt(443)
	return []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}, {Protocol: &tcp, Port: &dnsPort}},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{metadataServiceCIDR}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &httpsPort}},
		},
	}
}
