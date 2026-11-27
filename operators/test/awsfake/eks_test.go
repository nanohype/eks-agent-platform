/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

const (
	eksCluster = "c"
	eksRole    = "arn:aws:iam::123456789012:role/eks-agent-platform/tenants/c-a-tenant"
)

func createAssociation(ctx context.Context, f *awsfake.EKS, namespace, serviceAccount string) (string, error) {
	out, err := f.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{
		ClusterName: aws.String(eksCluster), Namespace: aws.String(namespace), ServiceAccount: aws.String(serviceAccount), RoleArn: aws.String(eksRole),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Association.AssociationId), nil
}

// TestEKSRefusesWhatEKSRefuses: one association per ServiceAccount, and no
// call on an association id EKS does not hold. Both errors are the SDK's own
// types, so a reconciler's errors.As against them is exercised for real.
func TestEKSRefusesWhatEKSRefuses(t *testing.T) {
	ctx := context.Background()
	inUse := func(err error) bool { return is[*ekstypes.ResourceInUseException](err) }
	notFound := func(err error) bool { return is[*ekstypes.ResourceNotFoundException](err) }
	cases := []struct {
		name string
		call func(*awsfake.EKS, string) error
		want func(error) bool
	}{
		{
			name: "a second association for the same namespace and ServiceAccount",
			call: func(f *awsfake.EKS, _ string) error {
				_, err := createAssociation(ctx, f, "tenants-a", "tenant-runtime")
				return err
			},
			want: inUse,
		},
		{
			name: "an association for another ServiceAccount in the namespace",
			call: func(f *awsfake.EKS, _ string) error {
				_, err := createAssociation(ctx, f, "tenants-a", "a-gateway")
				return err
			},
		},
		{
			name: "an association for the ServiceAccount name in another namespace",
			call: func(f *awsfake.EKS, _ string) error {
				_, err := createAssociation(ctx, f, "tenants-b", "tenant-runtime")
				return err
			},
		},
		{
			name: "describing an unknown association id",
			call: func(f *awsfake.EKS, _ string) error {
				_, err := f.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{ClusterName: aws.String(eksCluster), AssociationId: aws.String("a-404")})
				return err
			},
			want: notFound,
		},
		{
			name: "describing the held association",
			call: func(f *awsfake.EKS, id string) error {
				_, err := f.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{ClusterName: aws.String(eksCluster), AssociationId: aws.String(id)})
				return err
			},
		},
		{
			name: "deleting an unknown association id",
			call: func(f *awsfake.EKS, _ string) error {
				_, err := f.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(eksCluster), AssociationId: aws.String("a-404")})
				return err
			},
			want: notFound,
		},
		{
			name: "deleting the held association twice",
			call: func(f *awsfake.EKS, id string) error {
				in := &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(eksCluster), AssociationId: aws.String(id)}
				if _, err := f.DeletePodIdentityAssociation(ctx, in); err != nil {
					t.Fatalf("first delete: %v", err)
				}
				_, err := f.DeletePodIdentityAssociation(ctx, in)
				return err
			},
			want: notFound,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := awsfake.NewEKS()
			id, err := createAssociation(ctx, f, "tenants-a", "tenant-runtime")
			if err != nil {
				t.Fatal(err)
			}
			err = c.call(f, id)
			switch {
			case c.want == nil && err != nil:
				t.Errorf("got %v, want success", err)
			case c.want != nil && !c.want(err):
				t.Errorf("got %v, want the refusal EKS answers with", err)
			}
		})
	}
}

// TestEKSDescribesTheBinding: the list summary carries no role, Describe
// returns the one the association was created with, and a deleted
// association frees its ServiceAccount for a new one.
func TestEKSDescribesTheBinding(t *testing.T) {
	ctx := context.Background()
	f := awsfake.NewEKS()
	id, err := createAssociation(ctx, f, "tenants-a", "tenant-runtime")
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{
		ClusterName: aws.String(eksCluster), Namespace: aws.String("tenants-a"), ServiceAccount: aws.String("tenant-runtime"),
	})
	if err != nil || len(list.Associations) != 1 || aws.ToString(list.Associations[0].AssociationId) != id {
		t.Fatalf("list = %+v (%v), want the one association %s", list, err, id)
	}
	desc, err := f.DescribePodIdentityAssociation(ctx, &eks.DescribePodIdentityAssociationInput{ClusterName: aws.String(eksCluster), AssociationId: aws.String(id)})
	if err != nil || aws.ToString(desc.Association.RoleArn) != eksRole {
		t.Fatalf("describe = %+v (%v), want the role %s", desc, err, eksRole)
	}
	if role, ok := f.BoundRole("tenants-a", "tenant-runtime"); !ok || role != eksRole || f.RoleFor("tenants-a", "tenant-runtime") != eksRole {
		t.Errorf("BoundRole = (%q, %v), RoleFor = %q, want %s", role, ok, f.RoleFor("tenants-a", "tenant-runtime"), eksRole)
	}
	f.Rebind("tenants-a", "tenant-runtime", "arn:aws:iam::123456789012:role/someone-else")
	if got := f.RoleFor("tenants-a", "tenant-runtime"); got != "arn:aws:iam::123456789012:role/someone-else" {
		t.Errorf("RoleFor after Rebind = %q", got)
	}

	if _, err := f.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{ClusterName: aws.String(eksCluster), AssociationId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.BoundRole("tenants-a", "tenant-runtime"); ok {
		t.Error("the association survived its deletion")
	}
	if _, err := createAssociation(ctx, f, "tenants-a", "tenant-runtime"); err != nil {
		t.Errorf("a ServiceAccount whose association was deleted could not be bound again: %v", err)
	}
}
