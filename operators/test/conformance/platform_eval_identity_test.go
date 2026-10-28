/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

// evalRuntimeNs is the eval runtime namespace these tests hand the reconciler,
// and evalBucket the eval-reports bucket. The policy name restates the
// operator's so a rename upstream breaks these tests loudly.
const (
	condEvalIdentityReady = "EvalIdentityReady"
	evalRuntimeNs         = "eval-conformance"
	evalBucket            = "conformance-111111111111-us-west-2-eval-reports"
	evalPolicyName        = "eval-reports-access"
	evalRoleARNPrefix     = "arn:aws:iam::123456789012:role/eks-agent-platform/tenants/"
)

// memEKS is an in-memory awsclients.EKS holding Pod Identity associations by
// (namespace, serviceAccount), the key ListPodIdentityAssociations filters on.
// The list summary carries no role ARN, so the bound role is kept beside it and
// returned by DescribePodIdentityAssociation.
type memEKS struct {
	mu     sync.Mutex
	assocs map[string]ekstypes.PodIdentityAssociationSummary
	roles  map[string]string
	next   int
}

func newMemEKS() *memEKS {
	return &memEKS{assocs: map[string]ekstypes.PodIdentityAssociationSummary{}, roles: map[string]string{}}
}

func (m *memEKS) ListPodIdentityAssociations(_ context.Context, p *eks.ListPodIdentityAssociationsInput, _ ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &eks.ListPodIdentityAssociationsOutput{}
	if a, ok := m.assocs[aws.ToString(p.Namespace)+"/"+aws.ToString(p.ServiceAccount)]; ok {
		out.Associations = append(out.Associations, a)
	}
	return out, nil
}

func (m *memEKS) CreatePodIdentityAssociation(_ context.Context, p *eks.CreatePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	key := aws.ToString(p.Namespace) + "/" + aws.ToString(p.ServiceAccount)
	m.assocs[key] = ekstypes.PodIdentityAssociationSummary{
		AssociationId:  aws.String(fmt.Sprintf("a-%d", m.next)),
		ClusterName:    p.ClusterName,
		Namespace:      p.Namespace,
		ServiceAccount: p.ServiceAccount,
	}
	m.roles[key] = aws.ToString(p.RoleArn)
	return &eks.CreatePodIdentityAssociationOutput{}, nil
}

func (m *memEKS) DeletePodIdentityAssociation(_ context.Context, p *eks.DeletePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DeletePodIdentityAssociationOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, a := range m.assocs {
		if aws.ToString(a.AssociationId) == aws.ToString(p.AssociationId) {
			delete(m.assocs, k)
			delete(m.roles, k)
		}
	}
	return &eks.DeletePodIdentityAssociationOutput{}, nil
}

func (m *memEKS) DescribePodIdentityAssociation(_ context.Context, p *eks.DescribePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, a := range m.assocs {
		if aws.ToString(a.AssociationId) == aws.ToString(p.AssociationId) {
			return &eks.DescribePodIdentityAssociationOutput{Association: &ekstypes.PodIdentityAssociation{
				AssociationId:  a.AssociationId,
				Namespace:      a.Namespace,
				ServiceAccount: a.ServiceAccount,
				RoleArn:        aws.String(m.roles[k]),
			}}, nil
		}
	}
	return nil, &ekstypes.ResourceNotFoundException{Message: aws.String("no association " + aws.ToString(p.AssociationId))}
}

// rebind points the (namespace, serviceAccount) association at another role,
// the way an edit made outside the operator would.
func (m *memEKS) rebind(namespace, serviceAccount, roleARN string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roles[namespace+"/"+serviceAccount] = roleARN
}

// roleFor returns the role a (namespace, serviceAccount) is bound to.
func (m *memEKS) roleFor(namespace, serviceAccount string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	role, ok := m.roles[namespace+"/"+serviceAccount]
	return role, ok
}

var _ awsclients.EKS = (*memEKS)(nil)

func (m *memIAM) inlinePolicy(role, name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.inline[role][name]
	return doc, ok
}

func (m *memIAM) setInlinePolicy(role, name, doc string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inline[role][name] = doc
}

func (m *memIAM) hasRole(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.roles[name]
	return ok
}

func (m *memIAM) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

// newEvalIdentityReconciler is the scoping reconciler with an EKS client and
// an eval runtime wired, so a reconcile mints the eval identity.
func newEvalIdentityReconciler(iamFake *memIAM, eksFake *memEKS, bucket string) *controller.PlatformReconciler {
	r := newScopingReconciler(iamFake)
	r.EKS = eksFake
	r.Eval = controller.EvalIdentityConfig{Namespace: evalRuntimeNs, ReportsBucket: bucket}
	return r
}

func ensureEvalRuntimeNs(ctx context.Context, t *testing.T) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: evalRuntimeNs}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create eval runtime namespace: %v", err)
	}
}

// evalPlatform creates a Platform with a short name derived from the test name
// and a discriminator, and registers cleanup of its tenant namespace and eval
// ServiceAccount.
func evalPlatform(ctx context.Context, t *testing.T, discriminator string) *platformv1alpha1.Platform {
	t.Helper()
	h := uint64(1469598103934665603)
	for _, c := range []byte(t.Name() + "/" + discriminator) {
		h ^= uint64(c)
		h *= 1099511628211
	}
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ev-%08x", h&0xffffffff), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "generic",
			Tenant:   "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: controller.EvalServiceAccountName(p), Namespace: evalRuntimeNs}}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}})
		_ = k8sClient.Delete(ctx, sa)
	})
	return p
}

// evalRoleFor restates the <cluster>-<platform>-eval contract. The names
// evalPlatform derives keep it under IAM's 64-char limit, so the
// hash-truncation branch is not restated.
func evalRoleFor(p *platformv1alpha1.Platform) string {
	return scopingEnv + "-" + p.Name + "-eval"
}

type evalStatement struct {
	Sid       string                       `json:"Sid"`
	Effect    string                       `json:"Effect"`
	Action    []string                     `json:"Action"`
	Resource  []string                     `json:"Resource"`
	Condition map[string]map[string]string `json:"Condition,omitempty"`
}

func evalStatements(t *testing.T, doc string) []evalStatement {
	t.Helper()
	var parsed struct {
		Statement []evalStatement `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("unmarshal eval policy %q: %v", doc, err)
	}
	return parsed.Statement
}

// wantEvalStatements is the policy a Platform's eval role carries, written out
// in full: every resource the role reaches, and nothing else.
func wantEvalStatements(platform, bucket string) []evalStatement {
	bucketARN := "arn:aws:s3:::" + bucket
	return []evalStatement{
		{Sid: "ReadEvalManifests", Effect: "Allow", Action: []string{"s3:GetObject"},
			Resource: []string{bucketARN + "/" + platform + "/manifests/*"}},
		{Sid: "WriteEvalRuns", Effect: "Allow", Action: []string{"s3:PutObject"},
			Resource: []string{bucketARN + "/" + platform + "/runs/*"}},
		{Sid: "EvalReportsEncryption", Effect: "Allow", Action: []string{"kms:Decrypt", "kms:GenerateDataKey"},
			Resource: []string{"arn:aws:kms:us-west-2:123456789012:key/*"},
			Condition: map[string]map[string]string{
				"StringEquals": {"kms:EncryptionContext:aws:s3:arn": bucketARN},
				"StringLike":   {"kms:ViaService": "s3.us-west-2.amazonaws.com"},
			}},
	}
}

// assertEvalIdentityCondition checks the Platform's EvalIdentityReady
// condition as the API server holds it.
func assertEvalIdentityCondition(ctx context.Context, t *testing.T, p *platformv1alpha1.Platform, status metav1.ConditionStatus, reason, inMessage string) {
	t.Helper()
	got := getPlatform(ctx, t, p.Namespace, p.Name)
	c := apimeta.FindStatusCondition(got.Status.Conditions, condEvalIdentityReady)
	if c == nil {
		t.Fatalf("%s: no %s condition; conditions %+v", p.Name, condEvalIdentityReady, got.Status.Conditions)
	}
	if c.Status != status || c.Reason != reason || !strings.Contains(c.Message, inMessage) {
		t.Errorf("%s: %s = %s (%s) %q, want %s (%s) mentioning %q", p.Name, condEvalIdentityReady, c.Status, c.Reason, c.Message, status, reason, inMessage)
	}
}

func assertEvalPolicy(t *testing.T, iamFake *memIAM, p *platformv1alpha1.Platform, bucket string) {
	t.Helper()
	doc, ok := iamFake.inlinePolicy(evalRoleFor(p), evalPolicyName)
	if !ok {
		t.Fatalf("no %s policy on %s", evalPolicyName, evalRoleFor(p))
	}
	if got, want := evalStatements(t, doc), wantEvalStatements(p.Name, bucket); !reflect.DeepEqual(got, want) {
		t.Errorf("%s on %s:\n got  %+v\n want %+v", evalPolicyName, evalRoleFor(p), got, want)
	}
}

func TestPlatformReconciler_MintsAPerPlatformEvalIdentity(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	r := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)
	a, b := evalPlatform(ctx, t, "a"), evalPlatform(ctx, t, "b")
	reconcileIAM(ctx, t, r, a)
	reconcileIAM(ctx, t, r, b)

	for _, p := range []*platformv1alpha1.Platform{a, b} {
		saName := controller.EvalServiceAccountName(p)
		var sa corev1.ServiceAccount
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: evalRuntimeNs, Name: saName}, &sa); err != nil {
			t.Fatalf("%s: eval ServiceAccount %s/%s: %v", p.Name, evalRuntimeNs, saName, err)
		}
		if sa.Labels[controller.LabelPlatform] != p.Name {
			t.Errorf("%s: ServiceAccount labels %v do not name the Platform", p.Name, sa.Labels)
		}
		if !iamFake.hasRole(evalRoleFor(p)) {
			t.Fatalf("%s: no role %s", p.Name, evalRoleFor(p))
		}
		assertEvalPolicy(t, iamFake, p, evalBucket)
		role, ok := eksFake.roleFor(evalRuntimeNs, saName)
		if !ok || role != evalRoleARNPrefix+evalRoleFor(p) {
			t.Errorf("%s: %s/%s is bound to %q (bound=%v), want its own eval role", p.Name, evalRuntimeNs, saName, role, ok)
		}
		assertEvalIdentityCondition(ctx, t, p, metav1.ConditionTrue, "Bound", evalRoleFor(p))
		if got := getPlatform(ctx, t, p.Namespace, p.Name).Status.EvalRoleArn; got != evalRoleARNPrefix+evalRoleFor(p) {
			t.Errorf("%s: status.evalRoleArn = %q, want %s", p.Name, got, evalRoleARNPrefix+evalRoleFor(p))
		}
	}

	// Neither Platform's grant names the other's prefix.
	docA, _ := iamFake.inlinePolicy(evalRoleFor(a), evalPolicyName)
	if strings.Contains(docA, "/"+b.Name+"/") {
		t.Errorf("%s's eval policy reaches %s's prefix: %s", a.Name, b.Name, docA)
	}
	if controller.EvalServiceAccountName(a) == controller.EvalServiceAccountName(b) {
		t.Error("two Platforms share one eval ServiceAccount")
	}
}

func TestPlatformReconciler_EvalIdentityConvergesOnUpdate(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	p := evalPlatform(ctx, t, "p")
	reconcileIAM(ctx, t, newEvalIdentityReconciler(iamFake, eksFake, evalBucket), p)

	// A converged pass writes nothing.
	puts := iamFake.putCount()
	reconcileIAM(ctx, t, newEvalIdentityReconciler(iamFake, eksFake, evalBucket), p)
	if got := iamFake.putCount(); got != puts {
		t.Errorf("a converged reconcile made %d PutRolePolicy calls", got-puts)
	}

	// A policy widened out of band is narrowed back.
	iamFake.setInlinePolicy(evalRoleFor(p), evalPolicyName,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["*"]}]}`)
	reconcileIAM(ctx, t, newEvalIdentityReconciler(iamFake, eksFake, evalBucket), p)
	assertEvalPolicy(t, iamFake, p, evalBucket)

	// A moved bucket moves the grant with it; the old bucket is no longer named.
	const moved = "conformance-111111111111-us-west-2-eval-reports-2"
	reconcileIAM(ctx, t, newEvalIdentityReconciler(iamFake, eksFake, moved), p)
	assertEvalPolicy(t, iamFake, p, moved)
}

func TestPlatformReconciler_EvalIdentityRemovedOnDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	r := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)
	p := evalPlatform(ctx, t, "p")
	reconcileIAM(ctx, t, r, p)
	saName := controller.EvalServiceAccountName(p)
	if _, ok := eksFake.roleFor(evalRuntimeNs, saName); !ok {
		t.Fatal("precondition: the eval identity was never bound")
	}

	if err := k8sClient.Delete(ctx, p); err != nil {
		t.Fatalf("delete platform: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}

	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: evalRuntimeNs, Name: saName}, &corev1.ServiceAccount{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("eval ServiceAccount survived the Platform (err=%v)", err)
	}
	if _, ok := eksFake.roleFor(evalRuntimeNs, saName); ok {
		t.Error("the Pod Identity association survived the Platform")
	}
	if iamFake.hasRole(evalRoleFor(p)) {
		t.Errorf("role %s survived the Platform", evalRoleFor(p))
	}
	if iamFake.hasRole(tenantRoleFor(p)) {
		t.Errorf("tenant role %s survived the Platform", tenantRoleFor(p))
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: p.Namespace}, &platformv1alpha1.Platform{}); !apierrors.IsNotFound(err) {
		t.Errorf("the finalizer was not released (err=%v)", err)
	}
}

// TestPlatformReconciler_EvalIdentityHoldsWhileSuspended: the kill-switch pulls
// the tenant role's model access. The eval role holds none, so a suspended
// Platform keeps an eval identity that can still report what its runs measured.
func TestPlatformReconciler_EvalIdentityHoldsWhileSuspended(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	p := evalPlatform(ctx, t, "p")
	iamFake.seedRole(tenantRoleFor(p),
		iamtypes.Tag{Key: aws.String("platform.nanohype.dev/suspended"), Value: aws.String("true")},
		iamtypes.Tag{Key: aws.String("platform.nanohype.dev/suspended-reason"), Value: aws.String("budget-exceeded")},
	)
	reconcileIAM(ctx, t, newEvalIdentityReconciler(iamFake, eksFake, evalBucket), p)

	if got := getPlatform(ctx, t, p.Namespace, p.Name); got.Status.Phase != phaseSuspended {
		t.Fatalf("precondition: phase %q, want Suspended", got.Status.Phase)
	}
	if _, ok := iamFake.inlinePolicy(tenantRoleFor(p), scopingPolicyName); ok {
		t.Error("precondition: the suspended tenant role was written to")
	}
	assertEvalPolicy(t, iamFake, p, evalBucket)
	if _, ok := eksFake.roleFor(evalRuntimeNs, controller.EvalServiceAccountName(p)); !ok {
		t.Error("a suspended Platform's eval identity was not bound")
	}
}

// TestPlatformReconciler_EvalIdentityWaitsForTheRuntimeNamespace: a cluster
// whose eval runtime namespace is absent still brings its Platforms to Ready —
// the eval identity is deferred, and no role is minted for an account that
// cannot exist yet. Once the namespace arrives, the next reconcile mints it.
func TestPlatformReconciler_EvalIdentityWaitsForTheRuntimeNamespace(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	p := evalPlatform(ctx, t, "p")
	absentNs := "eval-late-" + strings.TrimPrefix(p.Name, "ev-")
	r := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)
	r.Eval.Namespace = absentNs
	saName := controller.EvalServiceAccountName(p)
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: absentNs}})
	})

	reconcileIAM(ctx, t, r, p)
	if got := getPlatform(ctx, t, p.Namespace, p.Name); got.Status.Phase != phaseReady {
		t.Fatalf("phase %q with no eval runtime namespace, want Ready", got.Status.Phase)
	}
	if iamFake.hasRole(evalRoleFor(p)) {
		t.Errorf("role %s minted with no namespace for its ServiceAccount", evalRoleFor(p))
	}
	if _, ok := eksFake.roleFor(absentNs, saName); ok {
		t.Error("an association was created for a ServiceAccount that cannot exist")
	}
	assertEvalIdentityCondition(ctx, t, p, metav1.ConditionFalse, "RuntimeNamespaceAbsent", absentNs)

	mustCreate(ctx, t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: absentNs}})
	reconcileIAM(ctx, t, r, p)
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: absentNs, Name: saName}, &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("eval ServiceAccount after the namespace arrived: %v", err)
	}
	assertEvalPolicy(t, iamFake, p, evalBucket)
	if role, ok := eksFake.roleFor(absentNs, saName); !ok || role != evalRoleARNPrefix+evalRoleFor(p) {
		t.Errorf("%s/%s is bound to %q (bound=%v), want its eval role", absentNs, saName, role, ok)
	}
	assertEvalIdentityCondition(ctx, t, p, metav1.ConditionTrue, "Bound", saName)
}

// evalCreateDenied fails CreateRole for every eval role, the way IAM refuses an
// operator role that holds no create grant for it.
func evalCreateDenied(name string) error {
	if strings.HasSuffix(name, "-eval") {
		return &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform: iam:CreateRole on resource: role " + name}
	}
	return nil
}

// TestPlatformReconciler_EvalIdentityFailureLeavesThePlatformReady: a Platform's
// readiness does not wait on its evals. An eval identity that cannot be minted
// is reported on EvalIdentityReady, while the reconcile still succeeds, writes
// the Platform's status, and requeues on its usual interval. The next pass that
// can mint the identity does.
func TestPlatformReconciler_EvalIdentityFailureLeavesThePlatformReady(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	iamFake.createErr = evalCreateDenied
	r := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)
	p := evalPlatform(ctx, t, "p")
	reconcileIAM(ctx, t, r, p)

	got := getPlatform(ctx, t, p.Namespace, p.Name)
	if got.Status.Phase != phaseReady {
		t.Fatalf("phase %q with an eval identity that cannot be minted, want Ready", got.Status.Phase)
	}
	if apimeta.FindStatusCondition(got.Status.Conditions, condModelAccessScoped) == nil {
		t.Error("the Platform's own conditions were not written")
	}
	if !iamFake.hasRole(tenantRoleFor(p)) {
		t.Errorf("tenant role %s not minted", tenantRoleFor(p))
	}
	assertEvalIdentityCondition(ctx, t, p, metav1.ConditionFalse, "ReconcileFailed", "AccessDenied")
	if _, ok := eksFake.roleFor(evalRuntimeNs, controller.EvalServiceAccountName(p)); ok {
		t.Error("an association was created for a role that was never minted")
	}

	iamFake.createErr = nil
	reconcileIAM(ctx, t, r, p)
	assertEvalIdentityCondition(ctx, t, p, metav1.ConditionTrue, "Bound", evalRoleFor(p))
	assertEvalPolicy(t, iamFake, p, evalBucket)
}

// TestPlatformReconciler_EvalIdentityFailureKeepsSuspensionReported: the
// kill-switch's state reaches the Platform's status whatever its eval identity
// does. A pass that returned early on an eval failure would leave a suspended
// Platform reading as whatever it read before.
func TestPlatformReconciler_EvalIdentityFailureKeepsSuspensionReported(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	iamFake.createErr = evalCreateDenied
	p := evalPlatform(ctx, t, "p")
	iamFake.seedRole(tenantRoleFor(p),
		iamtypes.Tag{Key: aws.String("platform.nanohype.dev/suspended"), Value: aws.String("true")},
		iamtypes.Tag{Key: aws.String("platform.nanohype.dev/suspended-reason"), Value: aws.String("budget-exceeded")},
	)
	reconcileIAM(ctx, t, newEvalIdentityReconciler(iamFake, eksFake, evalBucket), p)

	got := getPlatform(ctx, t, p.Namespace, p.Name)
	if got.Status.Phase != phaseSuspended {
		t.Fatalf("phase %q, want Suspended", got.Status.Phase)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Suspended"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Suspended condition %+v, want True", c)
	}
	assertEvalIdentityCondition(ctx, t, p, metav1.ConditionFalse, "ReconcileFailed", "AccessDenied")
}

// TestPlatformReconciler_EvalIdentityNeedsNoRootPathGrant runs the operator
// role as a tenant-path grant leaves it for the eval role: every call naming an
// eval role that does not exist is refused, as IAM authorizes it against the
// root-path name. The identity is still minted and torn down, and a Platform
// deleted before its eval role ever existed still releases its finalizer.
func TestPlatformReconciler_EvalIdentityNeedsNoRootPathGrant(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	iamFake.deniedAbsentSuffix = "-eval"
	r := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)

	minted := evalPlatform(ctx, t, "minted")
	reconcileIAM(ctx, t, r, minted)
	assertEvalIdentityCondition(ctx, t, minted, metav1.ConditionTrue, "Bound", evalRoleFor(minted))
	assertEvalPolicy(t, iamFake, minted, evalBucket)

	// Deferred on a namespace that never arrives, so no eval role is minted.
	deferred := evalPlatform(ctx, t, "deferred")
	late := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)
	late.Eval.Namespace = "eval-never-" + strings.TrimPrefix(deferred.Name, "ev-")
	reconcileIAM(ctx, t, late, deferred)
	if iamFake.hasRole(evalRoleFor(deferred)) {
		t.Fatalf("precondition: role %s was minted", evalRoleFor(deferred))
	}

	for _, c := range []struct {
		p *platformv1alpha1.Platform
		r *controller.PlatformReconciler
	}{{minted, r}, {deferred, late}} {
		if err := k8sClient.Delete(ctx, c.p); err != nil {
			t.Fatalf("delete %s: %v", c.p.Name, err)
		}
		if _, err := c.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: c.p.Name, Namespace: c.p.Namespace}}); err != nil {
			t.Fatalf("%s: finalizer reconcile: %v", c.p.Name, err)
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: c.p.Name, Namespace: c.p.Namespace}, &platformv1alpha1.Platform{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s: the finalizer was not released (err=%v)", c.p.Name, err)
		}
	}
	if iamFake.hasRole(evalRoleFor(minted)) {
		t.Errorf("role %s survived its Platform", evalRoleFor(minted))
	}
}

// TestPlatformReconciler_EvalRoleOutOfReachHoldsTheFinalizer runs the operator
// role with no grant on the eval role's root-path name, and then without its
// read of the minted role either. A Platform whose recorded eval role the
// operator can no longer read keeps its finalizer — the role and its grant into
// the eval-reports bucket stay owned by a Platform that still exists — and
// releases it once the role can be read and deleted.
func TestPlatformReconciler_EvalRoleOutOfReachHoldsTheFinalizer(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	iamFake.deniedAbsentSuffix = "-eval"
	r := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)
	p := evalPlatform(ctx, t, "p")
	reconcileIAM(ctx, t, r, p)
	role := evalRoleFor(p)
	if got := getPlatform(ctx, t, p.Namespace, p.Name).Status.EvalRoleArn; got != evalRoleARNPrefix+role {
		t.Fatalf("precondition: status.evalRoleArn = %q, want %s", got, evalRoleARNPrefix+role)
	}

	iamFake.mu.Lock()
	iamFake.refusedReads = map[string]bool{role: true}
	iamFake.mu.Unlock()
	if err := k8sClient.Delete(ctx, p); err != nil {
		t.Fatalf("delete platform: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}}
	if _, err := r.Reconcile(ctx, req); err == nil || !strings.Contains(err.Error(), "status.evalRoleArn records") {
		t.Fatalf("finalizer reconcile = %v, want it held on the recorded role", err)
	}
	if !iamFake.hasRole(role) {
		t.Fatalf("role %s was lost while the operator could not read it", role)
	}
	if got := getPlatform(ctx, t, p.Namespace, p.Name); len(got.Finalizers) == 0 {
		t.Fatal("the finalizer was released over a role the operator could not read")
	}

	iamFake.mu.Lock()
	iamFake.refusedReads = nil
	iamFake.mu.Unlock()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer reconcile once the role is readable: %v", err)
	}
	if iamFake.hasRole(role) {
		t.Errorf("role %s survived its Platform", role)
	}
	if err := k8sClient.Get(ctx, req.NamespacedName, &platformv1alpha1.Platform{}); !apierrors.IsNotFound(err) {
		t.Errorf("the finalizer was not released (err=%v)", err)
	}
}

// TestPlatformReconciler_EvalIdentityRebindsADriftedAssociation: an association
// for the eval account pointed at another role out of band would hand runs
// credentials the operator did not scope. The next reconcile binds the account
// back to the Platform's eval role, and the condition names that role.
func TestPlatformReconciler_EvalIdentityRebindsADriftedAssociation(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	ensureEvalRuntimeNs(ctx, t)

	iamFake, eksFake := newMemIAM(), newMemEKS()
	r := newEvalIdentityReconciler(iamFake, eksFake, evalBucket)
	p := evalPlatform(ctx, t, "p")
	reconcileIAM(ctx, t, r, p)
	saName := controller.EvalServiceAccountName(p)

	eksFake.rebind(evalRuntimeNs, saName, "arn:aws:iam::123456789012:role/someone-else")
	reconcileIAM(ctx, t, r, p)
	if role, _ := eksFake.roleFor(evalRuntimeNs, saName); role != evalRoleARNPrefix+evalRoleFor(p) {
		t.Errorf("%s/%s is bound to %q, want %s", evalRuntimeNs, saName, role, evalRoleARNPrefix+evalRoleFor(p))
	}
	assertEvalIdentityCondition(ctx, t, p, metav1.ConditionTrue, "Bound", evalRoleARNPrefix+evalRoleFor(p))
}
