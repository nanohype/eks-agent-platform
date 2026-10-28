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
// which the ServiceAccount, the role and the association binding them all exist.

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
	// reconciled or torn down, and the EvalSuite reconciler submits no runs.
	ReportsBucket string
}

func (c EvalIdentityConfig) enabled() bool { return c.ReportsBucket != "" }

func (c EvalIdentityConfig) namespace() string { return evalRuntimeNamespace(c.Namespace) }

// evalIdentityWired reports whether this reconciler mints eval identities: an
// eval runtime is configured, and the IAM and EKS clients the role and its
// association are made with are wired. On a running operator the three arrive
// together, because the eval-reports bucket is read from SSM through the same
// AWS clients. Without either client there is no role a run could be bound to,
// so nothing is created, nothing is torn down, and the Platform reports no
// EvalIdentityReady condition.
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
// The role's ARN goes on status.evalRoleArn as soon as the pass holds it —
// before the association is written, and whether or not its policy write
// succeeded — and the reconcile's status write does not skip on this pass's
// failure. A role a failed pass created is recorded all the same, and the
// record is what the finalizer reads (deleteEvalRole).
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
	roleARN, err := r.ensureEvalRole(ctx, p, r.IAMCfg)
	if roleARN != "" {
		p.Status.EvalRoleArn = roleARN
	}
	if err != nil {
		return evalIdentityUnmanaged, err
	}
	if err := r.ensurePodIdentityAssociation(ctx, r.IAMCfg, ns, sa.Name, roleARN); err != nil {
		return evalIdentityUnmanaged, err
	}
	return evalIdentityBound, nil
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
		case errors.As(err, &taken):
			return "", fmt.Errorf("iam CreateRole %s: the name is held by a role the operator cannot read under the tenant path %s (GetRole: %v): %w", name, path, getErr, err)
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

// deleteEvalIdentity is the finalizer counterpart of ensureEvalIdentity: the
// association, the ServiceAccount, then the role with its inline policy. Every
// step tolerates a resource that is already gone, so re-runs are safe.
//
// Gated like the ensure path: a reconciler that mints no eval identity has
// none to tear down. Datastores are not touched, as on every other finalizer
// step.
func (r *PlatformReconciler) deleteEvalIdentity(ctx context.Context, p *platformv1alpha1.Platform) error {
	if !r.evalIdentityWired() {
		return nil
	}
	ns := r.Eval.namespace()
	name := EvalServiceAccountName(p)
	if err := r.deletePodIdentityAssociation(ctx, r.IAMCfg, ns, name); err != nil {
		return err
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete eval ServiceAccount %s/%s: %w", ns, name, err)
	}
	return r.deleteEvalRole(ctx, p)
}

// deleteEvalRole removes the eval role with its inline policy.
//
// A refused read of the role is ambiguous. IAM answers that way for a name no
// role holds when the operator has no grant on the root-path name (see
// ensureEvalRole), and for an existing role the operator's grants no longer
// reach. status.evalRoleArn tells the two apart:
//
//   - No role recorded: no pass ever held one. The Platform was deleted before
//     a pass minted its role, or while the eval runtime namespace was absent,
//     and a refused read releases the finalizer.
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

// clearEvalRoleRecord empties status.evalRoleArn and writes it through, so the
// cleared record is persisted before the caller deletes the role.
func (r *PlatformReconciler) clearEvalRoleRecord(ctx context.Context, p *platformv1alpha1.Platform) error {
	recorded := p.Status.EvalRoleArn
	if recorded == "" {
		return nil
	}
	before := p.DeepCopy()
	p.Status.EvalRoleArn = ""
	if err := r.Status().Patch(ctx, p, client.MergeFrom(before)); err != nil {
		p.Status.EvalRoleArn = recorded
		return fmt.Errorf("clear status.evalRoleArn on Platform %s/%s before deleting its eval role: %w", p.Namespace, p.Name, err)
	}
	return nil
}
