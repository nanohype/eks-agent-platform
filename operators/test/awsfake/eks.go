/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

var _ awsclients.EKS = (*EKS)(nil)

// EKS is an in-memory awsclients.EKS holding Pod Identity associations by
// (namespace, serviceAccount), the key ListPodIdentityAssociations filters on.
// The list summary carries no role, so the bound role is kept beside it, by
// association id, and returned by DescribePodIdentityAssociation.
//
// It refuses two kinds of call, with EKS's errors: a second association for a
// (namespace, serviceAccount) that already has one gets
// ResourceInUseException, and Describe or Delete naming an association id the
// fake does not hold gets ResourceNotFoundException. The cluster name is not part of the key; every
// association is on the one cluster a reconciler serves.
type EKS struct {
	sync.Mutex

	// Associations maps "namespace/serviceAccount" to the id of the
	// association binding it. An association exists while an entry here names
	// its id.
	Associations map[string]string
	// Bound is what DescribePodIdentityAssociation returns for an association
	// id. CreatePodIdentityAssociation records the role it was handed; a test
	// seeds another role, or a target role, to stand for an association made
	// out of band, and a nil entry for one EKS describes as nothing.
	Bound map[string]*ekstypes.PodIdentityAssociation

	// CreateCalls records every CreatePodIdentityAssociation call past
	// CreateReturnsErr, refused ones included.
	CreateCalls []eks.CreatePodIdentityAssociationInput
	// DeleteCalls records every DeletePodIdentityAssociation call past
	// DeleteReturnsErr, refused ones included.
	DeleteCalls []eks.DeletePodIdentityAssociationInput
	// NextID numbers the associations the fake mints, a-1 first.
	NextID int

	// ListReturnsErr, CreateReturnsErr, DeleteReturnsErr and DescribeReturnsErr
	// fail their call.
	ListReturnsErr     error
	CreateReturnsErr   error
	DeleteReturnsErr   error
	DescribeReturnsErr error
	// FailOnlyFor narrows ListReturnsErr and CreateReturnsErr to calls naming
	// this ServiceAccount, so a test can fail one of a Platform's two
	// associations and not the other. Empty fails every call.
	FailOnlyFor string
}

// NewEKS returns an EKS holding no associations.
func NewEKS() *EKS {
	f := &EKS{}
	f.init()
	return f
}

// init makes the zero EKS usable. Callers hold f's lock.
func (f *EKS) init() {
	if f.Associations == nil {
		f.Associations = map[string]string{}
	}
	if f.Bound == nil {
		f.Bound = map[string]*ekstypes.PodIdentityAssociation{}
	}
}

// failsFor reports whether an injected error applies to a call naming
// serviceAccount.
func (f *EKS) failsFor(serviceAccount *string) bool {
	return f.FailOnlyFor == "" || f.FailOnlyFor == aws.ToString(serviceAccount)
}

// exists reports whether an association of this id is held. Callers hold f's
// lock.
func (f *EKS) exists(id string) bool {
	for _, held := range f.Associations {
		if held == id {
			return true
		}
	}
	return false
}

// mint numbers a new association. Callers hold f's lock.
func (f *EKS) mint() string {
	f.NextID++
	return fmt.Sprintf("a-%d", f.NextID)
}

// SeedAssociation stands for an association made out of band: the
// (namespace, serviceAccount) bound to roleARN, chaining to targetRoleARN when
// that is set. It returns the association's id.
func (f *EKS) SeedAssociation(namespace, serviceAccount, roleARN, targetRoleARN string) string {
	f.Lock()
	defer f.Unlock()
	f.init()
	id := f.mint()
	f.Associations[namespace+"/"+serviceAccount] = id
	a := &ekstypes.PodIdentityAssociation{
		AssociationId:  aws.String(id),
		Namespace:      aws.String(namespace),
		ServiceAccount: aws.String(serviceAccount),
		RoleArn:        aws.String(roleARN),
	}
	if targetRoleARN != "" {
		a.TargetRoleArn = aws.String(targetRoleARN)
	}
	f.Bound[id] = a
	return id
}

// RoleFor returns the role the (namespace, serviceAccount) association names,
// "" when there is none.
func (f *EKS) RoleFor(namespace, serviceAccount string) string {
	role, _ := f.BoundRole(namespace, serviceAccount)
	return role
}

// BoundRole returns the role the (namespace, serviceAccount) association
// names, and whether there is an association at all.
func (f *EKS) BoundRole(namespace, serviceAccount string) (string, bool) {
	f.Lock()
	defer f.Unlock()
	id, ok := f.Associations[namespace+"/"+serviceAccount]
	if !ok {
		return "", false
	}
	if a := f.Bound[id]; a != nil {
		return aws.ToString(a.RoleArn), true
	}
	return "", true
}

// Rebind points the (namespace, serviceAccount) association at another role,
// the way an edit made outside the operator would. It does nothing when there
// is no association to point.
func (f *EKS) Rebind(namespace, serviceAccount, roleARN string) {
	f.Lock()
	defer f.Unlock()
	f.init()
	id, ok := f.Associations[namespace+"/"+serviceAccount]
	if !ok {
		return
	}
	if f.Bound[id] == nil {
		f.Bound[id] = &ekstypes.PodIdentityAssociation{AssociationId: aws.String(id)}
	}
	f.Bound[id].RoleArn = aws.String(roleARN)
}

// copyAssociation is a copy of a held association as EKS returns it.
func copyAssociation(a *ekstypes.PodIdentityAssociation) *ekstypes.PodIdentityAssociation {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// ListPodIdentityAssociations lists the associations for the input's
// namespace and ServiceAccount, each filter applying when set, in key order.
func (f *EKS) ListPodIdentityAssociations(_ context.Context, in *eks.ListPodIdentityAssociationsInput, _ ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.ListReturnsErr != nil && f.failsFor(in.ServiceAccount) {
		return nil, f.ListReturnsErr
	}
	out := &eks.ListPodIdentityAssociationsOutput{}
	for _, key := range sortedKeys(f.Associations) {
		namespace, serviceAccount, _ := strings.Cut(key, "/")
		if in.Namespace != nil && aws.ToString(in.Namespace) != namespace {
			continue
		}
		if in.ServiceAccount != nil && aws.ToString(in.ServiceAccount) != serviceAccount {
			continue
		}
		id := f.Associations[key]
		cluster := in.ClusterName
		if a := f.Bound[id]; a != nil && a.ClusterName != nil {
			cluster = a.ClusterName
		}
		out.Associations = append(out.Associations, ekstypes.PodIdentityAssociationSummary{
			AssociationId:  aws.String(id),
			ClusterName:    cluster,
			Namespace:      aws.String(namespace),
			ServiceAccount: aws.String(serviceAccount),
		})
	}
	return out, nil
}

// CreatePodIdentityAssociation binds a (namespace, serviceAccount) that has no
// association to the input's role.
func (f *EKS) CreatePodIdentityAssociation(_ context.Context, in *eks.CreatePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.CreateReturnsErr != nil && f.failsFor(in.ServiceAccount) {
		return nil, f.CreateReturnsErr
	}
	f.CreateCalls = append(f.CreateCalls, *in)
	key := aws.ToString(in.Namespace) + "/" + aws.ToString(in.ServiceAccount)
	if held, ok := f.Associations[key]; ok {
		return nil, &ekstypes.ResourceInUseException{
			Message:     aws.String("Association already exists: " + held),
			ClusterName: in.ClusterName,
		}
	}
	id := f.mint()
	a := &ekstypes.PodIdentityAssociation{
		AssociationId:  aws.String(id),
		ClusterName:    in.ClusterName,
		Namespace:      in.Namespace,
		ServiceAccount: in.ServiceAccount,
		RoleArn:        in.RoleArn,
		TargetRoleArn:  in.TargetRoleArn,
	}
	f.Associations[key] = id
	f.Bound[id] = a
	return &eks.CreatePodIdentityAssociationOutput{Association: copyAssociation(a)}, nil
}

func noSuchAssociation(id string, cluster *string) error {
	return &ekstypes.ResourceNotFoundException{
		Message:     aws.String("No Pod Identity Association found with id " + id),
		ClusterName: cluster,
	}
}

// DescribePodIdentityAssociation returns what Bound holds for an association
// the fake holds.
func (f *EKS) DescribePodIdentityAssociation(_ context.Context, in *eks.DescribePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DescribePodIdentityAssociationOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.DescribeReturnsErr != nil {
		return nil, f.DescribeReturnsErr
	}
	id := aws.ToString(in.AssociationId)
	if !f.exists(id) {
		return nil, noSuchAssociation(id, in.ClusterName)
	}
	return &eks.DescribePodIdentityAssociationOutput{Association: copyAssociation(f.Bound[id])}, nil
}

// DeletePodIdentityAssociation removes an association the fake holds.
func (f *EKS) DeletePodIdentityAssociation(_ context.Context, in *eks.DeletePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DeletePodIdentityAssociationOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.init()
	if f.DeleteReturnsErr != nil {
		return nil, f.DeleteReturnsErr
	}
	f.DeleteCalls = append(f.DeleteCalls, *in)
	id := aws.ToString(in.AssociationId)
	if !f.exists(id) {
		return nil, noSuchAssociation(id, in.ClusterName)
	}
	for key, held := range f.Associations {
		if held == id {
			delete(f.Associations, key)
		}
	}
	gone := copyAssociation(f.Bound[id])
	delete(f.Bound, id)
	return &eks.DeletePodIdentityAssociationOutput{Association: gone}, nil
}
