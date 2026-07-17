/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

// Shared fixture constants for the controller unit tests. Kept in one place so
// the recurring tenant namespace and platform name aren't re-spelled as bare
// literals across the suite (goconst), and so target-10's agentctl consolidation
// has a single fixture vocabulary to build on.
const (
	ctrlTestNS       = "tenants-x"
	ctrlTestPlatform = "acme"
)
