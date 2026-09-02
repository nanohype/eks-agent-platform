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
// The invariant: every call in this module into a package that reaches a
// network is either bounded, with the bound named, or excused with the reason it
// issues no request. Which packages reach a network is read from each file's
// imports, so a call is a site because of what it talks to rather than because
// of what it is spelled — errors.New and client.New are told apart by their
// import paths, and NewForConfigOrDie is a site without anyone having written it
// down beside NewForConfig.

// networkPackages are the import PATHS whose symbols reach a network. The set is
// derived from what the module depends on rather than from what a call is
// spelled: a package either talks to a remote or it does not, and that is a fact
// about the dependency, while "which constructor names did someone think of" is
// a fact about the author.
//
// This is the half that has to be justified, and it is short enough to be. Every
// entry is a client library or the standard HTTP package; a remote this operator
// learns to talk to arrives as a new import, which is the moment to add it.
var networkPackages = []string{
	"net/http",
	"k8s.io/client-go",
	"sigs.k8s.io/controller-runtime",
	"k8s.io/apimachinery/pkg/util/net",
	"github.com/aws/aws-sdk-go-v2",
	"github.com/aws/smithy-go",
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients",
}

// httpRequestFuncs are net/http's package-level request helpers. They construct
// nothing — they use http.DefaultClient, which has no timeout — so a sweep
// looking only for constructions cannot see the two most likely accidental
// unbounded calls in Go.
var httpRequestFuncs = map[string]bool{
	"Get": true, "Post": true, "Head": true, "PostForm": true,
}

// boundedClients maps a site to the thing that ends its wait. The value is read
// by nothing; it is here so that adding a site means naming its bound, which is
// the step someone skips when the table is a list of paths.
var boundedClients = map[string]string{
	"cmd/main.go:main:awsclients.New":                                                   "awsHTTPTimeout per request and awsOpTimeout per operation, applied inside awsclients",
	"cmd/main.go:main:awsclients.NewPrometheusQuery":                                    "promQueryTimeout, on the HTTP client it is built with",
	"cmd/main.go:main:ctrl.GetConfigOrDie":                                              "reconcileCeiling, via controllerOptions on the manager",
	"cmd/main.go:main:ctrl.NewManager":                                                  "reconcileCeiling, carried to every controller it builds",
	"cmd/metrics-shim/main.go:run:http.Client":                                          "upstreamTimeout",
	"internal/agentctl/commands.go:clusterRESTConfig:config.GetConfig":                  "clusterRequestTimeout, set on the config it returns",
	"internal/agentctl/commands.go:newClusterClient:client.New":                         "clusterRequestTimeout, from the config it is handed",
	"internal/awsclients/clients.go:New:athena.NewFromConfig":                           "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/clients.go:New:awsconfig.LoadDefaultConfig":                    "awsHTTPTimeout on the config's HTTP client",
	"internal/awsclients/clients.go:New:awshttp.NewBuildableClient":                     "awsHTTPTimeout; this call is where it is applied",
	"internal/awsclients/clients.go:New:cloudwatch.NewFromConfig":                       "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/clients.go:New:eks.NewFromConfig":                              "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/clients.go:New:eventbridge.NewFromConfig":                      "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/clients.go:New:iam.NewFromConfig":                              "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/clients.go:New:s3.NewFromConfig":                               "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/clients.go:New:scheduler.NewFromConfig":                        "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/clients.go:New:ssm.NewFromConfig":                              "the shared aws.Config, plus the awsOpTimeout middleware every operation carries",
	"internal/awsclients/prometheus.go:NewPrometheusQuery:http.Client":                  "promQueryTimeout",
	"internal/controller/target_client.go:ClientFor:client.New":                         "vclusterRequestTimeout, from the config it is handed",
	"internal/controller/target_client.go:ClientFor:clientcmd.RESTConfigFromKubeConfig": "vclusterRequestTimeout, set on the parsed config",
}

// notAClient maps a site to the reason it issues no request. An entry here is a
// claim about what the code does, not a suppression.
var notAClient = map[string]string{
	"cmd/main.go:main:zap.New":                                                                       "builds the logger; it writes to stderr",
	"cmd/metrics-shim/main.go:queueDepth:http.NewRequest":                                            "builds a request; the client that sends it carries upstreamTimeout",
	"cmd/metrics-shim/main.go:run:http.NewServeMux":                                                  "routes inbound requests; it opens nothing",
	"internal/awsclients/prometheus.go:NewPrometheusQuery:v4.NewSigner":                              "signs a request before it is sent; it opens nothing",
	"internal/awsclients/prometheus.go:QueryScalar:http.NewRequestWithContext":                       "builds a request; the client that sends it carries promQueryTimeout",
	"internal/controller/agentfleet_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":      "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/agentsandbox_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":    "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/budget_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":          "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/eval_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":            "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/modelgateway_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":    "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/platform_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":        "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/sandboxpool_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":     "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/slo_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":             "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/tenant_controller.go:SetupWithManager:ctrl.NewControllerManagedBy":          "a builder that registers this reconciler with the manager; the manager's client is the one that talks to the API server",
	"internal/controller/vcluster.go:ensureVClusterClusterSecret:clientcmd.RESTConfigFromKubeConfig": "parses a kubeconfig for the TLS material a cluster-registration Secret carries; no client is built from it",
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
		imports := fileImports(f)
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				if name := constructorName(m, imports); name != "" {
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

// constructorName reports what a node opens, given the file's imports, and ""
// when it opens nothing.
//
// Within a network package every New* is a construction — which is what makes
// NewForConfigOrDie a site without anyone having listed it beside NewForConfig —
// and net/http's package-level request helpers and DefaultClient are sites
// because they issue a request through a client nobody constructed.
func constructorName(n ast.Node, imports map[string]string) string {
	pkgPath := func(e ast.Expr) (string, string, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", "", false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", "", false
		}
		path, ok := imports[id.Name]
		if !ok || !isNetworkPackage(path) {
			return "", "", false
		}
		return id.Name, sel.Sel.Name, true
	}

	switch v := n.(type) {
	case *ast.CallExpr:
		// http.DefaultClient.Do(...) — the selector's receiver is itself a
		// selector, so the package sits one level further in.
		if outer, ok := v.Fun.(*ast.SelectorExpr); ok {
			if alias, name, ok := pkgPath(outer.X); ok && name == "DefaultClient" {
				return alias + ".DefaultClient." + outer.Sel.Name
			}
		}
		alias, name, ok := pkgPath(v.Fun)
		if !ok {
			return ""
		}
		switch {
		case strings.HasPrefix(name, "New"):
			return alias + "." + name
		case name == "Dial" || name == "DialContext":
			return alias + "." + name
		case imports[alias] == "net/http" && httpRequestFuncs[name]:
			return alias + "." + name
		case name == "GetConfig" || name == "GetConfigOrDie" || name == "InClusterConfig":
			return alias + "." + name
		case name == "RESTConfigFromKubeConfig" || name == "BuildConfigFromFlags":
			return alias + "." + name
		case name == "LoadDefaultConfig" || name == "HTTPClientFor":
			return alias + "." + name
		}
	case *ast.CompositeLit:
		if alias, name, ok := pkgPath(v.Type); ok && name == "Client" {
			return alias + ".Client"
		}
	}
	return ""
}

func isNetworkPackage(path string) bool {
	for _, prefix := range networkPackages {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// fileImports maps each import's local name to its path, so a selector can be
// resolved to the package it actually names rather than to the identifier a file
// happens to have chosen.
func fileImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
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
