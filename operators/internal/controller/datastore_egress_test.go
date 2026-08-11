package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func platformWith(kinds ...platformv1alpha1.DatastoreKind) *platformv1alpha1.Platform {
	p := &platformv1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "tenants-t"}}
	for i, k := range kinds {
		p.Spec.Datastores = append(p.Spec.Datastores, platformv1alpha1.DatastoreSpec{
			Name: string(rune('a' + i)), Kind: k,
		})
	}
	return p
}

// The ports come from the declaration, and only for kinds that speak their own
// protocol.
//
// This is the bug that cost a live install: a tenant declared relational + cache,
// the substrate built Aurora and ElastiCache, the operator granted IAM to reach
// them — and default-deny egress dropped every packet, because the boundary was
// built from a constant instead of from spec.datastores. The Platform was Ready,
// the datastore was available, the credential was valid, and the connection timed
// out.
func TestDatastoreEgressPorts_DerivedFromTheDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []platformv1alpha1.DatastoreKind
		want  []int
	}{
		{"relational", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreRelational}, []int{5432}},
		{"cache", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreCache}, []int{6379}},
		{"stream", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreStream}, []int{9098}},
		{"relational+cache", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreRelational, platformv1alpha1.DatastoreCache}, []int{5432, 6379}},
		// Two relational stores are one port, not two rules.
		{"two relational", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreRelational, platformv1alpha1.DatastoreRelational}, []int{5432}},
		// The 443 kinds are deliberately absent — see datastoreEgressPorts.
		{"objectStore", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreObjectStore}, []int{}},
		{"keyValue", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreKeyValue}, []int{}},
		{"queue", []platformv1alpha1.DatastoreKind{platformv1alpha1.DatastoreQueue}, []int{}},
		{"none", nil, []int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := datastoreEgressPorts(platformWith(tc.kinds...))
			if len(got) != len(tc.want) {
				t.Fatalf("ports = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ports = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A tenant that declares no datastore must gain no egress it did not have.
//
// The fix opens ports, so the thing to prove is that it opens them for exactly
// the tenants that asked. A rule that appears unconditionally would widen every
// tenant's boundary on the way to fixing one.
func TestTenantEgressCiliumRules_NoDatastoreNoNewRule(t *testing.T) {
	base := tenantEgressCiliumRules(nil, nil)
	withPG := tenantEgressCiliumRules([]int{5432}, nil)

	if len(withPG) != len(base)+1 {
		t.Fatalf("declaring a datastore should add exactly one rule: base=%d with=%d", len(base), len(withPG))
	}
	for _, raw := range base {
		rule := raw.(map[string]interface{})
		ents, _ := rule["toEntities"].([]interface{})
		for _, e := range ents {
			if e == "all" {
				t.Fatal("the base tenant allow-list must not reach `all` — that is the gateway's " +
					"privilege and the reason application pods cannot call Bedrock directly")
			}
		}
	}
}

// And the rule it does add carries the declared ports, as TCP, to `all`.
func TestTenantEgressCiliumRules_DatastoreRuleShape(t *testing.T) {
	rules := tenantEgressCiliumRules([]int{5432, 6379}, nil)
	last := rules[len(rules)-1].(map[string]interface{})

	ents := last["toEntities"].([]interface{})
	if len(ents) != 1 || ents[0] != "all" {
		t.Fatalf("toEntities = %v, want [all]", ents)
	}
	ports := last["toPorts"].([]interface{})[0].(map[string]interface{})["ports"].([]interface{})
	if len(ports) != 2 {
		t.Fatalf("want 2 ports, got %v", ports)
	}
	for i, want := range []string{"5432", "6379"} {
		p := ports[i].(map[string]interface{})
		if p["port"] != want || p["protocol"] != "TCP" {
			t.Errorf("port[%d] = %v, want %s/TCP", i, p, want)
		}
	}
}

// The 443 kinds are bounded by hostname, and the hostname is the whole point.
//
// Bedrock answers on 443 from an in-VPC PrivateLink address, so neither a port
// nor a CIDR separates it from S3. Only the name does. A rule that widened to
// *.amazonaws.com — or to a bare 443 — would hand every application pod a direct
// route to the model plane and reduce the gateway to a convention.
func TestDatastoreFQDNs_NameTheServiceNotTheWildcard(t *testing.T) {
	got := datastoreFQDNs(platformWith(
		platformv1alpha1.DatastoreObjectStore,
		platformv1alpha1.DatastoreKeyValue,
		platformv1alpha1.DatastoreQueue,
	), "us-west-2")

	want := []string{
		"s3.us-west-2.amazonaws.com",
		"dynamodb.us-west-2.amazonaws.com",
		"sqs.us-west-2.amazonaws.com",
	}
	if len(got) != len(want) {
		t.Fatalf("fqdns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fqdns = %v, want %v", got, want)
		}
	}
	for _, n := range got {
		if strings.Contains(n, "*") {
			t.Errorf("%q is a pattern; a wildcard over amazonaws.com matches "+
				"bedrock-runtime and gives away the model boundary", n)
		}
		if strings.Contains(n, "bedrock") {
			t.Errorf("%q reaches the model plane directly, which is the gateway's job alone", n)
		}
	}
}

// The protocol kinds contribute no hostname, and the AWS kinds contribute no port.
// The two halves must not leak into each other.
func TestDatastoreFQDNs_ProtocolKindsContributeNoHostname(t *testing.T) {
	if got := datastoreFQDNs(platformWith(
		platformv1alpha1.DatastoreRelational,
		platformv1alpha1.DatastoreCache,
		platformv1alpha1.DatastoreStream,
	), "us-west-2"); len(got) != 0 {
		t.Errorf("relational/cache/stream need no AWS endpoint, got %v", got)
	}
	if got := datastoreEgressPorts(platformWith(
		platformv1alpha1.DatastoreObjectStore,
		platformv1alpha1.DatastoreKeyValue,
		platformv1alpha1.DatastoreQueue,
	)); len(got) != 0 {
		t.Errorf("objectStore/keyValue/queue are bounded by hostname, not port, got %v", got)
	}
}

// An unknown region fails CLOSED.
//
// The alternative to a named endpoint is a bare 443, and that is precisely the
// boundary this design refuses. A tenant reaching nothing is a visible outage; a
// tenant reaching everything on 443 is a silent hole.
func TestDatastoreFQDNs_UnknownRegionFailsClosed(t *testing.T) {
	if got := datastoreFQDNs(platformWith(platformv1alpha1.DatastoreObjectStore), ""); got != nil {
		t.Fatalf("an unknown region must yield no rule, got %v", got)
	}
}

// toFQDNs is inert unless cilium can observe the tenant's DNS answers, so the
// DNS rule has to carry an L7 matchPattern. Without it the policy is accepted,
// reports Valid, and silently denies every packet — which is the failure mode
// this whole file exists to stop repeating.
func TestTenantEgressCiliumRules_DNSRuleEnablesFQDNVisibility(t *testing.T) {
	rules := tenantEgressCiliumRules(nil, []string{"s3.us-west-2.amazonaws.com"})

	var sawDNSL7 bool
	for _, raw := range rules {
		rule := raw.(map[string]interface{})
		eps, _ := rule["toEndpoints"].([]interface{})
		if len(eps) == 0 {
			continue
		}
		lbls, _ := eps[0].(map[string]interface{})["matchLabels"].(map[string]interface{})
		if lbls["k8s:k8s-app"] != "kube-dns" {
			continue
		}
		for _, tp := range rule["toPorts"].([]interface{}) {
			if r, ok := tp.(map[string]interface{})["rules"].(map[string]interface{}); ok {
				if dns, ok := r["dns"].([]interface{}); ok && len(dns) > 0 {
					sawDNSL7 = true
				}
			}
		}
	}
	if !sawDNSL7 {
		t.Fatal("the kube-dns rule carries no L7 dns matchPattern — cilium cannot populate " +
			"its FQDN cache, so every toFQDNs rule matches nothing and silently denies")
	}
}
