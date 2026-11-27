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
