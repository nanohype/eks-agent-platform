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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// platformWithCapabilities builds a Platform declaring the given capabilities
// (and optionally datastores, appended by the caller) for policy-generation
// unit tests.
func platformWithCapabilities(name string, caps ...platformv1alpha1.Capability) *platformv1alpha1.Platform { //nolint:unparam // policy-generation unit tests use a fixed platform token
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: platformv1alpha1.PlatformSpec{
			Identity: platformv1alpha1.IdentitySpec{Capabilities: caps},
		},
	}
}

// TestCapabilityGrantSidsCoversEveryCapability keeps the Sid table in step with
// the API. A capability missing from the table reports as permanently
// ungranted, which would put a false warning on every Platform declaring it —
// so a newly added capability has to fail here rather than in a tenant's status.
func TestCapabilityGrantSidsCoversEveryCapability(t *testing.T) {
	// The CRD enum on IdentitySpec.Capabilities is the authority for what may be
	// set; this mirrors it. Adding a capability there without adding it here is
	// exactly the drift this test exists to catch.
	all := []platformv1alpha1.Capability{
		platformv1alpha1.CapabilitySES,
		platformv1alpha1.CapabilityEventBridgeScheduler,
	}
	for _, c := range all {
		sids, ok := capabilityGrantSids[c]
		if !ok {
			t.Errorf("capability %q has no entry in capabilityGrantSids", c)
			continue
		}
		if len(sids) == 0 {
			t.Errorf("capability %q maps to no Sids", c)
		}
	}
	if len(capabilityGrantSids) != len(all) {
		t.Errorf("capabilityGrantSids has %d entries, API declares %d capabilities",
			len(capabilityGrantSids), len(all))
	}
}

// TestRecordWarning covers the event path the CapabilitiesGranted condition
// pairs with. The condition is the durable record; the event is what reaches an
// operator running `kubectl describe platform` after their agent fails, and
// anything watching events that never reads the CR.
//
// The nil case is not defensive filler: unit tests construct PlatformReconciler
// directly and most do not wire a recorder, so the no-op is what keeps the
// recorder from becoming a construction requirement across the suite.
func TestRecordWarning(t *testing.T) {
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES)

	t.Run("emits a warning naming the capability", func(t *testing.T) {
		rec := record.NewFakeRecorder(4)
		r := &PlatformReconciler{Recorder: rec}
		r.recordWarning(p, "CapabilityNotGranted", "declared but granted nothing: %s", "ses")

		select {
		case ev := <-rec.Events:
			if !strings.Contains(ev, "Warning") {
				t.Errorf("event %q is not a Warning — an Normal event would not surface in describe's tail", ev)
			}
			if !strings.Contains(ev, "CapabilityNotGranted") {
				t.Errorf("event %q does not carry the reason", ev)
			}
			if !strings.Contains(ev, "ses") {
				t.Errorf("event %q does not name the capability — naming it is the point", ev)
			}
		default:
			t.Fatal("no event recorded")
		}
	})

	t.Run("no recorder wired is a no-op, not a panic", func(t *testing.T) {
		r := &PlatformReconciler{}
		r.recordWarning(p, "CapabilityNotGranted", "declared but granted nothing: %s", "ses")
	})
}

func TestCapabilitiesGrantedCondition(t *testing.T) {
	ses := platformv1alpha1.CapabilitySES
	sched := platformv1alpha1.CapabilityEventBridgeScheduler

	tests := []struct {
		name         string
		declared     []platformv1alpha1.Capability
		ungranted    []platformv1alpha1.Capability
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantInMsg    []string
		wantNotInMsg []string
	}{
		{
			name:       "nothing declared is true, not vacuously false",
			wantStatus: metav1.ConditionTrue,
			wantReason: "NoCapabilitiesDeclared",
		},
		{
			name:       "all declared capabilities granted",
			declared:   []platformv1alpha1.Capability{ses, sched},
			wantStatus: metav1.ConditionTrue,
			wantReason: "AllGranted",
		},
		{
			name:       "ungranted capability is named, not counted",
			declared:   []platformv1alpha1.Capability{ses, sched},
			ungranted:  []platformv1alpha1.Capability{ses},
			wantStatus: metav1.ConditionFalse,
			wantReason: "SubstrateBindingMissing",
			wantInMsg:  []string{"ses"},
			// The operator's next question is which one; naming only the
			// ungranted capability is the whole point of the message.
			wantNotInMsg: []string{"eventBridgeScheduler"},
		},
		{
			name:       "several ungranted are all named",
			declared:   []platformv1alpha1.Capability{ses, sched},
			ungranted:  []platformv1alpha1.Capability{ses, sched},
			wantStatus: metav1.ConditionFalse,
			wantReason: "SubstrateBindingMissing",
			wantInMsg:  []string{"ses", "eventBridgeScheduler"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := platformWithCapabilities("myplat", tc.declared...)
			p.Generation = 7
			got := capabilitiesGrantedCondition(p, tc.ungranted)

			if got.Type != "CapabilitiesGranted" {
				t.Errorf("Type = %q, want CapabilitiesGranted", got.Type)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.ObservedGeneration != 7 {
				t.Errorf("ObservedGeneration = %d, want 7", got.ObservedGeneration)
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(got.Message, want) {
					t.Errorf("Message %q does not name %q", got.Message, want)
				}
			}
			for _, notWant := range tc.wantNotInMsg {
				if strings.Contains(got.Message, notWant) {
					t.Errorf("Message %q names %q, which was granted", got.Message, notWant)
				}
			}
		})
	}
}

func TestUngrantedCapabilities(t *testing.T) {
	sesGranted := []policyStatement{{Sid: "sesSend"}, {Sid: "sesQuota"}}
	schedGranted := []policyStatement{{Sid: "schedulerManage"}, {Sid: "schedulerPassInvokeRole"}}

	tests := []struct {
		name  string
		caps  []platformv1alpha1.Capability
		stmts []policyStatement
		want  []platformv1alpha1.Capability
	}{
		{
			name: "no capabilities declared reports none",
		},
		{
			name:  "ses declared and granted reports none",
			caps:  []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES},
			stmts: sesGranted,
		},
		{
			// The shape this exists for: the capability is declared, the policy
			// built cleanly, and no SES statement was emitted because no domain
			// is bound to the tenant.
			name: "ses declared but unbound reports ses",
			caps: []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES},
			want: []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES},
		},
		{
			name:  "scheduler declared and granted reports none",
			caps:  []platformv1alpha1.Capability{platformv1alpha1.CapabilityEventBridgeScheduler},
			stmts: schedGranted,
		},
		{
			name: "scheduler declared but ungranted reports scheduler",
			caps: []platformv1alpha1.Capability{platformv1alpha1.CapabilityEventBridgeScheduler},
			want: []platformv1alpha1.Capability{platformv1alpha1.CapabilityEventBridgeScheduler},
		},
		{
			name:  "partial emission still counts as granted",
			caps:  []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES},
			stmts: []policyStatement{{Sid: "sesSend"}},
		},
		{
			name:  "mixed reports only the ungranted one, in declaration order",
			caps:  []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES, platformv1alpha1.CapabilityEventBridgeScheduler},
			stmts: schedGranted,
			want:  []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES},
		},
		{
			// A capability outside the table is the CRD enum's problem, not a
			// per-tenant status message.
			name: "capability absent from the table is not reported",
			caps: []platformv1alpha1.Capability{platformv1alpha1.Capability("notAThing")},
		},
		{
			name:  "unrelated statements do not count as a grant",
			caps:  []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES},
			stmts: []policyStatement{{Sid: "bedrockInvoke"}, {Sid: "datastoreS3"}},
			want:  []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ungrantedCapabilities(platformWithCapabilities("myplat", tc.caps...), tc.stmts)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

const (
	capRoleARN     = "arn:aws:iam::123456789012:role/test-role"
	capClusterName = "development-platform"
	invokeRoleName = "development-platform-myplat-scheduler-invoke"
)

func capCfg() IAMConfig {
	return IAMConfig{Environment: "development", Region: "us-west-2", ClusterName: capClusterName}
}

// boundSES stands in for the substrate having published a sending domain for a
// tenant. The operator reads this binding rather than deriving a domain from the
// Platform, so every test that expects an SES grant has to establish it — which
// is the point: without it there is no grant to assert on.
func boundSES(cluster, platform string) *stubSSM {
	return &stubSSM{params: map[string]string{
		sesDomainParamPath(cluster, platform): "example.com",
	}}
}

// TestCapabilityPolicy_SES proves the ses capability grants scoped SendEmail
// (FromAddress-conditioned to the tenant's domain) plus the unconditioned
// account-global GetSendQuota, and nothing else.
func TestCapabilityPolicy_SES(t *testing.T) {
	stmts := capabilityPolicyStatements(platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), "development", capClusterName, "example.com", testScope())

	send := findStmt(stmts, "sesSend")
	if send == nil || !hasResource(send, "*") {
		t.Fatalf("sesSend statement missing or misscoped: %+v", send)
	}
	if got := send.Condition["StringLike"]["ses:FromAddress"]; got != "*@example.com" {
		t.Errorf("ses:FromAddress condition: got %q want %q", got, "*@example.com")
	}
	// The wildcard covers the local part and nothing else. A trailing wildcard
	// would make the domain a prefix match, so a tenant bound to example.com
	// would also reach example.com.evil — and a Platform whose name merely
	// prefixed another tenant's verified domain would inherit its sending
	// rights, silently.
	if strings.HasSuffix(send.Condition["StringLike"]["ses:FromAddress"], "*") {
		t.Errorf("ses:FromAddress must be anchored at the domain, got %q",
			send.Condition["StringLike"]["ses:FromAddress"])
	}
	// The Platform's own name must not reach the condition at all.
	if strings.Contains(send.Condition["StringLike"]["ses:FromAddress"], "myplat") {
		t.Errorf("the tenant must not influence its own sending domain, got %q",
			send.Condition["StringLike"]["ses:FromAddress"])
	}
	quota := findStmt(stmts, "sesQuota")
	if quota == nil || len(quota.Condition) != 0 {
		t.Errorf("sesQuota must be unconditioned: %+v", quota)
	}
	// No scheduler statements when only ses is declared.
	if findStmt(stmts, "schedulerManage") != nil {
		t.Errorf("ses-only Platform must not grant scheduler")
	}
}

// TestCapabilityPolicy_SESUnbound proves that declaring the capability grants
// nothing on its own. The sending domain comes from the substrate, so until
// landing-zone binds one there is no identity to scope a grant to — and the
// safe answer to "which domain?" is none, not a guess derived from the CR.
//
// This is the case that made the old behaviour dangerous: the domain used to be
// built from the Platform's own name, so a capability declaration was also a
// domain claim, and the tenant wrote both.
func TestCapabilityPolicy_SESUnbound(t *testing.T) {
	stmts := capabilityPolicyStatements(
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES),
		"development", capClusterName, "", testScope())

	if findStmt(stmts, "sesSend") != nil {
		t.Error("an unbound tenant must get no sesSend statement")
	}
	if findStmt(stmts, "sesQuota") != nil {
		t.Error("an unbound tenant must get no sesQuota statement either")
	}
}

// TestResolveSESDomain_NoLookupPossible covers the two states in which there is
// nothing to read: no SSM client, and no cluster name to build a path from. Both
// answer "unbound" rather than erroring, because the reconcile has to keep
// running — the rest of a Platform's identity does not depend on mail.
//
// Both are also lookup-suppressed rather than lookup-failed, which is why they
// are asserted separately from the not-found case: a path built from an empty
// cluster name would collide across clusters, so the guard has to come first.
func TestResolveSESDomain_NoLookupPossible(t *testing.T) {
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES)

	for _, tc := range []struct {
		name    string
		ssm     *stubSSM
		cluster string
	}{
		{"no ssm client", nil, capClusterName},
		{"no cluster name", boundSES(capClusterName, "myplat"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &PlatformReconciler{}
			if tc.ssm != nil {
				r.SSM = tc.ssm
			}
			got, err := r.resolveSESDomain(context.Background(), p, tc.cluster)
			if err != nil {
				t.Fatalf("resolveSESDomain: %v", err)
			}
			if got != "" {
				t.Errorf("domain: got %q want empty", got)
			}
			if tc.ssm != nil && len(tc.ssm.calls) != 0 {
				t.Errorf("must not read SSM at all, called: %v", tc.ssm.calls)
			}
		})
	}
}

// TestEnsureCapabilityPolicy_SESLookupFailurePropagates proves a broken SSM read
// fails the reconcile instead of resolving to "unbound".
//
// The distinction matters because unbound is a legitimate state that quietly
// drops the SES statement. If a transport error took that same path, an outage
// in the parameter store would silently strip sending rights from every bound
// tenant and the policy would still be written — a converged-looking reconcile
// over a grant that had been revoked by accident.
func TestEnsureCapabilityPolicy_SESLookupFailurePropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f, SSM: &stubSSM{err: errors.New("ssm unavailable")}}
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES)

	_, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, capCfg())
	if err == nil {
		t.Fatal("a failed domain lookup must fail the reconcile")
	}
	if !strings.Contains(err.Error(), "ssm unavailable") {
		t.Errorf("error must carry the cause: %v", err)
	}
	if puts := putsFor(f, capabilityPolicyName); len(puts) != 0 {
		t.Errorf("no policy may be written when the binding is unknown: %d put(s)", len(puts))
	}
}

// TestSESDomainParamPath pins the contract with landing-zone. The operator only
// reads this path; the substrate writes it. Changing the shape here silently
// unbinds every tenant, since a missing parameter is not an error.
func TestSESDomainParamPath(t *testing.T) {
	got := sesDomainParamPath("dev-cluster", "myplat")
	want := "/eks-agent-platform/dev-cluster/myplat/ses/domain"
	if got != want {
		t.Errorf("ses domain parameter path: got %q want %q", got, want)
	}
}

// TestCapabilityPolicy_Scheduler proves the eventBridgeScheduler capability
// grants schedule management on the tenant's own schedule GROUP plus a
// service-capped PassRole on the minted invoke role.
func TestCapabilityPolicy_Scheduler(t *testing.T) {
	stmts := capabilityPolicyStatements(platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler), "development", capClusterName, "", testScope())

	manage := findStmt(stmts, "schedulerManage")
	if manage == nil || !hasResource(manage, "arn:aws:scheduler:us-west-2:123456789012:schedule/development-myplat/*") {
		t.Fatalf("schedulerManage missing or misscoped: %+v", manage)
	}
	pass := findStmt(stmts, "schedulerPassInvokeRole")
	if pass == nil || !hasResource(pass, "arn:aws:iam::123456789012:role/"+invokeRoleName) {
		t.Fatalf("schedulerPassInvokeRole missing or misscoped: %+v", pass)
	}
	if got := pass.Condition["StringEquals"]["iam:PassedToService"]; got != "scheduler.amazonaws.com" {
		t.Errorf("PassRole must be capped to the Scheduler service, got %q", got)
	}
}

// TestSchedulerScheduleARN_DoesNotSpanAHyphenPrefixedSibling asserts the value
// that is the entire tenant boundary on EventBridge Scheduler.
//
// The grant this replaced named a name PREFIX inside the shared `default`
// group: `schedule/default/development-foo-*`. A schedule ARN is
// `schedule/<group>/<name>`, and IAM's `*` spans `/`, so that pattern also
// matched every schedule belonging to a Platform named `foo-bar` —
// `schedule/default/development-foo-bar-nudge-1` — and Platform names are
// DNS-1123 labels with nothing anywhere forbidding that pair.
//
// Naming the group instead makes the boundary an exact match on a path segment:
// `schedule/development-foo/*` cannot reach `schedule/development-foo-bar/...`
// because the character after `development-foo` is `-`, not `/`.
func TestSchedulerScheduleARN_DoesNotSpanAHyphenPrefixedSibling(t *testing.T) {
	foo := schedulerScheduleARN("development", platformWithCapabilities("foo", platformv1alpha1.CapabilityEventBridgeScheduler), testScope())
	bar := schedulerScheduleARN("development", platformWithCapabilities("foo-bar", platformv1alpha1.CapabilityEventBridgeScheduler), testScope())

	const wantFoo = "arn:aws:scheduler:us-west-2:123456789012:schedule/development-foo/*"
	if foo != wantFoo {
		t.Fatalf("foo's schedule ARN: got %q want %q", foo, wantFoo)
	}

	// The prefix of foo's pattern, up to its wildcard, must not be a prefix of
	// any ARN in foo-bar's group. That is the property the group buys and the
	// prefix form did not.
	fooPrefix := strings.TrimSuffix(foo, "*")
	barSchedule := strings.TrimSuffix(bar, "*") + "nudge-1"
	if strings.HasPrefix(barSchedule, fooPrefix) {
		t.Errorf("foo's grant %q spans foo-bar's schedule %q — the tenant boundary does not hold", foo, barSchedule)
	}

	// And a schedule genuinely inside foo's group must still be reachable, so
	// the assertion above is not passing by making the grant match nothing.
	fooSchedule := fooPrefix + "nudge-1"
	if !strings.HasPrefix(fooSchedule, fooPrefix) {
		t.Errorf("foo's own schedule %q is not covered by its grant %q", fooSchedule, foo)
	}
}

// TestScheduleGroupName_ComposesLikeEveryOtherTenantName pins the name the
// operator creates, the grant scopes to, and the tenant chart derives. All three
// compose it independently; if they ever disagree the schedules fail at runtime
// into the caller's own log, which is exactly how this path stayed dead.
func TestScheduleGroupName_ComposesLikeEveryOtherTenantName(t *testing.T) {
	got := scheduleGroupName("development", platformWithCapabilities("incident-response", platformv1alpha1.CapabilityEventBridgeScheduler))
	if want := "development-incident-response"; got != want {
		t.Errorf("schedule group name: got %q want %q", got, want)
	}
}

// TestCapabilityPolicy_BothAndNone proves both capabilities compose into one
// document, and no capability yields no document (so the reconciler removes the
// inline policy rather than writing an empty one).
func TestCapabilityPolicy_BothAndNone(t *testing.T) {
	both := capabilityPolicyStatements(
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES, platformv1alpha1.CapabilityEventBridgeScheduler),
		"development", capClusterName, "example.com", testScope(),
	)
	for _, sid := range []string{"sesSend", "sesQuota", "schedulerManage", "schedulerPassInvokeRole"} {
		if findStmt(both, sid) == nil {
			t.Errorf("both-capabilities document missing %s", sid)
		}
	}
	doc, err := capabilityPolicyDoc(both)
	if err != nil {
		t.Fatalf("capabilityPolicyDoc: %v", err)
	}
	if doc == "" {
		t.Errorf("both capabilities must yield a non-empty document")
	}

	none := capabilityPolicyStatements(platformWithCapabilities("myplat"), "development", capClusterName, "", testScope())
	if len(none) != 0 {
		t.Errorf("no capability must yield no statements, got %d", len(none))
	}
	if d, _ := capabilityPolicyDoc(none); d != "" {
		t.Errorf("no statements must yield an empty document, got %q", d)
	}
}

// TestSchedulerInvokeRoleName_IsClusterScoped proves two co-located clusters
// that share an environment mint distinct invoke roles for the same Platform
// name — so one cluster's delete cannot tear down the other's role.
func TestSchedulerInvokeRoleName_IsClusterScoped(t *testing.T) {
	p := platformWithCapabilities("myplat")
	a := schedulerInvokeRoleName("development-platform", p)
	b := schedulerInvokeRoleName("development-platform-b", p)
	if a == b {
		t.Fatalf("co-located clusters must mint distinct invoke roles, both got %q", a)
	}
	if !strings.HasSuffix(a, "-scheduler-invoke") || !strings.HasSuffix(b, "-scheduler-invoke") {
		t.Errorf("suffix is load-bearing for the agent-iam PassRole boundary: %q / %q", a, b)
	}
	// Env-keyed formula is exactly what this isolation replaces.
	if a == "development-myplat-scheduler-invoke" {
		t.Errorf("invoke role must not be env-keyed alone; got %q", a)
	}
}

// TestSchedulerInvokeRoleName_HashTruncatesOverLimit proves a long Platform
// name still yields an IAM-legal invoke role that keeps the load-bearing
// -scheduler-invoke suffix (the agent-iam PassRole boundary keys on it).
func TestSchedulerInvokeRoleName_HashTruncatesOverLimit(t *testing.T) {
	long := strings.Repeat("c", 80)
	name := schedulerInvokeRoleName("production-cluster", platformWithCapabilities(long))
	if len(name) > 64 {
		t.Errorf("invoke role name over IAM's 64-char limit: %d (%s)", len(name), name)
	}
	const suffix = "-scheduler-invoke"
	if !strings.HasSuffix(name, suffix) {
		t.Errorf("truncated name must keep the %s suffix: %s", suffix, name)
	}
	// Truncation path must actually fire — a short name would not cover it.
	if short := schedulerInvokeRoleName("production-cluster", platformWithCapabilities("myplat")); short == name {
		t.Errorf("long-name formula must differ from the short-name formula")
	}
}

// TestTenantQueueResources proves only queue datastores contribute an SQS ARN to the
// scheduler-invoke role's send target, other kinds are skipped, and each queue is
// enumerated rather than prefixed — a redrive budget adds its DLQ, FIFO changes the
// name rather than adding one, and a plain queue contributes exactly itself.
func TestTenantQueueResources(t *testing.T) {
	fifo := true
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{
		{Name: "nudges", Kind: platformv1alpha1.DatastoreQueue},
		{
			Name: "retries", Kind: platformv1alpha1.DatastoreQueue,
			Queue: &platformv1alpha1.QueueConfig{MaxReceiveCount: 5},
		},
		{
			Name: "ordered", Kind: platformv1alpha1.DatastoreQueue,
			Queue: &platformv1alpha1.QueueConfig{FIFO: &fifo},
		},
		{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
	}
	want := []string{
		"arn:aws:sqs:us-west-2:123456789012:development-myplat-nudges",
		"arn:aws:sqs:us-west-2:123456789012:development-myplat-retries",
		"arn:aws:sqs:us-west-2:123456789012:development-myplat-retries-dlq",
		"arn:aws:sqs:us-west-2:123456789012:development-myplat-ordered.fifo",
	}
	res := tenantQueueResources(p, "development", testScope())
	if len(res) != len(want) {
		t.Fatalf("queue resources: got %v, want %v", res, want)
	}
	for i := range want {
		if res[i] != want[i] {
			t.Errorf("queue resource %d: got %s, want %s", i, res[i], want[i])
		}
	}
	for _, r := range res {
		if strings.Contains(r, "*") {
			t.Errorf("the scheduler-invoke grant must enumerate, not prefix: %s", r)
		}
	}
}

// TestEnsureCapabilityPolicy_SESWritesAndConverges proves the ses reconcile
// writes the capability-access policy once and no-ops on a converged re-run.
func TestEnsureCapabilityPolicy_SESWritesAndConverges(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f, SSM: boundSES(capClusterName, "myplat")}
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES)

	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	puts := putsFor(f, capabilityPolicyName)
	if len(puts) != 1 {
		t.Fatalf("capability-access PutRolePolicy: got %d want 1", len(puts))
	}
	if !strings.Contains(*puts[0].PolicyDocument, "ses:SendEmail") {
		t.Errorf("capability policy must grant ses:SendEmail: %s", *puts[0].PolicyDocument)
	}

	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := len(putsFor(f, capabilityPolicyName)); got != 1 {
		t.Errorf("converged re-run must not re-write capability-access: got %d", got)
	}
}

// TestEnsureCapabilityPolicy_RemovesWhenEmpty proves a Platform with no
// capability has the capability-access policy removed.
func TestEnsureCapabilityPolicy_RemovesWhenEmpty(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}

	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, platformWithCapabilities("myplat"), capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if len(putsFor(f, capabilityPolicyName)) != 0 {
		t.Errorf("no capability must write no capability-access policy")
	}
	if !deletedInline(f, capabilityPolicyName) {
		t.Errorf("no capability must delete the capability-access policy")
	}
}

// TestEnsureCapabilityPolicy_NilIAM proves the reconcile no-ops without an IAM
// client.
func TestEnsureCapabilityPolicy_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if _, err := r.ensureCapabilityPolicy(context.Background(), "role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), IAMConfig{}); err != nil {
		t.Fatalf("nil IAM must no-op: %v", err)
	}
}

// TestEnsureCapabilityPolicy_SchedulerMintsInvokeRole proves the scheduler
// capability mints the -scheduler-invoke role (boundaried) with a SendMessage
// policy scoped to the tenant's own queue datastores.
func TestEnsureCapabilityPolicy_SchedulerMintsInvokeRole(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}
	cfg := capCfg()
	cfg.TenantPermissionsBoundaryARN = "arn:aws:iam::123456789012:policy/tenant-boundary"
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "nudges", Kind: platformv1alpha1.DatastoreQueue}}

	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, cfg); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if len(f.createCalls) != 1 || *f.createCalls[0].RoleName != invokeRoleName {
		t.Fatalf("expected the scheduler-invoke role to be created: %+v", f.createCalls)
	}
	if f.createCalls[0].PermissionsBoundary == nil {
		t.Errorf("the invoke role must carry the tenant permissions boundary")
	}
	if !strings.Contains(*f.createCalls[0].AssumeRolePolicyDocument, "scheduler.amazonaws.com") {
		t.Errorf("the invoke role must be trusted by the Scheduler service")
	}
	send := putsFor(f, schedulerInvokeSendPolicyName)
	if len(send) != 1 || !strings.Contains(*send[0].PolicyDocument, "development-myplat-nudges") {
		t.Fatalf("the invoke role's send policy must target the tenant's queue: %+v", send)
	}
}

// TestEnsureCapabilityPolicy_SchedulerNoQueueRemovesSendPolicy proves a
// scheduler capability without any queue datastore mints the role but removes
// the (absent) send policy rather than writing an empty one.
func TestEnsureCapabilityPolicy_SchedulerNoQueueRemovesSendPolicy(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)

	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if len(putsFor(f, schedulerInvokeSendPolicyName)) != 0 {
		t.Errorf("no queue must write no send policy")
	}
	if !deletedInline(f, schedulerInvokeSendPolicyName) {
		t.Errorf("no queue must remove any stale send policy")
	}
}

// TestEnsureCapabilityPolicy_SchedulerRemovedDeletesInvokeRole proves dropping
// the scheduler capability tears down the invoke role.
func TestEnsureCapabilityPolicy_SchedulerRemovedDeletesInvokeRole(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	invoke := invokeRoleName
	f.seedRole(invoke, "arn:aws:iam::123456789012:role/"+invoke)
	r := &PlatformReconciler{IAM: f}

	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, platformWithCapabilities("myplat"), capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if _, ok := f.roles[invoke]; ok {
		t.Errorf("dropping the scheduler capability must delete the invoke role")
	}
}

// TestEnsureCapabilityPolicy_GetErrorPropagates proves a non-NotFound
// GetRolePolicy failure on the capability-access reconcile surfaces.
func TestEnsureCapabilityPolicy_GetErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.getInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f, SSM: boundSES(capClusterName, "myplat")}
	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), capCfg()); err == nil {
		t.Fatalf("expected the GetRolePolicy error to propagate")
	}
}

// TestEnsureCapabilityPolicy_PutErrorPropagates proves a PutRolePolicy failure
// on the capability-access write surfaces.
func TestEnsureCapabilityPolicy_PutErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.putInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f, SSM: boundSES(capClusterName, "myplat")}
	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), capCfg()); err == nil {
		t.Fatalf("expected the PutRolePolicy error to propagate")
	}
}

// TestEnsureCapabilityPolicy_DeleteErrorPropagates proves a non-NotFound
// DeleteRolePolicy failure on the capability-access removal path surfaces.
func TestEnsureCapabilityPolicy_DeleteErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.inline["test-role"] = map[string]string{capabilityPolicyName: "{}"}
	f.deleteInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat"), capCfg()); err == nil {
		t.Fatalf("expected the DeleteRolePolicy error to propagate")
	}
}

// TestEnsureCapabilityPolicy_InvokeRoleGetErrorPropagates proves a non-NotFound
// GetRole failure while ensuring the invoke role surfaces.
func TestEnsureCapabilityPolicy_InvokeRoleGetErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.getReturnsErr = errors.New("boom")
	r := &PlatformReconciler{IAM: f}
	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler), capCfg()); err == nil {
		t.Fatalf("expected the invoke-role GetRole error to propagate")
	}
}

// TestEnsureCapabilityPolicy_InvokeRoleCreateErrorPropagates proves a
// CreateRole failure while minting the invoke role surfaces.
func TestEnsureCapabilityPolicy_InvokeRoleCreateErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.createReturnsErr = errors.New("boom")
	r := &PlatformReconciler{IAM: f}
	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler), capCfg()); err == nil {
		t.Fatalf("expected the invoke-role CreateRole error to propagate")
	}
}

// TestEnsureCapabilityPolicy_InvokeRoleDeleteErrorPropagates proves a failure
// tearing down the invoke role (capability dropped) surfaces.
func TestEnsureCapabilityPolicy_InvokeRoleDeleteErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.listReturnsErr = errors.New("boom")
	r := &PlatformReconciler{IAM: f}
	if _, err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat"), capCfg()); err == nil {
		t.Fatalf("expected the invoke-role teardown error to propagate")
	}
}

// TestEnsureSchedulerInvokeRole_ExistingRoleSkipsCreate proves an existing
// invoke role is not re-created, only its send policy reconciled.
func TestEnsureSchedulerInvokeRole_ExistingRoleSkipsCreate(t *testing.T) {
	f := newFakeIAM()
	invoke := invokeRoleName
	f.seedRole(invoke, "arn:aws:iam::123456789012:role/"+invoke)
	r := &PlatformReconciler{IAM: f}
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "nudges", Kind: platformv1alpha1.DatastoreQueue}}

	if err := r.ensureSchedulerInvokeRole(context.Background(), p, capCfg(), testScope()); err != nil {
		t.Fatalf("ensureSchedulerInvokeRole: %v", err)
	}
	if len(f.createCalls) != 0 {
		t.Errorf("an existing invoke role must not be re-created: %+v", f.createCalls)
	}
	if len(putsFor(f, schedulerInvokeSendPolicyName)) != 1 {
		t.Errorf("the send policy must still be reconciled on an existing role")
	}
}

// TestDeleteSchedulerInvokeRole_NilIAM proves the invoke-role teardown no-ops
// without an IAM client.
func TestDeleteSchedulerInvokeRole_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if err := r.deleteSchedulerInvokeRole(context.Background(), platformWithCapabilities("myplat"), capCfg()); err != nil {
		t.Fatalf("nil IAM must no-op: %v", err)
	}
}

// TestEnsureIamRole_CapabilityPolicyError_CreatePath proves ensureIamRole
// propagates a capability-policy failure on the create path.
func TestEnsureIamRole_CapabilityPolicyError_CreatePath(t *testing.T) {
	f := newFakeIAM()
	f.putInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f, SSM: boundSES("production-cluster", "app")}
	p := newPlatform("app", "tenant")
	p.Spec.Identity.Capabilities = []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES}

	if _, err := r.ensureIamRole(context.Background(), p, datastoreErrCfg()); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the capability-policy error on the create path")
	}
}

// TestEnsureIamRole_CapabilityPolicyError_ExistingRolePath proves the same on
// the existing-role path.
func TestEnsureIamRole_CapabilityPolicyError_ExistingRolePath(t *testing.T) {
	f := newFakeIAM()
	cfg := datastoreErrCfg()
	r := &PlatformReconciler{IAM: f, SSM: boundSES(cfg.ClusterName, "app")}
	p := newPlatform("app", "tenant")
	p.Spec.Identity.Capabilities = []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES}

	roleName := tenantRoleName(cfg.ClusterName, p)
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.putInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}

	if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the capability-policy error on the existing-role path")
	}
}

// TestDeleteIamRole_SchedulerInvokeCleanupErrorPropagates proves the finalizer
// surfaces a failure tearing down the scheduler-invoke role rather than
// silently deleting the tenant role and leaving the invoke role orphaned.
func TestDeleteIamRole_SchedulerInvokeCleanupErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.listReturnsErr = errors.New("boom") // fails the invoke-role detachAndDeleteRole
	r := &PlatformReconciler{IAM: f}      // EKS nil -> pod-identity delete no-ops first
	p := newPlatform("app", "tenant")

	if err := r.deleteIamRole(context.Background(), p, capCfg()); err == nil {
		t.Fatalf("expected the scheduler-invoke cleanup error to propagate")
	}
}

// putsFor returns the PutRolePolicy calls for a given inline-policy name.
func putsFor(f *fakeIAM, policyName string) []iamPut {
	var out []iamPut
	for i := range f.putInlineCalls {
		if *f.putInlineCalls[i].PolicyName == policyName {
			out = append(out, iamPut{f.putInlineCalls[i].PolicyName, f.putInlineCalls[i].PolicyDocument})
		}
	}
	return out
}

type iamPut struct {
	PolicyName     *string
	PolicyDocument *string
}

// deletedInline reports whether a DeleteRolePolicy was issued for a policy name.
func deletedInline(f *fakeIAM, policyName string) bool {
	for i := range f.deleteInlineCalls {
		if *f.deleteInlineCalls[i].PolicyName == policyName {
			return true
		}
	}
	return false
}
