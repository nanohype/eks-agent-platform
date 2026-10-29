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
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
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
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&platformv1alpha1.Platform{})
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

// storedPlatform creates the analytics Platform in the reconciler's client,
// which a finalizer pass writes status.evalRoleArn back through.
func storedPlatform(t *testing.T, cl client.Client) *platformv1alpha1.Platform {
	t.Helper()
	p := newPlatform("analytics", "acme")
	if err := cl.Create(context.Background(), p); err != nil {
		t.Fatalf("create Platform: %v", err)
	}
	return p
}

// storedEvalRoleArn is status.evalRoleArn as the API server holds it.
func storedEvalRoleArn(t *testing.T, cl client.Client, p *platformv1alpha1.Platform) string {
	t.Helper()
	var got platformv1alpha1.Platform
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatalf("get Platform: %v", err)
	}
	return got.Status.EvalRoleArn
}

// accessDenied is IAM refusing a call naming the role.
func accessDenied(role string) error {
	return &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform: iam:GetRole on resource: role " + role}
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
	p := storedPlatform(t, cl)

	outcome, err := r.ensureEvalIdentity(ctx, p)
	if err != nil {
		t.Fatalf("ensureEvalIdentity: %v", err)
	}
	if outcome != evalIdentityBound {
		t.Errorf("outcome = %v, want evalIdentityBound", outcome)
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

	if p.Status.EvalRoleArn != evalTestRoleARN {
		t.Errorf("status.evalRoleArn = %q, want the minted role %s", p.Status.EvalRoleArn, evalTestRoleARN)
	}
	if got := storedEvalRoleArn(t, cl, p); got != evalTestRoleARN {
		t.Errorf("stored status.evalRoleArn = %q, want the minted role written through by the pass itself", got)
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
	statusPatches := 0
	r, f, fe, cl := evalIdentityReconciler(t, &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			statusPatches++
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	p := storedPlatform(t, cl)
	for i := 0; i < 3; i++ {
		if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
	if len(f.createCalls) != 1 || len(f.putInlineCalls) != 1 || len(fe.createCalls) != 1 || statusPatches != 1 {
		t.Errorf("writes over three passes: CreateRole %d, PutRolePolicy %d, CreatePodIdentityAssociation %d, status patches %d; want 1 each",
			len(f.createCalls), len(f.putInlineCalls), len(fe.createCalls), statusPatches)
	}
}

// TestEnsureEvalIdentity_ConvergesThePolicy covers the update path: a policy
// edited out of band is rewritten, and a role that exists picks up a changed
// bucket rather than keeping a grant into the old one.
func TestEnsureEvalIdentity_ConvergesThePolicy(t *testing.T) {
	ctx := context.Background()
	r, f, _, cl := evalIdentityReconciler(t, nil)
	p := storedPlatform(t, cl)
	role := evalRoleName(evalTestCluster, p)
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}

	f.inline[role][evalReportsPolicyName] = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["*"]}]}`
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	want, _ := evalReportsPolicyDoc("analytics", evalTestBucket, arnScopeFromRole(evalTestRoleARN, "us-west-2"))
	if !policyDocEqual(f.inline[role][evalReportsPolicyName], want) {
		t.Errorf("a drifted policy was not rewritten: %s", f.inline[role][evalReportsPolicyName])
	}

	r.Eval.ReportsBucket = "relocated-eval-reports"
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
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

// TestEvalIdentityNeedsBothAWSClients: the identity is a role bound to an
// account by Pod Identity, so a reconciler missing either client mints no part
// of it — an account alone is one no run can hold credentials through — and
// has none to tear down.
func TestEvalIdentityNeedsBothAWSClients(t *testing.T) {
	ctx := context.Background()
	for _, missing := range []string{"IAM", "EKS"} {
		t.Run(missing, func(t *testing.T) {
			r, _, _, cl := evalIdentityReconciler(t, nil)
			if missing == "IAM" {
				r.IAM = nil
			} else {
				r.EKS = nil
			}
			p := newPlatform("analytics", "acme")
			outcome, err := r.ensureEvalIdentity(ctx, p)
			if err != nil || outcome != evalIdentityUnmanaged {
				t.Fatalf("ensureEvalIdentity = (%v, %v), want (evalIdentityUnmanaged, nil)", outcome, err)
			}
			err = cl.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "eval-analytics"}, &corev1.ServiceAccount{})
			if !apierrors.IsNotFound(err) {
				t.Errorf("eval ServiceAccount created with no %s client (err=%v)", missing, err)
			}
			if err := r.deleteEvalIdentity(ctx, p); err != nil {
				t.Errorf("deleteEvalIdentity: %v", err)
			}
		})
	}
}

// TestEvalIdentityIsInertWithoutAnEvalRuntime: no reports bucket, no calls of
// any kind. A cluster that runs no evals has no eval identity, and the
// operator makes no IAM or EKS call on behalf of one.
func TestEvalIdentityIsInertWithoutAnEvalRuntime(t *testing.T) {
	ctx := context.Background()
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	r.Eval = EvalIdentityConfig{}
	f.getReturnsErr = errors.New("GetRole must not be called")
	f.listReturnsErr = errors.New("ListAttachedRolePolicies must not be called")
	fe.listReturnsErr = errors.New("ListPodIdentityAssociations must not be called")
	p := newPlatform("analytics", "acme")

	if outcome, err := r.ensureEvalIdentity(ctx, p); err != nil || outcome != evalIdentityUnmanaged {
		t.Errorf("ensureEvalIdentity = (%v, %v), want (evalIdentityUnmanaged, nil)", outcome, err)
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
// a cluster without the chart's eval runtime, not a failed pass — the outcome
// is deferred, and no role is minted for an account that does not exist yet.
func TestEnsureEvalIdentity_DefersWithoutTheRuntimeNamespace(t *testing.T) {
	r, f, fe, _ := evalIdentityReconciler(t, &interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, evalTestNamespace)
	}})
	f.getReturnsErr = errors.New("GetRole must not be called")
	fe.createReturnsErr = errors.New("CreatePodIdentityAssociation must not be called")
	if outcome, err := r.ensureEvalIdentity(context.Background(), newPlatform("analytics", "acme")); err != nil || outcome != evalIdentityDeferred {
		t.Errorf("ensureEvalIdentity = (%v, %v), want (evalIdentityDeferred, nil)", outcome, err)
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
			funcs: &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ServiceAccount); ok {
					return boom
				}
				return c.Create(ctx, obj, opts...)
			}},
			want: "ensure eval ServiceAccount evals/eval-analytics",
		},
		{name: "GetRole", inject: func(f *fakeIAM, _ *fakeEKS) { f.getReturnsErr = boom }, want: "iam GetRole production-cluster-analytics-eval"},
		{name: "CreateRole", inject: func(f *fakeIAM, _ *fakeEKS) { f.createReturnsErr = boom }, want: "iam CreateRole production-cluster-analytics-eval"},
		{
			name:   "CreateRole after a refused read",
			inject: func(f *fakeIAM, _ *fakeEKS) { f.absentReadsDenied, f.createReturnsErr = true, boom },
			want:   "iam CreateRole production-cluster-analytics-eval",
		},
		{
			name:   "PutRolePolicy",
			inject: func(f *fakeIAM, _ *fakeEKS) { f.putInlineReturnsErr = map[string]error{evalReportsPolicyName: boom} },
			want:   "iam PutRolePolicy production-cluster-analytics-eval/eval-reports-access",
		},
		{name: "CreatePodIdentityAssociation", inject: func(_ *fakeIAM, fe *fakeEKS) { fe.createReturnsErr = boom }, want: "eks CreatePodIdentityAssociation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, f, fe, cl := evalIdentityReconciler(t, c.funcs)
			if c.inject != nil {
				c.inject(f, fe)
			}
			outcome, err := r.ensureEvalIdentity(ctx, storedPlatform(t, cl))
			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error wrapping boom that names %q", err, c.want)
			}
			if outcome == evalIdentityBound {
				t.Error("a failed pass reported the identity bound")
			}
		})
	}
}

func TestDeleteEvalIdentity_RemovesEverythingItMinted(t *testing.T) {
	ctx := context.Background()
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	p := storedPlatform(t, cl)
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
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
	// record is status.evalRoleArn after the failed pass: kept until the role
	// has been read and the cleared record written, empty from then on.
	cases := []struct {
		name   string
		funcs  *interceptor.Funcs
		inject func(*fakeIAM, *fakeEKS)
		want   string
		record string
	}{
		{name: "association", inject: func(_ *fakeIAM, fe *fakeEKS) { fe.listReturnsErr = boom }, want: "eks ListPodIdentityAssociations", record: evalTestRoleARN},
		{
			name: "service account",
			funcs: &interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return boom
			}},
			want:   "delete eval ServiceAccount evals/eval-analytics",
			record: evalTestRoleARN,
		},
		{name: "role lookup", inject: func(f *fakeIAM, _ *fakeEKS) { f.getReturnsErr = boom }, want: "iam GetRole production-cluster-analytics-eval", record: evalTestRoleARN},
		{
			name: "record",
			funcs: &interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if obj.(*platformv1alpha1.Platform).Status.EvalRoleArn == "" {
					return boom
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			}},
			want:   "clear status.evalRoleArn on Platform eks-agent-platform/analytics",
			record: evalTestRoleARN,
		},
		{name: "role", inject: func(f *fakeIAM, _ *fakeEKS) { f.deleteReturnsErr = boom }, want: "iam DeleteRole", record: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, f, fe, cl := evalIdentityReconciler(t, c.funcs)
			p := storedPlatform(t, cl)
			if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
				t.Fatal(err)
			}
			if c.inject != nil {
				c.inject(f, fe)
			}
			err := r.deleteEvalIdentity(ctx, p)
			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error wrapping boom that names %q", err, c.want)
			}
			if p.Status.EvalRoleArn != c.record {
				t.Errorf("status.evalRoleArn = %q after the failed pass, want %q", p.Status.EvalRoleArn, c.record)
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

// TestEnsureEvalIdentity_NeedsNoGrantOnTheRootPathName drives the operator
// role as a tenant-path read grant leaves it: IAM refuses the existence check
// for an eval role that is not there yet, rather than answering NoSuchEntity.
// The identity is still minted, a later pass reads the role through the tenant
// path and creates nothing, and the finalizer releases whether or not the role
// is still there to read.
func TestEnsureEvalIdentity_NeedsNoGrantOnTheRootPathName(t *testing.T) {
	ctx := context.Background()
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	f.absentReadsDenied = true
	p := storedPlatform(t, cl)

	for pass := 1; pass <= 2; pass++ {
		outcome, err := r.ensureEvalIdentity(ctx, p)
		if err != nil || outcome != evalIdentityBound {
			t.Fatalf("pass %d: ensureEvalIdentity = (%v, %v), want (evalIdentityBound, nil)", pass, outcome, err)
		}
	}
	if len(f.createCalls) != 1 || len(fe.createCalls) != 1 {
		t.Errorf("CreateRole %d, CreatePodIdentityAssociation %d over two passes; want 1 each", len(f.createCalls), len(fe.createCalls))
	}

	for pass := 1; pass <= 2; pass++ {
		if err := r.deleteEvalIdentity(ctx, p); err != nil {
			t.Fatalf("delete pass %d: %v", pass, err)
		}
	}
	if _, ok := f.roles["production-cluster-analytics-eval"]; ok {
		t.Error("eval role still present")
	}
}

// TestDeleteEvalIdentity_ReleasesWhenNoRoleWasMinted: a Platform deleted
// before a pass minted its eval role — or while the eval runtime namespace was
// absent — has nothing in IAM to remove, whether IAM says so with NoSuchEntity
// or by refusing the read, and no call is made against the absent role.
func TestDeleteEvalIdentity_ReleasesWhenNoRoleWasMinted(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("absentReadsDenied=%v", denied), func(t *testing.T) {
			r, f, _, _ := evalIdentityReconciler(t, nil)
			f.absentReadsDenied = denied
			f.listReturnsErr = errors.New("ListAttachedRolePolicies called for a role that is not there")
			f.deleteReturnsErr = errors.New("DeleteRole called for a role that is not there")
			if err := r.deleteEvalIdentity(context.Background(), newPlatform("analytics", "acme")); err != nil {
				t.Errorf("deleteEvalIdentity: %v", err)
			}
		})
	}
}

// TestEvalRoleOutsideTheTenantPathIsNeverAdopted: the operator mints the eval
// role only under the tenant path, so a role of its name anywhere else is not
// one it minted. It is neither written to, bound to the Platform's runs, nor
// deleted with the Platform.
func TestEvalRoleOutsideTheTenantPathIsNeverAdopted(t *testing.T) {
	ctx := context.Background()
	r, f, fe, _ := evalIdentityReconciler(t, nil)
	const name = "production-cluster-analytics-eval"
	f.seedRole(name, "arn:aws:iam::123456789012:role/"+name)
	f.roles[name].Path = aws.String("/")
	p := newPlatform("analytics", "acme")

	outcome, err := r.ensureEvalIdentity(ctx, p)
	if err == nil || !strings.Contains(err.Error(), `under path "/"`) {
		t.Fatalf("ensureEvalIdentity = (%v, %v), want a refusal naming the foreign path", outcome, err)
	}
	if len(f.putInlineCalls) != 0 || len(fe.createCalls) != 0 {
		t.Errorf("wrote to or bound a role the operator did not mint: PutRolePolicy %d, CreatePodIdentityAssociation %d",
			len(f.putInlineCalls), len(fe.createCalls))
	}

	if err := r.deleteEvalIdentity(ctx, p); err != nil {
		t.Fatalf("deleteEvalIdentity: %v", err)
	}
	if _, ok := f.roles[name]; !ok {
		t.Error("deleted a role the operator did not mint")
	}
}

// TestEnsureEvalRole_RefusesANameHeldOutOfSight: the read is refused and the
// create finds the name taken, so a role of this name exists that the operator
// cannot read under the tenant path. It is not adopted, and the error carries
// both answers.
func TestEnsureEvalRole_RefusesANameHeldOutOfSight(t *testing.T) {
	r, f, fe, _ := evalIdentityReconciler(t, nil)
	f.absentReadsDenied = true
	f.createReturnsErr = &iamtypes.EntityAlreadyExistsException{Message: aws.String("Role with name production-cluster-analytics-eval already exists.")}

	_, err := r.ensureEvalIdentity(context.Background(), newPlatform("analytics", "acme"))
	var taken *iamtypes.EntityAlreadyExistsException
	if !errors.As(err, &taken) {
		t.Fatalf("got %v, want EntityAlreadyExists", err)
	}
	for _, want := range []string{"cannot read under the tenant path /eks-agent-platform/tenants/", "AccessDenied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
	if len(fe.createCalls) != 0 {
		t.Error("bound the account to a role the operator could not read")
	}
}

func TestIsIAMAccessDenied(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"NoSuchEntity", &iamtypes.NoSuchEntityException{}, false},
		{"AccessDenied", &smithy.GenericAPIError{Code: "AccessDenied"}, true},
		{"wrapped AccessDenied", fmt.Errorf("op: %w", &smithy.GenericAPIError{Code: "AccessDenied"}), true},
		{"throttled", &smithy.GenericAPIError{Code: "Throttling"}, false},
		{"not an API error", errors.New("dial tcp: i/o timeout"), false},
	}
	for _, c := range cases {
		if got := isIAMAccessDenied(c.err); got != c.want {
			t.Errorf("%s: isIAMAccessDenied = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEvalIdentityCondition walks the condition through every state a pass can
// leave. Only a pass that bound the identity end to end reads True, because
// the EvalSuite reconciler submits runs on exactly that.
func TestEvalIdentityCondition(t *testing.T) {
	r, _, _, _ := evalIdentityReconciler(t, nil)
	p := newPlatform("analytics", "acme")
	p.Generation = 4
	get := func() *metav1.Condition {
		return apimeta.FindStatusCondition(p.Status.Conditions, conditionEvalIdentityReady)
	}

	p.Status.EvalRoleArn = evalTestRoleARN
	r.setEvalIdentityCondition(p, evalIdentityBound, nil)
	c := get()
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Bound" || c.ObservedGeneration != 4 {
		t.Fatalf("bound pass: %+v", c)
	}
	for _, want := range []string{"evals/eval-analytics", evalTestRoleARN} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("bound message %q does not name %q", c.Message, want)
		}
	}

	r.setEvalIdentityCondition(p, evalIdentityUnmanaged, errors.New("iam CreateRole production-cluster-analytics-eval: boom"))
	c = get()
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ReconcileFailed" ||
		!strings.Contains(c.Message, "iam CreateRole production-cluster-analytics-eval: boom") {
		t.Errorf("failed pass: %+v", c)
	}

	r.setEvalIdentityCondition(p, evalIdentityDeferred, nil)
	c = get()
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "RuntimeNamespaceAbsent" || !strings.Contains(c.Message, "namespace evals") {
		t.Errorf("deferred pass: %+v", c)
	}

	r.setEvalIdentityCondition(p, evalIdentityUnmanaged, nil)
	if c := get(); c != nil {
		t.Errorf("a reconciler that mints no eval identity left %+v on the Platform", c)
	}
}

// TestEnsureEvalIdentity_RecordsTheRoleItHolds: status.evalRoleArn is what
// tells the finalizer a role was minted, so a pass writes the role through to
// the API server as soon as it holds one — even a pass that then fails — and
// records nothing for a role it neither found under the tenant path nor
// created.
func TestEnsureEvalIdentity_RecordsTheRoleItHolds(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	cases := []struct {
		name   string
		inject func(*fakeIAM, *fakeEKS)
		want   string
	}{
		{"bound", func(*fakeIAM, *fakeEKS) {}, evalTestRoleARN},
		{"policy write fails", func(f *fakeIAM, _ *fakeEKS) { f.putInlineReturnsErr = map[string]error{evalReportsPolicyName: boom} }, evalTestRoleARN},
		{"association fails", func(_ *fakeIAM, fe *fakeEKS) { fe.createReturnsErr = boom }, evalTestRoleARN},
		{"create fails", func(f *fakeIAM, _ *fakeEKS) { f.createReturnsErr = boom }, ""},
		{"name held under another path", func(f *fakeIAM, _ *fakeEKS) {
			f.seedRole("production-cluster-analytics-eval", "arn:aws:iam::123456789012:role/production-cluster-analytics-eval")
			f.roles["production-cluster-analytics-eval"].Path = aws.String("/")
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, f, fe, cl := evalIdentityReconciler(t, nil)
			c.inject(f, fe)
			p := storedPlatform(t, cl)
			_, _ = r.ensureEvalIdentity(ctx, p)
			if p.Status.EvalRoleArn != c.want {
				t.Errorf("status.evalRoleArn = %q, want %q", p.Status.EvalRoleArn, c.want)
			}
			if got := storedEvalRoleArn(t, cl, p); got != c.want {
				t.Errorf("stored status.evalRoleArn = %q, want %q written through by the pass itself", got, c.want)
			}
		})
	}
}

// TestDeleteEvalRole_HoldsOnARefusedReadOfARecordedRole: once a role is
// recorded, a refused read is the operator's own role out of its reach, not an
// absent one. Releasing the finalizer would leave the role and its grant into
// the eval-reports bucket behind, so the finalizer holds — the role, its policy
// and the record all stay — until the role can be read and deleted.
func TestDeleteEvalRole_HoldsOnARefusedReadOfARecordedRole(t *testing.T) {
	ctx := context.Background()
	r, f, _, cl := evalIdentityReconciler(t, nil)
	p := storedPlatform(t, cl)
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := cl.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	const role = "production-cluster-analytics-eval"

	f.getReturnsErr = accessDenied(role)
	err := r.deleteEvalIdentity(ctx, p)
	if err == nil || !isIAMAccessDenied(err) || !strings.Contains(err.Error(), "status.evalRoleArn records "+evalTestRoleARN) {
		t.Fatalf("deleteEvalIdentity = %v, want the refusal, naming the recorded role", err)
	}
	if _, ok := f.roles[role]; !ok || len(f.inline[role]) != 1 {
		t.Errorf("the role or its policy was removed on a refused read: role present %v, inline %v", ok, f.inline[role])
	}
	if got := storedEvalRoleArn(t, cl, p); got != evalTestRoleARN {
		t.Errorf("stored status.evalRoleArn = %q, want the record kept while the role is out of reach", got)
	}

	f.getReturnsErr = nil
	if err := r.deleteEvalIdentity(ctx, p); err != nil {
		t.Fatalf("deleteEvalIdentity once the role is readable: %v", err)
	}
	if _, ok := f.roles[role]; ok {
		t.Error("eval role survived a finalizer that could read it")
	}
}

// TestDeleteEvalRole_ClearsTheRecordBeforeDeletingTheRole: the record goes
// once a read has found the role, ahead of the deletion, so a deletion that
// fails leaves the role to the next pass's read rather than to the record.
func TestDeleteEvalRole_ClearsTheRecordBeforeDeletingTheRole(t *testing.T) {
	ctx := context.Background()
	r, f, _, cl := evalIdentityReconciler(t, nil)
	p := storedPlatform(t, cl)
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := cl.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}

	f.deleteReturnsErr = errors.New("boom")
	if err := r.deleteEvalIdentity(ctx, p); err == nil {
		t.Fatal("deleteEvalIdentity reported success over a failed DeleteRole")
	}
	if got := storedEvalRoleArn(t, cl, p); got != "" {
		t.Errorf("stored status.evalRoleArn = %q after the role was read; want it cleared before the delete", got)
	}

	f.deleteReturnsErr = nil
	if err := r.deleteEvalIdentity(ctx, p); err != nil {
		t.Fatalf("retried deleteEvalIdentity: %v", err)
	}
	if _, ok := f.roles["production-cluster-analytics-eval"]; ok {
		t.Error("eval role survived the retried finalizer")
	}
}

// TestDeleteEvalRole_ARetriedFinalizerReleases: with no grant on the root-path
// name, a deleted role's read is refused like any absent name's. A finalizer
// retried after a later step failed reads the Platform afresh, and must find no
// record left to hold it on its own completed deletion.
func TestDeleteEvalRole_ARetriedFinalizerReleases(t *testing.T) {
	ctx := context.Background()
	r, f, _, cl := evalIdentityReconciler(t, nil)
	f.absentReadsDenied = true
	p := storedPlatform(t, cl)
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := cl.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteEvalIdentity(ctx, p); err != nil {
		t.Fatalf("deleteEvalIdentity: %v", err)
	}

	var again platformv1alpha1.Platform
	if err := cl.Get(ctx, client.ObjectKeyFromObject(p), &again); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteEvalIdentity(ctx, &again); err != nil {
		t.Errorf("retried deleteEvalIdentity held on the role it had deleted: %v", err)
	}
}

// TestEnsureEvalIdentity_RebindsAnAssociationToAnotherRole: an association for
// the eval account that names any role but the eval role would hand runs
// credentials the operator did not scope while EvalIdentityReady names the eval
// role. The pass replaces it before reporting the identity bound.
func TestEnsureEvalIdentity_RebindsAnAssociationToAnotherRole(t *testing.T) {
	ctx := context.Background()
	r, _, fe, cl := evalIdentityReconciler(t, nil)
	fe.seedAssociation(evalTestNamespace, "eval-analytics", "arn:aws:iam::123456789012:role/someone-else", "")
	p := storedPlatform(t, cl)

	outcome, err := r.ensureEvalIdentity(ctx, p)
	if err != nil || outcome != evalIdentityBound {
		t.Fatalf("ensureEvalIdentity = (%v, %v), want (evalIdentityBound, nil)", outcome, err)
	}
	if got := fe.roleFor(evalTestNamespace, "eval-analytics"); got != evalTestRoleARN {
		t.Errorf("evals/eval-analytics is bound to %q, want the eval role %s", got, evalTestRoleARN)
	}
}

// TestEnsureEvalIdentity_RecordWriteFailureBindsNothing: the role is bound to
// the account only once its record is on the API server, so a pass whose
// record write is refused binds nothing and says why. The value stays on the
// Platform the pass holds, for the reconcile's closing status write, and the
// next pass records the role it finds under the tenant path and binds it.
func TestEnsureEvalIdentity_RecordWriteFailureBindsNothing(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	refuse := true
	r, f, fe, cl := evalIdentityReconciler(t, &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if refuse {
				return boom
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	p := storedPlatform(t, cl)

	outcome, err := r.ensureEvalIdentity(ctx, p)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "record eval role "+evalTestRoleARN+" in status.evalRoleArn of Platform eks-agent-platform/analytics") {
		t.Fatalf("ensureEvalIdentity = %v, want the refused record write, naming the role", err)
	}
	if outcome == evalIdentityBound {
		t.Error("a pass whose record write failed reported the identity bound")
	}
	if len(fe.createCalls) != 0 {
		t.Error("the role was bound to the account with no record of it on the API server")
	}
	if p.Status.EvalRoleArn != evalTestRoleARN {
		t.Errorf("status.evalRoleArn on the pass's Platform = %q, want %s kept for the closing status write", p.Status.EvalRoleArn, evalTestRoleARN)
	}

	refuse = false
	next := refetched(t, cl, p)
	if outcome, err := r.ensureEvalIdentity(ctx, next); err != nil || outcome != evalIdentityBound {
		t.Fatalf("next pass: ensureEvalIdentity = (%v, %v), want (evalIdentityBound, nil)", outcome, err)
	}
	if got := storedEvalRoleArn(t, cl, p); got != evalTestRoleARN {
		t.Errorf("stored status.evalRoleArn = %q after the next pass, want %s", got, evalTestRoleARN)
	}
	if len(f.createCalls) != 1 {
		t.Errorf("CreateRole calls = %d; the next pass records the role it finds rather than minting another", len(f.createCalls))
	}
}

// refetched is p as the API server holds it, the way the next reconcile reads
// it.
func refetched(t *testing.T, cl client.Client, p *platformv1alpha1.Platform) *platformv1alpha1.Platform {
	t.Helper()
	var got platformv1alpha1.Platform
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatalf("get Platform: %v", err)
	}
	return &got
}

// TestEnsureEvalIdentity_ClosingStatusWriteLandsOnTheRecord: the reconcile
// writes the Platform's status once more at the end of the pass. The record's
// patch moves the Platform's resourceVersion, and that closing write must land
// rather than conflict, and must keep the record. The pass also keeps the spec
// it read: the patch's response is not decoded into it.
func TestEnsureEvalIdentity_ClosingStatusWriteLandsOnTheRecord(t *testing.T) {
	ctx := context.Background()
	r, _, _, cl := evalIdentityReconciler(t, nil)
	p := storedPlatform(t, cl)

	edited := refetched(t, cl, p)
	edited.Spec.Persona = "edited-mid-pass"
	if err := cl.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}

	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatalf("ensureEvalIdentity: %v", err)
	}
	if p.Spec.Persona != "generic" {
		t.Errorf("spec.persona on the pass's Platform = %q; the pass must keep reconciling the spec it read", p.Spec.Persona)
	}
	p.Status.Phase = phaseReady
	if err := cl.Status().Update(ctx, p); err != nil {
		t.Fatalf("the reconcile's closing status write after the record: %v", err)
	}
	got := refetched(t, cl, p)
	if got.Status.EvalRoleArn != evalTestRoleARN || got.Status.Phase != phaseReady {
		t.Errorf("stored status = (evalRoleArn %q, phase %q), want (%s, %s)", got.Status.EvalRoleArn, got.Status.Phase, evalTestRoleARN, phaseReady)
	}
	if got.Spec.Persona != "edited-mid-pass" {
		t.Errorf("stored spec.persona = %q; a status write must not carry the pass's spec back", got.Spec.Persona)
	}
}

// TestEnsureEvalRole_NamesACreateRace: EntityAlreadyExists after GetRole
// answered NoSuchEntity is a role created between the two calls, not one the
// operator cannot read, and the error says which.
func TestEnsureEvalRole_NamesACreateRace(t *testing.T) {
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	f.createReturnsErr = &iamtypes.EntityAlreadyExistsException{Message: aws.String("Role with name production-cluster-analytics-eval already exists.")}

	_, err := r.ensureEvalIdentity(context.Background(), storedPlatform(t, cl))
	var taken *iamtypes.EntityAlreadyExistsException
	if !errors.As(err, &taken) {
		t.Fatalf("got %v, want EntityAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "created after GetRole answered NoSuchEntity") || strings.Contains(err.Error(), "cannot read") {
		t.Errorf("error %q does not name the create race, or claims a read was refused", err)
	}
	if len(fe.createCalls) != 0 {
		t.Error("bound the account to a role the pass did not hold")
	}
}

// TestDeleteEvalIdentity_TearsDownARecordedRoleOnceTheRuntimeIsUnconfigured:
// the identity was minted under an eval runtime that is no longer configured.
// The record still drives the teardown — the role with its grant into the
// eval-reports bucket, its association and its ServiceAccount all go — so a
// later Platform of the same name inherits none of them.
func TestDeleteEvalIdentity_TearsDownARecordedRoleOnceTheRuntimeIsUnconfigured(t *testing.T) {
	ctx := context.Background()
	r, f, fe, cl := evalIdentityReconciler(t, nil)
	p := storedPlatform(t, cl)
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	r.Eval.ReportsBucket = ""
	if r.evalIdentityWired() {
		t.Fatal("precondition: the reconciler still mints eval identities")
	}

	if err := r.deleteEvalIdentity(ctx, refetched(t, cl, p)); err != nil {
		t.Fatalf("deleteEvalIdentity: %v", err)
	}
	if _, ok := f.roles["production-cluster-analytics-eval"]; ok {
		t.Error("the recorded eval role survived its Platform")
	}
	if len(fe.associations) != 0 {
		t.Errorf("associations left behind: %v", fe.associations)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "eval-analytics"}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("eval ServiceAccount still present (err=%v)", err)
	}
}

// TestDeleteEvalIdentity_HoldsARecordedRoleWithNoClientToRemoveIt: a reconciler
// missing the IAM or the EKS client cannot remove what the record names, so it
// holds the finalizer and says so rather than releasing it over the role.
func TestDeleteEvalIdentity_HoldsARecordedRoleWithNoClientToRemoveIt(t *testing.T) {
	for _, missing := range []string{"IAM", "EKS"} {
		t.Run(missing, func(t *testing.T) {
			r, f, _, _ := evalIdentityReconciler(t, nil)
			if missing == "IAM" {
				r.IAM = nil
			} else {
				r.EKS = nil
			}
			p := newPlatform("analytics", "acme")
			p.Status.EvalRoleArn = evalTestRoleARN
			f.seedRole("production-cluster-analytics-eval", evalTestRoleARN)

			err := r.deleteEvalIdentity(context.Background(), p)
			if err == nil || !strings.Contains(err.Error(), "status.evalRoleArn records eval role "+evalTestRoleARN) ||
				!strings.Contains(err.Error(), "the finalizer holds") {
				t.Fatalf("deleteEvalIdentity = %v, want the finalizer held on the recorded role", err)
			}
			if _, ok := f.roles["production-cluster-analytics-eval"]; !ok {
				t.Error("the recorded role was removed by a reconciler reporting it could not")
			}
		})
	}
}

// evalSchedule is a CronWorkflow the EvalSuite reconciler would have made for
// platform in namespace, suspended or not.
func evalSchedule(name, namespace, platform string, suspended bool) *unstructured.Unstructured {
	cw := &unstructured.Unstructured{}
	cw.SetGroupVersionKind(argoWorkflowsGV.WithKind("CronWorkflow"))
	cw.SetName(name)
	cw.SetNamespace(namespace)
	cw.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "eks-agent-platform", LabelPlatform: platform})
	spec := map[string]any{"schedule": "0 3 * * *"}
	if suspended {
		spec["suspend"] = true
	}
	_ = unstructured.SetNestedField(cw.Object, spec, "spec")
	return cw
}

// suspended reports a stored CronWorkflow's spec.suspend.
func suspended(t *testing.T, cl client.Client, namespace, name string) bool {
	t.Helper()
	cw := &unstructured.Unstructured{}
	cw.SetGroupVersionKind(argoWorkflowsGV.WithKind("CronWorkflow"))
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, cw); err != nil {
		t.Fatalf("get CronWorkflow %s/%s: %v", namespace, name, err)
	}
	v, _, _ := unstructured.NestedBool(cw.Object, "spec", "suspend")
	return v
}

// TestDeleteEvalIdentity_SuspendsThePlatformsSchedulesFirst: a schedule left
// running fires a run under an account the finalizer is about to delete, and
// that run creates no pod — not even the exit handler that would report it. So
// the Platform's CronWorkflows in the eval runtime namespace are suspended
// before the ServiceAccount goes, and no other Platform's are touched.
func TestDeleteEvalIdentity_SuspendsThePlatformsSchedulesFirst(t *testing.T) {
	ctx := context.Background()
	var suspendedAtDelete []bool
	r, _, _, cl := evalIdentityReconciler(t, &interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.ServiceAccount); ok {
				suspendedAtDelete = append(suspendedAtDelete, suspended(t, c, evalTestNamespace, "acme-nightly"))
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	p := storedPlatform(t, cl)
	if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, cw := range []*unstructured.Unstructured{
		evalSchedule("acme-nightly", evalTestNamespace, "analytics", false),
		evalSchedule("acme-hourly", evalTestNamespace, "analytics", true),
		evalSchedule("acme-other", evalTestNamespace, "analytics-two", false),
		evalSchedule("acme-elsewhere", "somewhere-else", "analytics", false),
	} {
		if err := cl.Create(ctx, cw); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.deleteEvalIdentity(ctx, refetched(t, cl, p)); err != nil {
		t.Fatalf("deleteEvalIdentity: %v", err)
	}
	if len(suspendedAtDelete) != 1 || !suspendedAtDelete[0] {
		t.Errorf("schedule suspended when the ServiceAccount was deleted: %v, want [true]", suspendedAtDelete)
	}
	for _, c := range []struct {
		namespace, name string
		want            bool
	}{
		{evalTestNamespace, "acme-nightly", true},
		{evalTestNamespace, "acme-hourly", true},
		{evalTestNamespace, "acme-other", false},
		{"somewhere-else", "acme-elsewhere", false},
	} {
		if got := suspended(t, cl, c.namespace, c.name); got != c.want {
			t.Errorf("%s/%s suspended = %v, want %v", c.namespace, c.name, got, c.want)
		}
	}
}

// TestDeleteEvalIdentity_ScheduleErrors: a cluster without the CronWorkflow kind
// has no schedule to suspend and the teardown goes on; any other failure to
// list or suspend one holds the finalizer before the account is deleted.
func TestDeleteEvalIdentity_ScheduleErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	isSchedule := func(obj client.ObjectList) bool {
		u, ok := obj.(*unstructured.UnstructuredList)
		return ok && u.GetKind() == "CronWorkflowList"
	}
	cases := []struct {
		name  string
		funcs *interceptor.Funcs
		want  string
	}{
		{
			name: "kind not installed",
			funcs: &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if isSchedule(list) {
					return &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "argoproj.io", Kind: "CronWorkflow"}}
				}
				return c.List(ctx, list, opts...)
			}},
		},
		{
			name: "list",
			funcs: &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if isSchedule(list) {
					return boom
				}
				return c.List(ctx, list, opts...)
			}},
			want: "list eval schedules for Platform analytics in evals",
		},
		{
			name: "suspend",
			funcs: &interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return boom
			}},
			want: "suspend argo CronWorkflow evals/acme-nightly",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _, _, cl := evalIdentityReconciler(t, c.funcs)
			p := storedPlatform(t, cl)
			if _, err := r.ensureEvalIdentity(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := cl.Create(ctx, evalSchedule("acme-nightly", evalTestNamespace, "analytics", false)); err != nil {
				t.Fatal(err)
			}
			err := r.deleteEvalIdentity(ctx, refetched(t, cl, p))
			saErr := cl.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "eval-analytics"}, &corev1.ServiceAccount{})
			if c.want == "" {
				if err != nil || !apierrors.IsNotFound(saErr) {
					t.Errorf("deleteEvalIdentity = %v (ServiceAccount err=%v), want the teardown completed", err, saErr)
				}
				return
			}
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("deleteEvalIdentity = %v, want an error wrapping boom that names %q", err, c.want)
			}
			if saErr != nil {
				t.Errorf("the ServiceAccount was touched before its schedules were suspended (err=%v)", saErr)
			}
		})
	}
}
