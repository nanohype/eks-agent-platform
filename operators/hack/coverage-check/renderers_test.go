/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot is the operators module, two directories above this package.
const moduleRoot = "../.."

// rbacAPIPath is the import path whose RBAC object types grant Kubernetes
// permissions when a file constructs them.
const rbacAPIPath = "k8s.io/api/rbac/v1"

// rbacGrantTypes are the rbac/v1 types whose construction grants or binds a
// permission.
var rbacGrantTypes = map[string]bool{
	"PolicyRule":         true,
	"Role":               true,
	"ClusterRole":        true,
	"RoleBinding":        true,
	"ClusterRoleBinding": true,
}

// iamPolicyVersion is the policy-language version every IAM, KMS and S3 policy
// document carries.
const iamPolicyVersion = `"2012-10-17"`

// policyDocumentTypes are the internal/controller types the operator's IAM and
// KMS policy documents are assembled from.
var policyDocumentTypes = map[string]bool{
	"policyDocument":  true,
	"policyStatement": true,
}

// minRenderers sits below the number of authorization renderers under
// internal/, so a walk that stops finding them fails instead of passing over
// files it never judged. It does not prove each shape is still recognised:
// TestRendersAuthorization_RecognisesEachShape does.
const minRenderers = 8

// rendersAuthorization reports whether a parsed file contains a shape that
// builds an authorization document: a composite literal of an rbac/v1 grant
// type or of a policy document type, or the IAM policy-language version
// literal.
func rendersAuthorization(file *ast.File) bool {
	rbacAliases := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != rbacAPIPath {
			continue
		}
		if imp.Name != nil {
			rbacAliases[imp.Name.Name] = true
		} else {
			rbacAliases["v1"] = true
		}
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING && x.Value == iamPolicyVersion {
				found = true
			}
		case *ast.CompositeLit:
			found = isAuthorizationType(x.Type, rbacAliases)
		}
		return !found
	})
	return found
}

// isAuthorizationType reports whether a composite literal's type is, or is a
// slice or map of, an RBAC grant type or a policy document type.
func isAuthorizationType(expr ast.Expr, rbacAliases map[string]bool) bool {
	switch t := expr.(type) {
	case *ast.ArrayType:
		return isAuthorizationType(t.Elt, rbacAliases)
	case *ast.MapType:
		return isAuthorizationType(t.Value, rbacAliases)
	case *ast.StarExpr:
		return isAuthorizationType(t.X, rbacAliases)
	case *ast.Ident:
		return policyDocumentTypes[t.Name]
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && rbacAliases[pkg.Name] && rbacGrantTypes[t.Sel.Name]
	}
	return false
}

// authorizationRenderers walks internal/ and returns the module-relative path
// of every non-test Go file that renders an authorization document, plus the
// number of files it parsed.
func authorizationRenderers(t *testing.T) ([]string, int) {
	t.Helper()
	fset := token.NewFileSet()
	var renderers []string
	examined := 0
	root := filepath.Join(moduleRoot, "internal")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		examined++
		if rendersAuthorization(file) {
			rel, err := filepath.Rel(moduleRoot, path)
			if err != nil {
				return err
			}
			renderers = append(renderers, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(renderers)
	return renderers, examined
}

// TestAuthorizationRenderersCarryTheFileFloor holds every file under internal/
// that rendersAuthorization detects at a 100% file floor.
func TestAuthorizationRenderersCarryTheFileFloor(t *testing.T) {
	renderers, examined := authorizationRenderers(t)
	if examined == 0 {
		t.Fatal("parsed no Go files under internal/; the walk found nothing to judge")
	}
	if len(renderers) < minRenderers {
		t.Fatalf("found %d authorization renderers in %d files, below the floor of %d: the detector no longer recognises a shape the code uses",
			len(renderers), examined, minRenderers)
	}
	for _, path := range renderers {
		floor, ok := floors.fileFloors[path]
		if !ok {
			t.Errorf("%s renders an authorization document and has no fileFloors entry; add it at 100", path)
			continue
		}
		if floor != 100 {
			t.Errorf("%s renders an authorization document and is floored at %.0f, not 100", path, floor)
		}
	}
	t.Logf("%d authorization renderers in %d files examined under internal/", len(renderers), examined)
}

// TestFileFloorsNameFilesThatExist fails on a fileFloors entry whose file is
// gone, since the gate cannot measure a file that does not exist.
func TestFileFloorsNameFilesThatExist(t *testing.T) {
	if len(floors.fileFloors) == 0 {
		t.Fatal("fileFloors is empty")
	}
	for path := range floors.fileFloors {
		if _, err := os.Stat(filepath.Join(moduleRoot, filepath.FromSlash(path))); err != nil {
			t.Errorf("fileFloors names %s: %v", path, err)
		}
	}
}

// TestRendersAuthorization_RecognisesEachShape proves the detector can see
// every shape it claims to, and passes a file that only reads RBAC.
func TestRendersAuthorization_RecognisesEachShape(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"rbac rule under an alias", `package x
import rbacv1 "k8s.io/api/rbac/v1"
var _ = []rbacv1.PolicyRule{{Verbs: []string{"get"}}}`, true},
		{"rbac binding under the default name", `package x
import "k8s.io/api/rbac/v1"
var _ = &v1.ClusterRoleBinding{}`, true},
		{"policy version literal", `package x
var _ = map[string]any{"Version": "2012-10-17"}`, true},
		{"policy statement slice", `package x
type policyStatement struct{}
var _ = []policyStatement{{}}`, true},
		{"rbac read only", `package x
import rbacv1 "k8s.io/api/rbac/v1"
func f(r *rbacv1.ClusterRole) int { return len(r.Rules) }`, false},
		{"same type name from another package", `package x
import other "example.com/other"
var _ = other.ClusterRole{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := rendersAuthorization(file); got != tc.want {
				t.Errorf("rendersAuthorization = %v, want %v", got, tc.want)
			}
		})
	}
}
