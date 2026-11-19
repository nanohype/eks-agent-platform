/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// extProcContainerName is the container Envoy AI Gateway's pod webhook injects
// into every Envoy pod of a Gateway that has AIGatewayRoutes: the external
// processor that translates each request, applies the route's model mapping and
// SigV4-signs it for Bedrock. Envoy's ext_proc filter is configured with
// FailureModeAllow false, so an Envoy pod without one answers every model
// request with HTTP 500. ai-gateway (mutationNamePrefix + "extproc",
// internal/controller/gateway_mutator.go at v1.2.1).
const extProcContainerName = "ai-gateway-extproc"

// The labels Envoy Gateway stamps on every proxy pod it renders for a Gateway
// (OwningGatewayNameLabel / OwningGatewayNamespaceLabel,
// internal/gatewayapi/translator.go at v1.10.0). Both are selected on, so a pod
// of a same-named Gateway in another namespace is never counted.
const (
	envoyOwningGatewayNameLabel      = "gateway.envoyproxy.io/owning-gateway-name"
	envoyOwningGatewayNamespaceLabel = "gateway.envoyproxy.io/owning-gateway-namespace"
)

// The DataPlaneProgrammed reasons for a Gateway Envoy Gateway reports
// Programmed whose serving pods cannot yet sign a request.
const (
	// reasonNoReadyEnvoyPod: Programmed, but the pod read found no Ready,
	// non-terminating Envoy pod — the Deployment's status and its pods
	// disagreeing for a moment, or every pod being replaced at once.
	reasonNoReadyEnvoyPod = "NoReadyEnvoyPod"
	// reasonExtProcNotInjected: a Ready Envoy pod carries no extproc container.
	// The observed case is the first pod of every new Gateway, admitted before
	// Envoy AI Gateway had written the filter-config Secret.
	reasonExtProcNotInjected = "ExtProcNotInjected"
	// reasonExtProcNotReady: every Ready Envoy pod carries the extproc, and at
	// least one of those containers is not passing its readiness probe.
	reasonExtProcNotReady = "ExtProcNotReady"
)

// envoyRolloutRequeueInterval is how often a ModelGateway held on its Envoy
// pods looks again. The wait is a ReplicaSet rollout Envoy AI Gateway's own
// controller has already started, so it ends in seconds rather than the
// minute a fresh Gateway takes to reach Programmed.
const envoyRolloutRequeueInterval = 5 * time.Second

// awaitingEnvoyPods reports whether a DataPlaneProgrammed reason is one of the
// pod-level holds above, which re-check faster than an unprogrammed Gateway.
func awaitingEnvoyPods(reason string) bool {
	switch reason {
	case reasonNoReadyEnvoyPod, reasonExtProcNotInjected, reasonExtProcNotReady:
		return true
	}
	return false
}

// envoyPodsCanSign decides whether a Programmed Gateway's serving Envoy pods
// can all answer a model request.
//
// Programmed is not enough. Envoy Gateway sets it on AvailableReplicas > 0
// (internal/gatewayapi/status/gateway.go at v1.10.0), and Envoy AI Gateway
// injects the extproc only at pod CREATE — and admits the pod unmutated when the
// Gateway's filter-config Secret is missing (gateway_mutator.go). Its controller
// cannot write that Secret until the Envoy Deployment and its pods exist, so the
// first pod of every new Gateway runs without the extproc, Programmed goes True
// on it, and every request it takes returns 500 until the controller's
// annotation bump rolls a second ReplicaSet. Ready therefore waits until every
// Ready, non-terminating Envoy pod carries a ready extproc.
//
// The pods are read through APIReader — uncached, namespace-scoped and
// label-selected to this Gateway — so the operator holds no Pod informer for a
// check that runs once per pass. Terminating pods are skipped: they have left
// the Service's endpoints and take no new connection.
func (r *ModelGatewayReconciler) envoyPodsCanSign(ctx context.Context, n gatewayNames, programmed string) (dataPlaneState, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods,
		client.InNamespace(n.namespace),
		client.MatchingLabels{
			envoyOwningGatewayNameLabel:      n.gateway,
			envoyOwningGatewayNamespaceLabel: n.namespace,
		},
	); err != nil {
		return dataPlaneState{}, fmt.Errorf("list Envoy pods of Gateway %s/%s: %w", n.namespace, n.gateway, err)
	}

	ref := n.namespace + "/" + n.gateway
	var serving, notInjected, notReady, incoming []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		injected, ready := extProcStatus(pod)
		if !podIsReady(pod) {
			if injected {
				incoming = append(incoming, pod.Name)
			}
			continue
		}
		serving = append(serving, pod.Name)
		switch {
		case !injected:
			notInjected = append(notInjected, pod.Name)
		case !ready:
			notReady = append(notReady, pod.Name)
		}
	}
	for _, s := range [][]string{serving, notInjected, notReady, incoming} {
		sort.Strings(s)
	}

	switch {
	case len(serving) == 0:
		return dataPlaneState{
			status: metav1.ConditionFalse,
			reason: reasonNoReadyEnvoyPod,
			message: fmt.Sprintf("Gateway %s is Programmed, but no Ready, non-terminating Envoy pod of it was found, so nothing is serving the routes yet",
				ref),
		}, nil
	case len(notInjected) > 0:
		msg := fmt.Sprintf("Gateway %s is Programmed, but Ready Envoy pod(s) %s carry no %s container and answer every model request with HTTP 500; Envoy AI Gateway admitted them before it had written the Gateway's filter-config Secret and rolls the Deployment once it has",
			ref, strings.Join(notInjected, ", "), extProcContainerName)
		if len(incoming) > 0 {
			msg += fmt.Sprintf(" (pod(s) %s carrying it are not Ready yet)", strings.Join(incoming, ", "))
		}
		return dataPlaneState{status: metav1.ConditionFalse, reason: reasonExtProcNotInjected, message: msg}, nil
	case len(notReady) > 0:
		return dataPlaneState{
			status: metav1.ConditionFalse,
			reason: reasonExtProcNotReady,
			message: fmt.Sprintf("Gateway %s is Programmed, but the %s container of Ready Envoy pod(s) %s is not ready, so requests those pods take cannot be signed for Bedrock",
				ref, extProcContainerName, strings.Join(notReady, ", ")),
		}, nil
	}
	return dataPlaneState{
		status: metav1.ConditionTrue,
		reason: reasonDataPlaneProgrammed,
		message: fmt.Sprintf("Gateway %s is Programmed (%s) and every Ready Envoy pod (%s) carries a ready %s",
			ref, programmed, strings.Join(serving, ", "), extProcContainerName),
	}, nil
}

// podIsReady reports the pod's Ready condition, which is what puts it behind
// the gateway Service.
func podIsReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// extProcStatus reports whether the pod carries the extproc and whether that
// container is serving. Envoy AI Gateway injects it in one of two shapes:
//
//   - a native sidecar — an init container with restartPolicy Always — on
//     Kubernetes 1.33 and later. Its status is in initContainerStatuses, where
//     Ready is the readiness-probe result for a restartable init container
//     (the extproc probes /health on its admin port). For a non-restartable one
//     Ready only means it ran to completion, which is why the restart policy is
//     checked and a running state is required;
//   - a regular container otherwise, whose status is in containerStatuses.
func extProcStatus(pod *corev1.Pod) (injected, ready bool) {
	statuses := pod.Status.ContainerStatuses
	found := false
	for _, c := range pod.Spec.InitContainers {
		if c.Name != extProcContainerName {
			continue
		}
		if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			// A run-to-completion init container is not a sidecar: it is not
			// running while Envoy serves, so nothing answers the ext_proc call.
			return true, false
		}
		found, statuses = true, pod.Status.InitContainerStatuses
		break
	}
	if !found {
		for _, c := range pod.Spec.Containers {
			if c.Name == extProcContainerName {
				found = true
				break
			}
		}
	}
	if !found {
		return false, false
	}
	for _, s := range statuses {
		if s.Name == extProcContainerName {
			return true, s.Ready && s.State.Running != nil
		}
	}
	return true, false
}
