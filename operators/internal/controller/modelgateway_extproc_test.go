/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

// extProcShape is how (and whether) an Envoy pod carries the extproc.
type extProcShape int

const (
	// noExtProc: the pod was admitted unmutated — the first pod of every new
	// Gateway, created before Envoy AI Gateway wrote the filter-config Secret.
	noExtProc extProcShape = iota
	// nativeSidecar: an init container with restartPolicy Always, the shape
	// Envoy AI Gateway injects on Kubernetes 1.33 and later.
	nativeSidecar
	// regularContainer: the shape it injects on older clusters.
	regularContainer
	// oneShotInit: an init container without restartPolicy Always. Not a
	// shape Envoy AI Gateway produces; it is the case where Ready on an init
	// container status means "ran to completion", not "is serving".
	oneShotInit
)

// envoyPodSpec describes one pod for newEnvoyPod.
type envoyPodSpec struct {
	name string
	// namespace and gateway default to the test Platform's gateway.
	namespace, gateway string
	podReady           bool
	extProc            extProcShape
	extProcReady       bool
	terminating        bool
}

// testGatewayNamespace and testGatewayName are where the reconciler renders the
// test Platform's Gateway.
const (
	testGatewayNamespace = "tenants-" + ctrlTestPlatform
	testGatewayName      = ctrlTestPlatform + "-gateway"
)

// newEnvoyPod builds an Envoy pod the way Envoy Gateway labels one and Envoy AI
// Gateway's webhook (did or did not) mutate it, with the status the kubelet
// would report.
func newEnvoyPod(s envoyPodSpec) *corev1.Pod {
	ns, gw := s.namespace, s.gateway
	if ns == "" {
		ns = testGatewayNamespace
	}
	if gw == "" {
		gw = testGatewayName
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: ns,
			Labels: map[string]string{
				envoyOwningGatewayNameLabel:      gw,
				envoyOwningGatewayNamespaceLabel: ns,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "envoy", Image: "envoy"}}},
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "envoy", Ready: s.podReady, State: running}}
	extStatus := corev1.ContainerStatus{Name: extProcContainerName, Ready: s.extProcReady, State: running, Started: ptr.To(true)}
	extProc := corev1.Container{Name: extProcContainerName, Image: "extproc"}
	switch s.extProc {
	case nativeSidecar:
		extProc.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, extProc)
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, extStatus)
	case regularContainer:
		pod.Spec.Containers = append(pod.Spec.Containers, extProc)
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, extStatus)
	case oneShotInit:
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, extProc)
		// What the kubelet reports for an init container that ran and exited 0.
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{
			Name: extProcContainerName, Ready: true,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		})
	}
	ready := corev1.ConditionFalse
	if s.podReady {
		ready = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
	if s.terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"test.nanohype.dev/hold"}
	}
	return pod
}

// servingEnvoyPod is a Ready Envoy pod with a ready native extproc — the shape
// of a gateway that can answer.
func servingEnvoyPod(name string) *corev1.Pod {
	return newEnvoyPod(envoyPodSpec{name: name, podReady: true, extProc: nativeSidecar, extProcReady: true})
}

// seedServingEnvoyPod writes a serving Envoy pod through cl, for the tests that
// read pods through the reconciler's Client fallback and need a Programmed
// Gateway to reach Ready.
func seedServingEnvoyPod(t *testing.T, cl client.Client) {
	t.Helper()
	if err := cl.Create(context.Background(), servingEnvoyPod(testGatewayName+"-serving")); err != nil {
		t.Fatalf("seed Envoy pod: %v", err)
	}
}

// TestModelGatewayReadyWaitsForEveryEnvoyPodToSign holds Ready, the
// DataPlaneProgrammed condition and the published contract to the Envoy pods
// behind a Programmed Gateway: every Ready, non-terminating one must carry a
// ready ai-gateway-extproc.
//
// The failure this guards against was observed live. Envoy AI Gateway injects
// the extproc only at pod CREATE and admits a pod unmutated while the Gateway's
// filter-config Secret is missing, which it always is for the first pod of a new
// Gateway. Envoy Gateway reports that Gateway Programmed on its one available
// replica, the ModelGateway went Ready and published its routes, and every
// request the pod took returned HTTP 500 until Envoy AI Gateway rolled a second
// ReplicaSet.
func TestModelGatewayReadyWaitsForEveryEnvoyPodToSign(t *testing.T) {
	cases := []struct {
		name       string
		pods       []envoyPodSpec
		wantPhase  string
		wantReason string
		// wantInMsg are the pod names the condition message must carry;
		// wantNotInMsg are ones it must not.
		wantInMsg    []string
		wantNotInMsg []string
	}{
		{
			name:       "no Envoy pods",
			wantPhase:  phaseProvisioning,
			wantReason: reasonNoReadyEnvoyPod,
		},
		{
			// The observed case: one Ready pod, admitted before the Secret.
			name:       "single Ready pod without the extproc",
			pods:       []envoyPodSpec{{name: "envoy-a", podReady: true, extProc: noExtProc}},
			wantPhase:  phaseProvisioning,
			wantReason: reasonExtProcNotInjected,
			wantInMsg:  []string{"envoy-a"},
		},
		{
			// Envoy AI Gateway's annotation bump has rolled a second
			// ReplicaSet; its pod carries the extproc but is not Ready, so
			// the sidecar-less pod is still the one serving.
			name: "old sidecar-less pod serving while the rolled pod comes up",
			pods: []envoyPodSpec{
				{name: "envoy-old", podReady: true, extProc: noExtProc},
				{name: "envoy-new", extProc: nativeSidecar},
			},
			wantPhase:  phaseProvisioning,
			wantReason: reasonExtProcNotInjected,
			wantInMsg:  []string{"envoy-old", "envoy-new"},
		},
		{
			name: "every Ready pod carries a ready native extproc",
			pods: []envoyPodSpec{
				{name: "envoy-a", podReady: true, extProc: nativeSidecar, extProcReady: true},
				{name: "envoy-b", podReady: true, extProc: nativeSidecar, extProcReady: true},
			},
			wantPhase:  phaseReady,
			wantReason: reasonDataPlaneProgrammed,
			wantInMsg:  []string{"envoy-a", "envoy-b"},
		},
		{
			// Clusters before 1.33 get the extproc as a regular container.
			name:       "regular-container extproc, ready",
			pods:       []envoyPodSpec{{name: "envoy-a", podReady: true, extProc: regularContainer, extProcReady: true}},
			wantPhase:  phaseReady,
			wantReason: reasonDataPlaneProgrammed,
		},
		{
			name:       "regular-container extproc, not ready",
			pods:       []envoyPodSpec{{name: "envoy-a", podReady: true, extProc: regularContainer}},
			wantPhase:  phaseProvisioning,
			wantReason: reasonExtProcNotReady,
			wantInMsg:  []string{"envoy-a"},
		},
		{
			// A terminating pod has left the Service's endpoints and takes no
			// new connection, so the old sidecar-less pod draining out does
			// not hold the gateway.
			name: "terminating sidecar-less pod ignored",
			pods: []envoyPodSpec{
				{name: "envoy-old", podReady: true, terminating: true},
				{name: "envoy-new", podReady: true, extProc: nativeSidecar, extProcReady: true},
			},
			wantPhase:    phaseReady,
			wantReason:   reasonDataPlaneProgrammed,
			wantNotInMsg: []string{"envoy-old"},
		},
		{
			// Pods of another Gateway — in this namespace, or of a same-named
			// Gateway in another namespace — are not this Gateway's data plane.
			name: "other Gateway and other namespace pods ignored",
			pods: []envoyPodSpec{
				{name: "envoy-ours", podReady: true, extProc: nativeSidecar, extProcReady: true},
				{name: "envoy-other-gateway", gateway: "someone-else-gateway", podReady: true},
				{name: "envoy-other-namespace", namespace: "tenants-elsewhere", podReady: true},
			},
			wantPhase:    phaseReady,
			wantReason:   reasonDataPlaneProgrammed,
			wantNotInMsg: []string{"envoy-other-gateway", "envoy-other-namespace"},
		},
		{
			name:       "native extproc injected but not ready",
			pods:       []envoyPodSpec{{name: "envoy-a", podReady: true, extProc: nativeSidecar}},
			wantPhase:  phaseProvisioning,
			wantReason: reasonExtProcNotReady,
			wantInMsg:  []string{"envoy-a"},
		},
		{
			// Ready on a non-restartable init container status means it ran
			// to completion; nothing is running to answer ext_proc.
			name:       "extproc as a run-to-completion init container",
			pods:       []envoyPodSpec{{name: "envoy-a", podReady: true, extProc: oneShotInit}},
			wantPhase:  phaseProvisioning,
			wantReason: reasonExtProcNotReady,
			wantInMsg:  []string{"envoy-a"},
		},
		{
			// A not-Ready pod is not behind the Service; the Ready one with a
			// ready extproc is what serves.
			name: "not-Ready sidecar-less pod does not hold a serving gateway",
			pods: []envoyPodSpec{
				{name: "envoy-serving", podReady: true, extProc: nativeSidecar, extProcReady: true},
				{name: "envoy-pending"},
			},
			wantPhase:  phaseReady,
			wantReason: reasonDataPlaneProgrammed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mgwScheme(t)
			ctx := context.Background()
			mg := twoRouteGateway()
			mg.Finalizers = []string{modelGatewayFinalizer}
			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(readyTestPlatform(), mg).WithStatusSubresource(mg).Build()
			// The pods live only behind APIReader: the reconciler must read
			// them there, uncached, and not through its (cached) Client.
			podObjs := make([]client.Object, 0, len(tc.pods))
			for _, p := range tc.pods {
				podObjs = append(podObjs, newEnvoyPod(p))
			}
			reader := fake.NewClientBuilder().WithScheme(s).WithObjects(podObjs...).Build()
			r := &ModelGatewayReconciler{Client: cl, APIReader: reader, Scheme: s, Region: "us-west-2"}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mg.Namespace, Name: mg.Name}}

			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("Reconcile (render): %v", err)
			}
			setGatewayProgrammed(t, cl, "True", "Programmed")
			got, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			var st agentsv1alpha1.ModelGateway
			if err := cl.Get(ctx, req.NamespacedName, &st); err != nil {
				t.Fatal(err)
			}
			if st.Status.Phase != tc.wantPhase {
				t.Fatalf("status.phase: got %q want %q (%+v)", st.Status.Phase, tc.wantPhase, findCondition(st.Status.Conditions, dataPlaneConditionType))
			}
			dp := findCondition(st.Status.Conditions, dataPlaneConditionType)
			if dp == nil {
				t.Fatalf("no %s condition written", dataPlaneConditionType)
			}
			wantStatus := metav1.ConditionFalse
			if tc.wantPhase == phaseReady {
				wantStatus = metav1.ConditionTrue
			}
			if dp.Status != wantStatus || dp.Reason != tc.wantReason {
				t.Errorf("%s: got %s/%s want %s/%s (%s)", dataPlaneConditionType, dp.Status, dp.Reason, wantStatus, tc.wantReason, dp.Message)
			}
			for _, name := range tc.wantInMsg {
				if !strings.Contains(dp.Message, name) {
					t.Errorf("%s message %q does not name pod %q", dataPlaneConditionType, dp.Message, name)
				}
			}
			for _, name := range tc.wantNotInMsg {
				if strings.Contains(dp.Message, name) {
					t.Errorf("%s message %q names pod %q, which is not this Gateway's serving data plane", dataPlaneConditionType, dp.Message, name)
				}
			}

			if tc.wantPhase == phaseReady {
				if st.Status.Endpoint == "" || len(st.Status.Routes) != len(mg.Spec.Routes) {
					t.Errorf("a gateway whose pods can sign must publish its contract, got endpoint %q routes %v", st.Status.Endpoint, st.Status.Routes)
				}
				if got.RequeueAfter != readyRecheckInterval {
					t.Errorf("Ready requeue: got %v want %v", got.RequeueAfter, readyRecheckInterval)
				}
				return
			}
			if st.Status.Endpoint != "" || len(st.Status.Routes) != 0 {
				t.Errorf("a gateway whose pods cannot sign published endpoint %q and routes %v", st.Status.Endpoint, st.Status.Routes)
			}
			// Held on its pods, it is mid-rollout and looks again sooner than an
			// unprogrammed Gateway would.
			if got.RequeueAfter != envoyRolloutRequeueInterval {
				t.Errorf("requeue while held on Envoy pods: got %v want %v", got.RequeueAfter, envoyRolloutRequeueInterval)
			}
		})
	}
}

// TestModelGatewayReadyRegressesWhenASidecarLessPodAppears: Ready is re-read on
// every recheck, so a Ready gateway that later gains a Ready Envoy pod with no
// extproc — a pod created while the filter-config Secret was briefly missing —
// returns to Provisioning and withdraws its endpoint and routes.
func TestModelGatewayReadyRegressesWhenASidecarLessPodAppears(t *testing.T) {
	s := mgwScheme(t)
	ctx := context.Background()
	mg := twoRouteGateway()
	mg.Finalizers = []string{modelGatewayFinalizer}
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(readyTestPlatform(), mg).WithStatusSubresource(mg).Build()
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(servingEnvoyPod("envoy-a")).Build()
	r := &ModelGatewayReconciler{Client: cl, APIReader: reader, Scheme: s, Region: "us-west-2"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mg.Namespace, Name: mg.Name}}
	stored := func() agentsv1alpha1.ModelGateway {
		t.Helper()
		var got agentsv1alpha1.ModelGateway
		if err := cl.Get(ctx, req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (render): %v", err)
	}
	setGatewayProgrammed(t, cl, "True", "Programmed")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (serving): %v", err)
	}
	if st := stored(); st.Status.Phase != phaseReady || st.Status.Endpoint == "" {
		t.Fatalf("serving gateway: phase %q endpoint %q, want Ready with an endpoint", st.Status.Phase, st.Status.Endpoint)
	}

	if err := reader.Create(ctx, newEnvoyPod(envoyPodSpec{name: "envoy-b", podReady: true})); err != nil {
		t.Fatal(err)
	}
	got, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile (recheck): %v", err)
	}
	st := stored()
	if st.Status.Phase != phaseProvisioning || st.Status.Endpoint != "" || len(st.Status.Routes) != 0 {
		t.Errorf("a Ready gateway that gained a sidecar-less pod: phase %q endpoint %q routes %v, want Provisioning with nothing published",
			st.Status.Phase, st.Status.Endpoint, st.Status.Routes)
	}
	dp := findCondition(st.Status.Conditions, dataPlaneConditionType)
	if dp == nil || dp.Reason != reasonExtProcNotInjected || !strings.Contains(dp.Message, "envoy-b") || strings.Contains(dp.Message, "envoy-a") {
		t.Errorf("%s: got %+v want False/%s naming envoy-b only", dataPlaneConditionType, dp, reasonExtProcNotInjected)
	}
	if got.RequeueAfter != envoyRolloutRequeueInterval {
		t.Errorf("requeue: got %v want %v", got.RequeueAfter, envoyRolloutRequeueInterval)
	}
}

// TestModelGatewayEnvoyPodsReadFallsBackToClient: with no APIReader set — a
// reconciler built outside SetupWithManager — the pods are read through Client.
func TestModelGatewayEnvoyPodsReadFallsBackToClient(t *testing.T) {
	s := mgwScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(servingEnvoyPod("envoy-a")).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s}
	dp, err := r.envoyPodsCanSign(context.Background(), gatewayNames{namespace: testGatewayNamespace, gateway: testGatewayName}, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if dp.status != metav1.ConditionTrue {
		t.Errorf("got %+v, want True from the pod written through Client", dp)
	}
}

// TestAwaitingEnvoyPods names exactly the reasons that requeue at the rollout
// cadence: the pod-level holds, not an unprogrammed Gateway.
func TestAwaitingEnvoyPods(t *testing.T) {
	for reason, want := range map[string]bool{
		reasonNoReadyEnvoyPod:       true,
		reasonExtProcNotInjected:    true,
		reasonExtProcNotReady:       true,
		reasonGatewayNotProgrammed:  false,
		reasonAwaitingGatewayStatus: false,
		reasonDataPlaneProgrammed:   false,
		reasonDataPlaneNotRendered:  false,
	} {
		if got := awaitingEnvoyPods(reason); got != want {
			t.Errorf("awaitingEnvoyPods(%q): got %v want %v", reason, got, want)
		}
	}
}

// TestExtProcStatusWithoutAContainerStatus: a pod whose spec carries the
// extproc but whose status does not report it yet is injected and not ready.
func TestExtProcStatusWithoutAContainerStatus(t *testing.T) {
	pod := newEnvoyPod(envoyPodSpec{name: "envoy-a", podReady: true, extProc: nativeSidecar, extProcReady: true})
	pod.Status.InitContainerStatuses = nil
	if injected, ready := extProcStatus(pod); !injected || ready {
		t.Errorf("got injected=%v ready=%v, want injected and not ready", injected, ready)
	}
	pod.Status.Conditions = nil
	if podIsReady(pod) {
		t.Error("a pod with no Ready condition reported Ready")
	}
}

// TestModelGatewayEnvoyPodListErrorIsAReconcileError: a pod read that fails is
// not evidence about the pods either way, so it is retried as an error rather
// than reported as a hold or as Ready.
func TestModelGatewayEnvoyPodListErrorIsAReconcileError(t *testing.T) {
	s := mgwScheme(t)
	reader := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	r := &ModelGatewayReconciler{Client: reader, APIReader: reader, Scheme: s}
	_, err := r.envoyPodsCanSign(context.Background(), gatewayNames{namespace: testGatewayNamespace, gateway: testGatewayName}, "ok")
	if err == nil || !strings.Contains(err.Error(), "apiserver unavailable") || !strings.Contains(err.Error(), testGatewayName) {
		t.Errorf("got %v, want the list error wrapped with the Gateway it was for", err)
	}
}
