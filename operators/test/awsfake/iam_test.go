/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"

	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

const (
	trustDoc  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"pods.eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`
	policyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"bedrock:InvokeModel","Resource":"*"}]}`
	roleName  = "c-analytics-tenant"
)

// sizedPolicy is a permissions policy of exactly n characters, white space
// excluded, carrying pad spaces between its members that IAM does not count.
// Its one statement's Sid takes up the length.
func sizedPolicy(n, pad int) string {
	return sized(n, pad, `"Effect":"Deny","Action":"bedrock:InvokeModel","Resource":"*"`)
}

// sizedTrust is a trust policy of exactly n characters, white space excluded,
// carrying pad spaces that IAM does not count.
func sizedTrust(n, pad int) string {
	return sized(n, pad, `"Effect":"Allow","Principal":{"Service":"pods.eks.amazonaws.com"},"Action":"sts:AssumeRole"`)
}

// sized is a policy document of exactly n characters, white space excluded,
// with members in its one statement and pad spaces after its Version.
func sized(n, pad int, members string) string {
	head, body, tail := `{"Version":"2012-10-17",`, `"Statement":[{`+members+`,"Sid":"`, `"}]}`
	return head + strings.Repeat(" ", pad) + body + strings.Repeat("x", n-len(head)-len(body)-len(tail)) + tail
}

// apiCode is the error code an SDK error carries, "" for none.
func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// is reports whether err is, or wraps, an error of type *T.
func is[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

func createRole(ctx context.Context, f *awsfake.IAM, name, trust string) error {
	_, err := f.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		Path:                     aws.String("/eks-agent-platform/tenants/"),
		AssumeRolePolicyDocument: aws.String(trust),
	})
	return err
}

func putPolicy(ctx context.Context, f *awsfake.IAM, role, name, doc string) error {
	_, err := f.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String(role), PolicyName: aws.String(name), PolicyDocument: aws.String(doc),
	})
	return err
}

// withRole is a fake holding roleName, created as the operator creates one.
func withRole(t *testing.T) *awsfake.IAM {
	t.Helper()
	f := awsfake.NewIAM()
	if err := createRole(context.Background(), f, roleName, trustDoc); err != nil {
		t.Fatalf("create %s: %v", roleName, err)
	}
	return f
}

// mustPut writes an inline policy onto roleName that the test needs in place.
func mustPut(t *testing.T, f *awsfake.IAM, name, doc string) {
	t.Helper()
	if err := putPolicy(context.Background(), f, roleName, name, doc); err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
}

// TestIAMRefusesWhatIAMRefuses is every refusal the fake models, each beside a
// call one step inside the same rule that IAM accepts, so a check that refused
// everything would fail here too.
func TestIAMRefusesWhatIAMRefuses(t *testing.T) {
	ctx := context.Background()
	noSuchEntity := func(err error) bool { return is[*iamtypes.NoSuchEntityException](err) }
	accessDenied := func(err error) bool { return apiCode(err) == "AccessDenied" }
	validation := func(err error) bool { return apiCode(err) == "ValidationError" }
	malformed := func(err error) bool { return is[*iamtypes.MalformedPolicyDocumentException](err) }
	limit := func(err error) bool { return is[*iamtypes.LimitExceededException](err) }
	conflict := func(err error) bool { return is[*iamtypes.DeleteConflictException](err) }

	cases := []struct {
		name string
		// absent starts the fake with no role; otherwise it holds roleName.
		absent bool
		setup  func(*testing.T, *awsfake.IAM)
		call   func(*awsfake.IAM) error
		// want classifies the error; nil wants success.
		want func(error) bool
	}{
		// CreateRole
		{
			name: "CreateRole: a 65-character name", absent: true,
			call: func(f *awsfake.IAM) error { return createRole(ctx, f, strings.Repeat("r", 65), trustDoc) },
			want: validation,
		},
		{
			name: "CreateRole: a 64-character name", absent: true,
			call: func(f *awsfake.IAM) error { return createRole(ctx, f, strings.Repeat("r", 64), trustDoc) },
		},
		{
			name: "CreateRole: a name a role already holds",
			call: func(f *awsfake.IAM) error { return createRole(ctx, f, roleName, trustDoc) },
			want: func(err error) bool { return is[*iamtypes.EntityAlreadyExistsException](err) },
		},
		{
			name: "CreateRole: a trust policy that is not JSON", absent: true,
			call: func(f *awsfake.IAM) error { return createRole(ctx, f, roleName, `{"Version":"2012-10-17",`) },
			want: malformed,
		},
		{
			name: "CreateRole: a trust policy that is JSON but not an object", absent: true,
			call: func(f *awsfake.IAM) error { return createRole(ctx, f, roleName, `["sts:AssumeRole"]`) },
			want: malformed,
		},
		{
			name: "CreateRole: a trust policy of JSON null", absent: true,
			call: func(f *awsfake.IAM) error { return createRole(ctx, f, roleName, "null") },
			want: malformed,
		},
		{
			name: "CreateRole: a trust policy whose statement names no principal", absent: true,
			call: func(f *awsfake.IAM) error {
				return createRole(ctx, f, roleName, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sts:AssumeRole"}]}`)
			},
			want: malformed,
		},
		{
			name: "CreateRole: a trust policy whose statement names a resource", absent: true,
			call: func(f *awsfake.IAM) error {
				return createRole(ctx, f, roleName, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",`+
					`"Principal":{"Service":"pods.eks.amazonaws.com"},"Action":"sts:AssumeRole","Resource":"*"}]}`)
			},
			want: malformed,
		},
		{
			name: "CreateRole: a trust policy with one statement object rather than an array", absent: true,
			call: func(f *awsfake.IAM) error {
				return createRole(ctx, f, roleName, `{"Version":"2012-10-17","Statement":{"Effect":"Allow",`+
					`"Principal":{"Service":"pods.eks.amazonaws.com"},"Action":"sts:AssumeRole"}}`)
			},
		},
		{
			name: "CreateRole: a trust policy one character over the quota", absent: true,
			call: func(f *awsfake.IAM) error {
				return createRole(ctx, f, roleName, sizedTrust(awsfake.TrustPolicyMaxChars+1, 0))
			},
			want: limit,
		},
		{
			name: "CreateRole: a trust policy at the quota, white space not counted", absent: true,
			call: func(f *awsfake.IAM) error {
				return createRole(ctx, f, roleName, sizedTrust(awsfake.TrustPolicyMaxChars, 500))
			},
		},

		// UpdateAssumeRolePolicy
		{
			name: "UpdateAssumeRolePolicy: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String(roleName), PolicyDocument: aws.String(trustDoc)})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "UpdateAssumeRolePolicy: a document that is not JSON",
			call: func(f *awsfake.IAM) error {
				_, err := f.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String(roleName), PolicyDocument: aws.String("{")})
				return err
			},
			want: malformed,
		},
		{
			name: "UpdateAssumeRolePolicy: a document over the quota",
			call: func(f *awsfake.IAM) error {
				_, err := f.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
					RoleName: aws.String(roleName), PolicyDocument: aws.String(sizedTrust(awsfake.TrustPolicyMaxChars+1, 0)),
				})
				return err
			},
			want: limit,
		},
		{
			name: "UpdateAssumeRolePolicy: a document at the quota",
			call: func(f *awsfake.IAM) error {
				_, err := f.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
					RoleName: aws.String(roleName), PolicyDocument: aws.String(sizedTrust(awsfake.TrustPolicyMaxChars, 0)),
				})
				return err
			},
		},

		// PutRolePolicy
		{
			name: "PutRolePolicy: an absent role", absent: true,
			call: func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, "p", policyDoc) },
			want: noSuchEntity,
		},
		{
			name: "PutRolePolicy: a 129-character policy name",
			call: func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, strings.Repeat("p", 129), policyDoc) },
			want: validation,
		},
		{
			name: "PutRolePolicy: a 128-character policy name",
			call: func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, strings.Repeat("p", 128), policyDoc) },
		},
		{
			name: "PutRolePolicy: a document that is not JSON",
			call: func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, "p", `{"Statement":[}`) },
			want: malformed,
		},
		{
			name: "PutRolePolicy: a document with no Statement",
			call: func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17"}`) },
			want: malformed,
		},
		{
			name: "PutRolePolicy: a Statement of JSON null",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":null}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: a Version IAM does not accept",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-18","Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"*"}]}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: no Version, which IAM reads as 2008-10-17",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"*"}]}`)
			},
		},
		{
			name: "PutRolePolicy: an Effect in the wrong case",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":[{"Effect":"deny","Action":"s3:*","Resource":"*"}]}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: a statement with no Effect",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":[{"Action":"s3:*","Resource":"*"}]}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: a statement with no Action or NotAction",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Resource":"*"}]}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: an Action that is neither a string nor strings",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":7,"Resource":"*"}]}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: a statement with no Resource or NotResource",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:*"}]}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: a statement naming a principal",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":[{"Effect":"Deny",`+
					`"Principal":"*","Action":"s3:*","Resource":"*"}]}`)
			},
			want: malformed,
		},
		{
			name: "PutRolePolicy: NotAction and NotResource, with an array of actions beside them",
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "p", `{"Version":"2012-10-17","Statement":[`+
					`{"Effect":"Deny","NotAction":"bedrock:*","NotResource":"arn:aws:bedrock:*::foundation-model/*"},`+
					`{"Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:Converse"],"Resource":["*"]}]}`)
			},
		},
		{
			name:  "PutRolePolicy: a role's inline policies one character over the quota together",
			setup: func(t *testing.T, f *awsfake.IAM) { mustPut(t, f, "first", sizedPolicy(6000, 0)) },
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "second", sizedPolicy(awsfake.InlinePolicyMaxChars-6000+1, 0))
			},
			want: limit,
		},
		{
			name:  "PutRolePolicy: a role's inline policies at the quota together, white space not counted",
			setup: func(t *testing.T, f *awsfake.IAM) { mustPut(t, f, "first", sizedPolicy(6000, 300)) },
			call: func(f *awsfake.IAM) error {
				return putPolicy(ctx, f, roleName, "second", sizedPolicy(awsfake.InlinePolicyMaxChars-6000, 300))
			},
		},
		{
			name:  "PutRolePolicy: a replacement counts in place of the policy it replaces",
			setup: func(t *testing.T, f *awsfake.IAM) { mustPut(t, f, "first", sizedPolicy(9000, 0)) },
			call:  func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, "first", sizedPolicy(10000, 0)) },
		},
		{
			name: "PutRolePolicy: a replacement that takes the role over the quota",
			setup: func(t *testing.T, f *awsfake.IAM) {
				mustPut(t, f, "first", sizedPolicy(6000, 0))
				mustPut(t, f, "second", sizedPolicy(4000, 0))
			},
			call: func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, "first", sizedPolicy(6300, 0)) },
			want: limit,
		},

		// Calls naming an absent role.
		{
			name: "GetRole: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "GetRole: an absent role, AbsentReadsDenied", absent: true,
			setup: func(_ *testing.T, f *awsfake.IAM) { f.AbsentReadsDenied = true },
			call: func(f *awsfake.IAM) error {
				_, err := f.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
				return err
			},
			want: accessDenied,
		},
		{
			name:  "GetRole: a present role, AbsentReadsDenied",
			setup: func(_ *testing.T, f *awsfake.IAM) { f.AbsentReadsDenied = true },
			call: func(f *awsfake.IAM) error {
				_, err := f.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
				return err
			},
		},
		{
			name:  "GetRole: a present role named in RefusedReads",
			setup: func(_ *testing.T, f *awsfake.IAM) { f.RefusedReads = map[string]bool{roleName: true} },
			call: func(f *awsfake.IAM) error {
				_, err := f.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
				return err
			},
			want: accessDenied,
		},
		{
			name: "PutRolePolicy: an absent role matching DeniedAbsentSuffix", absent: true,
			setup: func(_ *testing.T, f *awsfake.IAM) { f.DeniedAbsentSuffix = "-tenant" },
			call:  func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, "p", policyDoc) },
			want:  accessDenied,
		},
		{
			name:  "PutRolePolicy: a present role matching DeniedAbsentSuffix",
			setup: func(_ *testing.T, f *awsfake.IAM) { f.DeniedAbsentSuffix = "-tenant" },
			call:  func(f *awsfake.IAM) error { return putPolicy(ctx, f, roleName, "p", policyDoc) },
		},
		{
			name: "TagRole: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(roleName), Tags: []iamtypes.Tag{{Key: aws.String("k"), Value: aws.String("v")}}})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "AttachRolePolicy: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{RoleName: aws.String(roleName), PolicyArn: aws.String("arn:aws:iam::aws:policy/X")})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "ListAttachedRolePolicies: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName)})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "ListRolePolicies: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(roleName)})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "GetRolePolicy: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String("p")})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "GetRolePolicy: a policy the role does not hold",
			call: func(f *awsfake.IAM) error {
				_, err := f.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String("p")})
				return err
			},
			want: noSuchEntity,
		},

		// Removing what is not there.
		{
			name: "DetachRolePolicy: an ARN the role does not hold",
			call: func(f *awsfake.IAM) error {
				_, err := f.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(roleName), PolicyArn: aws.String("arn:aws:iam::aws:policy/X")})
				return err
			},
			want: noSuchEntity,
		},
		{
			name:  "DetachRolePolicy: an ARN the role holds",
			setup: func(_ *testing.T, f *awsfake.IAM) { f.SeedAttachment(roleName, "arn:aws:iam::aws:policy/X") },
			call: func(f *awsfake.IAM) error {
				_, err := f.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(roleName), PolicyArn: aws.String("arn:aws:iam::aws:policy/X")})
				return err
			},
		},
		{
			name: "DeleteRolePolicy: a policy the role does not hold",
			call: func(f *awsfake.IAM) error {
				_, err := f.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String("p")})
				return err
			},
			want: noSuchEntity,
		},
		{
			name: "DeleteRolePolicy: a policy on an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String("p")})
				return err
			},
			want: noSuchEntity,
		},

		// DeleteRole
		{
			name: "DeleteRole: an absent role", absent: true,
			call: func(f *awsfake.IAM) error {
				_, err := f.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
				return err
			},
			want: noSuchEntity,
		},
		{
			name:  "DeleteRole: a role still holding a managed policy",
			setup: func(_ *testing.T, f *awsfake.IAM) { f.SeedAttachment(roleName, "arn:aws:iam::aws:policy/X") },
			call: func(f *awsfake.IAM) error {
				_, err := f.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
				return err
			},
			want: conflict,
		},
		{
			name:  "DeleteRole: a role still holding an inline policy",
			setup: func(t *testing.T, f *awsfake.IAM) { mustPut(t, f, "p", policyDoc) },
			call: func(f *awsfake.IAM) error {
				_, err := f.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
				return err
			},
			want: conflict,
		},
		{
			name: "DeleteRole: a role holding no policy",
			call: func(f *awsfake.IAM) error {
				_, err := f.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
				return err
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := awsfake.NewIAM()
			if !c.absent {
				f = withRole(t)
			}
			if c.setup != nil {
				c.setup(t, f)
			}
			err := c.call(f)
			switch {
			case c.want == nil && err != nil:
				t.Errorf("got %v, want success", err)
			case c.want != nil && !c.want(err):
				t.Errorf("got %v (code %q), want the refusal IAM answers with", err, apiCode(err))
			}
		})
	}
}

// TestIAMRefusalsChangeNothing: a refused write leaves the state as it was.
func TestIAMRefusalsChangeNothing(t *testing.T) {
	ctx := context.Background()
	f := withRole(t)
	if err := putPolicy(ctx, f, roleName, "p", policyDoc); err != nil {
		t.Fatal(err)
	}
	if err := putPolicy(ctx, f, roleName, "p", "{"); err == nil {
		t.Fatal("a malformed replacement was accepted")
	}
	if err := createRole(ctx, f, roleName, `{"Version":"2012-10-17","Statement":[]}`); err == nil {
		t.Fatal("a second role of the name was accepted")
	}
	if doc, _ := f.InlinePolicy(roleName, "p"); doc != policyDoc {
		t.Errorf("a refused PutRolePolicy changed the policy to %q", doc)
	}
	if got := aws.ToString(f.Roles[roleName].AssumeRolePolicyDocument); got != trustDoc {
		t.Errorf("a refused CreateRole changed the trust policy to %q", got)
	}
	if f.PutCount() != 1 || len(f.PutInlineCalls) != 2 || len(f.CreateCalls) != 2 {
		t.Errorf("Puts %d, PutInlineCalls %d, CreateCalls %d; want 1 stored, every attempt recorded (2, 2)",
			f.PutCount(), len(f.PutInlineCalls), len(f.CreateCalls))
	}
}

// TestIAMReturnsDocumentsURLEncoded: GetRole and GetRolePolicy return their
// documents percent-encoded per RFC 3986, as IAM does, while the state holds
// them as written.
func TestIAMReturnsDocumentsURLEncoded(t *testing.T) {
	ctx := context.Background()
	const (
		spacedTrust  = `{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": {"AWS": "arn:aws:iam::123456789012:role/a+b"}, "Action": "sts:AssumeRole"}]}`
		spacedPolicy = `{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::b/a+b/*"}]}`
	)
	f := awsfake.NewIAM()
	created, err := f.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String(roleName), AssumeRolePolicyDocument: aws.String(spacedTrust)})
	if err != nil {
		t.Fatal(err)
	}
	if err := putPolicy(ctx, f, roleName, "p", spacedPolicy); err != nil {
		t.Fatal(err)
	}
	role, err := f.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := f.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String("p")})
	if err != nil {
		t.Fatal(err)
	}
	for what, c := range map[string]struct{ got, written string }{
		"CreateRole trust policy": {aws.ToString(created.Role.AssumeRolePolicyDocument), spacedTrust},
		"GetRole trust policy":    {aws.ToString(role.Role.AssumeRolePolicyDocument), spacedTrust},
		"GetRolePolicy document":  {aws.ToString(pol.PolicyDocument), spacedPolicy},
	} {
		got := c.got
		if strings.ContainsAny(got, `{}" +:`) {
			t.Errorf("%s %q is not percent-encoded", what, got)
		}
		if !strings.Contains(got, "%20") || !strings.Contains(got, "%2B") {
			t.Errorf("%s %q does not spell a space %%20 and a plus %%2B", what, got)
		}
		if decoded, err := url.QueryUnescape(got); err != nil || decoded != c.written {
			t.Errorf("%s decodes to %q (%v), want the document as written", what, decoded, err)
		}
	}
	if got := aws.ToString(f.Roles[roleName].AssumeRolePolicyDocument); got != spacedTrust {
		t.Errorf("stored trust policy %q, want it as written", got)
	}
	if doc, _ := f.InlinePolicy(roleName, "p"); doc != spacedPolicy {
		t.Errorf("stored inline policy %q, want it as written", doc)
	}
}

// TestIAMTagRoleAppliesTags: TagRole adds a new key and replaces the value of
// one the role carries, and GetRole reads both back.
func TestIAMTagRoleAppliesTags(t *testing.T) {
	ctx := context.Background()
	f := awsfake.NewIAM()
	f.SeedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName,
		iamtypes.Tag{Key: aws.String("ManagedBy"), Value: aws.String("eks-agent-platform")},
		iamtypes.Tag{Key: aws.String("platform.nanohype.dev/suspended"), Value: aws.String("false")})
	if _, err := f.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(roleName), Tags: []iamtypes.Tag{
		{Key: aws.String("platform.nanohype.dev/suspended"), Value: aws.String("true")},
		{Key: aws.String("platform.nanohype.dev/suspended-reason"), Value: aws.String("budget-exceeded")},
	}}); err != nil {
		t.Fatal(err)
	}
	out, err := f.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tag := range out.Role.Tags {
		got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	want := map[string]string{
		"ManagedBy":                              "eks-agent-platform",
		"platform.nanohype.dev/suspended":        "true",
		"platform.nanohype.dev/suspended-reason": "budget-exceeded",
	}
	if !reflect.DeepEqual(got, want) || len(out.Role.Tags) != len(want) {
		t.Errorf("tags after TagRole = %v, want %v", out.Role.Tags, want)
	}
	if len(f.TagCalls) != 1 {
		t.Errorf("TagCalls = %d, want 1", len(f.TagCalls))
	}
}

// TestIAMRoleARNCarriesThePath: a role's ARN names its path, as IAM's does,
// and a seeded role reads its path back out of the ARN it was given.
func TestIAMRoleARNCarriesThePath(t *testing.T) {
	ctx := context.Background()
	f := awsfake.NewIAM()
	out, err := f.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("c-a-tenant"), Path: aws.String("/eks-agent-platform/tenants/"), AssumeRolePolicyDocument: aws.String(trustDoc),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(out.Role.Arn); got != "arn:aws:iam::123456789012:role/eks-agent-platform/tenants/c-a-tenant" {
		t.Errorf("ARN under the tenant path = %q", got)
	}
	root, err := f.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String("c-a-scheduler-invoke"), AssumeRolePolicyDocument: aws.String(trustDoc)})
	if err != nil {
		t.Fatal(err)
	}
	if got, path := aws.ToString(root.Role.Arn), aws.ToString(root.Role.Path); got != "arn:aws:iam::123456789012:role/c-a-scheduler-invoke" || path != "/" {
		t.Errorf("a role created with no path: ARN %q, path %q, want the root", got, path)
	}
	for arn, wantPath := range map[string]string{
		"arn:aws:iam::123456789012:role/x":                            "/",
		"arn:aws:iam::123456789012:role/eks-agent-platform/tenants/x": "/eks-agent-platform/tenants/",
	} {
		if got := aws.ToString(f.SeedRole("x", arn).Path); got != wantPath {
			t.Errorf("SeedRole(%s) path = %q, want %q", arn, got, wantPath)
		}
	}
	if got := aws.ToString(f.SeedRoleAtPath("/p/", "y").Arn); got != f.RoleARN("/p/", "y") || got != "arn:aws:iam::123456789012:role/p/y" {
		t.Errorf("SeedRoleAtPath ARN = %q", got)
	}
}

// TestIAMPaginationResumesAfterTheMarker: removing each page's items before
// asking for the next, as a finalizer's detach loop does, skips none, and the
// role then deletes cleanly.
func TestIAMPaginationResumesAfterTheMarker(t *testing.T) {
	ctx := context.Background()
	f := withRole(t)
	f.PageBoundary, f.InlinePageSize = 2, 2
	for i := 0; i < 5; i++ {
		f.SeedAttachment(roleName, fmt.Sprintf("arn:aws:iam::123456789012:policy/p%d", i))
		if err := putPolicy(ctx, f, roleName, fmt.Sprintf("inline-%d", i), policyDoc); err != nil {
			t.Fatal(err)
		}
	}
	pages := 0
	var marker *string
	for {
		out, err := f.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName), Marker: marker})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, ap := range out.AttachedPolicies {
			if _, err := f.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(roleName), PolicyArn: ap.PolicyArn}); err != nil {
				t.Fatal(err)
			}
		}
		if !out.IsTruncated {
			break
		}
		marker = out.Marker
	}
	if pages != 3 {
		t.Errorf("five attachments at a page size of 2 listed in %d pages, want 3", pages)
	}
	marker = nil
	for {
		out, err := f.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(roleName), Marker: marker})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range out.PolicyNames {
			if _, err := f.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String(name)}); err != nil {
				t.Fatal(err)
			}
		}
		if !out.IsTruncated {
			break
		}
		marker = out.Marker
	}
	if _, err := f.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)}); err != nil {
		t.Errorf("DeleteRole after a paginated teardown: %v; left %v and %v", err, f.AttachmentsFor(roleName), f.Inline[roleName])
	}
}

// TestIAMIsSafeForConcurrentUse drives one fake from many goroutines, the way
// a manager running reconcilers in envtest would.
func TestIAMIsSafeForConcurrentUse(t *testing.T) {
	ctx := context.Background()
	f := awsfake.NewIAM()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("role-%d", i)
			_ = createRole(ctx, f, name, trustDoc)
			_ = putPolicy(ctx, f, name, "p", policyDoc)
			_, _ = f.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
			_ = f.HasRole(name)
		}(i)
	}
	wg.Wait()
	if len(f.Roles) != 16 || f.PutCount() != 16 {
		t.Errorf("roles %d, puts %d after 16 concurrent creates and puts", len(f.Roles), f.PutCount())
	}
}
