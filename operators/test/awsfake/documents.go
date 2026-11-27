/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake

import (
	"encoding/json"
	"net/url"
	"strings"
	"unicode"
)

// isJSONObject reports whether doc is one JSON object and nothing else: what
// IAM requires of a policy document and EventBridge of an event's Detail.
// "null" decodes without error into a nil map, so a nil result is refused too.
func isJSONObject(doc string) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal([]byte(doc), &obj) == nil && obj != nil
}

// policyKind is which IAM policy document a grammar check reads.
type policyKind int

const (
	// permissionsPolicy is an inline policy on a role: it grants actions on
	// resources, and names no principal.
	permissionsPolicy policyKind = iota
	// trustPolicy is a role's trust policy: it names the principals that may
	// assume the role, and no resource, because the role is the resource.
	trustPolicy
)

// policyGrammarError is the message IAM refuses doc with, as a document of the
// given kind, for a break in the policy grammar's structure; "" when the
// structure holds. doc is a JSON object, which the caller has checked. The
// structure is a Version, when present, of 2012-10-17 or 2008-10-17, and a
// Statement holding one statement object or an array of them. Each statement
// carries an Effect of Allow or Deny (case-sensitive) and an Action or
// NotAction. A permissions-policy statement also carries a Resource or
// NotResource, and no Principal or NotPrincipal. A trust-policy statement
// carries a Principal or NotPrincipal, and no Resource or NotResource. Values
// inside the structure (condition operators, ARNs, action names) are not read.
func policyGrammarError(doc string, kind policyKind) string {
	const syntax = "Syntax errors in policy."
	var policy map[string]json.RawMessage
	if json.Unmarshal([]byte(doc), &policy) != nil {
		return syntax
	}
	if raw, ok := policy["Version"]; ok {
		var version string
		if json.Unmarshal(raw, &version) != nil || (version != "2012-10-17" && version != "2008-10-17") {
			return "The policy failed legacy parsing"
		}
	}
	raw, ok := policy["Statement"]
	if !ok || string(raw) == "null" {
		return syntax
	}
	var statements []map[string]json.RawMessage
	if json.Unmarshal(raw, &statements) != nil {
		var one map[string]json.RawMessage
		if json.Unmarshal(raw, &one) != nil || one == nil {
			return syntax
		}
		statements = []map[string]json.RawMessage{one}
	}
	for _, st := range statements {
		if st == nil {
			return syntax
		}
		var effect string
		if json.Unmarshal(st["Effect"], &effect) != nil || (effect != "Allow" && effect != "Deny") {
			return syntax
		}
		actions, ok := stringsElement(st, "Action", "NotAction")
		if !ok {
			return syntax
		}
		if !actions {
			return "Policy statement must contain actions."
		}
		_, principal := st["Principal"]
		_, notPrincipal := st["NotPrincipal"]
		_, resource := st["Resource"]
		_, notResource := st["NotResource"]
		switch kind {
		case permissionsPolicy:
			if principal || notPrincipal {
				return "Policy document should not specify a principal."
			}
			resources, ok := stringsElement(st, "Resource", "NotResource")
			if !ok {
				return syntax
			}
			if !resources {
				return "Policy statement must contain resources."
			}
		case trustPolicy:
			if resource || notResource {
				return "Has prohibited field Resource"
			}
			if !principal && !notPrincipal {
				return "Missing required field Principal"
			}
		}
	}
	return ""
}

// stringsElement reports whether statement st carries the element key or its
// negation notKey (present), and whether whichever it carries holds a string
// or an array of strings, the two shapes IAM reads Action and Resource in (ok).
// A statement carrying neither is ok and not present.
func stringsElement(st map[string]json.RawMessage, key, notKey string) (present, ok bool) {
	raw, has := st[key]
	if !has {
		raw, has = st[notKey]
	}
	if !has {
		return false, true
	}
	if string(raw) == "null" {
		return true, false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return true, true
	}
	var many []string
	return true, json.Unmarshal(raw, &many) == nil && many != nil
}

// policySize is a policy document's size as IAM counts it against a quota: in
// characters, with white space excluded.
func policySize(doc string) int {
	n := 0
	for _, r := range doc {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// urlEncoded renders a policy document the way IAM returns one from GetRole
// and GetRolePolicy: percent-encoded per RFC 3986. QueryEscape encodes every
// byte outside the unreserved set, a literal "+" included, and spells a space
// "+", which RFC 3986 spells %20.
func urlEncoded(doc string) string {
	return strings.ReplaceAll(url.QueryEscape(doc), "+", "%20")
}
