/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestPlatformNew_ByteForByteFixture is the output-fidelity gate for the CLI:
// `platform new` must reproduce, byte-for-byte, the golden fixture committed
// under testdata/platform-new for every persona. Any drift in the scaffolder
// (indentation, quoting, folding, key order, or the default case set) fails
// here, so the emitted bytes only ever change on purpose alongside a fixture
// update.
func TestPlatformNew_ByteForByteFixture(t *testing.T) {
	for _, persona := range []string{
		"sales-ops", "support", "finance", "ops", "founder", "eng", "marketing", "legal", "generic",
	} {
		t.Run(persona, func(t *testing.T) {
			got, _, err := RenderPlatformNew(PlatformNewOptions{
				Name:       "acme-assist",
				Tenant:     "acme",
				Persona:    persona,
				MonthlyUsd: 250,
			})
			if err != nil {
				t.Fatalf("RenderPlatformNew: %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "platform-new", persona+".yaml"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("scaffold for persona %q is not byte-identical to the golden fixture.\n--- got ---\n%s\n--- want ---\n%s", persona, got, want)
			}
		})
	}
}

// TestPlatformNew_ReadmeMatchesFixture pins the generated README bytes.
func TestPlatformNew_ReadmeMatchesFixture(t *testing.T) {
	_, readme, err := RenderPlatformNew(PlatformNewOptions{Name: "acme-assist", Tenant: "acme", Persona: "generic", MonthlyUsd: 250})
	if err != nil {
		t.Fatalf("RenderPlatformNew: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "platform-new", "README.md"))
	if err != nil {
		t.Fatalf("read golden README: %v", err)
	}
	if readme != string(want) {
		t.Errorf("README mismatch.\n--- got ---\n%s\n--- want ---\n%s", readme, want)
	}
}

// TestPlatformNew_EmitsFiveDocsWiredToOnePlatform ports the TypeScript
// "emits the five-document tenant scaffold wired to one Platform" case: the
// rendered YAML parses into Platform, BudgetPolicy, ModelGateway, AgentFleet,
// EvalSuite, and every non-Platform doc references the Platform.
func TestPlatformNew_EmitsFiveDocsWiredToOnePlatform(t *testing.T) {
	yamlBytes, _, err := RenderPlatformNew(PlatformNewOptions{Name: "acme-assist", Tenant: "acme", Persona: "generic", MonthlyUsd: 250})
	if err != nil {
		t.Fatalf("RenderPlatformNew: %v", err)
	}
	docs := parseDocs(t, yamlBytes)
	if len(docs) != 5 {
		t.Fatalf("want 5 documents, got %d", len(docs))
	}
	wantKinds := []string{"Platform", "BudgetPolicy", "ModelGateway", "AgentFleet", "EvalSuite"}
	for i, want := range wantKinds {
		if got := docs[i]["kind"]; got != want {
			t.Errorf("doc %d kind: got %v want %q", i, got, want)
		}
	}
	for i := 1; i < len(docs); i++ {
		spec, _ := docs[i]["spec"].(map[string]any)
		ref, _ := spec["platformRef"].(map[string]any)
		if ref["name"] != "acme-assist" {
			t.Errorf("doc %d platformRef: got %v want acme-assist", i, ref["name"])
		}
	}
	evalSpec, _ := docs[4]["spec"].(map[string]any)
	fleetRef, _ := evalSpec["agentFleetRef"].(map[string]any)
	if fleetRef["name"] != "acme-assist-fleet" {
		t.Errorf("eval agentFleetRef: got %v want acme-assist-fleet", fleetRef["name"])
	}
}

// TestPlatformNew_CarriesBudgetAndTenant ports the TypeScript "carries the
// budget and tenant through to the emitted specs" case.
func TestPlatformNew_CarriesBudgetAndTenant(t *testing.T) {
	yamlBytes, _, err := RenderPlatformNew(PlatformNewOptions{Name: "acme-assist", Tenant: "acme", Persona: "generic", MonthlyUsd: 250})
	if err != nil {
		t.Fatalf("RenderPlatformNew: %v", err)
	}
	docs := parseDocs(t, yamlBytes)
	platformSpec, _ := docs[0]["spec"].(map[string]any)
	if platformSpec["tenant"] != "acme" {
		t.Errorf("platform tenant: got %v want acme", platformSpec["tenant"])
	}
	budget, _ := platformSpec["budget"].(map[string]any)
	if budget["name"] != "acme-assist-budget" {
		t.Errorf("platform budget name: got %v want acme-assist-budget", budget["name"])
	}
	budgetSpec, _ := docs[1]["spec"].(map[string]any)
	if budgetSpec["monthlyUsd"] != "250" {
		t.Errorf("budget monthlyUsd: got %v want \"250\" (string)", budgetSpec["monthlyUsd"])
	}
	if budgetSpec["killSwitchEnabled"] != true {
		t.Errorf("budget killSwitchEnabled: got %v want true", budgetSpec["killSwitchEnabled"])
	}
}

// TestPlatformNew_RoutesFleetAtPrimary ports the TypeScript "routes the fleet
// agent at the gateway primary route for the persona" case, including that the
// route model comes from the shared model-default SSOT.
func TestPlatformNew_RoutesFleetAtPrimary(t *testing.T) {
	yamlBytes, _, err := RenderPlatformNew(PlatformNewOptions{Name: "acme-assist", Tenant: "acme", Persona: "support", MonthlyUsd: 250})
	if err != nil {
		t.Fatalf("RenderPlatformNew: %v", err)
	}
	docs := parseDocs(t, yamlBytes)
	gatewaySpec, _ := docs[2]["spec"].(map[string]any)
	routes, _ := gatewaySpec["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %d", len(routes))
	}
	route0, _ := routes[0].(map[string]any)
	if route0["name"] != "primary" {
		t.Errorf("route name: got %v want primary", route0["name"])
	}
	if route0["modelId"] != parsedModelDefaults.Personas["support"].PrimaryModelID {
		t.Errorf("route modelId: got %v want SSOT %q", route0["modelId"], parsedModelDefaults.Personas["support"].PrimaryModelID)
	}
	fleetSpec, _ := docs[3]["spec"].(map[string]any)
	agents, _ := fleetSpec["agents"].([]any)
	agent0, _ := agents[0].(map[string]any)
	if agent0["modelRoute"] != "primary" {
		t.Errorf("agent modelRoute: got %v want primary", agent0["modelRoute"])
	}
}

// TestPlatformNew_LegalGetsHipaa asserts the legal persona flips hipaa on while
// the others leave it off — the one persona-conditioned compliance field.
func TestPlatformNew_LegalGetsHipaa(t *testing.T) {
	for _, tc := range []struct {
		persona string
		hipaa   bool
	}{{"legal", true}, {"generic", false}, {"finance", false}} {
		yamlBytes, _, err := RenderPlatformNew(PlatformNewOptions{Name: "x", Tenant: "acme", Persona: tc.persona, MonthlyUsd: 250})
		if err != nil {
			t.Fatalf("RenderPlatformNew(%s): %v", tc.persona, err)
		}
		docs := parseDocs(t, yamlBytes)
		spec, _ := docs[0]["spec"].(map[string]any)
		compliance, _ := spec["compliance"].(map[string]any)
		if compliance["hipaa"] != tc.hipaa {
			t.Errorf("persona %q hipaa: got %v want %v", tc.persona, compliance["hipaa"], tc.hipaa)
		}
	}
}

// TestPlatformNew_RejectsUnknownPersona ports the "rejects personas outside the
// schema" case.
func TestPlatformNew_RejectsUnknownPersona(t *testing.T) {
	_, _, err := RenderPlatformNew(PlatformNewOptions{Name: "x", Tenant: "acme", Persona: "astrologer", MonthlyUsd: 250})
	if err == nil || !strings.Contains(err.Error(), "unknown persona") {
		t.Errorf("want unknown-persona error, got %v", err)
	}
}

// TestPlatformNew_RequiresNameAndTenant covers the required inputs.
func TestPlatformNew_RequiresNameAndTenant(t *testing.T) {
	if _, _, err := RenderPlatformNew(PlatformNewOptions{Tenant: "acme", Persona: "generic"}); err == nil {
		t.Error("want error for missing name")
	}
	if _, _, err := RenderPlatformNew(PlatformNewOptions{Name: "x", Persona: "generic"}); err == nil {
		t.Error("want error for missing tenant")
	}
}

// TestWritePlatformNew_WritesAndRefusesOverwrite ports the "refuses to
// overwrite an existing scaffold directory" case and confirms the files land.
func TestWritePlatformNew_WritesAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path, err := WritePlatformNew(PlatformNewOptions{Name: "acme-assist", Tenant: "acme", Persona: "generic", MonthlyUsd: 250, Output: dir})
	if err != nil {
		t.Fatalf("WritePlatformNew: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("platform.yaml not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "acme-assist", "README.md")); err != nil {
		t.Fatalf("README.md not written: %v", err)
	}
	// The written file must equal the golden fixture byte-for-byte.
	got, _ := os.ReadFile(path)
	want, _ := os.ReadFile(filepath.Join("testdata", "platform-new", "generic.yaml"))
	if string(got) != string(want) {
		t.Errorf("written platform.yaml is not byte-identical to the golden fixture")
	}
	// Second write refuses to overwrite.
	if _, err := WritePlatformNew(PlatformNewOptions{Name: "acme-assist", Tenant: "acme", Persona: "generic", MonthlyUsd: 250, Output: dir}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("want refuse-to-overwrite error, got %v", err)
	}
}

// vocabularyFixtureOptions is the one scaffold that exercises the optional half
// of the Platform declaration. Kept beside the fixture it pins so the two move
// together.
func vocabularyFixtureOptions(t *testing.T) PlatformNewOptions {
	t.Helper()
	vocab, err := ParseVocabulary("acme-assist", VocabularyFlags{
		Datastores: []string{
			"name=corpus,kind=objectStore",
			"name=chunks,kind=keyValue,partitionKey=docId:S,sortKey=chunk:N",
			"name=work,kind=queue,deletionPolicy=Delete",
		},
		Capabilities:      []string{"eventBridgeScheduler"},
		DirectSecretReads: []string{"vendor/embed-api-key"},
		Operators:         []string{"operator@example.com"},
	})
	if err != nil {
		t.Fatalf("ParseVocabulary: %v", err)
	}
	return PlatformNewOptions{
		Name: "acme-assist", Tenant: "acme", Persona: "eng", MonthlyUsd: 250,
		Vocabulary: vocab,
	}
}

// TestPlatformNew_VocabularyFixture pins the bytes of a scaffold that carries
// datastores, capabilities, direct secret reads and attribution. The nine persona
// fixtures all render with the vocabulary off, so without this case the whole
// emission path is unpinned — every optional key is omit-when-absent, and the
// absent branch is the only one they cover.
func TestPlatformNew_VocabularyFixture(t *testing.T) {
	got, _, err := RenderPlatformNew(vocabularyFixtureOptions(t))
	if err != nil {
		t.Fatalf("RenderPlatformNew: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "platform-new", "vocabulary.yaml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("vocabulary scaffold is not byte-identical to the golden fixture.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestPlatformNew_AttributeTypeSurvivesRoundTrip is the regression case for a
// scalar the encoder got wrong: `N` is a boolean in YAML 1.1, the dialect kubectl
// and sigs.k8s.io/yaml read. Emitted bare, a DynamoDB numeric key's type parses
// back as `false` against a string enum — valid YAML, rejected at admission, and
// invisible in the emitted file to anyone who does not know the dialect.
func TestPlatformNew_AttributeTypeSurvivesRoundTrip(t *testing.T) {
	yamlBytes, _, err := RenderPlatformNew(vocabularyFixtureOptions(t))
	if err != nil {
		t.Fatalf("RenderPlatformNew: %v", err)
	}
	if !strings.Contains(string(yamlBytes), `type: "N"`) {
		t.Error(`the numeric key's type must be emitted quoted as type: "N"`)
	}

	docs := parseDocs(t, yamlBytes)
	spec, _ := docs[0]["spec"].(map[string]any)
	datastores, _ := spec["datastores"].([]any)
	if len(datastores) != 3 {
		t.Fatalf("want 3 datastores, got %d", len(datastores))
	}
	chunks, _ := datastores[1].(map[string]any)
	keyValue, _ := chunks["keyValue"].(map[string]any)
	sortKey, _ := keyValue["sortKey"].(map[string]any)
	if sortKey["type"] != "N" {
		t.Errorf("sort key type round-tripped as %#v, want the string \"N\"", sortKey["type"])
	}
	partitionKey, _ := keyValue["partitionKey"].(map[string]any)
	if partitionKey["type"] != "S" {
		t.Errorf("partition key type round-tripped as %#v, want the string \"S\"", partitionKey["type"])
	}
}

// TestPlatformNew_OmitsAbsentVocabulary asserts the encoder omits every optional
// key rather than emitting it empty. set() is unconditional, so an empty list
// would render `capabilities:` with a null value — which the API server reads as
// an explicit null, not as absent.
func TestPlatformNew_OmitsAbsentVocabulary(t *testing.T) {
	yamlBytes, _, err := RenderPlatformNew(PlatformNewOptions{
		Name: "acme-assist", Tenant: "acme", Persona: "generic", MonthlyUsd: 250,
	})
	if err != nil {
		t.Fatalf("RenderPlatformNew: %v", err)
	}
	for _, key := range []string{"datastores", "attribution", "capabilities", "directSecretReads"} {
		if strings.Contains(string(yamlBytes), key+":") {
			t.Errorf("%q should be omitted entirely when not declared, but appears in the output", key)
		}
	}
	docs := parseDocs(t, yamlBytes)
	spec, _ := docs[0]["spec"].(map[string]any)
	if _, present := spec["datastores"]; present {
		t.Error("spec.datastores should be absent, not null")
	}
}

// parseDocs splits a multi-document YAML byte slice and unmarshals each into a
// generic map for assertions.
func parseDocs(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(string(b), "\n---\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("unmarshal doc: %v\n%s", err, raw)
		}
		out = append(out, m)
	}
	return out
}
