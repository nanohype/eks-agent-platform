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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The eval identity is what a Platform's eval runs hold: a ServiceAccount in the
// eval runtime namespace, bound by an EKS Pod Identity association to a role
// that reaches only that Platform's own prefixes in the shared eval-reports
// bucket. One per Platform, so a run for one Platform cannot read another's
// cases or overwrite another's reports — the eval-reports bucket is shared, and
// this role is the only boundary inside it.
//
// The ServiceAccount lives in the eval runtime namespace rather than the tenant
// namespace. Eval Workflows run there because the eval-runner WorkflowTemplate
// does and a workflowTemplateRef resolves only within the Workflow's own
// namespace; and a ServiceAccount in the tenant namespace would be usable by
// any pod the tenant's AppProject deploys there, handing the tenant the eval
// identity and the writeback into its own EvalSuite's score.
//
// The Platform reports its eval identity on the EvalIdentityReady condition, and
// the outcome travels there rather than in the reconcile's result: a Platform's
// own readiness does not wait on its evals. The EvalSuite reconciler submits a
// run only while that condition is True, which is written only after a pass in
// which the ServiceAccount, the role and the association binding them all exist,
// and holds a suite's schedule suspended while it is not.

// evalReportsPolicyName is the eval role's one inline policy.
const evalReportsPolicyName = "eval-reports-access"

// conditionEvalIdentityReady is the Platform condition reporting whether its
// eval identity is bound end to end. The EvalSuite reconciler gates every run
// on it.
const conditionEvalIdentityReady = "EvalIdentityReady"

// EvalIdentityConfig locates the eval runtime the per-Platform eval identity is
// minted into. main.go wires it from the eval-runtime component's SSM outputs —
// the same values the EvalSuite reconciler submits runs with, so the identity
// and the runs that hold it agree on namespace and bucket by construction.
type EvalIdentityConfig struct {
	// Namespace is the eval runtime namespace: where the eval-runner
	// WorkflowTemplate lives, where every eval Workflow runs, and so where each
	// Platform's eval ServiceAccount is created. Empty means
	// defaultEvalRunnerNamespace.
	Namespace string
	// ReportsBucket is the eval-reports bucket the role's grants are scoped
	// into. Empty means the cluster runs no eval runtime: no eval identity is
	// minted, and the EvalSuite reconciler submits no runs. An identity a
	// Platform already records on status.evalRoleArn is still torn down by its
	// finalizer (deleteEvalIdentity).
	ReportsBucket string
}

func (c EvalIdentityConfig) enabled() bool { return c.ReportsBucket != "" }

func (c EvalIdentityConfig) namespace() string { return evalRuntimeNamespace(c.Namespace) }

// evalIdentityWired reports whether this reconciler mints eval identities: an
// eval runtime is configured, and the IAM and EKS clients the role and its
// association are made with are wired. On a running operator the three arrive
// together, because the eval-reports bucket is read from SSM through the same
// AWS clients. Without either client there is no role a run could be bound to,
// so nothing is created and the Platform reports no EvalIdentityReady
// condition. Teardown is not gated on it: deleteEvalIdentity removes whatever
// status.evalRoleArn and status.evalNamespace record, whatever this reconciler
// is configured with.
func (r *PlatformReconciler) evalIdentityWired() bool {
	return r.Eval.enabled() && r.IAM != nil && r.EKS != nil
}

// EvalServiceAccountName is the ServiceAccount a Platform's eval Workflows run
// under, in the eval runtime namespace: eval-<platform>, hash-truncated to the
// 63-char label limit the same way PlatformNamespace is. Exported so the
// conformance suite addresses it by the rule the reconcilers use.
func EvalServiceAccountName(p *platformv1alpha1.Platform) string {
	return boundedName("eval-", p.Name, "", 63)
}

// evalRoleName is the IAM role a Platform's eval runs hold:
//
//	<cluster-name>-<platform.name>-eval
//
// Same 64-char cap and hash truncation as tenantRoleName.
func evalRoleName(clusterName string, p *platformv1alpha1.Platform) string {
	return boundedName(clusterName+"-", p.Name, evalRoleSuffix, iamRoleNameMax)
}

// evalRoleTags mirrors tenantRoleTags with Component eval-iam, so cost
// attribution and the cloudgov tag gate group the role under its Platform
// while telling it apart from the tenant role.
func evalRoleTags(p *platformv1alpha1.Platform, cfg IAMConfig) []iamtypes.Tag {
	return roleTagsWithComponent(p, cfg, "eval-iam")
}

// evalReportsPolicyDoc renders the eval role's grant, derived from the Platform
// name and the bucket alone. It matches the eval-runner WorkflowTemplate's
// layout exactly:
//
//   - s3:GetObject on <bucket>/<platform>/manifests/* — resolve-cases pulls a
//     cases manifest from there.
//   - s3:PutObject on <bucket>/<platform>/runs/* — score uploads report.html
//     and junit.xml to runs/<suite>/<runId>/.
//   - kms:Decrypt and kms:GenerateDataKey, only through S3 and only for this
//     bucket. The bucket is SSE-KMS with an S3 Bucket Key, which makes the
//     bucket ARN the encryption context of every KMS request S3 makes for it;
//     the operator is not handed the key's ARN, so the key is matched by those
//     two conditions rather than named.
//
// The trailing slash after the Platform name is the delimiter: Platform foo's
// grant must not reach foo-bar/…, and foo/* cannot. Nothing else is granted —
// no s3:ListBucket, no Bedrock: eval-runner reaches the model through the
// Platform's ModelGateway over HTTP, holding no AWS credential for it.
func evalReportsPolicyDoc(platform, bucket string, scope arnScope) (string, error) {
	bucketARN := fmt.Sprintf("arn:%s:s3:::%s", scope.partition(), bucket)
	prefix := bucketARN + "/" + platform + "/"
	stmts := []policyStatement{
		{
			Sid:      "ReadEvalManifests",
			Effect:   "Allow",
			Action:   []string{"s3:GetObject"},
			Resource: []string{prefix + "manifests/*"},
		},
		{
			Sid:      "WriteEvalRuns",
			Effect:   "Allow",
			Action:   []string{"s3:PutObject"},
			Resource: []string{prefix + "runs/*"},
		},
		{
			Sid:      "EvalReportsEncryption",
			Effect:   "Allow",
			Action:   []string{"kms:Decrypt", "kms:GenerateDataKey"},
			Resource: []string{fmt.Sprintf("arn:%s:kms:%s:%s:key/*", scope.partition(), scope.region(), scope.account())},
			Condition: map[string]map[string]string{
				"StringEquals": {"kms:EncryptionContext:aws:s3:arn": bucketARN},
				"StringLike":   {"kms:ViaService": "s3." + scope.region() + ".amazonaws.com"},
			},
		},
	}
	b, err := json.Marshal(policyDocument{Version: "2012-10-17", Statement: stmts})
	if err != nil {
		return "", fmt.Errorf("marshal eval reports policy: %w", err) //coverage:ignore json.Marshal of a policyDocument of string fields cannot fail
	}
	return string(b), nil
}

// evalIdentityLabels is the Platform's canonical label set with the component
// naming the eval identity, so the ServiceAccount is attributable to its
// Platform from the eval runtime namespace.
func (r *PlatformReconciler) evalIdentityLabels(p *platformv1alpha1.Platform) map[string]string {
	l := r.labelsForPlatform(p)
	l["app.kubernetes.io/component"] = "eval"
	return l
}

// evalIdentityOutcome is what an ensureEvalIdentity pass that returned no error
// established.
type evalIdentityOutcome int

const (
	// evalIdentityUnmanaged: this reconciler mints no eval identity (see
	// evalIdentityWired).
	evalIdentityUnmanaged evalIdentityOutcome = iota
	// evalIdentityDeferred: the eval runtime namespace does not exist.
	evalIdentityDeferred
	// evalIdentityBound: the ServiceAccount, the role and the association
	// binding them all exist.
	evalIdentityBound
)

// ensureEvalIdentity reconciles a Platform's eval identity: the
// ServiceAccount, the role with its eval-reports-access policy, and the Pod
// Identity association binding the two. The role carries no model access, so
// the kill-switch has nothing on it to pull, and it reconciles whether or not
// the Platform is suspended.
//
// An absent eval runtime namespace defers the whole identity instead of
// failing: the namespace arrives with the operator chart's evalRuntime bundle,
// no role is minted for an account that cannot exist yet, and a later pass
// mints it.
//
// A pass that fails after creating the ServiceAccount leaves it bound to
// nothing until a later pass completes, which is why runs are gated on the
// EvalIdentityReady condition this pass's outcome sets, not on the account
// alone.
//
// The role's ARN and the namespace the pass binds it in are written through to
// status.evalRoleArn and status.evalNamespace as soon as the pass holds the
// role (recordEvalRole), whether or not its policy write succeeded, and before
// the association binds anything to it. The record is what the finalizer reads
// (deleteEvalIdentity, deleteEvalRole), and the reconcile can return before its
// closing status write, or have that write refused, after this step has run,
// so the record does not wait for that write. A pass whose record write fails
// binds nothing and reports the failure, and the next pass finds the role under
// the tenant path and records it.
//
// A record naming another namespace is an identity bound under an earlier eval
// runtime namespace. Its account and association are removed from there before
// the record names this one (retireMovedEvalAccount), so the namespace the
// record names is the only one an association binding the role can sit in.
func (r *PlatformReconciler) ensureEvalIdentity(ctx context.Context, p *platformv1alpha1.Platform) (evalIdentityOutcome, error) {
	if !r.evalIdentityWired() {
		return evalIdentityUnmanaged, nil
	}
	ns := r.Eval.namespace()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: EvalServiceAccountName(p), Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = r.evalIdentityLabels(p)
		return nil
	}); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("eval runtime namespace absent; eval identity deferred", "namespace", ns, "serviceAccount", sa.Name)
			return evalIdentityDeferred, nil
		}
		return evalIdentityUnmanaged, fmt.Errorf("ensure eval ServiceAccount %s/%s: %w", ns, sa.Name, err)
	}
	roleARN, roleErr := r.ensureEvalRole(ctx, p, r.IAMCfg)
	if roleARN != "" {
		if err := r.retireMovedEvalAccount(ctx, p, ns); err != nil {
			return evalIdentityUnmanaged, err
		}
		if err := r.recordEvalRole(ctx, p, roleARN, ns); err != nil {
			return evalIdentityUnmanaged, err
		}
	}
	if roleErr != nil {
		return evalIdentityUnmanaged, roleErr
	}
	if err := r.ensurePodIdentityAssociation(ctx, r.IAMCfg, ns, sa.Name, roleARN); err != nil {
		return evalIdentityUnmanaged, err
	}
	return evalIdentityBound, nil
}

// recordEvalRole sets status.evalRoleArn to roleARN and status.evalNamespace to
// ns and patches both through to the API server. A record that already names
// both is not written again, so a converged pass makes no write. A refused
// patch still leaves the values on p for the reconcile's closing status write.
func (r *PlatformReconciler) recordEvalRole(ctx context.Context, p *platformv1alpha1.Platform, roleARN, ns string) error {
	if p.Status.EvalRoleArn == roleARN && p.Status.EvalNamespace == ns {
		return nil
	}
	if err := r.patchEvalRecord(ctx, p, roleARN, ns); err != nil {
		return fmt.Errorf("record eval role %s and namespace %s in status.evalRoleArn and status.evalNamespace of Platform %s/%s: %w",
			roleARN, ns, p.Namespace, p.Name, err)
	}
	return nil
}

// patchEvalRecord sets status.evalRoleArn and status.evalNamespace on p and
// writes both through to the API server in one status patch, which is what
// keeps the two fields set and cleared together.
//
// The values are set on p before the patch and stay there whatever its result;
// a caller that must not keep them on a refused patch restores them. The patch
// itself is a merge patch of a copy: it carries no resourceVersion, so it
// cannot conflict, and the rest of the pass keeps working on the spec it read
// rather than on whatever the API server returns. Only the copy's
// resourceVersion is carried back to p, so a later write of p lands on top of
// this patch instead of conflicting with it. That is sound because this
// reconciler is the only writer of Platform status, and it reconciles one
// Platform on one worker at a time.
func (r *PlatformReconciler) patchEvalRecord(ctx context.Context, p *platformv1alpha1.Platform, roleARN, ns string) error {
	before := p.DeepCopy()
	p.Status.EvalRoleArn, p.Status.EvalNamespace = roleARN, ns
	patched := p.DeepCopy()
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(before)); err != nil {
		return err
	}
	p.ResourceVersion = patched.ResourceVersion
	return nil
}

// setEvalIdentityCondition records one ensureEvalIdentity pass on the
// Platform's EvalIdentityReady condition. True only for a pass that bound the
// identity end to end; False, with the reason, for a deferred or failed one.
// A reconciler that mints no eval identity removes the condition, since the
// Platform then has no eval identity to report on.
func (r *PlatformReconciler) setEvalIdentityCondition(p *platformv1alpha1.Platform, outcome evalIdentityOutcome, err error) {
	if err == nil && outcome == evalIdentityUnmanaged {
		apimeta.RemoveStatusCondition(&p.Status.Conditions, conditionEvalIdentityReady)
		return
	}
	ns := r.Eval.namespace()
	cond := metav1.Condition{
		Type:               conditionEvalIdentityReady,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: p.Generation,
	}
	switch {
	case err != nil:
		cond.Reason = "ReconcileFailed"
		cond.Message = err.Error() + "; no eval run is submitted until the identity is bound"
	case outcome == evalIdentityDeferred:
		cond.Reason = "RuntimeNamespaceAbsent"
		cond.Message = fmt.Sprintf("eval runtime namespace %s does not exist; no eval run is submitted until it does and the identity is minted in it", ns)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Bound"
		cond.Message = fmt.Sprintf("ServiceAccount %s/%s is bound by EKS Pod Identity to role %s",
			ns, EvalServiceAccountName(p), p.Status.EvalRoleArn)
	}
	upsertCondition(&p.Status.Conditions, cond)
}

// ensureEvalRole creates the eval role when no role under the tenant path
// holds its name — under that path, with the tenant permissions boundary and
// the Pod Identity trust the tenant role also carries — and converges its
// inline policy on every pass, so a changed bucket or a policy edited out of
// band is rewritten. It returns the role's ARN once it holds the role, found
// under the tenant path or created, including from a pass whose policy write
// failed, so the caller can record it.
//
// IAM authorizes a call naming an existing role against that role's ARN, path
// included, and a call naming no existing role against the root-path ARN
// role/<name>. The operator's grants reach the roles it mints through the
// tenant path, so its read of its own eval role is authorized, while a read of
// a name no role holds may be refused instead of answered NoSuchEntity. Here
// either answer is read as no role under the tenant path holding the name, so
// the eval role needs no grant on its root-path name: CreateRole, which names
// the path, settles whether the name is free.
//
// A role of this name the operator did not mint is never adopted. One that
// GetRole returns under another path is refused outright. One that GetRole is
// refused for, because it sits outside what the operator can read, surfaces as
// EntityAlreadyExists when CreateRole tries the name, and is refused there.
// EntityAlreadyExists after a NoSuchEntity is a role created between the two
// calls; the pass fails, and the next one reads the role and holds it to the
// same path rule.
func (r *PlatformReconciler) ensureEvalRole(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig) (string, error) {
	name := evalRoleName(cfg.ClusterName, p)
	path := cfg.tenantPath()

	var roleARN string
	getOut, getErr := r.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	switch {
	case getErr == nil:
		if got := aws.ToString(getOut.Role.Path); got != path {
			return "", fmt.Errorf("eval role %s exists under path %q, not the tenant path %s; the operator does not adopt a role it did not mint", name, got, path)
		}
		roleARN = aws.ToString(getOut.Role.Arn)
	case !isIAMNotFound(getErr) && !isIAMAccessDenied(getErr):
		return "", fmt.Errorf("iam GetRole %s: %w", name, getErr)
	default:
		trust, err := assumeRolePolicyForPodIdentity()
		if err != nil {
			return "", err //coverage:ignore unreachable — see assumeRolePolicyForPodIdentity
		}
		createInput := &iam.CreateRoleInput{
			RoleName:                 aws.String(name),
			Path:                     aws.String(path),
			AssumeRolePolicyDocument: aws.String(trust),
			Description:              aws.String(fmt.Sprintf("Eval role for Platform %s (tenant %s)", p.Name, p.Spec.Tenant)),
			Tags:                     evalRoleTags(p, cfg),
		}
		if cfg.TenantPermissionsBoundaryARN != "" {
			createInput.PermissionsBoundary = aws.String(cfg.TenantPermissionsBoundaryARN)
		}
		createOut, err := r.IAM.CreateRole(ctx, createInput)
		var taken *iamtypes.EntityAlreadyExistsException
		switch {
		case errors.As(err, &taken) && isIAMAccessDenied(getErr):
			return "", fmt.Errorf("iam CreateRole %s: the name is held by a role the operator cannot read under the tenant path %s (GetRole: %v): %w", name, path, getErr, err)
		case errors.As(err, &taken):
			return "", fmt.Errorf("iam CreateRole %s: a role of this name was created after GetRole answered NoSuchEntity; the next pass reads it and holds it to the tenant path %s: %w", name, path, err)
		case err != nil:
			return "", fmt.Errorf("iam CreateRole %s: %w", name, err)
		}
		roleARN = aws.ToString(createOut.Role.Arn)
	}

	desired, err := evalReportsPolicyDoc(p.Name, r.Eval.ReportsBucket, arnScopeFromRole(roleARN, cfg.Region))
	if err != nil {
		return "", err //coverage:ignore unreachable — see evalReportsPolicyDoc
	}
	if err := r.reconcileInlinePolicy(ctx, name, evalReportsPolicyName, desired); err != nil {
		return roleARN, err
	}
	return roleARN, nil
}

// deleteEvalIdentity is the finalizer counterpart of ensureEvalIdentity: in
// each namespace evalTeardownNamespaces names it suspends the Platform's eval
// schedules and removes the association and the ServiceAccount, then it
// removes the role with its inline policy. Every step tolerates a resource
// that is already gone, so re-runs are safe.
//
// It runs when this reconciler mints eval identities or when the Platform
// records an eval role, so removing an identity does not depend on the
// configuration it was minted under still being in place: a cluster whose eval
// runtime is unconfigured, or configured with another namespace, after a role
// was minted still tears down that role and the account and association
// status.evalNamespace records. A recorded role with no IAM or EKS client to
// remove it with holds the finalizer. Releasing it would leave the role with
// its grant into the eval-reports bucket, its association and its
// ServiceAccount behind, and a later Platform of the same name would inherit
// all three. Datastores are not touched, as on every other finalizer step.
func (r *PlatformReconciler) deleteEvalIdentity(ctx context.Context, p *platformv1alpha1.Platform) error {
	recorded := p.Status.EvalRoleArn
	if !r.evalIdentityWired() && recorded == "" {
		return nil
	}
	if r.IAM == nil || r.EKS == nil {
		return fmt.Errorf("status.evalRoleArn records eval role %s, and this reconciler has no IAM and EKS clients to remove it, its Pod Identity association and its ServiceAccount with; the finalizer holds until both are wired", recorded)
	}
	for _, ns := range r.evalTeardownNamespaces(p) {
		if err := r.removeEvalAccount(ctx, p, ns); err != nil {
			return err
		}
	}
	return r.deleteEvalRole(ctx, p)
}

// evalTeardownNamespaces names every namespace the finalizer removes the
// Platform's eval account and association from. The namespace
// status.evalNamespace records holds any association binding the recorded
// role. While this reconciler mints eval identities, the configured namespace
// is added: a pass creates the account there before it records anything, so an
// account can sit there unrecorded. With no namespace recorded, the configured
// namespace is the only one a pass can have created the account in.
func (r *PlatformReconciler) evalTeardownNamespaces(p *platformv1alpha1.Platform) []string {
	configured, recorded := r.Eval.namespace(), p.Status.EvalNamespace
	switch {
	case recorded == "" || recorded == configured:
		return []string{configured}
	case r.evalIdentityWired():
		return []string{recorded, configured}
	default:
		return []string{recorded}
	}
}

// retireMovedEvalAccount removes the Platform's eval account and its
// association from the namespace status.evalNamespace records when that is not
// ns, the namespace the pass binds the identity in. ensureEvalIdentity moves
// the record to ns only once this has succeeded, so a failure here leaves the
// record naming the namespace the association still sits in, and the next pass
// or the finalizer removes it from there.
func (r *PlatformReconciler) retireMovedEvalAccount(ctx context.Context, p *platformv1alpha1.Platform, ns string) error {
	recorded := p.Status.EvalNamespace
	if recorded == "" || recorded == ns {
		return nil
	}
	log.FromContext(ctx).Info("eval runtime namespace moved; removing the eval identity's account from the recorded one",
		"recorded", recorded, "namespace", ns)
	return r.removeEvalAccount(ctx, p, recorded)
}

// removeEvalAccount suspends the Platform's eval schedules in ns, then deletes
// the Pod Identity association binding its eval ServiceAccount there and the
// ServiceAccount itself, in that order, so no schedule fires a run under an
// account that is gone and no association outlives the account it names.
func (r *PlatformReconciler) removeEvalAccount(ctx context.Context, p *platformv1alpha1.Platform, ns string) error {
	name := EvalServiceAccountName(p)
	if err := r.suspendEvalSchedules(ctx, p, ns); err != nil {
		return err
	}
	if err := r.deletePodIdentityAssociation(ctx, r.IAMCfg, ns, name); err != nil {
		return err
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete eval ServiceAccount %s/%s: %w", ns, name, err)
	}
	return nil
}

// suspendEvalSchedules suspends every CronWorkflow the EvalSuite reconciler
// made for this Platform in namespace ns, ahead of the ServiceAccount's
// deletion there, so no schedule fires a run under an account that no longer
// exists. The EvalSuite reconciler keeps a schedule suspended while
// its Platform is being deleted or absent, and resumes it on the pass that
// finds a Platform of that name with its identity bound. A cluster without the
// CronWorkflow kind has no schedules to suspend.
func (r *PlatformReconciler) suspendEvalSchedules(ctx context.Context, p *platformv1alpha1.Platform, ns string) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(argoWorkflowsGV.WithKind("CronWorkflowList"))
	if err := r.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "eks-agent-platform",
		LabelPlatform:                  p.Name,
	}); err != nil {
		if isNoKindMatch(err) {
			return nil
		}
		return fmt.Errorf("list eval schedules for Platform %s in %s: %w", p.Name, ns, err)
	}
	for i := range list.Items {
		if err := suspendCronWorkflow(ctx, r.Client, &list.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// deleteEvalRole removes the eval role with its inline policy.
//
// A refused read of the role is ambiguous. IAM answers that way for a name no
// role holds when the operator has no grant on the root-path name (see
// ensureEvalRole), and for an existing role the operator's grants no longer
// reach. status.evalRoleArn tells the two apart:
//
//   - No role recorded: a refused read releases the finalizer. A pass records
//     the role before it binds anything to it (recordEvalRole), so the record
//     is empty for a Platform no pass held a role for — deleted before one was
//     minted, or while the eval runtime namespace was absent — and for one
//     whose record write failed after CreateRole. That role sits under the
//     tenant path, which the operator's grants reach, so its read is answered
//     and the role is deleted below; only a role those grants no longer reach
//     is released unread.
//   - A role recorded: only NoSuchEntity means the role is gone. A refused read
//     is the operator's own role out of its reach, and releasing the finalizer
//     would leave the role and its grant into the eval-reports bucket behind,
//     so the finalizer holds and retries.
//
// The record is cleared after a read has found the role under the tenant path
// and before the role is deleted. A deleted role's read is refused like any
// absent name's, so a record outliving the deletion would hold a finalizer
// retried after a later step fails on a role that no longer exists.
//
// A role of the name under another path is not one the operator minted and is
// left alone.
func (r *PlatformReconciler) deleteEvalRole(ctx context.Context, p *platformv1alpha1.Platform) error {
	name := evalRoleName(r.IAMCfg.ClusterName, p)
	out, err := r.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	switch {
	case isIAMNotFound(err):
		return nil
	case isIAMAccessDenied(err) && p.Status.EvalRoleArn == "":
		return nil
	case isIAMAccessDenied(err):
		return fmt.Errorf("iam GetRole %s: %w; status.evalRoleArn records %s, so the finalizer holds until the role can be read and deleted",
			name, err, p.Status.EvalRoleArn)
	case err != nil:
		return fmt.Errorf("iam GetRole %s: %w", name, err)
	case aws.ToString(out.Role.Path) != r.IAMCfg.tenantPath():
		log.FromContext(ctx).Info("eval role name is held by a role outside the tenant path; leaving it",
			"role", name, "path", aws.ToString(out.Role.Path))
		return nil
	}
	if err := r.clearEvalRoleRecord(ctx, p); err != nil {
		return err
	}
	return r.detachAndDeleteRole(ctx, name)
}

// clearEvalRoleRecord empties status.evalRoleArn and status.evalNamespace and
// writes them through, so the cleared record is persisted before the caller
// deletes the role. A refused write restores both on p, so p goes on matching
// the record the API server still holds.
func (r *PlatformReconciler) clearEvalRoleRecord(ctx context.Context, p *platformv1alpha1.Platform) error {
	role, ns := p.Status.EvalRoleArn, p.Status.EvalNamespace
	if role == "" && ns == "" {
		return nil
	}
	if err := r.patchEvalRecord(ctx, p, "", ""); err != nil {
		p.Status.EvalRoleArn, p.Status.EvalNamespace = role, ns
		return fmt.Errorf("clear status.evalRoleArn and status.evalNamespace on Platform %s/%s before deleting its eval role: %w", p.Namespace, p.Name, err)
	}
	return nil
}
