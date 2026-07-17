/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// platform new scaffolds a single-Platform tenant directory: a five-document
// platform.yaml (Platform + BudgetPolicy + ModelGateway + AgentFleet +
// EvalSuite) plus a README. It is the persona-flexed happy-path scaffolder the
// persona guides (docs/personas/*) and the tenant chart reference. The model
// family/id come from the embedded model-default SSOT (model_defaults.json);
// the starter agent name + system prompt come from the scaffoldCopy table
// below.
//
// The YAML is emitted by a small ordered block-encoder (encodeDoc / foldFlow
// below) rather than a general marshaller, so the rendered bytes are stable and
// exactly reproducible — a golden fixture per persona
// (testdata/platform-new/*.yaml) pins the output byte-for-byte.

// PlatformNewOptions captures the inputs to the platform-new scaffolder.
type PlatformNewOptions struct {
	Name       string
	Tenant     string
	Persona    string
	MonthlyUsd int
	Output     string
}

// scaffoldCopy is the persona-specific starter agent: a name and a system
// prompt flexed to the persona's job. The model family/id are not here — they
// come from the model-default SSOT so there is one place to bump a default
// model.
var scaffoldCopy = map[string]struct {
	AgentName    string
	SystemPrompt string
}{
	"sales-ops": {"objection-handler", "You help sales-ops staff handle customer objections with cited references."},
	"support":   {"ticket-summarizer", "You summarize support tickets into a one-paragraph diagnosis + next step."},
	"finance":   {"financial-memo", "You draft financial memos. Always show your assumptions and cite sources."},
	"marketing": {"campaign-brief", "You draft campaign briefs in 5 sections. Be concise and concrete."},
	"ops":       {"oncall-summarizer", "You summarize on-call incidents into a runbook update candidate."},
	"founder":   {"strategy-memo", "You help draft strategy memos. Push back on weak reasoning."},
	"eng":       {"adr-drafter", "You draft Architectural Decision Records. Show trade-offs explicitly."},
	"legal":     {"policy-reviewer", "You review policy text against jurisdiction-specific compliance requirements."},
	"generic":   {"assistant", "You are a helpful assistant."},
}

// NewPlatformCmd wires `agentctl platform new`.
func NewPlatformCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "platform",
		Short: "Scaffold + manage Platform tenants",
	}
	cmd.AddCommand(newPlatformNewCmd())
	return cmd
}

func newPlatformNewCmd() *cobra.Command {
	var (
		name       string
		tenant     string
		persona    string
		monthlyUsd int
		output     string
	)
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Scaffold a new Platform tenant (Platform + Budget + Gateway + Fleet + Eval) into a directory",
		Long: `Writes <output>/<name>/platform.yaml (a five-document CR set) and a README,
using persona-flexed defaults. Apply with:

    agentctl platform new --name marketing-team --tenant acme --persona marketing --monthly-usd 2500
    kubectl apply -f marketing-team/platform.yaml

List supported personas with 'agentctl persona list'.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := WritePlatformNew(PlatformNewOptions{
				Name:       name,
				Tenant:     tenant,
				Persona:    persona,
				MonthlyUsd: monthlyUsd,
				Output:     output,
			})
			if err != nil {
				return err
			}
			fmt.Println("wrote", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Platform name (lowercase, alphanumeric + hyphens)")
	cmd.Flags().StringVar(&tenant, "tenant", "", "owning Tenant ID")
	cmd.Flags().StringVar(&persona, "persona", "generic", "one of: sales-ops, support, finance, ops, founder, eng, marketing, legal, generic")
	cmd.Flags().IntVar(&monthlyUsd, "monthly-usd", 500, "monthly USD budget")
	cmd.Flags().StringVar(&output, "output", ".", "output directory")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("tenant")
	return cmd
}

// WritePlatformNew renders the scaffold and writes it to <output>/<name>/,
// refusing to overwrite an existing directory. Returns the platform.yaml path.
func WritePlatformNew(opts PlatformNewOptions) (string, error) {
	yamlBytes, readme, err := RenderPlatformNew(opts)
	if err != nil {
		return "", err
	}
	outDir := filepath.Join(opts.Output, opts.Name)
	if _, err := os.Stat(outDir); err == nil {
		return "", fmt.Errorf("refusing to overwrite existing directory: %s", outDir)
	}
	// Scaffold output the user asked for on their own machine, to be read,
	// committed, and `kubectl apply`-ed — conventional 0755/0644, not secrets.
	if err := os.MkdirAll(outDir, 0o755); err != nil { //nolint:gosec // user-facing scaffold directory, not sensitive
		return "", fmt.Errorf("create %s: %w", outDir, err)
	}
	yamlPath := filepath.Join(outDir, "platform.yaml")
	if err := os.WriteFile(yamlPath, yamlBytes, 0o644); err != nil { //nolint:gosec // user-facing scaffold file, meant to be read + committed
		return "", fmt.Errorf("write %s: %w", yamlPath, err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "README.md"), []byte(readme), 0o644); err != nil { //nolint:gosec // user-facing scaffold file, meant to be read + committed
		return "", fmt.Errorf("write README.md: %w", err)
	}
	return yamlPath, nil
}

// RenderPlatformNew produces the five-document platform.yaml bytes and the
// README string for a Platform tenant scaffold. Pure: no filesystem access, so
// the golden fixtures can assert on the exact bytes.
func RenderPlatformNew(opts PlatformNewOptions) ([]byte, string, error) {
	if opts.Name == "" {
		return nil, "", fmt.Errorf("platform name required")
	}
	if opts.Tenant == "" {
		return nil, "", fmt.Errorf("tenant id required")
	}
	if opts.Persona == "" {
		opts.Persona = "generic"
	}
	sc, ok := scaffoldCopy[opts.Persona]
	if !ok {
		return nil, "", unknownPersonaErr(opts.Persona)
	}
	model, ok := parsedModelDefaults.Personas[opts.Persona]
	if !ok {
		return nil, "", unknownPersonaErr(opts.Persona)
	}

	name := opts.Name
	docs := []*ynode{
		newMap().
			set("apiVersion", str("platform.nanohype.dev/v1alpha1")).
			set("kind", str("Platform")).
			set("metadata", newMap().
				set("name", str(name)).
				set("labels", newMap().
					set("agents.nanohype.dev/persona", str(opts.Persona)).
					set("agents.nanohype.dev/tenant", str(opts.Tenant)))).
			set("spec", newMap().
				set("displayName", str(name)).
				set("persona", str(opts.Persona)).
				set("tenant", str(opts.Tenant)).
				set("isolation", str("namespace")).
				set("budget", newMap().set("name", str(name+"-budget"))).
				set("identity", newMap().set("allowedModelFamilies", strSeq(model.Family))).
				set("compliance", newMap().
					set("soc2", boolean(true)).
					set("hipaa", boolean(opts.Persona == "legal")))),
		newMap().
			set("apiVersion", str("governance.nanohype.dev/v1alpha1")).
			set("kind", str("BudgetPolicy")).
			set("metadata", newMap().
				set("name", str(name+"-budget")).
				set("labels", newMap().set("agents.nanohype.dev/tenant", str(opts.Tenant)))).
			set("spec", newMap().
				set("platformRef", newMap().set("name", str(name))).
				set("monthlyUsd", str(strconv.Itoa(opts.MonthlyUsd))).
				set("alertThresholdsPercent", intSeq(50, 80, 100)).
				set("killSwitchEnabled", boolean(true))),
		newMap().
			set("apiVersion", str("agents.nanohype.dev/v1alpha1")).
			set("kind", str("ModelGateway")).
			set("metadata", newMap().set("name", str(name+"-gateway"))).
			set("spec", newMap().
				set("platformRef", newMap().set("name", str(name))).
				set("routes", mapSeq(
					newMap().
						set("name", str("primary")).
						set("modelFamily", str(model.Family)).
						set("modelId", str(model.PrimaryModelID)).
						set("rateLimit", integer(60))))),
		newMap().
			set("apiVersion", str("agents.nanohype.dev/v1alpha1")).
			set("kind", str("AgentFleet")).
			set("metadata", newMap().set("name", str(name+"-fleet"))).
			set("spec", newMap().
				set("platformRef", newMap().set("name", str(name))).
				set("scaling", newMap().
					set("enabled", boolean(true)).
					set("min", integer(1)).
					set("max", integer(5)).
					set("queueDepthTrigger", integer(10))).
				set("agents", mapSeq(
					newMap().
						set("name", str(sc.AgentName)).
						set("systemPrompt", str(sc.SystemPrompt)).
						set("modelRoute", str("primary"))))),
		newMap().
			set("apiVersion", str("governance.nanohype.dev/v1alpha1")).
			set("kind", str("EvalSuite")).
			set("metadata", newMap().set("name", str(name+"-eval"))).
			set("spec", newMap().
				set("platformRef", newMap().set("name", str(name))).
				set("agentFleetRef", newMap().set("name", str(name+"-fleet"))).
				set("schedule", str("0 6 * * *")).
				set("passThreshold", str("0.85")).
				set("cases", mapSeq(
					newMap().
						set("name", str("smoke-test")).
						set("input", str("Reply with 'pong'.")).
						set("expectContains", strSeq("pong")).
						set("maxLatencyMs", integer(5000))))),
	}

	rendered := make([]string, len(docs))
	for i, d := range docs {
		rendered[i] = encodeMap(d, 0)
	}
	yamlBytes := []byte(strings.Join(rendered, "---\n"))

	readme := fmt.Sprintf("# %s\n\nGenerated tenant scaffold for persona **%s**.\n\nApply: `kubectl apply -f platform.yaml`.\n", name, opts.Persona)
	return yamlBytes, readme, nil
}

func unknownPersonaErr(name string) error {
	keys := make([]string, 0, len(scaffoldCopy))
	for k := range scaffoldCopy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("unknown persona %q; supported: %s", name, strings.Join(keys, ", "))
}

// ── Ordered block-YAML encoder ───────────────────────────────────────────────
//
// Reproduces the block-style output the retired TypeScript scaffolder emitted
// via the `yaml` package (eemeli/yaml) with default options: 2-space indent,
// indented block sequences, plain scalars quoted only when they would otherwise
// resolve as a number/bool/null, and plain scalars folded at an 80-column line
// width. Golden fixtures pin the bytes.

type scalarKind int

const (
	kString scalarKind = iota
	kInt
	kBool
)

// ynode is an ordered YAML node: a scalar, a block map, a sequence of scalars,
// or a sequence of maps. Only the shapes the scaffold emits are modelled.
type ynode struct {
	scalar   *scalarVal
	mapKeys  []string
	mapVals  []*ynode
	strItems []string
	intItems []int
	mapItems []*ynode
	isMap    bool
	isStrSeq bool
	isIntSeq bool
	isMapSeq bool
}

type scalarVal struct {
	kind scalarKind
	s    string
	i    int
	b    bool
}

func str(s string) *ynode       { return &ynode{scalar: &scalarVal{kind: kString, s: s}} }
func integer(i int) *ynode      { return &ynode{scalar: &scalarVal{kind: kInt, i: i}} }
func boolean(b bool) *ynode     { return &ynode{scalar: &scalarVal{kind: kBool, b: b}} }
func strSeq(v ...string) *ynode { return &ynode{isStrSeq: true, strItems: v} }
func intSeq(v ...int) *ynode    { return &ynode{isIntSeq: true, intItems: v} }
func mapSeq(v ...*ynode) *ynode { return &ynode{isMapSeq: true, mapItems: v} }
func newMap() *ynode            { return &ynode{isMap: true} }

func (m *ynode) set(key string, val *ynode) *ynode {
	m.mapKeys = append(m.mapKeys, key)
	m.mapVals = append(m.mapVals, val)
	return m
}

// encodeMap renders an ordered map at the given indent, returning text that
// ends with a trailing newline.
func encodeMap(m *ynode, indent int) string {
	var b strings.Builder
	pad := strings.Repeat(" ", indent)
	for i, key := range m.mapKeys {
		v := m.mapVals[i]
		switch {
		case v.scalar != nil:
			b.WriteString(pad + key + ": " + renderScalar(v.scalar, indent, len(key)+2) + "\n")
		case v.isMap:
			b.WriteString(pad + key + ":\n")
			b.WriteString(encodeMap(v, indent+2))
		case v.isStrSeq:
			b.WriteString(pad + key + ":\n")
			for _, it := range v.strItems {
				b.WriteString(strings.Repeat(" ", indent+2) + "- " + renderScalar(&scalarVal{kind: kString, s: it}, indent+2, 2) + "\n")
			}
		case v.isIntSeq:
			b.WriteString(pad + key + ":\n")
			for _, it := range v.intItems {
				b.WriteString(strings.Repeat(" ", indent+2) + "- " + strconv.Itoa(it) + "\n")
			}
		case v.isMapSeq:
			b.WriteString(pad + key + ":\n")
			for _, it := range v.mapItems {
				b.WriteString(encodeSeqMapItem(it, indent+2))
			}
		}
	}
	return b.String()
}

// encodeSeqMapItem renders a map as a block-sequence item: the map is rendered
// at dashIndent+2, then the first line's leading whitespace is rewritten to the
// "- " dash marker (dashIndent spaces + "- " has the same width as dashIndent+2
// spaces, so downstream columns are unchanged).
func encodeSeqMapItem(m *ynode, dashIndent int) string {
	body := encodeMap(m, dashIndent+2)
	marker := strings.Repeat(" ", dashIndent) + "- "
	return marker + body[dashIndent+2:]
}

// renderScalar renders a scalar value. keyLen is the column offset of the value
// on its first line (len(key)+2 for "key: "), used by the plain-scalar folder.
func renderScalar(s *scalarVal, indent, indentAtStart int) string {
	switch s.kind {
	case kInt:
		return strconv.Itoa(s.i)
	case kBool:
		if s.b {
			return "true"
		}
		return "false"
	default:
		if needsDoubleQuote(s.s) {
			return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s.s) + `"`
		}
		// Plain scalars fold at the value's continuation indent (indent+2).
		return foldFlow(s.s, strings.Repeat(" ", indent+2), indentAtStart)
	}
}

var (
	yamlIntRe   = regexp.MustCompile(`^[-+]?[0-9]+$`)
	yamlFloatRe = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
)

// needsDoubleQuote reports whether a string would be misread as a non-string
// scalar (int, float, bool, null) or is otherwise unsafe as a plain block
// scalar, and so must be quoted — matching eemeli/yaml's plain-scalar guard for
// the value shapes this scaffold emits.
func needsDoubleQuote(s string) bool {
	if s == "" {
		return true
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "~", "yes", "no", "on", "off":
		return true
	}
	if yamlIntRe.MatchString(s) || yamlFloatRe.MatchString(s) {
		return true
	}
	if strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return true
	}
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") || strings.Contains(s, " #") {
		return true
	}
	switch s[0] {
	case '!', '&', '*', '?', '|', '>', '@', '`', '"', '\'', '%', '#', ',', '[', ']', '{', '}', ':', '-':
		return true
	}
	return false
}

// foldFlow reproduces eemeli/yaml's foldFlowLines in FOLD_FLOW mode: a plain
// scalar longer than the line budget is broken on interior spaces, each
// continuation line prefixed with `indent`. indentAtStart is the column the
// value starts at on its first line.
func foldFlow(text, indent string, indentAtStart int) string {
	const lineWidth = 80
	const minContentWidth = 20
	endStep := 1 + lineWidth - len(indent)
	if 1+minContentWidth > endStep {
		endStep = 1 + minContentWidth
	}
	if len(text) <= endStep {
		return text
	}
	var folds []int
	end := lineWidth - len(indent)
	overflowLimit := lineWidth - max(2, minContentWidth)
	if indentAtStart > overflowLimit {
		folds = append(folds, 0)
	} else {
		end = lineWidth - indentAtStart
	}
	split := -1
	var prev byte
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '\n' {
			end = i + len(indent) + endStep
			split = -1
		} else {
			if ch == ' ' && prev != 0 && prev != ' ' && prev != '\n' && prev != '\t' {
				if i+1 < len(text) {
					next := text[i+1]
					if next != ' ' && next != '\n' && next != '\t' {
						split = i
					}
				}
			}
			if i >= end && split > 0 {
				folds = append(folds, split)
				end = split + endStep
				split = -1
			}
		}
		prev = ch
	}
	if len(folds) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text[:folds[0]])
	for k, fold := range folds {
		segEnd := len(text)
		if k+1 < len(folds) {
			segEnd = folds[k+1]
		}
		if fold == 0 {
			b.WriteString("\n" + indent + text[:segEnd])
		} else {
			b.WriteString("\n" + indent + text[fold+1:segEnd])
		}
	}
	return b.String()
}
