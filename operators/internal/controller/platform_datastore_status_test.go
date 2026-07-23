/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"testing"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// TestDatastoreStatuses_DeterministicIdentity proves each declared datastore is
// reported with the ARN and (for the deterministic kinds) the connection
// endpoint composed from the <env>-<platform>-<datastore> convention, and that a
// FIFO queue's status name carries the .fifo suffix while Aurora/cache/stream
// leave their generated-id endpoints empty.
func TestDatastoreStatuses_DeterministicIdentity(t *testing.T) {
	fifo := true
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "kv", Kind: platformv1alpha1.DatastoreKeyValue},
		platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore},
		platformv1alpha1.DatastoreSpec{Name: "q", Kind: platformv1alpha1.DatastoreQueue},
		platformv1alpha1.DatastoreSpec{Name: "qf", Kind: platformv1alpha1.DatastoreQueue, Queue: &platformv1alpha1.QueueConfig{FIFO: &fifo}},
		platformv1alpha1.DatastoreSpec{Name: "ca", Kind: platformv1alpha1.DatastoreCache},
		platformv1alpha1.DatastoreSpec{Name: "st", Kind: platformv1alpha1.DatastoreStream},
	)

	sts := datastoreStatuses(p, "development", testScope(), "Ready")
	if len(sts) != 7 {
		t.Fatalf("got %d statuses want 7", len(sts))
	}
	by := map[string]platformv1alpha1.DatastoreStatus{}
	for _, s := range sts {
		by[s.Name] = s
		if s.Phase != "Ready" {
			t.Errorf("%s phase: got %q want Ready", s.Name, s.Phase)
		}
	}

	if by["obj"].ARN != "arn:aws:s3:::development-myplat-obj-123456789012" || by["obj"].Endpoint != "development-myplat-obj-123456789012" {
		t.Errorf("objectStore identity wrong: %+v", by["obj"])
	}
	if by["kv"].ARN != "arn:aws:dynamodb:us-west-2:123456789012:table/development-myplat-kv" || by["kv"].Endpoint != "development-myplat-kv" {
		t.Errorf("keyValue identity wrong: %+v", by["kv"])
	}
	if by["q"].ARN != "arn:aws:sqs:us-west-2:123456789012:development-myplat-q" ||
		by["q"].Endpoint != "https://sqs.us-west-2.amazonaws.com/123456789012/development-myplat-q" {
		t.Errorf("queue identity wrong: %+v", by["q"])
	}
	if by["qf"].ARN != "arn:aws:sqs:us-west-2:123456789012:development-myplat-qf.fifo" {
		t.Errorf("FIFO queue must carry the .fifo suffix: %+v", by["qf"])
	}
	if by["ca"].ARN != "arn:aws:elasticache:us-west-2:123456789012:replicationgroup:development-myplat-ca" || by["ca"].Endpoint != "" {
		t.Errorf("cache identity wrong (endpoint must be empty — generated id): %+v", by["ca"])
	}
	if by["st"].ARN != "arn:aws:kafka:us-west-2:123456789012:cluster/development-myplat-st" || by["st"].Endpoint != "" {
		t.Errorf("stream identity wrong (endpoint must be empty — generated id): %+v", by["st"])
	}
	if by["db"].ARN != "arn:aws:rds:us-west-2:123456789012:cluster:development-myplat-db" || by["db"].Endpoint != "" || by["db"].SecretName != "" {
		t.Errorf("relational identity wrong (endpoint/secret resolved out-of-band): %+v", by["db"])
	}
}

// TestDatastoreStatuses_EmptyWhenNone proves a Platform with no datastores
// reports a nil list (clears any prior status).
func TestDatastoreStatuses_EmptyWhenNone(t *testing.T) {
	if got := datastoreStatuses(platformWithDatastores("myplat"), "development", testScope(), "Ready"); got != nil {
		t.Errorf("no datastores must report nil, got %+v", got)
	}
}
