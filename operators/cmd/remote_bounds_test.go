/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every construction that can issue a remote request needs something that ends
// the wait, and the ones that lack it are invisible: a call with no deadline
// does not fail, it stops returning, and the goroutine holding it looks
// identical to one doing work.
//
// The set is swept out of the module's own source rather than listed, because a
// list is a record of what someone remembered. Each site is then either bounded
// — with the bound named — or excused with the reason it issues nothing, and a
// site in neither table fails this.

// clientConstructors are selectors distinctive enough that any package spelling
// one is opening a connection. A constructor absent from here is a hole in the
// sweep rather than a passing site.
var clientConstructors = map[string]bool{
	"NewWithWatch":             true,
	"NewManager":               true,
	"NewFromConfig":            true, // every aws-sdk-go-v2 service client
	"NewForConfig":             true, // client-go typed and dynamic clients
	"GetConfig":                true,
	"GetConfigOrDie":           true,
	"InClusterConfig":          true,
	"RESTConfigFromKubeConfig": true,
	"BuildConfigFromFlags":     true,
	"LoadDefaultConfig":        true, // aws config, which carries the HTTP client
	"HTTPClientFor":            true,
	"DialContext":              true,
}

// genericConstructors are spelled by packages that open nothing — errors.New and
// zap.New read identically to client.New — so for these the package decides.
// Keeping them qualified is what stops the sweep filling with sites whose reason
// for exemption is "this one is a logger", which is the shape a reader stops
// reading.
var genericConstructors = map[string]bool{"New": true, "Dial": true}

var clientPackages = map[string]bool{
	"client":     true,
	"awsclients": true,
	"kubernetes": true,
	"dynamic":    true,
	"discovery":  true,
	"rest":       true,
	"clientcmd":  true,
	"http":       true,
}

// boundedClients maps a site to the thing that ends its wait. The value is read
// by nothing; it is here so that adding a site means naming its bound, which is
// the step someone skips when the table is a list of paths.
var boundedClients = map[string]string{
	// The host API server. Deliberately NOT a rest.Config timeout: that applies
	// to every request the client issues, watches included, so a short value
	// re-lists every informer on each expiry. reconcileTimeout bounds the calls
	// a reconcile makes, and the reflector bounds the watches — see
	// TestAWatchIsBoundedWithoutTheClientConfig, which reads that from the
	// dependency rather than asserting it.
	"cmd/main.go:main:GetConfigOrDie": "reconcileTimeout, via controllerOptions on the manager",
	"cmd/main.go:main:NewManager":     "reconcileTimeout, carried to every controller it builds",

	"cmd/main.go:main:awsclients.New":                                  "awsHTTPTimeout per request and awsOpTimeout per operation, applied inside awsclients",
	"internal/awsclients/clients.go:New:LoadDefaultConfig":             "awsHTTPTimeout on the config's HTTP client",
	"internal/awsclients/clients.go:New:NewFromConfig":                 "the same config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/prometheus.go:NewPrometheusQuery:http.Client": "promQueryTimeout",

	"cmd/metrics-shim/main.go:run:http.Client": "upstreamTimeout",

	"internal/agentctl/commands.go:clusterRESTConfig:GetConfig": "clusterRequestTimeout, set on the config it returns",
	"internal/agentctl/commands.go:newClusterClient:client.New": "clusterRequestTimeout, from the config it is handed",

	"internal/controller/target_client.go:ClientFor:RESTConfigFromKubeConfig": "vclusterRequestTimeout, set on the parsed config",
	"internal/controller/target_client.go:ClientFor:client.New":               "vclusterRequestTimeout, from the config it is handed",
}

// notAClient maps a site to the reason it issues no request. An entry here is a
// claim about what the code does, not a suppression.
var notAClient = map[string]string{
	"internal/controller/vcluster.go:ensureVClusterClusterSecret:RESTConfigFromKubeConfig": "parses a kubeconfig for the TLS material it carries, which is marshalled into a cluster-registration Secret; no client is built from it and no request is issued",
}

func TestEveryRemoteConstructionCarriesABound(t *testing.T) {
	sites := remoteConstructionSites(t)

	var unclassified []string
	for _, site := range sites {
		if _, ok := boundedClients[site]; ok {
			continue
		}
		if _, ok := notAClient[site]; ok {
			continue
		}
		unclassified = append(unclassified, site)
	}
	if len(unclassified) > 0 {
		t.Errorf("%d construction(s) that can open a connection are in neither table:\n  %s\n\n"+
			"Add each to boundedClients with what ends its wait, or to notAClient with why it issues "+
			"no request. A call with no deadline does not fail — it stops returning.",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}

	found := map[string]bool{}
	for _, s := range sites {
		found[s] = true
	}
	for site := range boundedClients {
		if !found[site] {
			t.Errorf("boundedClients names %s, which the sweep no longer finds; delete the entry rather "+
				"than leaving a bound recorded against nothing", site)
		}
	}
	for site := range notAClient {
		if !found[site] {
			t.Errorf("notAClient excuses %s, which the sweep no longer finds; delete the entry", site)
		}
	}
}

// remoteConstructionSites walks the module's shipped source and returns one
// entry per construction, keyed by file, enclosing function and constructor —
// not by line, so a site keeps its identity when the code above it moves.
func remoteConstructionSites(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}

	var sites []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "bin" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasSuffix(path, "zz_generated.deepcopy.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				if name := constructorName(m); name != "" {
					sites = append(sites, rel+":"+fn.Name.Name+":"+name)
				}
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if len(sites) == 0 {
		t.Fatal("the sweep found no remote construction anywhere in the module, which cannot be true; " +
			"it is matching nothing and would pass whatever the code did")
	}
	sort.Strings(sites)
	return dedupe(sites)
}

// constructorName reports what a node constructs when that is something able to
// open a connection, and "" otherwise. A distinctive selector counts wherever it
// appears; a generic one counts only from a package that builds clients.
func constructorName(n ast.Node) string {
	switch v := n.(type) {
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if clientConstructors[sel.Sel.Name] {
			return sel.Sel.Name
		}
		if genericConstructors[sel.Sel.Name] {
			if pkg, ok := sel.X.(*ast.Ident); ok && clientPackages[pkg.Name] {
				return pkg.Name + "." + sel.Sel.Name
			}
		}
	case *ast.CompositeLit:
		if sel, ok := v.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Client" {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" {
				return "http.Client"
			}
		}
	}
	return ""
}

func dedupe(in []string) []string {
	out := in[:0]
	var last string
	for _, s := range in {
		if s != last {
			out = append(out, s)
		}
		last = s
	}
	return out
}
