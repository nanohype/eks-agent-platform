/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// The IAM limits the fake enforces, at IAM's defaults. IAM sizes a policy in
// characters with white space excluded.
const (
	// RoleNameMaxLen is the longest role name IAM accepts.
	RoleNameMaxLen = 64
	// PolicyNameMaxLen is the longest inline policy name IAM accepts.
	PolicyNameMaxLen = 128
	// TrustPolicyMaxChars is IAM's default quota on one role's trust policy
	// (ACLSizePerRole). An account can raise it; the fake models the default.
	TrustPolicyMaxChars = 2048
	// InlinePolicyMaxChars is IAM's quota on the combined size of every inline
	// policy one role holds.
	InlinePolicyMaxChars = 10240
	// DefaultAccountID is the account NewIAM mints role ARNs in.
	DefaultAccountID = "123456789012"
	// listDefaultMaxItems is the page size an IAM list call defaults to.
	listDefaultMaxItems = 100
)

var _ awsclients.IAM = (*IAM)(nil)

// IAM is an in-memory awsclients.IAM holding roles, their managed policy
// attachments and their inline policies.
//
// It refuses what IAM refuses. CreateRole refuses a name over RoleNameMaxLen
// (ValidationError), a name a role already holds (EntityAlreadyExists), a trust
// policy that is not a JSON object (MalformedPolicyDocument) and one over
// TrustPolicyMaxChars (LimitExceeded); UpdateAssumeRolePolicy holds the new
// trust policy to the same two document rules. PutRolePolicy refuses a policy
// name over PolicyNameMaxLen, a document that is not a JSON object, and a
// document that takes the role's inline policies past InlinePolicyMaxChars
// together, counted with it in place of any same-name policy it replaces. Every
// call naming a role that does not exist gets NoSuchEntity, unless an
// AccessDenied knob below applies. DetachRolePolicy of an ARN the role does not
// hold and DeleteRolePolicy of a policy it does not hold get NoSuchEntity, and
// DeleteRole while the role still holds a managed or an inline policy gets
// DeleteConflict. GetRole and GetRolePolicy return documents URL-encoded, as
// IAM does; the state below holds them as written.
//
// The knobs inject what a test needs beyond that. Each injected error is
// returned ahead of every check the fake makes, and each recorder says whether
// it records a call ahead of its call's knob or only past it.
type IAM struct {
	sync.Mutex

	// AccountID is the account CreateRole and SeedRoleAtPath mint role ARNs
	// in. Empty means DefaultAccountID.
	AccountID string

	// Roles holds every role by name. A role's AssumeRolePolicyDocument is
	// stored as written; GetRole encodes it on the way out.
	Roles map[string]*iamtypes.Role
	// Attached holds each role's managed policy ARNs.
	Attached map[string]map[string]struct{}
	// Inline holds each role's inline policy documents by policy name, as
	// written; GetRolePolicy encodes them on the way out.
	Inline map[string]map[string]string

	// CreateCalls records every CreateRole call past CreateReturnsErr and
	// CreateErr, the ones the fake then refused included.
	CreateCalls []iam.CreateRoleInput
	// UpdateAssumeCalls records every UpdateAssumeRolePolicy call past
	// UpdateAssumeErr, refused ones included.
	UpdateAssumeCalls []iam.UpdateAssumeRolePolicyInput
	// TrustUpdates counts every UpdateAssumeRolePolicy call, ahead of
	// UpdateAssumeErr.
	TrustUpdates int
	// AttachCalls records every AttachRolePolicy call past AttachReturnsErr,
	// refused ones included.
	AttachCalls []iam.AttachRolePolicyInput
	// DetachCalls records every DetachRolePolicy call past DetachReturnsErr,
	// refused ones included.
	DetachCalls []iam.DetachRolePolicyInput
	// PutInlineCalls records every PutRolePolicy call, ahead of
	// PutInlineReturnsErr.
	PutInlineCalls []iam.PutRolePolicyInput
	// Puts counts the PutRolePolicy calls that stored a document.
	Puts int
	// DeleteInlineCalls records every DeleteRolePolicy call, ahead of
	// DeleteInlineReturnsErr.
	DeleteInlineCalls []iam.DeleteRolePolicyInput
	// TagCalls records every TagRole call.
	TagCalls []iam.TagRoleInput
	// ListCalls counts every ListAttachedRolePolicies call, ahead of
	// ListReturnsErr.
	ListCalls int

	// GetReturnsErr fails every GetRole call.
	GetReturnsErr error
	// CreateReturnsErr fails every CreateRole call.
	CreateReturnsErr error
	// CreateErr, when set, fails CreateRole for each name it returns an error
	// for.
	CreateErr func(name string) error
	// DeleteReturnsErr fails every DeleteRole call.
	DeleteReturnsErr error
	// UpdateAssumeErr fails every UpdateAssumeRolePolicy call.
	UpdateAssumeErr error
	// AttachReturnsErr fails AttachRolePolicy for the policy ARNs it holds.
	AttachReturnsErr map[string]error
	// DetachReturnsErr fails every DetachRolePolicy call.
	DetachReturnsErr error
	// ListReturnsErr fails every ListAttachedRolePolicies call.
	ListReturnsErr error
	// ListInlineReturnsErr fails every ListRolePolicies call.
	ListInlineReturnsErr error
	// GetInlineReturnsErr, PutInlineReturnsErr and DeleteInlineReturnsErr fail
	// GetRolePolicy, PutRolePolicy and DeleteRolePolicy for the policy names
	// they hold, so one inline policy's reconcile can fail while the others
	// succeed.
	GetInlineReturnsErr    map[string]error
	PutInlineReturnsErr    map[string]error
	DeleteInlineReturnsErr map[string]error

	// AbsentReadsDenied answers GetRole on a name no role holds with
	// AccessDenied instead of NoSuchEntity: what IAM returns to an identity
	// whose read grant covers the tenant path but not the root-path name.
	AbsentReadsDenied bool
	// DeniedAbsentSuffix answers every call naming an absent role whose name
	// ends in it with AccessDenied instead of NoSuchEntity: IAM's answer to an
	// identity whose grants cover the tenant path but not that root-path name.
	DeniedAbsentSuffix string
	// RefusedReads answers GetRole with AccessDenied for the roles it names,
	// present or not: an identity whose grants no longer reach them.
	RefusedReads map[string]bool

	// PageBoundary, when > 0, is the page size of ListAttachedRolePolicies, and
	// InlinePageSize that of ListRolePolicies; IAM's default is 100. A page's
	// Marker is the last name it returned and the next page resumes after it,
	// so a detach or delete loop running over the list skips nothing.
	PageBoundary   int
	InlinePageSize int

	roleIDs int
}

// NewIAM returns an IAM holding no roles, minting ARNs in DefaultAccountID.
func NewIAM() *IAM {
	f := &IAM{}
	f.init()
	return f
}

// init makes the zero IAM usable. Callers hold f's lock.
func (f *IAM) init() {
	if f.AccountID == "" {
		f.AccountID = DefaultAccountID
	}
	if f.Roles == nil {
		f.Roles = map[string]*iamtypes.Role{}
	}
	if f.Attached == nil {
		f.Attached = map[string]map[string]struct{}{}
	}
	if f.Inline == nil {
		f.Inline = map[string]map[string]string{}
	}
	if f.AttachReturnsErr == nil {
		f.AttachReturnsErr = map[string]error{}
	}
}

// RoleARN is the ARN IAM gives a role of this name under path, in the fake's
// account.
func (f *IAM) RoleARN(path, name string) string {
	f.Lock()
	defer f.Unlock()
	f.init()
	return f.roleARN(path, name)
}

func (f *IAM) roleARN(path, name string) string {
	return "arn:aws:iam::" + f.AccountID + ":role" + path + name
}

// SeedRole stores a role as one made out of band would read: name, the given
// ARN, the path that ARN carries, and tags. It replaces a role of the name and
// keeps whatever policies one held.
func (f *IAM) SeedRole(name, arn string, tags ...iamtypes.Tag) *iamtypes.Role {
	f.Lock()
	defer f.Unlock()
	f.init()
	return f.seed(name, arn, pathOfARN(arn), tags)
}

// SeedRoleAtPath stores a role under path with the ARN CreateRole would have
// given it, carrying tags.
func (f *IAM) SeedRoleAtPath(path, name string, tags ...iamtypes.Tag) *iamtypes.Role {
	f.Lock()
	defer f.Unlock()
	f.init()
	return f.seed(name, f.roleARN(path, name), path, tags)
}

// seed stores a role. Callers hold f's lock.
func (f *IAM) seed(name, arn, path string, tags []iamtypes.Tag) *iamtypes.Role {
	f.roleIDs++
	role := &iamtypes.Role{
		RoleName: aws.String(name),
		RoleId:   aws.String(fmt.Sprintf("AROAFAKE%012d", f.roleIDs)),
		Arn:      aws.String(arn),
		Path:     aws.String(path),
		Tags:     append([]iamtypes.Tag(nil), tags...),
	}
	f.Roles[name] = role
	return role
}

// pathOfARN is the path a role ARN carries: everything between "role" and the
// role's name, "/" for a role at the root.
func pathOfARN(arn string) string {
	_, rest, ok := strings.Cut(arn, ":role/")
	if !ok {
		return "/"
	}
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return "/"
	}
	return "/" + rest[:i+1]
}

// SeedAttachment attaches a managed policy to a role, as one attached out of
// band would read.
func (f *IAM) SeedAttachment(roleName, policyARN string) {
	f.Lock()
	defer f.Unlock()
	f.init()
	f.attach(roleName, policyARN)
}

// attach records policyARN on roleName. Callers hold f's lock.
func (f *IAM) attach(roleName, policyARN string) {
	if f.Attached[roleName] == nil {
		f.Attached[roleName] = map[string]struct{}{}
	}
	f.Attached[roleName][policyARN] = struct{}{}
}

// AttachmentsFor returns the managed policy ARNs attached to a role, sorted.
func (f *IAM) AttachmentsFor(roleName string) []string {
	f.Lock()
	defer f.Unlock()
	return sortedKeys(f.Attached[roleName])
}

// HasRole reports whether a role of the name exists.
func (f *IAM) HasRole(name string) bool {
	f.Lock()
	defer f.Unlock()
	_, ok := f.Roles[name]
	return ok
}

// InlinePolicy returns a role's inline policy document as written, and whether
// the role holds it.
func (f *IAM) InlinePolicy(roleName, policyName string) (string, bool) {
	f.Lock()
	defer f.Unlock()
	doc, ok := f.Inline[roleName][policyName]
	return doc, ok
}

// SetInlinePolicy writes a role's inline policy document directly, bypassing
// every check: an edit made outside the operator.
func (f *IAM) SetInlinePolicy(roleName, policyName, doc string) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.Inline[roleName] == nil {
		f.Inline[roleName] = map[string]string{}
	}
	f.Inline[roleName][policyName] = doc
}

// PutCount is Puts, read under the lock.
func (f *IAM) PutCount() int {
	f.Lock()
	defer f.Unlock()
	return f.Puts
}

// TrustUpdateCount is TrustUpdates, read under the lock.
func (f *IAM) TrustUpdateCount() int {
	f.Lock()
	defer f.Unlock()
	return f.TrustUpdates
}

// role returns the named role, or IAM's answer to a call naming one that does
// not exist. Callers hold f's lock.
func (f *IAM) role(op, name string) (*iamtypes.Role, error) {
	if r, ok := f.Roles[name]; ok {
		return r, nil
	}
	if f.DeniedAbsentSuffix != "" && strings.HasSuffix(name, f.DeniedAbsentSuffix) {
		return nil, iamAccessDenied(op, name)
	}
	return nil, &iamtypes.NoSuchEntityException{Message: aws.String("The role with name " + name + " cannot be found.")}
}

func iamAccessDenied(op, name string) error {
	return &smithy.GenericAPIError{
		Code:    "AccessDenied",
		Message: "User is not authorized to perform: iam:" + op + " on resource: role " + name,
	}
}

// iamValidationError is how the SDK surfaces IAM's ValidationError, which it
// models no type for.
func iamValidationError(field, value string, maxLen int) error {
	return &smithy.GenericAPIError{
		Code: "ValidationError",
		Message: fmt.Sprintf("1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: "+
			"Member must have length less than or equal to %d", value, field, maxLen),
	}
}

// checkName holds a role or policy name to IAM's length bounds.
func checkName(field, name string, maxLen int) error {
	if name == "" || len(name) > maxLen {
		return iamValidationError(field, name, maxLen)
	}
	return nil
}

// checkTrustPolicy holds a trust policy to IAM's document rules.
func checkTrustPolicy(roleName, doc string) error {
	if !isJSONObject(doc) {
		return &iamtypes.MalformedPolicyDocumentException{Message: aws.String("This policy contains invalid Json")}
	}
	if n := policySize(doc); n > TrustPolicyMaxChars {
		return &iamtypes.LimitExceededException{Message: aws.String(fmt.Sprintf(
			"Cannot exceed quota for ACLSizePerRole: %d (role %s, trust policy %d characters)", TrustPolicyMaxChars, roleName, n))}
	}
	return nil
}

// encodedRole is a copy of a stored role as GetRole and CreateRole return it,
// its trust policy URL-encoded.
func encodedRole(r *iamtypes.Role) *iamtypes.Role {
	out := *r
	out.Tags = append([]iamtypes.Tag(nil), r.Tags...)
	if r.AssumeRolePolicyDocument != nil {
		out.AssumeRolePolicyDocument = aws.String(urlEncoded(aws.ToString(r.AssumeRolePolicyDocument)))
	}
	return &out
}

// CreateRole creates a role under the input's path (the root when unset), with
// its trust policy, tags and permissions boundary.
func (f *IAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	name := aws.ToString(in.RoleName)
	if f.CreateReturnsErr != nil {
		return nil, f.CreateReturnsErr
	}
	if f.CreateErr != nil {
		if err := f.CreateErr(name); err != nil {
			return nil, err
		}
	}
	f.CreateCalls = append(f.CreateCalls, *in)
	if err := checkName("roleName", name, RoleNameMaxLen); err != nil {
		return nil, err
	}
	if _, ok := f.Roles[name]; ok {
		return nil, &iamtypes.EntityAlreadyExistsException{Message: aws.String("Role with name " + name + " already exists.")}
	}
	trust := aws.ToString(in.AssumeRolePolicyDocument)
	if err := checkTrustPolicy(name, trust); err != nil {
		return nil, err
	}
	path := aws.ToString(in.Path)
	if path == "" {
		path = "/"
	}
	role := f.seed(name, f.roleARN(path, name), path, in.Tags)
	role.AssumeRolePolicyDocument = aws.String(trust)
	role.Description = in.Description
	role.MaxSessionDuration = in.MaxSessionDuration
	if role.MaxSessionDuration == nil {
		role.MaxSessionDuration = aws.Int32(3600)
	}
	if in.PermissionsBoundary != nil {
		role.PermissionsBoundary = &iamtypes.AttachedPermissionsBoundary{
			PermissionsBoundaryArn:  in.PermissionsBoundary,
			PermissionsBoundaryType: iamtypes.PermissionsBoundaryAttachmentTypePolicy,
		}
	}
	return &iam.CreateRoleOutput{Role: encodedRole(role)}, nil
}

// GetRole returns a copy of the named role, its trust policy URL-encoded.
func (f *IAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.GetReturnsErr != nil {
		return nil, f.GetReturnsErr
	}
	name := aws.ToString(in.RoleName)
	if f.RefusedReads[name] {
		return nil, iamAccessDenied("GetRole", name)
	}
	if _, ok := f.Roles[name]; !ok && f.AbsentReadsDenied {
		return nil, iamAccessDenied("GetRole", name)
	}
	role, err := f.role("GetRole", name)
	if err != nil {
		return nil, err
	}
	return &iam.GetRoleOutput{Role: encodedRole(role)}, nil
}

// DeleteRole deletes a role that holds no managed or inline policy.
func (f *IAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.DeleteReturnsErr != nil {
		return nil, f.DeleteReturnsErr
	}
	name := aws.ToString(in.RoleName)
	if _, err := f.role("DeleteRole", name); err != nil {
		return nil, err
	}
	if len(f.Attached[name]) > 0 {
		return nil, &iamtypes.DeleteConflictException{Message: aws.String("Cannot delete entity, must detach all policies first.")}
	}
	if len(f.Inline[name]) > 0 {
		return nil, &iamtypes.DeleteConflictException{Message: aws.String("Cannot delete entity, must delete policies first.")}
	}
	delete(f.Roles, name)
	delete(f.Attached, name)
	delete(f.Inline, name)
	return &iam.DeleteRoleOutput{}, nil
}

// TagRole adds the input's tags to the role, replacing the value of any key it
// already carries.
func (f *IAM) TagRole(_ context.Context, in *iam.TagRoleInput, _ ...func(*iam.Options)) (*iam.TagRoleOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	f.TagCalls = append(f.TagCalls, *in)
	role, err := f.role("TagRole", aws.ToString(in.RoleName))
	if err != nil {
		return nil, err
	}
	for _, tag := range in.Tags {
		replaced := false
		for i := range role.Tags {
			if aws.ToString(role.Tags[i].Key) == aws.ToString(tag.Key) {
				role.Tags[i].Value = tag.Value
				replaced = true
			}
		}
		if !replaced {
			role.Tags = append(role.Tags, tag)
		}
	}
	return &iam.TagRoleOutput{}, nil
}

// UpdateAssumeRolePolicy replaces a role's trust policy.
func (f *IAM) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	f.TrustUpdates++
	if f.UpdateAssumeErr != nil {
		return nil, f.UpdateAssumeErr
	}
	f.UpdateAssumeCalls = append(f.UpdateAssumeCalls, *in)
	name := aws.ToString(in.RoleName)
	role, err := f.role("UpdateAssumeRolePolicy", name)
	if err != nil {
		return nil, err
	}
	doc := aws.ToString(in.PolicyDocument)
	if err := checkTrustPolicy(name, doc); err != nil {
		return nil, err
	}
	role.AssumeRolePolicyDocument = aws.String(doc)
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

// AttachRolePolicy attaches a managed policy to a role. Attaching one the role
// already holds succeeds and changes nothing, as in IAM.
func (f *IAM) AttachRolePolicy(_ context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	arn := aws.ToString(in.PolicyArn)
	if err := f.AttachReturnsErr[arn]; err != nil {
		return nil, err
	}
	f.AttachCalls = append(f.AttachCalls, *in)
	name := aws.ToString(in.RoleName)
	if _, err := f.role("AttachRolePolicy", name); err != nil {
		return nil, err
	}
	f.attach(name, arn)
	return &iam.AttachRolePolicyOutput{}, nil
}

// DetachRolePolicy detaches a managed policy the role holds.
func (f *IAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.DetachReturnsErr != nil {
		return nil, f.DetachReturnsErr
	}
	f.DetachCalls = append(f.DetachCalls, *in)
	name, arn := aws.ToString(in.RoleName), aws.ToString(in.PolicyArn)
	if _, err := f.role("DetachRolePolicy", name); err != nil {
		return nil, err
	}
	if _, ok := f.Attached[name][arn]; !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("Policy " + arn + " was not found.")}
	}
	delete(f.Attached[name], arn)
	return &iam.DetachRolePolicyOutput{}, nil
}

// ListAttachedRolePolicies lists a role's managed policies in ARN order, a
// page at a time.
func (f *IAM) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	f.ListCalls++
	if f.ListReturnsErr != nil {
		return nil, f.ListReturnsErr
	}
	name := aws.ToString(in.RoleName)
	if _, err := f.role("ListAttachedRolePolicies", name); err != nil {
		return nil, err
	}
	arns, next := page(sortedKeys(f.Attached[name]), aws.ToString(in.Marker), pageSize(f.PageBoundary, in.MaxItems))
	out := &iam.ListAttachedRolePoliciesOutput{AttachedPolicies: make([]iamtypes.AttachedPolicy, 0, len(arns))}
	for _, arn := range arns {
		out.AttachedPolicies = append(out.AttachedPolicies, iamtypes.AttachedPolicy{
			PolicyArn:  aws.String(arn),
			PolicyName: aws.String(arn[strings.LastIndex(arn, "/")+1:]),
		})
	}
	if next != "" {
		out.IsTruncated, out.Marker = true, aws.String(next)
	}
	return out, nil
}

// GetRolePolicy returns one of a role's inline policies, its document
// URL-encoded.
func (f *IAM) GetRolePolicy(_ context.Context, in *iam.GetRolePolicyInput, _ ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	roleName, policyName := aws.ToString(in.RoleName), aws.ToString(in.PolicyName)
	if err := f.GetInlineReturnsErr[policyName]; err != nil {
		return nil, err
	}
	if _, err := f.role("GetRolePolicy", roleName); err != nil {
		return nil, err
	}
	doc, ok := f.Inline[roleName][policyName]
	if !ok {
		return nil, noSuchRolePolicy(roleName, policyName)
	}
	return &iam.GetRolePolicyOutput{
		RoleName:       in.RoleName,
		PolicyName:     in.PolicyName,
		PolicyDocument: aws.String(urlEncoded(doc)),
	}, nil
}

func noSuchRolePolicy(roleName, policyName string) error {
	return &iamtypes.NoSuchEntityException{Message: aws.String("The role policy with name " + policyName + " cannot be found on role " + roleName + ".")}
}

// PutRolePolicy writes one of a role's inline policies, replacing a policy of
// the same name.
func (f *IAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	f.PutInlineCalls = append(f.PutInlineCalls, *in)
	roleName, policyName := aws.ToString(in.RoleName), aws.ToString(in.PolicyName)
	if err := f.PutInlineReturnsErr[policyName]; err != nil {
		return nil, err
	}
	if err := checkName("policyName", policyName, PolicyNameMaxLen); err != nil {
		return nil, err
	}
	if _, err := f.role("PutRolePolicy", roleName); err != nil {
		return nil, err
	}
	doc := aws.ToString(in.PolicyDocument)
	if !isJSONObject(doc) {
		return nil, &iamtypes.MalformedPolicyDocumentException{Message: aws.String("Syntax errors in policy.")}
	}
	total := policySize(doc)
	for name, held := range f.Inline[roleName] {
		if name != policyName {
			total += policySize(held)
		}
	}
	if total > InlinePolicyMaxChars {
		return nil, &iamtypes.LimitExceededException{Message: aws.String(fmt.Sprintf(
			"Maximum policy size of %d bytes exceeded for role %s (inline policies would total %d characters)", InlinePolicyMaxChars, roleName, total))}
	}
	if f.Inline[roleName] == nil {
		f.Inline[roleName] = map[string]string{}
	}
	f.Inline[roleName][policyName] = doc
	f.Puts++
	return &iam.PutRolePolicyOutput{}, nil
}

// DeleteRolePolicy deletes one of a role's inline policies.
func (f *IAM) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	f.DeleteInlineCalls = append(f.DeleteInlineCalls, *in)
	roleName, policyName := aws.ToString(in.RoleName), aws.ToString(in.PolicyName)
	if err := f.DeleteInlineReturnsErr[policyName]; err != nil {
		return nil, err
	}
	if _, err := f.role("DeleteRolePolicy", roleName); err != nil {
		return nil, err
	}
	if _, ok := f.Inline[roleName][policyName]; !ok {
		return nil, noSuchRolePolicy(roleName, policyName)
	}
	delete(f.Inline[roleName], policyName)
	return &iam.DeleteRolePolicyOutput{}, nil
}

// ListRolePolicies lists a role's inline policy names in order, a page at a
// time.
func (f *IAM) ListRolePolicies(_ context.Context, in *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.ListInlineReturnsErr != nil {
		return nil, f.ListInlineReturnsErr
	}
	roleName := aws.ToString(in.RoleName)
	if _, err := f.role("ListRolePolicies", roleName); err != nil {
		return nil, err
	}
	names, next := page(sortedKeys(f.Inline[roleName]), aws.ToString(in.Marker), pageSize(f.InlinePageSize, in.MaxItems))
	out := &iam.ListRolePoliciesOutput{PolicyNames: names}
	if next != "" {
		out.IsTruncated, out.Marker = true, aws.String(next)
	}
	return out, nil
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pageSize is how many items one list call returns: the knob when set, IAM's
// default otherwise, and never more than the caller's MaxItems.
func pageSize(knob int, maxItems *int32) int {
	size := listDefaultMaxItems
	if knob > 0 {
		size = knob
	}
	if maxItems != nil && *maxItems > 0 && int(*maxItems) < size {
		size = int(*maxItems)
	}
	return size
}

// page returns the sorted keys after marker, at most size of them, and the
// marker for the page after: the last key returned, or "" on the last page.
// Resuming after the marker's key rather than at an offset is what keeps a
// loop that removes each page's items before asking for the next from
// skipping any.
func page(keys []string, marker string, size int) ([]string, string) {
	start := 0
	if marker != "" {
		start = sort.Search(len(keys), func(i int) bool { return keys[i] > marker })
	}
	if start+size >= len(keys) {
		return keys[start:], ""
	}
	out := keys[start : start+size]
	return out, out[len(out)-1]
}
