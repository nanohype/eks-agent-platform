/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The address a pod is GIVEN and the address it is ALLOWED to reach are one
// fact written in six places: OTEL_EXPORTER_OTLP_ENDPOINT, stamped on every pod
// the operator builds, and five egress legs — tenant and gateway on each policy
// engine, plus the per-fleet policy. All six render from collectorNamespace and
// the otlp*Port constants, which is the right design and is not a gate.
//
// It is not a gate because the assertions over it compared the endpoint against
// the very constants the endpoint is built from. That holds for whatever the
// constants say, so it cannot fail, and a literal port written into a leg left
// the whole suite green — on the tenant leg of both engines, where the existing
// tests check that a rule NAMES the collector namespace and never check which
// ports it opens.
//
// So the subject here is the address the operator actually stamps, read back
// out of the rendered env, and every leg is asked whether it admits THAT. A
// literal on either side then disagrees with the other five.
//
// This is the subsystem where a drift matters most and shows least: an export
// into a closed egress leaves the pod Running and the collector healthy, and the
// series simply never arrive.

// otlpReceiverPortsFromSpec are the ports the OpenTelemetry protocol assigns to
// each transport by default, and which an OTLP receiver therefore listens on
// unless it is deliberately configured otherwise.
//
// Written here rather than read from otlpGRPCPort/otlpHTTPPort for the reason
// collectorNamespaceFromCatalog is written out in otlp_egress_test.go: a
// constant compared against itself agrees with whatever it says. This is the
// external fact the constants are answerable to.
var otlpReceiverPortsFromSpec = map[string]int{
	"grpc":          4317,
	"http/protobuf": 4318,
}

// stampedTelemetryAddress reads the destination off the env the operator builds
// for a pod, not off the helper that renders it. The env is what reaches a
// container; a pod given none falls back to localhost, where nothing listens.
func stampedTelemetryAddress(t *testing.T) (namespace string, port int, protocol string) {
	t.Helper()
	stamped := map[string]string{}
	for _, e := range withOTelResourceAttrs(nil, newPlatform(ctrlTestPlatform, "team"), "anthropic", "production") {
		stamped[e.Name] = e.Value
	}

	raw := stamped[otelEndpointEnvName]
	if raw == "" {
		t.Fatalf("no %s is stamped; an OTel SDK without one exports to localhost and drops every span with the pod Running", otelEndpointEnvName)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("stamped endpoint %q does not parse as a URL: %v", raw, err)
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stamped endpoint %q names no port, so the SDK would use the scheme default — which no OTLP receiver listens on", raw)
	}

	const clusterSuffix = ".svc.cluster.local"
	host := u.Hostname()
	if !strings.HasSuffix(host, clusterSuffix) {
		t.Fatalf("stamped endpoint host %q is not an in-cluster Service name; egress rules select a namespace, and a name outside the cluster names none", host)
	}
	parts := strings.Split(strings.TrimSuffix(host, clusterSuffix), ".")
	if len(parts) != 2 {
		t.Fatalf("stamped endpoint host %q is not <service>.<namespace>%s, so no namespace can be read out of it", host, clusterSuffix)
	}
	return parts[1], port, stamped[otelProtocolEnvName]
}

func TestStampedEndpointAddressesThePortItsProtocolSpeaks(t *testing.T) {
	_, port, protocol := stampedTelemetryAddress(t)

	want, ok := otlpReceiverPortsFromSpec[protocol]
	if !ok {
		t.Fatalf("%s = %q, which is not an OTLP transport; the SDK rejects it at startup and the pod exports nothing", otelProtocolEnvName, protocol)
	}
	if port != want {
		t.Errorf("the operator stamps protocol %q against port %d, and OTLP assigns %q to port %d. "+
			"Every export then fails with a protocol error only the workload's own logs carry", protocol, port, protocol, want)
	}

	// The egress legs open the two constants below. They are ports a collector
	// listens on only while they are the protocol's ports.
	if otlpGRPCPort != otlpReceiverPortsFromSpec["grpc"] {
		t.Errorf("otlpGRPCPort = %d and OTLP assigns gRPC to %d; every leg opens a port the receiver is not on", otlpGRPCPort, otlpReceiverPortsFromSpec["grpc"])
	}
	if otlpHTTPPort != otlpReceiverPortsFromSpec["http/protobuf"] {
		t.Errorf("otlpHTTPPort = %d and OTLP assigns http/protobuf to %d", otlpHTTPPort, otlpReceiverPortsFromSpec["http/protobuf"])
	}
}

// telemetryEgressLeg is one rendered policy that opens a path to the collector.
// open returns the ports that policy opens toward the named namespace.
type telemetryEgressLeg struct {
	name string
	open func(t *testing.T, namespace string) []int
}

func telemetryEgressLegs() []telemetryEgressLeg {
	return []telemetryEgressLeg{
		{"tenantEgressCiliumRules", func(t *testing.T, ns string) []int {
			return ciliumPortsToward(tenantEgressCiliumRules(nil, nil), ns)
		}},
		{"gatewayEgressCiliumRules", func(t *testing.T, ns string) []int {
			return ciliumPortsToward(gatewayEgressCiliumRules(), ns)
		}},
		{"ensureNetworkPolicy", func(t *testing.T, ns string) []int {
			p := newPlatform(ctrlTestPlatform, "team")
			cl := newPolicyClient(t)
			r := &PlatformReconciler{Client: cl, NetworkEngine: "kubernetes"}
			if err := r.ensureNetworkPolicy(context.Background(), p); err != nil {
				t.Fatalf("ensureNetworkPolicy: %v", err)
			}
			return policyPortsToward(fetchPolicy(t, cl, p, "tenant-egress"), ns)
		}},
		{"ensureGatewayEgressPolicy", func(t *testing.T, ns string) []int {
			p := newPlatform(ctrlTestPlatform, "team")
			cl := newPolicyClient(t)
			r := &PlatformReconciler{Client: cl, NetworkEngine: "kubernetes"}
			if err := r.ensureGatewayEgressPolicy(context.Background(), p); err != nil {
				t.Fatalf("ensureGatewayEgressPolicy: %v", err)
			}
			return policyPortsToward(fetchPolicy(t, cl, p, "gateway-egress"), ns)
		}},
		{"ensureFleetNetworkPolicy", func(t *testing.T, ns string) []int {
			p := newPlatform(ctrlTestPlatform, "team")
			cl := newPolicyClient(t)
			r := &AgentFleetReconciler{Client: cl, NetworkEngine: "kubernetes"}
			fleet := &agentsv1alpha1.AgentFleet{ObjectMeta: metav1.ObjectMeta{Name: "workers", Namespace: PlatformNamespace(p)}}
			if err := r.ensureFleetNetworkPolicy(context.Background(), fleet, p); err != nil {
				t.Fatalf("ensureFleetNetworkPolicy: %v", err)
			}
			return policyPortsToward(fetchPolicy(t, cl, p, "fleet-workers"), ns)
		}},
	}
}

func TestEveryEgressLegAdmitsTheAddressStamped(t *testing.T) {
	namespace, port, _ := stampedTelemetryAddress(t)

	for _, leg := range telemetryEgressLegs() {
		t.Run(leg.name, func(t *testing.T) {
			ports := leg.open(t, namespace)
			if len(ports) == 0 {
				t.Fatalf("%s opens no rule toward %q, the namespace the stamped endpoint addresses; "+
					"a pod exporting there hits default-deny while staying Running", leg.name, namespace)
			}
			for _, got := range ports {
				if got == port {
					return
				}
			}
			t.Errorf("the operator stamps port %d and %s opens %v toward %q; "+
				"the address given and the address allowed are the same fact and these disagree", port, leg.name, ports, namespace)
		})
	}
}

// notEgressLegs are the functions that name collectorNamespace for a reason
// other than opening a path to it. Each carries the reason rather than sitting
// in a silent skip list, so the scan below can treat everything else as a leg.
var notEgressLegs = map[string]string{
	"otelExporterEndpoint": "gives the address rather than allowing one — the other half of this seam, " +
		"held by TestStampedEndpointAddressesThePortItsProtocolSpeaks",
	"ensureTenantIngressPolicy": "the opposite direction: it admits the collector's scrape INTO the tenant " +
		"namespace and names no OTLP port, so the address stamped on a pod says nothing about it",
}

func TestEveryFunctionNamingTheCollectorIsCovered(t *testing.T) {
	// A table of legs is a list of the ones someone thought of. The package's
	// own source decides the membership instead: a function that names
	// collectorNamespace either opens a path to it or hands out its address,
	// and a sixth leg added without a row here fails this.
	covered := map[string]bool{}
	for _, leg := range telemetryEgressLegs() {
		covered[leg.name] = true
	}

	for _, fn := range functionsNamingTheCollector(t) {
		if covered[fn] {
			continue
		}
		if _, ok := notEgressLegs[fn]; ok {
			continue
		}
		t.Errorf("%s names collectorNamespace and no leg in telemetryEgressLegs covers it; "+
			"its ports are never compared against the address the operator stamps", fn)
	}
	for name := range notEgressLegs {
		if !nameIsDeclared(t, name) {
			t.Errorf("notEgressLegs excuses %s, which this package does not declare; delete the entry rather than leaving an exemption nothing can reach", name)
		}
	}
}

// functionsNamingTheCollector returns every function in the package's shipped
// sources whose body mentions collectorNamespace.
func functionsNamingTheCollector(t *testing.T) []string {
	t.Helper()
	var out []string
	forEachPackageFile(t, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == "collectorNamespace" {
					out = append(out, fn.Name.Name)
					return false
				}
				return true
			})
			return true
		})
	})
	if len(out) == 0 {
		t.Fatal("no function in this package names collectorNamespace, so this test would pass vacuously")
	}
	return out
}

func nameIsDeclared(t *testing.T, name string) bool {
	t.Helper()
	var found bool
	forEachPackageFile(t, func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == name {
				found = true
			}
			return true
		})
	})
	return found
}

// forEachPackageFile parses the package's shipped sources. Test files are
// excluded: a leg has to be reachable from the operator, not from a fixture.
func forEachPackageFile(t *testing.T, visit func(*ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		visit(f)
	}
}

func ciliumPortsToward(rules []interface{}, namespace string) []int {
	var ports []int
	for _, rule := range rules {
		r, _ := rule.(map[string]interface{})
		eps, ok, _ := unstructured.NestedSlice(r, "toEndpoints")
		if !ok {
			continue
		}
		var names bool
		for _, e := range eps {
			ep, _ := e.(map[string]interface{})
			labels, _, _ := unstructured.NestedStringMap(ep, "matchLabels")
			if labels["k8s:io.kubernetes.pod.namespace"] == namespace {
				names = true
			}
		}
		if !names {
			continue
		}
		toPorts, _, _ := unstructured.NestedSlice(r, "toPorts")
		for _, tp := range toPorts {
			pm, _ := tp.(map[string]interface{})
			list, _, _ := unstructured.NestedSlice(pm, "ports")
			for _, entry := range list {
				e, _ := entry.(map[string]interface{})
				if p, err := strconv.Atoi(fmt.Sprint(e["port"])); err == nil {
					ports = append(ports, p)
				}
			}
		}
	}
	return ports
}

func policyPortsToward(np *networkingv1.NetworkPolicy, namespace string) []int {
	var ports []int
	for _, rule := range np.Spec.Egress {
		var names bool
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == namespace {
				names = true
			}
		}
		if !names {
			continue
		}
		for _, p := range rule.Ports {
			if p.Port != nil {
				ports = append(ports, int(p.Port.IntVal))
			}
		}
	}
	return ports
}

func newPolicyClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register networking/v1: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func fetchPolicy(t *testing.T, cl client.Client, p *platformv1alpha1.Platform, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	np := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: name}
	if err := cl.Get(context.Background(), key, np); err != nil {
		t.Fatalf("%s NetworkPolicy was not created: %v", name, err)
	}
	return np
}
