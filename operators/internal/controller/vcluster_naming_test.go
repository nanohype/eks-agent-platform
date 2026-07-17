/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// referenceSafeConcatName is a hand-transcription of vcluster's upstream
// pkg/util/translate.SafeConcatName (v0.35.x), kept deliberately separate from
// the production safeConcatName so the two can be diffed. If the production copy
// drifts from vcluster's algorithm, TestSafeConcatName_ByteIdenticalToUpstream
// catches it against this independent reference across a wide input sweep.
//
//	func SafeConcatName(name ...string) string {
//		fullPath := strings.Join(name, "-")
//		if len(fullPath) > 63 {
//			digest := sha256.Sum256([]byte(fullPath))
//			return strings.ReplaceAll(fullPath[0:52]+"-"+hex.EncodeToString(digest[0:])[0:10], ".-", "-")
//		}
//		return fullPath
//	}
func referenceSafeConcatName(name ...string) string {
	fullPath := strings.Join(name, "-")
	if len(fullPath) > 63 {
		digest := sha256.Sum256([]byte(fullPath))
		return strings.ReplaceAll(fullPath[0:52]+"-"+hex.EncodeToString(digest[0:])[0:10], ".-", "-")
	}
	return fullPath
}

// TestSafeConcatName_Golden pins exact bytes for the shapes that matter: an
// un-truncated synced SA name, a truncated one (a maximal 63-char virtual
// namespace), the dot-collapse edge (a prefix ending on a dot), and the exact
// 63-char no-hash boundary. The truncated golden's 10-hex tail comes from
// SHA-256 of the full joined string — copy it wrong and this fails.
func TestSafeConcatName_Golden(t *testing.T) {
	longNs := "tenants-" + strings.Repeat("a", 55) // 63 chars
	boundaryNs := strings.Repeat("a", 35)          // makes the join exactly 63
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "untruncated synced SA",
			got:  syncedHostSAName("tenants-demo", "vcluster"),
			want: "tenant-runtime-x-tenants-demo-x-vcluster",
		},
		{
			name: "truncated synced SA (63-char virtual ns)",
			got:  syncedHostSAName(longNs, "vcluster"),
			want: "tenant-runtime-x-tenants-aaaaaaaaaaaaaaaaaaaaaaaaaaa-ffac9cfce6",
		},
		{
			name: "dot-collapse at the truncation boundary",
			got:  safeConcatName(strings.Repeat("a", 51)+".", strings.Repeat("b", 30)),
			want: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a6d8259638",
		},
		{
			name: "exact 63-char boundary, no hash",
			got:  syncedHostSAName(boundaryNs, "vcluster"),
			want: "tenant-runtime-x-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-x-vcluster",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Fatalf("got  %q (len %d)\nwant %q (len %d)", c.got, len(c.got), c.want, len(c.want))
			}
			if len(c.got) > 63 {
				t.Fatalf("result exceeds the 63-char DNS-label limit: %d", len(c.got))
			}
		})
	}
}

// TestSafeConcatName_ByteIdenticalToUpstream sweeps a wide range of inputs —
// short, exactly-63, over-63, dot-bearing, multi-part — and asserts the
// production safeConcatName is byte-identical to the independent upstream
// transcription. This is the guard that the operator's cross-check of the
// syncer-written name actually reproduces vcluster's algorithm.
func TestSafeConcatName_ByteIdenticalToUpstream(t *testing.T) {
	// Single-string inputs of every length across the truncation boundary.
	for n := 0; n <= 130; n++ {
		in := strings.Repeat("a", n)
		if got, want := safeConcatName(in), referenceSafeConcatName(in); got != want {
			t.Fatalf("len %d: got %q want %q", n, got, want)
		}
	}
	// Multi-part inputs shaped like real synced-SA names, plus dot-bearing and
	// mixed-length parts that push the join across 63 in different ways.
	multi := [][]string{
		{"tenant-runtime", "x", "tenants-demo", "x", "vcluster"},
		{"tenant-runtime", "x", strings.Repeat("n", 63), "x", "vcluster"},
		{"tenant-runtime", "x", strings.Repeat("n", 40) + "." + strings.Repeat("m", 20), "x", "vcluster"},
		{strings.Repeat("a", 50) + ".", "-", strings.Repeat("b", 20)},
		{"a", "b", "c"},
		{strings.Repeat("x", 52), strings.Repeat("y", 11)},
		{strings.Repeat("x", 51) + ".", strings.Repeat("y", 12)},
	}
	for i, parts := range multi {
		if got, want := safeConcatName(parts...), referenceSafeConcatName(parts...); got != want {
			t.Fatalf("multi[%d] %v: got %q want %q", i, parts, got, want)
		}
	}
}

// TestSyncedHostSAName_LengthMath is the AWS-facing length guarantee: the synced
// host ServiceAccount name is the target of an EKS Pod Identity association, so it
// must never exceed 63 characters for any platform name the operator accepts —
// including names long enough to force PlatformNamespace's own hash truncation
// first, then push safeConcatName over its boundary too (double truncation).
func TestSyncedHostSAName_LengthMath(t *testing.T) {
	platformNames := []string{
		"a",
		"demo",
		"marketing-team",
		strings.Repeat("p", 30),
		strings.Repeat("p", 63),  // PlatformNamespace hash-truncates this
		strings.Repeat("p", 253), // maximal RFC1123 subdomain; still bounded
	}
	for _, pn := range platformNames {
		virtualNs := "tenants-" + pn
		if len(virtualNs) > 63 {
			// mirror PlatformNamespace's cap so we feed a realistic virtual ns
			virtualNs = virtualNs[:63]
		}
		name := syncedHostSAName(virtualNs, vclusterInstanceName)
		if len(name) == 0 {
			t.Fatalf("platform %q: empty synced SA name", pn)
		}
		if len(name) > 63 {
			t.Fatalf("platform %q: synced SA name %q exceeds 63 chars (%d)", pn, name, len(name))
		}
		// Determinism: same inputs, same output.
		if again := syncedHostSAName(virtualNs, vclusterInstanceName); again != name {
			t.Fatalf("platform %q: non-deterministic synced SA name %q vs %q", pn, name, again)
		}
	}
}

// TestSyncedHostSAName_HappyPathIsLegible documents the intent behind the fixed
// short vcluster instance name + short virtual namespace: the common case stays
// in the un-truncated, human-readable regime (no hash), which is easier to match
// against `aws eks list-pod-identity-associations` output during an incident.
func TestSyncedHostSAName_HappyPathIsLegible(t *testing.T) {
	name := syncedHostSAName("tenants-acme", vclusterInstanceName)
	if want := fmt.Sprintf("tenant-runtime-x-tenants-acme-x-%s", vclusterInstanceName); name != want {
		t.Fatalf("happy path: got %q want %q", name, want)
	}
	if len(name) >= 52 {
		t.Fatalf("happy-path name unexpectedly near the truncation boundary: %q (%d)", name, len(name))
	}
}
