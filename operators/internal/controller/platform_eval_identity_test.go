/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	evalTestCluster   = "production-cluster"
	evalTestNamespace = "evals"
	evalTestBucket    = "production-cluster-111111111111-us-west-2-eval-reports"
	evalTestBoundary  = "arn:aws:iam::123456789012:policy/eks-agent-platform/tenant-boundary"
	evalTestRoleARN   = "arn:aws:iam::123456789012:role/" + evalTestCluster + "-analytics-eval"
)

// evalIdentityReconciler wires a PlatformReconciler with the in-memory IAM and
// EKS fakes and an eval runtime, over a fake client that may carry interceptors.
func evalIdentityReconciler(t *testing.T, funcs *interceptor.Funcs) (*PlatformReconciler, *fakeIAM, *fakeEKS, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	cl := b.Build()
	f, fe := newFakeIAM(), newFakeEKS()
	r := &PlatformReconciler{
		Client: cl,
		Scheme: s,
		IAM:    f,
		EKS:    fe,
		IAMCfg: IAMConfig{
			TenantIAMPath:                "/eks-agent-platform/tenants/",
			TenantBaselinePolicyARN:      "arn:aws:iam::aws:policy/EksAgentBaseline",
			TenantPermissionsBoundaryARN: evalTestBoundary,
			ClusterName:                  evalTestCluster,
			Environment:                  "production",
			Region:                       "us-west-2",
		},
		Eval: EvalIdentityConfig{Namespace: evalTestNamespace, ReportsBucket: evalTestBucket},
	}
	return r, f, fe, cl
}

// iamGlob reports whether an IAM resource pattern matches an ARN. IAM's `*`
// spans any run of characters, `/` included, which is what makes a missing
// delimiter after the Platform name a cross-Platform grant.
func iamGlob(pattern, arn string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(arn)
}

type evalPolicy struct {
	Version   string `json:"Version"`
	Statement []struct {
		Sid       string                       `json:"Sid"`
		Effect    string                       `json:"Effect"`
		Action    []string                     `json:"Action"`
		Resource  []string                     `json:"Resource"`
		Condition map[string]map[string]string `json:"Condition"`
	} `json:"Statement"`
}

func parseEvalPolicy(t *testing.T, doc string) evalPolicy {
	t.Helper()
	var p evalPolicy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("unmarshal eval policy %q: %v", doc, err)
	}
	return p
}

func evalPolicyFor(t *testing.T, platform, bucket string, scope arnScope) evalPolicy {
	t.Helper()
	doc, err := evalReportsPolicyDoc(platform, bucket, scope)
	if err != nil {
		t.Fatalf("evalReportsPolicyDoc: %v", err)
	}
	return parseEvalPolicy(t, doc)
}

// TestPerPlatformNamesKeepTheirShape pins every per-Platform name boundedName
// derives. Each one names a live object — a namespace, or a role in AWS — and a
// name that moves orphans the object under the old one while minting a second.
func TestPerPlatformNamesKeepTheirShape(t *testing.T) {
	long := newPlatform("a-very-long-platform-name-that-runs-well-past-every-limit-we-have", "acme")
	short := newPlatform("analytics", "acme")
	cases := []struct{ name, got, want string }{
		{"namespace, short", PlatformNamespace(short), "tenants-analytics"},
		{"namespace, long", PlatformNamespace(long), "tenants-a-very-long-platform-name-that-runs-well-past--99353f73"},
		{"tenant role, short", tenantRoleName(evalTestCluster, short), "production-cluster-analytics-tenant"},
		{"tenant role, long", tenantRoleName(evalTestCluster, long), "production-cluster-a-very-long-platform-name-tha-99353f73-tenant"},
		{"session role, long", sessionRoleName(evalTestCluster, long), "production-cluster-a-very-long-platform-name-th-99353f73-session"},
		{"scheduler role, long", schedulerInvokeRoleName(evalTestCluster, long), "production-cluster-a-very-long-platfor-99353f73-scheduler-invoke"},
		{"eval role, short", evalRoleName(evalTestCluster, short), "production-cluster-analytics-eval"},
		{"eval role, long", evalRoleName(evalTestCluster, long), "production-cluster-a-very-long-platform-name-that--99353f73-eval"},
		{"eval account, short", EvalServiceAccountName(short), "eval-analytics"},
		{"eval account, long", EvalServiceAccountName(long), "eval-a-very-long-platform-name-that-runs-well-past-eve-99353f73"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if n := len(evalRoleName(evalTestCluster, long)); n != iamRoleNameMax {
		t.Errorf("a truncated eval role name is %d chars; truncation fills the IAM limit of %d exactly", n, iamRoleNameMax)
	}
	if n := len(EvalServiceAccountName(long)); n != 63 {
		t.Errorf("a truncated eval ServiceAccount name is %d chars, want the 63-char label limit", n)
	}
}

// TestBoundedNameKeepsLongNamesApart is the reason the hash is there: two names
// that share everything the cut keeps must still land on different results, or
// two Platforms share one role.
func TestBoundedNameKeepsLongNamesApart(t *testing.T) {
	stem := strings.Repeat("x", 70)
	a, b := boundedName("c-", stem+"a", "-eval", 64), boundedName("c-", stem+"b", "-eval", 64)
	if a == b {
		t.Fatalf("two long names sharing their kept prefix both bound to %q", a)
	}
	for _, got := range []string{a, b} {
		if !strings.HasSuffix(got, "-eval") || len(got) != 64 {
			t.Errorf("bounded name %q: the suffix must survive and the result fill the limit", got)
		}
	}
}

// TestBoundedNameWithNoRoomForTheHash covers a prefix and suffix that leave no
// room for any of the name: the result runs over the limit rather than
// panicking on a negative slice, and the API it is handed refuses it by name.
func TestBoundedNameWithNoRoomForTheHash(t *testing.T) {
	cluster := strings.Repeat("c", 60)
	got := boundedName(cluster+"-", "analytics", "-eval", 64)
	if !strings.HasPrefix(got, cluster+"--") || !strings.HasSuffix(got, "-eval") {
		t.Errorf("got %q, want the prefix, an empty cut, the hash and the suffix", got)
	}
	if len(got) <= 64 {
		t.Errorf("got a %d-char name; with no room for the hash the result cannot fit", len(got))
	}
}

func TestTenantPathNormalizes(t *testing.T) {
	cases := map[string]string{
		"":             "/eks-agent-platform/tenants/",
		"/custom/path": "/custom/path/",
		"/custom/":     "/custom/",
	}
	for in, want := range cases {
		if got := (IAMConfig{TenantIAMPath: in}).tenantPath(); got != want {
			t.Errorf("tenantPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEvalReportsPolicyGrantsExactlyThePlatformsPrefixes is the generated
// policy, statement by statement. Anything added to it widens what one
// Platform's eval run can touch in a bucket every Platform shares.
func TestEvalReportsPolicyGrantsExactlyThePlatformsPrefixes(t *testing.T) {
	scope := arnScope{Partition: "aws", Region: "us-west-2", AccountID: "123456789012"}
	p := evalPolicyFor(t, "analytics", evalTestBucket, scope)

	if p.Version != "2012-10-17" {
		t.Errorf("Version = %q", p.Version)
	}
	bucketARN := "arn:aws:s3:::" + evalTestBucket
	type stmt struct {
		Sid, Effect string
		Action      []string
		Resource    []string
		Condition   map[string]map[string]string
	}
	want := []stmt{
		{"ReadEvalManifests", "Allow", []string{"s3:GetObject"}, []string{bucketARN + "/analytics/manifests/*"}, nil},
		{"WriteEvalRuns", "Allow", []string{"s3:PutObject"}, []string{bucketARN + "/analytics/runs/*"}, nil},
		{"EvalReportsEncryption", "Allow", []string{"kms:Decrypt", "kms:GenerateDataKey"},
			[]string{"arn:aws:kms:us-west-2:123456789012:key/*"},
			map[string]map[string]string{
				"StringEquals": {"kms:EncryptionContext:aws:s3:arn": bucketARN},
				"StringLike":   {"kms:ViaService": "s3.us-west-2.amazonaws.com"},
			}},
	}
	if len(p.Statement) != len(want) {
		t.Fatalf("policy has %d statements, want %d: %+v", len(p.Statement), len(want), p.Statement)
	}
	for i, w := range want {
		g := p.Statement[i]
		got := stmt{g.Sid, g.Effect, g.Action, g.Resource, g.Condition}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("statement %d:\n got  %+v\n want %+v", i, got, w)
		}
	}
}

// TestEvalReportsPolicyFollowsTheScope pins the partition and the unresolved
// fallbacks. With no region or account the KMS grant wildcards them, and the
// encryption-context condition — the bucket ARN — is what still confines it.
func TestEvalReportsPolicyFollowsTheScope(t *testing.T) {
	p := evalPolicyFor(t, "analytics", "b", arnScope{Partition: "aws-us-gov"})
	if got := p.Statement[0].Resource[0]; got != "arn:aws-us-gov:s3:::b/analytics/manifests/*" {
		t.Errorf("manifests resource = %q, want the role's partition", got)
	}
	enc := p.Statement[2]
	if got := enc.Resource[0]; got != "arn:aws-us-gov:kms:*:*:key/*" {
		t.Errorf("kms resource = %q", got)
	}
	if got := enc.Condition["StringLike"]["kms:ViaService"]; got != "s3.*.amazonaws.com" {
		t.Errorf("kms:ViaService = %q, want the region wildcarded", got)
	}
	if got := enc.Condition["StringEquals"]["kms:EncryptionContext:aws:s3:arn"]; got != "arn:aws-us-gov:s3:::b" {
		t.Errorf("encryption context = %q, want the bucket ARN", got)
	}
}

// TestEvalReportsPolicyDoesNotSpanPlatforms is the isolation property itself,
// evaluated the way IAM evaluates it. foo's grant must not reach foo-bar's
// objects, which a prefix without its trailing slash would.
func TestEvalReportsPolicyDoesNotSpanPlatforms(t *testing.T) {
	scope := arnScope{Partition: "aws", Region: "us-west-2", AccountID: "123456789012"}
	p := evalPolicyFor(t, "foo", "b", scope)
	read, write := p.Statement[0].Resource[0], p.Statement[1].Resource[0]

	own := map[string]string{
		"arn:aws:s3:::b/foo/manifests/smoke.json":                   read,
		"arn:aws:s3:::b/foo/runs/nightly/20260101T000000Z-u/r.html": write,
	}
	for obj, pattern := range own {
		if !iamGlob(pattern, obj) {
			t.Errorf("%s does not cover the Platform's own object %s", pattern, obj)
		}
	}
	foreign := []string{
		"arn:aws:s3:::b/foo-bar/manifests/smoke.json",
		"arn:aws:s3:::b/foo-bar/runs/nightly/x/report.html",
		"arn:aws:s3:::b/bar/manifests/smoke.json",
		"arn:aws:s3:::b/bar/runs/nightly/x/report.html",
		"arn:aws:s3:::other/foo/manifests/smoke.json",
	}
	for _, obj := range foreign {
		for _, pattern := range []string{read, write} {
			if iamGlob(pattern, obj) {
				t.Errorf("%s reaches %s, which belongs to another Platform or bucket", pattern, obj)
			}
		}
	}
	// Read and write stay on their own prefixes: a run cannot rewrite the cases
	// it is scored against, nor read back what other runs reported.
	if iamGlob(write, "arn:aws:s3:::b/foo/manifests/smoke.json") {
		t.Errorf("the write grant %s reaches the manifests prefix", write)
	}
	if iamGlob(read, "arn:aws:s3:::b/foo/runs/nightly/x/report.html") {
		t.Errorf("the read grant %s reaches the runs prefix", read)
	}
}

func TestEvalIdentityConfigDefaults(t *testing.T) {
	if (EvalIdentityConfig{}).enabled() {
		t.Error("no reports bucket must mean no eval identity")
	}
	if got := (EvalIdentityConfig{}).namespace(); got != defaultEvalRunnerNamespace {
		t.Errorf("default namespace = %q", got)
	}
	if got := (EvalIdentityConfig{Namespace: "evals"}).namespace(); got != "evals" {
		t.Errorf("configured namespace = %q", got)
	}
}

func TestEnsureEvalIdentity_Creates(t *testing.T) {
	ctx := context.Background()
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	p := newPlatform("analytics", "acme")

	if err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatalf("ensureEvalIdentity: %v", err)
	}

	var sa corev1.ServiceAccount
	if err := cl.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "eval-analytics"}, &sa); err != nil {
		t.Fatalf("eval ServiceAccount not created in the eval runtime namespace: %v", err)
	}
	if sa.Labels[LabelPlatform] != "analytics" || sa.Labels["app.kubernetes.io/component"] != "eval" {
		t.Errorf("ServiceAccount labels = %v, want the Platform's labels with component eval", sa.Labels)
	}
	if len(sa.Annotations) != 0 {
		t.Errorf("ServiceAccount carries annotations %v; Pod Identity binds it, no role-arn annotation", sa.Annotations)
	}

	if len(f.createCalls) != 1 {
		t.Fatalf("CreateRole calls = %d, want 1", len(f.createCalls))
	}
	in := f.createCalls[0]
	if got := aws.ToString(in.RoleName); got != "production-cluster-analytics-eval" {
		t.Errorf("role name = %q", got)
	}
	if got := aws.ToString(in.Path); got != "/eks-agent-platform/tenants/" {
		t.Errorf("role path = %q, want the tenant path the operator's grants are scoped to", got)
	}
	if got := aws.ToString(in.PermissionsBoundary); got != evalTestBoundary {
		t.Errorf("permissions boundary = %q, want the tenant boundary", got)
	}
	trust, _ := assumeRolePolicyForPodIdentity()
	if aws.ToString(in.AssumeRolePolicyDocument) != trust {
		t.Errorf("trust policy = %s, want the Pod Identity trust", aws.ToString(in.AssumeRolePolicyDocument))
	}
	if got := tagValue(in.Tags, "Component"); got != "eval-iam" {
		t.Errorf("Component tag = %q, want eval-iam", got)
	}
	if got, want := tagValue(in.Tags, platformIDTagKey), platformCostID(evalTestCluster, "analytics"); got != want {
		t.Errorf("%s tag = %q, want %q (the tenant role's attribution key)", platformIDTagKey, got, want)
	}

	// The role holds the one inline policy and no managed policy. The tenant
	// baseline is the Bedrock grant; attaching it here would hand every eval run
	// a model path around the Platform's model scoping.
	if n := len(f.attachCalls); n != 0 {
		t.Errorf("AttachRolePolicy called %d times on the eval role; it carries no managed policy", n)
	}
	got := f.inline["production-cluster-analytics-eval"]
	if len(got) != 1 {
		t.Fatalf("eval role inline policies = %v, want exactly %s", got, evalReportsPolicyName)
	}
	want, _ := evalReportsPolicyDoc("analytics", evalTestBucket, arnScopeFromRole(evalTestRoleARN, "us-west-2"))
	if !policyDocEqual(got[evalReportsPolicyName], want) {
		t.Errorf("eval-reports-access =\n %s\nwant\n %s", got[evalReportsPolicyName], want)
	}

	if len(fe.createCalls) != 1 {
		t.Fatalf("CreatePodIdentityAssociation calls = %d, want 1", len(fe.createCalls))
	}
	a := fe.createCalls[0]
	if aws.ToString(a.ClusterName) != evalTestCluster || aws.ToString(a.Namespace) != evalTestNamespace ||
		aws.ToString(a.ServiceAccount) != "eval-analytics" || aws.ToString(a.RoleArn) != evalTestRoleARN {
		t.Errorf("association = (%s, %s, %s → %s), want (%s, %s, eval-analytics → %s)",
			aws.ToString(a.ClusterName), aws.ToString(a.Namespace), aws.ToString(a.ServiceAccount), aws.ToString(a.RoleArn),
			evalTestCluster, evalTestNamespace, evalTestRoleARN)
	}
}

// TestEnsureEvalIdentity_ConvergedPassWritesNothing: the reconcile runs every
// poll interval for every Platform, so a pass over converged state must make no
// write.
func TestEnsureEvalIdentity_ConvergedPassWritesNothing(t *testing.T) {
	ctx := context.Background()
	r, f, fe, _ := evalIdentityReconciler(t, nil)
	p := newPlatform("analytics", "acme")
	for i := 0; i < 3; i++ {
		if err := r.ensureEvalIdentity(ctx, p); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
	if len(f.createCalls) != 1 || len(f.putInlineCalls) != 1 || len(fe.createCalls) != 1 {
		t.Errorf("writes over three passes: CreateRole %d, PutRolePolicy %d, CreatePodIdentityAssociation %d; want 1 each",
			len(f.createCalls), len(f.putInlineCalls), len(fe.createCalls))
	}
}

// TestEnsureEvalIdentity_ConvergesThePolicy covers the update path: a policy
// edited out of band is rewritten, and a role that exists picks up a changed
// bucket rather than keeping a grant into the old one.
func TestEnsureEvalIdentity_ConvergesThePolicy(t *testing.T) {
	ctx := context.Background()
	r, f, _, _ := evalIdentityReconciler(t, nil)
	p := newPlatform("analytics", "acme")
	role := evalRoleName(evalTestCluster, p)
	if err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}

	f.inline[role][evalReportsPolicyName] = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["*"]}]}`
	if err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	want, _ := evalReportsPolicyDoc("analytics", evalTestBucket, arnScopeFromRole(evalTestRoleARN, "us-west-2"))
	if !policyDocEqual(f.inline[role][evalReportsPolicyName], want) {
		t.Errorf("a drifted policy was not rewritten: %s", f.inline[role][evalReportsPolicyName])
	}

	r.Eval.ReportsBucket = "relocated-eval-reports"
	if err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	doc := f.inline[role][evalReportsPolicyName]
	if !strings.Contains(doc, "arn:aws:s3:::relocated-eval-reports/analytics/runs/*") || strings.Contains(doc, evalTestBucket) {
		t.Errorf("after the bucket moved the policy still names the old one: %s", doc)
	}
	if len(f.createCalls) != 1 {
		t.Errorf("CreateRole calls = %d; an existing role is converged in place, not recreated", len(f.createCalls))
	}
}

// TestEnsureEvalIdentity_ReconcilesTheAccountWithoutIAM: the ServiceAccount is
// the EvalSuite reconciler's precondition, so it exists wherever runs can be
// submitted, AWS wired or not.
func TestEnsureEvalIdentity_ReconcilesTheAccountWithoutIAM(t *testing.T) {
	ctx := context.Background()
	r, _, _, cl := evalIdentityReconciler(t, nil)
	r.IAM, r.EKS = nil, nil
	if err := r.ensureEvalIdentity(ctx, newPlatform("analytics", "acme")); err != nil {
		t.Fatalf("ensureEvalIdentity: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "eval-analytics"}, &corev1.ServiceAccount{}); err != nil {
		t.Errorf("no ServiceAccount without an IAM client: %v", err)
	}
}

// TestEvalIdentityIsInertWithoutAnEvalRuntime: no reports bucket, no calls of
// any kind — a cluster that never ran evals holds no grant for the -eval role
// name, so a stray call would fail the reconcile or wedge the finalizer.
func TestEvalIdentityIsInertWithoutAnEvalRuntime(t *testing.T) {
	ctx := context.Background()
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	r.Eval = EvalIdentityConfig{}
	f.getReturnsErr = errors.New("GetRole must not be called")
	f.listReturnsErr = errors.New("ListAttachedRolePolicies must not be called")
	fe.listReturnsErr = errors.New("ListPodIdentityAssociations must not be called")
	p := newPlatform("analytics", "acme")

	if err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Errorf("ensureEvalIdentity: %v", err)
	}
	if err := r.deleteEvalIdentity(ctx, p); err != nil {
		t.Errorf("deleteEvalIdentity: %v", err)
	}
	var list corev1.ServiceAccountList
	if err := cl.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("ServiceAccounts created with no eval runtime: %v", list.Items)
	}
}

// TestEnsureEvalIdentity_DefersWithoutTheRuntimeNamespace: the API server
// answers a ServiceAccount create in an absent namespace with NotFound. That is
// a cluster without the chart's eval runtime, not a failed reconcile — the
// Platform goes on to Ready, and no role is minted for an account that does
// not exist yet.
func TestEnsureEvalIdentity_DefersWithoutTheRuntimeNamespace(t *testing.T) {
	r, f, fe, _ := evalIdentityReconciler(t, &interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, evalTestNamespace)
	}})
	f.getReturnsErr = errors.New("GetRole must not be called")
	fe.createReturnsErr = errors.New("CreatePodIdentityAssociation must not be called")
	if err := r.ensureEvalIdentity(context.Background(), newPlatform("analytics", "acme")); err != nil {
		t.Errorf("ensureEvalIdentity = %v, want the identity deferred", err)
	}
	if len(f.roles) != 0 || len(fe.createCalls) != 0 {
		t.Errorf("minted AWS identity for an account that does not exist: roles=%v associations=%v", f.roles, fe.createCalls)
	}
}

func TestEnsureEvalIdentity_Errors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	cases := []struct {
		name   string
		funcs  *interceptor.Funcs
		inject func(*fakeIAM, *fakeEKS)
		want   string
	}{
		{
			name: "service account write",
			funcs: &interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return boom
			}},
			want: "ensure eval ServiceAccount evals/eval-analytics",
		},
		{name: "GetRole", inject: func(f *fakeIAM, _ *fakeEKS) { f.getReturnsErr = boom }, want: "iam GetRole production-cluster-analytics-eval"},
		{name: "CreateRole", inject: func(f *fakeIAM, _ *fakeEKS) { f.createReturnsErr = boom }, want: "iam CreateRole production-cluster-analytics-eval"},
		{
			name:   "PutRolePolicy",
			inject: func(f *fakeIAM, _ *fakeEKS) { f.putInlineReturnsErr = map[string]error{evalReportsPolicyName: boom} },
			want:   "iam PutRolePolicy production-cluster-analytics-eval/eval-reports-access",
		},
		{name: "CreatePodIdentityAssociation", inject: func(_ *fakeIAM, fe *fakeEKS) { fe.createReturnsErr = boom }, want: "eks CreatePodIdentityAssociation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, f, fe, _ := evalIdentityReconciler(t, c.funcs)
			if c.inject != nil {
				c.inject(f, fe)
			}
			err := r.ensureEvalIdentity(ctx, newPlatform("analytics", "acme"))
			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error wrapping boom that names %q", err, c.want)
			}
		})
	}
}

func TestDeleteEvalIdentity_RemovesEverythingItMinted(t *testing.T) {
	ctx := context.Background()
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	p := newPlatform("analytics", "acme")
	if err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 2; pass++ {
		if err := r.deleteEvalIdentity(ctx, p); err != nil {
			t.Fatalf("delete pass %d: %v", pass, err)
		}
	}
	if len(fe.associations) != 0 {
		t.Errorf("associations left behind: %v", fe.associations)
	}
	err := cl.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "eval-analytics"}, &corev1.ServiceAccount{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("eval ServiceAccount still present (err=%v)", err)
	}
	if _, ok := f.roles["production-cluster-analytics-eval"]; ok {
		t.Error("eval role still present")
	}
	if len(f.inline["production-cluster-analytics-eval"]) != 0 {
		t.Errorf("inline policies left on the deleted role: %v", f.inline["production-cluster-analytics-eval"])
	}
}

func TestDeleteEvalIdentity_Errors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	cases := []struct {
		name   string
		funcs  *interceptor.Funcs
		inject func(*fakeIAM, *fakeEKS)
		want   string
	}{
		{name: "association", inject: func(_ *fakeIAM, fe *fakeEKS) { fe.listReturnsErr = boom }, want: "eks ListPodIdentityAssociations"},
		{
			name: "service account",
			funcs: &interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return boom
			}},
			want: "delete eval ServiceAccount evals/eval-analytics",
		},
		{name: "role", inject: func(f *fakeIAM, _ *fakeEKS) { f.deleteReturnsErr = boom }, want: "iam DeleteRole"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, f, fe, _ := evalIdentityReconciler(t, c.funcs)
			p := newPlatform("analytics", "acme")
			if err := r.ensureEvalIdentity(ctx, p); err != nil {
				t.Fatal(err)
			}
			if c.inject != nil {
				c.inject(f, fe)
			}
			err := r.deleteEvalIdentity(ctx, p)
			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error wrapping boom that names %q", err, c.want)
			}
		})
	}
}

// TestEvalRoleIsNotTheTenantRole: the eval role and the tenant role are
// distinct identities, so neither Platform's tenant workloads nor its eval runs
// hold the other's grants.
func TestEvalRoleIsNotTheTenantRole(t *testing.T) {
	p := newPlatform("analytics", "acme")
	if evalRoleName(evalTestCluster, p) == tenantRoleName(evalTestCluster, p) {
		t.Fatal("the eval role and the tenant role share a name")
	}
	tags := evalRoleTags(p, IAMConfig{ClusterName: evalTestCluster})
	if got := tagValue(tags, "Component"); got != "eval-iam" {
		t.Errorf("Component = %q", got)
	}
	tenant := tenantRoleTags(p, IAMConfig{ClusterName: evalTestCluster})
	if tagValue(tags, platformIDTagKey) != tagValue(tenant, platformIDTagKey) {
		t.Error("the eval role must attribute to the same Platform as the tenant role")
	}
}
