/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func testScope() arnScope {
	return arnScope{Partition: "aws", Region: "us-west-2", AccountID: "123456789012"}
}

func platformWithDatastores(name string, ds ...platformv1alpha1.DatastoreSpec) *platformv1alpha1.Platform { //nolint:unparam // policy-generation unit tests use a fixed platform token
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       platformv1alpha1.PlatformSpec{Datastores: ds},
	}
}

func findStmt(stmts []policyStatement, sid string) *policyStatement {
	for i := range stmts {
		if stmts[i].Sid == sid {
			return &stmts[i]
		}
	}
	return nil
}

// The shape RDS actually produces: rds!cluster-<cluster-resource-id>-<suffix>,
// every segment AWS-generated. Nothing from the tenant naming convention appears
// in it, which is why the ARN has to be published rather than composed.
const testDBSecretARN = "arn:aws:secretsmanager:us-west-2:123456789012:secret:rds!cluster-4f9c2b1a-7de3-4c11-9a02-1f7b6c5d8e90-Ab3xYz"

func hasResource(s *policyStatement, want string) bool {
	for _, r := range s.Resource {
		if r == want {
			return true
		}
	}
	return false
}

// TestDatastoreActionsAreDataPlaneOnly pins the verbs a tenant gets on its own
// datastores. Nothing referenced any of these six lists — the Resource scoping
// is covered elsewhere and is what keeps a tenant inside its own stores, but
// what it may *do* there was unobserved.
//
// The intent is that every action is data-plane: a tenant reads and writes its
// own data and does not create, delete or reconfigure the store itself. Control-
// plane verbs are the ones that escape the Resource scoping's purpose rather
// than its letter — DeleteTable destroys the store the scoping was protecting,
// PutBucketPolicy and sqs:AddPermission rewrite the rule that does the
// protecting, and SetQueueAttributes can repoint a queue's redrive at another
// tenant's.
//
// Two mechanisms, and they are not equals. The literal sets are the check that
// converges: DeepEqual fails on any addition and any removal. The scan below is
// a tripwire for the case the sets cannot see, and it cannot express the intent
// above — see the comment where it is defined.
func TestDatastoreActionsAreDataPlaneOnly(t *testing.T) {
	lists := map[string][]string{
		"dynamo":   datastoreDynamoActions,
		"s3object": datastoreS3ObjectActions,
		"s3bucket": datastoreS3BucketActions,
		"sqs":      datastoreSQSActions,
		"kafka":    datastoreKafkaActions,
		"secret":   datastoreSecretActions,
	}

	want := map[string][]string{
		"dynamo": {
			"dynamodb:GetItem", "dynamodb:BatchGetItem", "dynamodb:Query", "dynamodb:Scan",
			"dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem",
			"dynamodb:BatchWriteItem", "dynamodb:ConditionCheckItem", "dynamodb:DescribeTable",
		},
		"s3object": {"s3:GetObject", "s3:PutObject", "s3:DeleteObject"},
		"s3bucket": {"s3:ListBucket", "s3:GetBucketLocation"},
		"sqs": {
			"sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage",
			"sqs:GetQueueAttributes", "sqs:GetQueueUrl", "sqs:ChangeMessageVisibility",
		},
		"kafka": {
			"kafka-cluster:Connect", "kafka-cluster:DescribeCluster", "kafka-cluster:DescribeTopic",
			"kafka-cluster:CreateTopic", "kafka-cluster:WriteData", "kafka-cluster:ReadData",
			"kafka-cluster:DescribeGroup", "kafka-cluster:AlterGroup",
		},
		"secret": {"secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"},
	}

	for kind, got := range lists {
		if !reflect.DeepEqual(got, want[kind]) {
			t.Errorf("%s actions = %v, want %v", kind, got, want[kind])
		}
	}

	// The literal sets above are the converging check: DeepEqual fails on any
	// addition and any removal, so nothing gets past them on its own.
	//
	// What follows is a tripwire for the one case they cannot see — someone adds
	// a verb to the production list AND updates the `want` literal here to match,
	// which is the ordinary way a test gets "fixed". Then the sets agree and the
	// change is unreviewed. This fires on the shapes below regardless.
	//
	// It is a tripwire, not a proof. A denylist cannot express "every verb is
	// data-plane" — it expresses "these shapes are known-bad", and AWS adds verbs
	// faster than any such list converges. Anyone adding a verb here should read
	// it as a prompt to think, not as a clearance.
	//
	// Two rules rather than a longer list of names, because these two generalise:
	// anything touching Policy or Permission is access-control manipulation, and
	// anything that Creates or Deletes a store noun is provisioning.
	// kafka-cluster:CreateTopic is the deliberate exception — a topic is the
	// tenant's own namespace on a shared cluster, confined by the same prefix
	// scoping, closer to writing a key than to provisioning a store.
	accessControl := regexp.MustCompile(`Policy|Permission`)
	provisioning := regexp.MustCompile(`^(Create|Delete)(Table|Bucket|Queue|Secret|Cluster|Stream|DB)`)
	// Named shapes the two rules above do not cover. Not exhaustive by
	// construction — each entry is one that escaped, kept so it cannot escape
	// twice.
	knownBad := []string{
		"SetQueueAttributes", "UpdateTable", "UpdateSecret",
		"UpdateContinuousBackups", "UpdateTimeToLive",
		"TagResource", "UntagResource", "PutLifecycleConfiguration",
	}

	for kind, actions := range lists {
		for _, a := range actions {
			verb := a[strings.Index(a, ":")+1:]
			switch {
			case verb == "CreateTopic":
				// documented exception, see above
			case accessControl.MatchString(verb):
				t.Errorf("%s grants %q — it edits the store's own access policy, which rewrites the rule "+
					"the Resource scoping relies on rather than operating within it", kind, a)
			case provisioning.MatchString(verb):
				t.Errorf("%s grants %q — it creates or destroys the store the Resource scoping exists to "+
					"protect, rather than using it", kind, a)
			default:
				for _, bad := range knownBad {
					if verb == bad {
						t.Errorf("%s grants %q — it reconfigures the store rather than using it", kind, a)
					}
				}
			}
		}
	}

	if !strings.Contains(strings.Join(datastoreS3ObjectActions, ","), "s3:GetObject") {
		t.Error("the s3 object grant must include GetObject or a tenant cannot read its own bucket")
	}
}

// TestDatastorePolicy_ScopesEachKind proves every datastore kind is granted its
// minimal actions scoped to the tenant-substrate naming convention
// (<env>-<platform>-<datastore>, S3 account-qualified), cache contributes no
// statement, and no Resource is a bare wildcard.
func TestDatastorePolicy_ScopesEachKind(t *testing.T) {
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "kv", Kind: platformv1alpha1.DatastoreKeyValue},
		platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore},
		platformv1alpha1.DatastoreSpec{Name: "q", Kind: platformv1alpha1.DatastoreQueue},
		platformv1alpha1.DatastoreSpec{Name: "ca", Kind: platformv1alpha1.DatastoreCache},
		platformv1alpha1.DatastoreSpec{Name: "st", Kind: platformv1alpha1.DatastoreStream},
	)
	stmts := datastorePolicyStatements(p, "development", testScope(),
		map[string]string{"db": testDBSecretARN})

	// S3: bucket + object statements, account-qualified name.
	if s := findStmt(stmts, "s3bucketobj"); s == nil || !hasResource(s, "arn:aws:s3:::development-myplat-obj-123456789012") {
		t.Errorf("s3 bucket statement missing or misscoped: %+v", s)
	}
	if s := findStmt(stmts, "s3objectobj"); s == nil || !hasResource(s, "arn:aws:s3:::development-myplat-obj-123456789012/*") {
		t.Errorf("s3 object statement missing or misscoped: %+v", s)
	}

	// DynamoDB: table + index.
	if s := findStmt(stmts, "dynamodbkv"); s == nil ||
		!hasResource(s, "arn:aws:dynamodb:us-west-2:123456789012:table/development-myplat-kv") ||
		!hasResource(s, "arn:aws:dynamodb:us-west-2:123456789012:table/development-myplat-kv/index/*") {
		t.Errorf("dynamodb statement missing or misscoped: %+v", s)
	}

	// SQS: the exact queue, enumerated. This datastore declares no redrive budget,
	// so it has one queue and no DLQ — and the grant must say so, because a prefix
	// that covered "the DLQ it might have" also covers a sibling Platform's queues.
	if s := findStmt(stmts, "sqsq"); s == nil ||
		!hasResource(s, "arn:aws:sqs:us-west-2:123456789012:development-myplat-q") ||
		len(s.Resource) != 1 {
		t.Errorf("sqs statement missing or misscoped: %+v", s)
	}

	// MSK: cluster/topic/group ARNs under the tenant's own cluster name.
	if s := findStmt(stmts, "mskst"); s == nil ||
		!hasResource(s, "arn:aws:kafka:us-west-2:123456789012:cluster/development-myplat-st/*") ||
		!hasResource(s, "arn:aws:kafka:us-west-2:123456789012:topic/development-myplat-st/*") ||
		!hasResource(s, "arn:aws:kafka:us-west-2:123456789012:group/development-myplat-st/*") {
		t.Errorf("msk statement missing or misscoped: %+v", s)
	}

	// relational: the grant names the one secret this tenant's own Aurora cluster
	// owns, resolved from SSM. It must be the literal published ARN — a pattern
	// here is a cross-tenant read, because every Aurora master secret in the
	// account shares the rds!cluster- prefix.
	if s := findStmt(stmts, "relationalSecrets"); s == nil || !hasResource(s, testDBSecretARN) {
		t.Errorf("relational secret statement missing or not scoped to the published ARN: %+v", s)
	}

	// cache contributes no statement — nothing named for "ca".
	for _, s := range stmts {
		if strings.Contains(s.Sid, "ca") && s.Sid != "relationalSecrets" {
			t.Errorf("cache datastore should produce no statement, found: %s", s.Sid)
		}
	}

	// no bare-wildcard Resource, and every statement is an Allow.
	for _, s := range stmts {
		if s.Effect != "Allow" {
			t.Errorf("statement %s must be Allow, got %s", s.Sid, s.Effect)
		}
		for _, r := range s.Resource {
			if r == "*" {
				t.Errorf("statement %s has a bare-wildcard Resource", s.Sid)
			}
		}
	}
}

// TestDatastorePolicy_EmptyWhenNoIAMDatastores proves a Platform with no
// datastores — or only a cache, which needs no IAM — produces no policy, so the
// reconciler removes the inline policy rather than writing an empty one.
func TestDatastorePolicy_EmptyWhenNoIAMDatastores(t *testing.T) {
	none := datastorePolicyStatements(platformWithDatastores("myplat"), "development", testScope(), nil)
	if len(none) != 0 {
		t.Errorf("no datastores must yield no statements, got %d", len(none))
	}
	doc, err := datastorePolicyDoc(none)
	if err != nil {
		t.Fatalf("datastorePolicyDoc: %v", err)
	}
	if doc != "" {
		t.Errorf("empty statements must yield an empty document, got %q", doc)
	}

	cacheOnly := datastorePolicyStatements(
		platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "ca", Kind: platformv1alpha1.DatastoreCache}),
		"development", testScope(), nil,
	)
	if len(cacheOnly) != 0 {
		t.Errorf("a cache-only Platform needs no IAM statement, got %d", len(cacheOnly))
	}
}

// TestDatastorePolicy_RelationalSecretDeduped proves multiple relational
// datastores share the single RDS-managed-secret grant rather than emitting a
// redundant statement each.
func TestDatastorePolicy_RelationalSecretDeduped(t *testing.T) {
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "a", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "b", Kind: platformv1alpha1.DatastoreRelational},
	)
	stmts := datastorePolicyStatements(p, "development", testScope(),
		map[string]string{"a": testDBSecretARN, "b": testDBSecretARN + "-b"})
	secretCount := 0
	for _, s := range stmts {
		if s.Sid == "relationalSecrets" {
			secretCount++
		}
	}
	if secretCount != 1 {
		t.Errorf("two relational datastores must share one secret grant, got %d", secretCount)
	}
}

// TestEnsureDatastorePolicy_WritesAndConverges proves the reconcile writes the
// inline datastore-access policy once and then no-ops on a converged re-run.
func TestEnsureDatastorePolicy_WritesAndConverges(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{Environment: "development", Region: "us-west-2"}
	p := platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore})

	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", p, cfg); err != nil {
		t.Fatalf("ensureDatastorePolicy: %v", err)
	}
	if len(f.putInlineCalls) != 1 {
		t.Fatalf("PutRolePolicy calls: got %d want 1", len(f.putInlineCalls))
	}
	if got := *f.putInlineCalls[0].PolicyName; got != datastorePolicyName {
		t.Errorf("policy name: got %q want %q", got, datastorePolicyName)
	}
	if doc := *f.putInlineCalls[0].PolicyDocument; !strings.Contains(doc, "development-myplat-obj-123456789012") {
		t.Errorf("policy document not scoped to the datastore's bucket: %s", doc)
	}

	// Converged re-run must not write again.
	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", p, cfg); err != nil {
		t.Fatalf("ensureDatastorePolicy re-run: %v", err)
	}
	if len(f.putInlineCalls) != 1 {
		t.Errorf("converged re-run must not re-write: got %d PutRolePolicy calls", len(f.putInlineCalls))
	}
}

// TestEnsureDatastorePolicy_RemovesWhenEmpty proves a Platform with no
// IAM-needing datastore has the inline policy removed, so a cleared declaration
// leaves no stale grant.
func TestEnsureDatastorePolicy_RemovesWhenEmpty(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{Environment: "development", Region: "us-west-2"}

	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", platformWithDatastores("myplat"), cfg); err != nil {
		t.Fatalf("ensureDatastorePolicy: %v", err)
	}
	if len(f.putInlineCalls) != 0 {
		t.Errorf("no datastores must write no policy: got %d PutRolePolicy calls", len(f.putInlineCalls))
	}
	if len(f.deleteInlineCalls) != 1 || *f.deleteInlineCalls[0].PolicyName != datastorePolicyName {
		t.Errorf("no datastores must delete the datastore-access policy: got %+v", f.deleteInlineCalls)
	}
}

// TestEnsureDatastorePolicy_NilIAM proves the reconcile no-ops when the operator
// has no IAM client (the guard the fake tests never exercise).
func TestEnsureDatastorePolicy_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if err := r.ensureDatastorePolicy(context.Background(), "role", "arn:aws:iam::123456789012:role/role",
		platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}),
		IAMConfig{}); err != nil {
		t.Fatalf("nil IAM must no-op: %v", err)
	}
}

// TestEnsureDatastorePolicy_DeleteErrorPropagates proves a non-NotFound
// DeleteRolePolicy failure on the removal path surfaces rather than being
// swallowed.
func TestEnsureDatastorePolicy_DeleteErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("role", "arn:aws:iam::123456789012:role/role")
	f.deleteInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	// No datastores -> the reconcile deletes the inline policy; the injected
	// error must propagate.
	if err := r.ensureDatastorePolicy(context.Background(), "role", "arn:aws:iam::123456789012:role/role",
		platformWithDatastores("myplat"), IAMConfig{Environment: "development", Region: "us-west-2"}); err == nil {
		t.Fatalf("expected the DeleteRolePolicy error to propagate")
	}
}

// TestEnsureDatastorePolicy_PutErrorPropagates proves a PutRolePolicy failure on
// the write path surfaces.
func TestEnsureDatastorePolicy_PutErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("role", "arn:aws:iam::123456789012:role/role")
	f.putInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureDatastorePolicy(context.Background(), "role", "arn:aws:iam::123456789012:role/role",
		platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}),
		IAMConfig{Environment: "development", Region: "us-west-2"}); err == nil {
		t.Fatalf("expected the PutRolePolicy error to propagate")
	}
}

func datastoreErrCfg() IAMConfig {
	return IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/baseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
		Region:                  "us-west-2",
	}
}

// TestEnsureIamRole_DatastorePolicyError_CreatePath proves ensureIamRole
// propagates a datastore-policy failure on the create path (the model-scoping
// reconcile still succeeds — only datastore-access errs).
func TestEnsureIamRole_DatastorePolicyError_CreatePath(t *testing.T) {
	f := newFakeIAM()
	f.getInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}}

	if _, err := r.ensureIamRole(context.Background(), p, datastoreErrCfg()); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the datastore-policy error on the create path")
	}
}

// TestEnsureIamRole_DatastorePolicyError_ExistingRolePath proves the same on the
// existing-role path (role already present, not suspended).
func TestEnsureIamRole_DatastorePolicyError_ExistingRolePath(t *testing.T) {
	f := newFakeIAM()
	cfg := datastoreErrCfg()
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}}

	roleName := tenantRoleName(cfg.ClusterName, p)
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.getInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}

	if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the datastore-policy error on the existing-role path")
	}
}

// TestDatastorePolicy_UnresolvedRelationalSecretGrantsNothing proves the
// fail-closed half of the scoping contract. A Platform can reconcile before its
// Aurora cluster exists, so the published ARN is legitimately absent for a while.
// The tempting fallback — grant on the rds!cluster-* prefix until the real ARN
// arrives — hands the tenant every other tenant's master credentials, so the
// correct behaviour is no grant at all.
func TestDatastorePolicy_UnresolvedRelationalSecretGrantsNothing(t *testing.T) {
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "kv", Kind: platformv1alpha1.DatastoreKeyValue},
	)

	stmts := datastorePolicyStatements(p, "development", testScope(), nil)

	if s := findStmt(stmts, "relationalSecrets"); s != nil {
		t.Errorf("an unresolved master-secret ARN must produce no secret grant, got %+v", s)
	}
	// The rest of the declaration is unaffected — one missing parameter does not
	// strip a tenant of the grants that do resolve.
	if s := findStmt(stmts, "dynamodbkv"); s == nil {
		t.Error("the other datastores' grants must still be emitted")
	}
}

// TestDatastorePolicy_NoResourceCrossesTenants is the regression guard for the
// class, not for one instance of it: a Resource whose wildcard does not terminate,
// so it also matches a sibling Platform's resources.
//
// It was written for a Secrets Manager grant on
// arn:aws:secretsmanager:<region>:<account>:secret:rds!cluster-*, which every Aurora
// cluster in the account matches — and it only ever looked at Secrets Manager, so it
// could not see the same defect on SQS, where `<env>-<platform>-<datastore>*` matched
// every queue of a Platform named `<platform>-<datastore>`.
//
// So it asserts over every resource of every statement. A "*" is only safe when the
// character before it is a delimiter that cannot appear inside the name it follows —
// "/" or "." — because that is what stops the pattern spilling into the next name.
// AWS resource names admit "-", so "-*" terminates nothing.
//
// The Platform pair is deliberate: `orders` with datastore `events` composes the same
// token as `orders-events` with datastore `q`, which is the collision itself and the
// house naming style.
func TestDatastorePolicy_NoResourceCrossesTenants(t *testing.T) {
	fifo := true
	victim := platformWithDatastores("orders",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore},
		platformv1alpha1.DatastoreSpec{Name: "kv", Kind: platformv1alpha1.DatastoreKeyValue},
		platformv1alpha1.DatastoreSpec{Name: "stream", Kind: platformv1alpha1.DatastoreStream},
		platformv1alpha1.DatastoreSpec{
			Name: "events", Kind: platformv1alpha1.DatastoreQueue,
			Queue: &platformv1alpha1.QueueConfig{MaxReceiveCount: 3},
		},
		platformv1alpha1.DatastoreSpec{
			Name: "fifoq", Kind: platformv1alpha1.DatastoreQueue,
			Queue: &platformv1alpha1.QueueConfig{FIFO: &fifo},
		},
	)

	stmts := datastorePolicyStatements(victim, "development", testScope(),
		map[string]string{"db": testDBSecretARN})

	for _, s := range stmts {
		for _, r := range s.Resource {
			for i, c := range r {
				if c != '*' {
					continue
				}
				if i == 0 {
					t.Errorf("statement %s grants on a bare wildcard: %s", s.Sid, r)
					continue
				}
				if prev := r[i-1]; prev != '/' && prev != '.' {
					t.Errorf("statement %s has a wildcard that does not terminate — %q precedes it, "+
						"so the pattern also matches a sibling Platform whose name extends this one: %s",
						s.Sid, string(prev), r)
				}
			}
			if strings.Contains(r, ":secretsmanager:") && !strings.Contains(r, testDBSecretARN) {
				t.Errorf("statement %s names a secret this tenant does not own: %s", s.Sid, r)
			}
		}
	}
}

// TestQueueGrantDoesNotReachASiblingPlatform is the collision stated as an outcome
// rather than as a shape: Platform `orders` with datastore `events` composes the same
// token as Platform `orders-events`, and the assertion is that neither one's grant
// reaches the other's queues. That is the property a reader of the grant is entitled
// to assume, and the wildcard silently gave it away.
//
// Residual, stated because the test does not cover it and a green run should not
// imply it does: the composed name `<env>-<platform>-<datastore>` is not injective,
// because "-" is both the separator and a legal character on both sides. Platform
// `orders` with datastore `events-x` and Platform `orders-events` with datastore `x`
// compose the same queue name, and no IAM scoping can separate two resources that
// ARE the same resource. That is a naming-authority question for the CRD and
// tenant-substrate, not something the policy generator can fix.
func TestQueueGrantDoesNotReachASiblingPlatform(t *testing.T) {
	orders := platformWithDatastores("orders",
		platformv1alpha1.DatastoreSpec{
			Name: "events", Kind: platformv1alpha1.DatastoreQueue,
			Queue: &platformv1alpha1.QueueConfig{MaxReceiveCount: 3},
		},
	)
	ordersEvents := platformWithDatastores("orders-events",
		platformv1alpha1.DatastoreSpec{
			Name: "q", Kind: platformv1alpha1.DatastoreQueue,
			Queue: &platformv1alpha1.QueueConfig{MaxReceiveCount: 3},
		},
	)

	granted := map[string]bool{}
	for _, s := range datastorePolicyStatements(orders, "development", testScope(), nil) {
		for _, r := range s.Resource {
			granted[r] = true
		}
	}

	for _, s := range datastorePolicyStatements(ordersEvents, "development", testScope(), nil) {
		for _, r := range s.Resource {
			if granted[r] {
				t.Errorf("Platform orders is granted %s, which belongs to Platform orders-events", r)
			}
			// Not merely a different string — orders must not MATCH it either.
			for g := range granted {
				if strings.HasSuffix(g, "*") && strings.HasPrefix(r, strings.TrimSuffix(g, "*")) {
					t.Errorf("Platform orders is granted pattern %s, which matches Platform orders-events resource %s", g, r)
				}
			}
		}
	}
}

// TestMasterSecretParamPath pins the contract with the tenant-substrate component,
// which publishes at this exact path. The two sides are in different repos, so a
// drift here is only visible as a tenant that silently loses its database grant.
func TestMasterSecretParamPath(t *testing.T) {
	got := masterSecretParamPath("development-platform", "myplat", "db")
	want := "/eks-agent-platform/development-platform/tenant-substrate/myplat/db/master_secret_arn"
	if got != want {
		t.Errorf("SSM path drifted from what tenant-substrate publishes:\n got %s\nwant %s", got, want)
	}
}

// stubSSM is an in-memory awsclients.SSM for the master-secret lookup. params
// maps a full parameter path to its value; a path that is absent produces the
// ParameterNotFound the real service returns, and err short-circuits everything.
type stubSSM struct {
	params map[string]string
	err    error
	calls  []string
	// nilParameter reproduces the shape the SDK permits but the service does not
	// normally return: a 200 with no Parameter body. Treated as unpublished
	// rather than dereferenced.
	nilParameter bool
}

func (s *stubSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	name := aws.ToString(in.Name)
	s.calls = append(s.calls, name)
	if s.err != nil {
		return nil, s.err
	}
	if s.nilParameter {
		return &ssm.GetParameterOutput{}, nil
	}
	v, ok := s.params[name]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	if v == "" {
		// A parameter that exists but carries no value — the substrate applied
		// before the cluster finished, or someone blanked it by hand.
		return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Name: in.Name}}, nil
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Name: in.Name, Value: aws.String(v)}}, nil
}

func (s *stubSSM) GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	panic("GetParametersByPath not used by the master-secret lookup")
}

// TestResolveRelationalSecretARNs covers every way the lookup can land. The
// distinction that matters throughout: "resolved" is the only state that yields a
// grant, and every other state — absent, blank, no client, no cluster name — must
// report unresolved rather than fall back to a broader pattern.
func TestResolveRelationalSecretARNs(t *testing.T) {
	const cluster = "development-platform"
	dbPath := masterSecretParamPath(cluster, "myplat", "db")

	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore},
	)

	t.Run("published parameter resolves", func(t *testing.T) {
		s := &stubSSM{params: map[string]string{dbPath: testDBSecretARN}}
		r := &PlatformReconciler{SSM: s}

		resolved, unresolved, err := r.resolveRelationalSecretARNs(context.Background(), p, cluster)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved["db"] != testDBSecretARN {
			t.Errorf("db not resolved: %v", resolved)
		}
		if len(unresolved) != 0 {
			t.Errorf("nothing should be unresolved, got %v", unresolved)
		}
		// Non-relational kinds are never looked up — one call, not two.
		if len(s.calls) != 1 || s.calls[0] != dbPath {
			t.Errorf("expected exactly one lookup at %s, got %v", dbPath, s.calls)
		}
	})

	t.Run("absent parameter is unresolved, not an error", func(t *testing.T) {
		r := &PlatformReconciler{SSM: &stubSSM{params: map[string]string{}}}

		resolved, unresolved, err := r.resolveRelationalSecretARNs(context.Background(), p, cluster)
		if err != nil {
			t.Fatalf("a not-yet-applied substrate must not fail reconcile: %v", err)
		}
		if len(resolved) != 0 {
			t.Errorf("nothing should resolve, got %v", resolved)
		}
		if len(unresolved) != 1 || unresolved[0] != "db" {
			t.Errorf("db should be reported unresolved, got %v", unresolved)
		}
	})

	t.Run("blank value is unresolved", func(t *testing.T) {
		r := &PlatformReconciler{SSM: &stubSSM{params: map[string]string{dbPath: ""}}}

		_, unresolved, err := r.resolveRelationalSecretARNs(context.Background(), p, cluster)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(unresolved) != 1 {
			t.Errorf("a valueless parameter must not be treated as resolved, got %v", unresolved)
		}
	})

	t.Run("a real SSM failure propagates", func(t *testing.T) {
		r := &PlatformReconciler{SSM: &stubSSM{err: errors.New("throttled")}}

		if _, _, err := r.resolveRelationalSecretARNs(context.Background(), p, cluster); err == nil {
			t.Error("an SSM error is not a missing parameter and must not be swallowed")
		}
	})

	t.Run("a response with no parameter body is unresolved, not a panic", func(t *testing.T) {
		r := &PlatformReconciler{SSM: &stubSSM{nilParameter: true}}

		_, unresolved, err := r.resolveRelationalSecretARNs(context.Background(), p, cluster)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(unresolved) != 1 {
			t.Errorf("an empty response must read as unpublished, got %v", unresolved)
		}
	})

	t.Run("no client or no cluster name is unresolved", func(t *testing.T) {
		noClient := &PlatformReconciler{}
		if _, unresolved, err := noClient.resolveRelationalSecretARNs(context.Background(), p, cluster); err != nil || len(unresolved) != 1 {
			t.Errorf("without an SSM client the datastore is unresolved, got %v / %v", unresolved, err)
		}

		noCluster := &PlatformReconciler{SSM: &stubSSM{params: map[string]string{dbPath: testDBSecretARN}}}
		if _, unresolved, err := noCluster.resolveRelationalSecretARNs(context.Background(), p, ""); err != nil || len(unresolved) != 1 {
			t.Errorf("without a cluster name the path cannot be composed, got %v / %v", unresolved, err)
		}
	})

	t.Run("a Platform with no relational datastore makes no call", func(t *testing.T) {
		s := &stubSSM{params: map[string]string{}}
		r := &PlatformReconciler{SSM: s}
		objOnly := platformWithDatastores("myplat",
			platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore})

		if _, _, err := r.resolveRelationalSecretARNs(context.Background(), objOnly, cluster); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(s.calls) != 0 {
			t.Errorf("no relational datastore means no lookup, got %v", s.calls)
		}
	})
}

// TestEnsureDatastorePolicy_UnresolvedSecretStillWritesOtherGrants proves a
// Platform whose Aurora cluster has not published yet still gets the rest of its
// policy. The tenant is degraded — no database credential — not broken, and it
// converges on its own once the substrate applies.
func TestEnsureDatastorePolicy_UnresolvedSecretStillWritesOtherGrants(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
	r := &PlatformReconciler{IAM: f, SSM: &stubSSM{params: map[string]string{}}}
	cfg := IAMConfig{Environment: "development", Region: "us-west-2", ClusterName: "development-platform"}
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore},
	)

	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", p, cfg); err != nil {
		t.Fatalf("an unpublished secret must not fail the whole policy: %v", err)
	}
	if len(f.putInlineCalls) != 1 {
		t.Fatalf("PutRolePolicy calls: got %d want 1", len(f.putInlineCalls))
	}
	doc := *f.putInlineCalls[0].PolicyDocument
	if !strings.Contains(doc, "development-myplat-obj-123456789012") {
		t.Errorf("the object store grant should still be written: %s", doc)
	}
	if strings.Contains(doc, "secretsmanager") {
		t.Errorf("no secret grant may be written before the ARN is published: %s", doc)
	}
}

// TestEnsureDatastorePolicy_SSMFailureIsNotSilent proves a throttled or denied
// parameter read fails the reconcile rather than being mistaken for "no secret
// published", which would quietly strip a working tenant's database grant on the
// next converge.
func TestEnsureDatastorePolicy_SSMFailureIsNotSilent(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
	r := &PlatformReconciler{IAM: f, SSM: &stubSSM{err: errors.New("throttled")}}
	cfg := IAMConfig{Environment: "development", Region: "us-west-2", ClusterName: "development-platform"}
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational})

	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", p, cfg); err == nil {
		t.Error("an SSM failure must surface, not degrade the tenant silently")
	}
	if len(f.putInlineCalls) != 0 {
		t.Errorf("no policy should be written when the lookup failed, got %d writes", len(f.putInlineCalls))
	}
}
