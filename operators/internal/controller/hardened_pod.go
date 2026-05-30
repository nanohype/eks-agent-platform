/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
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
func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptrTo(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
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
