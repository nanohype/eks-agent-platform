#!/usr/bin/env python3
"""The operator image the chart names must be able to honour the chart's own contract.

WHY THIS EXISTS

`charts/operator` ships the eval WorkflowTemplate, and `operators/` ships the
reconciler that submits workflows against it. Those two are one contract in two
files: the template DECLARES the parameters a run needs, the reconciler SUPPLIES
them. They are versioned separately — the chart by `version`, the binary by
`appVersion` — so a chart can name a binary from a different generation of the
contract, and every gate here stays green while it does.

That is not hypothetical. Chart 0.6.2 shipped a WorkflowTemplate whose route
contract is three parameters:

    model-route-base-url    the gateway prefix for the wire format
    model-route             the name that goes in the request body's `model`
    model-route-api         which body to build and which response to expect

with `appVersion: "0.5.0"` still naming the image built before that change,
whose reconciler supplied a single `gateway-url` and none of the three.

Nothing failed. `check-image-refs.py` asks whether the referenced image RESOLVES,
and 0.5.0 resolves — it is published, signed and pullable. It has no way to ask
whether the image is the same GENERATION as the templates beside it.
`check-eval-workflow.py` asserts the template's pods can be admitted and its
scripts can run, both of which stay true. helm lint, kubeconform and trivy all
pass: the manifest is valid.

The runtime consequence is the worst available shape. The three parameters carry
`value: "REPLACE_BY_EVALSUITE"` defaults — required, because Argo Workflows v4+
rejects a null parameter value — so an older reconciler that never overrides them
does not crash the workflow. The run proceeds and POSTs to the literal string
`REPLACE_BY_EVALSUITE`. Every case errors, the suite scores zero, and the result
reads as "the model failed every eval" rather than "these two artifacts are from
different releases."

WHAT THIS CHECKS

    A. Every parameter the WorkflowTemplate marks REPLACE_BY_EVALSUITE — the
       sentinel that means "the reconciler fills this in per run" — is supplied
       by the reconciler AT THE COMMIT THE CHART'S appVersion NAMES, not at HEAD.
       HEAD is what a developer reads and is exactly the thing that misleads:
       the source can be perfectly correct and shipping a binary that is not.

    B. The reverse skew: every parameter that reconciler supplies is DECLARED by
       the template. Argo accepts an undeclared parameter silently, so a
       reconciler still sending `gateway-url` at a template that dropped it
       produces no error anywhere — the value simply goes nowhere.

    C. Every CRD the chart installs declares the same fields, in every version,
       as the CRD generated at that tag (operators/config/crd/bases), and the
       chart installs every CRD that tag generates. A field the binary has no Go
       struct field for is dropped on decode; a field the binary writes that the
       chart's CRD does not declare is pruned by the API server; a kind the
       binary watches and the chart does not install keeps it from starting.
       Descriptions, defaults and validation are not compared: they change what
       the API server documents or admits, not what the binary can decode, and
       a documentation-only or admission-only chart bump is legitimate.

The appVersion is resolved to source through the release tag (`operator-v<X>`),
which is the only thing that builds an image — see `.github/workflows/release.yaml`.
A missing tag is a failure, not a skip: it means the chart names a version this
repo cannot account for.

TWO MODES

    default    the merge gate. A and B fail it. A C mismatch is reported as a
               HOLD and does not: the chart's CRDs are generated from HEAD's Go
               types (the crd-docs job diffs them), and an operator tag can only
               be cut from a commit after its merge, so every change to a CRD
               field merges ahead of the release that can honour it. Failing the
               merge on C would forbid changing a CRD field at all.

    --release  the publication gate, run by the charts-oci job before it
               packages charts/operator. A, B and C all fail it, and the job
               holds the operator chart rather than publish it — so the chart
               is published only once appVersion names an operator release
               whose CRDs match the ones it installs.

WHAT IT DOES NOT COVER, DELIBERATELY

    Parameters marked REPLACE_BY_APPLICATIONSET are filled by eks-gitops, in
    another repo this gate cannot read. They are reported and not enforced.

    Conditional supply. `cases-inline` and `cases-manifest` are appended inside
    branches, and this counts them as supplied. Deciding whether a branch is
    reachable for a given EvalSuite is beyond static reach, and a gate that
    claimed to do it would be claiming more than it delivers.

Usage:
    scripts/check-runtime-contract.py             # merge gate: A and B fail, C holds
    scripts/check-runtime-contract.py --list      # print both sides, then check
    scripts/check-runtime-contract.py --release   # publication gate: A, B and C fail
    scripts/check-runtime-contract.py --self-test # prove C's comparison can fail
"""

from __future__ import annotations

import argparse

import re
import subprocess
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import EXIT_CANNOT_EVALUATE, require_binary
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - environment guard
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
CHART_YAML = ROOT / "charts" / "operator" / "Chart.yaml"
WORKFLOW_TEMPLATE = ROOT / "charts" / "operator" / "files" / "eval-runtime" / "workflow-template.yaml"
RECONCILER = "operators/internal/controller/eval_reconcile.go"
CHART_CRDS = ROOT / "charts" / "operator" / "crds"
OPERATOR_CRDS = "operators/config/crd/bases"

# The sentinel a parameter's default carries when the reconciler is responsible
# for it. Distinct from REPLACE_BY_APPLICATIONSET, which names a different filler
# in a different repo.
SENTINEL_RECONCILER = "REPLACE_BY_EVALSUITE"
SENTINEL_APPLICATIONSET = "REPLACE_BY_APPLICATIONSET"

# A supplied parameter is a map literal carrying BOTH "name" and "value". The
# "value" key is load-bearing: `workflowTemplateRef` is also a
# map[string]any{"name": ...} and is not a parameter. Matching on "name" alone
# reports the template ref as a thirteenth parameter that no template declares.
SUPPLIED_PARAM = re.compile(r'map\[string\]any\{"name":\s*"([a-z0-9-]+)",\s*"value":')


def run(*args: str) -> str:
    proc = subprocess.run(args, cwd=ROOT, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.exit(f"FAIL: `{' '.join(args)}` exited {proc.returncode}\n{proc.stderr.strip()}")
    return proc.stdout


def chart_versions() -> tuple[str, str]:
    """(version, appVersion) from the operator chart's Chart.yaml."""
    chart = yaml.safe_load(CHART_YAML.read_text())
    app_version = str(chart.get("appVersion", "")).strip()
    if not app_version:
        sys.exit(f"FAIL: {CHART_YAML.relative_to(ROOT)} declares no appVersion.")
    return str(chart.get("version", "")).strip(), app_version


def field_paths(schema: object, prefix: str = "") -> set[str]:
    """Every field path an OpenAPI v3 schema declares.

    `[]` marks a list item and `{}` a map value, so a field moving between an
    object, a list of them and a map of them reads as a different path, which
    is what it is to a decoder.
    """
    if not isinstance(schema, dict):
        return set()
    out: set[str] = set()
    for name, sub in (schema.get("properties") or {}).items():
        path = f"{prefix}.{name}" if prefix else name
        out.add(path)
        out |= field_paths(sub, path)
    out |= field_paths(schema.get("items"), prefix + "[]")
    out |= field_paths(schema.get("additionalProperties"), prefix + "{}")
    return out


def crd_fields(text: str, where: str) -> dict[str, set[str]]:
    """version name -> the field paths its schema declares, for one CRD document.

    A document that parses to no version, or to a version with no schema, is
    not a CRD with no fields: comparing it would pass everything, so it stops
    the gate instead.
    """
    try:
        doc = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        print(f"FAIL: {where} is not parseable YAML:\n{exc}")
        sys.exit(EXIT_CANNOT_EVALUATE)
    versions = ((doc or {}).get("spec") or {}).get("versions") or []
    out: dict[str, set[str]] = {}
    for v in versions:
        schema = ((v or {}).get("schema") or {}).get("openAPIV3Schema")
        if not isinstance(schema, dict) or not v.get("name"):
            print(f"FAIL: {where} has a version with no name or no openAPIV3Schema; it cannot be compared.")
            sys.exit(EXIT_CANNOT_EVALUATE)
        out[v["name"]] = field_paths(schema)
    if not out:
        print(f"FAIL: parsed no versions out of {where}; an empty CRD would match any binary.")
        sys.exit(EXIT_CANNOT_EVALUATE)
    return out


def crd_skew(chart: dict[str, dict[str, set[str]]], binary: dict[str, dict[str, set[str]]], ref: str) -> list[str]:
    """Check C: the chart's CRDs against the CRDs the binary at `ref` generates.

    Both arguments map a CRD file name to crd_fields() of it.
    """
    findings: list[str] = []
    for name in sorted(set(binary) - set(chart)):
        findings.append(f"{name}: the operator at {ref} reconciles this kind, and the chart does not install its CRD")
    for name in sorted(set(chart) - set(binary)):
        findings.append(f"{name}: the chart installs this CRD, and the operator at {ref} has no such kind")
    for name in sorted(set(chart) & set(binary)):
        for version in sorted(set(binary[name]) - set(chart[name])):
            findings.append(f"{name}: the operator at {ref} serves {version}, which the chart's CRD does not")
        for version in sorted(set(chart[name]) - set(binary[name])):
            findings.append(f"{name}: the chart's CRD serves {version}, which the operator at {ref} does not")
        for version in sorted(set(chart[name]) & set(binary[name])):
            for path in sorted(chart[name][version] - binary[name][version]):
                findings.append(f"{name} {version}: the chart's CRD declares {path}, which the operator at {ref} does not decode")
            for path in sorted(binary[name][version] - chart[name][version]):
                findings.append(f"{name} {version}: the operator at {ref} declares {path}, which the chart's CRD prunes")
    return findings


def chart_crds() -> dict[str, dict[str, set[str]]]:
    files = sorted(CHART_CRDS.glob("*.yaml"))
    if not files:
        print(f"FAIL: no CRDs under {CHART_CRDS.relative_to(ROOT)}; check C would compare nothing.")
        sys.exit(EXIT_CANNOT_EVALUATE)
    return {f.name: crd_fields(f.read_text(), str(f.relative_to(ROOT))) for f in files}


def binary_crds(ref: str) -> dict[str, dict[str, set[str]]]:
    names = [
        line.rsplit("/", 1)[-1]
        for line in run("git", "ls-tree", "--name-only", f"{ref}:{OPERATOR_CRDS}").split()
        if line.endswith(".yaml")
    ]
    if not names:
        print(f"FAIL: no CRDs under {OPERATOR_CRDS} at {ref}; check C would compare nothing.")
        sys.exit(EXIT_CANNOT_EVALUATE)
    return {n: crd_fields(run("git", "show", f"{ref}:{OPERATOR_CRDS}/{n}"), f"{OPERATOR_CRDS}/{n} at {ref}") for n in names}


def declared_parameters() -> dict[str, str | None]:
    """Parameter name -> default value, from the template's arguments.parameters."""
    try:
        doc = yaml.safe_load(WORKFLOW_TEMPLATE.read_text())
    except yaml.YAMLError as exc:
        sys.exit(
            f"FAIL: {WORKFLOW_TEMPLATE.relative_to(ROOT)} is not parseable YAML, so the "
            f"contract it declares cannot be read:\n{exc}"
        )
    params = (doc or {}).get("spec", {}).get("arguments", {}).get("parameters")
    if not params:
        sys.exit(
            f"FAIL: parsed no parameters out of {WORKFLOW_TEMPLATE.relative_to(ROOT)}.\n"
            "       An empty parse satisfies every comparison below, so it fails here "
            "instead of passing vacuously."
        )
    return {p["name"]: p.get("value") for p in params if isinstance(p, dict) and "name" in p}


def supplied_parameters(ref: str) -> set[str]:
    """Parameter names the reconciler supplies, read at a git ref rather than at HEAD."""
    source = run("git", "show", f"{ref}:{RECONCILER}")
    supplied = set(SUPPLIED_PARAM.findall(source))
    if not supplied:
        sys.exit(
            f"FAIL: parsed no supplied parameters out of {RECONCILER} at {ref}.\n"
            "       Either the construction changed shape or SUPPLIED_PARAM stopped "
            "matching it. An empty set would pass check A silently, so it fails here."
        )
    return supplied


def main(listing: bool, release: bool) -> int:
    require_binary("git", "read the committed tree this compares the runtime contract against")

    chart_version, app_version = chart_versions()
    tag = f"operator-v{app_version}"

    tags = run("git", "tag", "--list", tag).split()
    if tag not in tags:
        print(f"FAIL: chart appVersion {app_version} names release tag {tag}, which does not exist.")
        print("      Only a tag builds an operator image (.github/workflows/release.yaml), so an")
        print("      appVersion with no tag behind it names an image nothing ever built.")
        print("      If the tag exists upstream, this checkout is missing it — fetch-depth: 0.")
        return 1

    commit = run("git", "rev-parse", "--short", f"{tag}^{{commit}}").strip()
    declared = declared_parameters()
    supplied = supplied_parameters(tag)

    reconciler_owned = {n for n, v in declared.items() if v == SENTINEL_RECONCILER}
    appset_owned = {n for n, v in declared.items() if v == SENTINEL_APPLICATIONSET}

    if not reconciler_owned:
        print(f"FAIL: no declared parameter carries the {SENTINEL_RECONCILER} sentinel.")
        print("      Check A compares against that set, so an empty one makes this gate assert")
        print("      nothing while reporting success. Either the contract changed shape or the")
        print("      sentinel was renamed; both need a human.")
        return 1

    if listing:
        print(f"chart appVersion : {app_version}  ->  {tag} ({commit})")
        print(f"declared by template ({len(declared)}):")
        for name in sorted(declared):
            owner = {
                SENTINEL_RECONCILER: "reconciler",
                SENTINEL_APPLICATIONSET: "applicationset",
            }.get(declared[name], "default")
            print(f"  {name:<24} [{owner}]")
        print(f"supplied by reconciler at {tag} ({len(supplied)}):")
        for name in sorted(supplied):
            print(f"  {name}")
        print()

    failures: list[str] = []

    # A. every reconciler-owned parameter is actually supplied by that binary
    missing = sorted(reconciler_owned - supplied)
    for name in missing:
        failures.append(
            f"template requires '{name}' from the reconciler, but the binary at {tag} "
            f"({commit}) never supplies it — a run gets the literal "
            f"'{SENTINEL_RECONCILER}'"
        )

    # B. every supplied parameter is declared; Argo drops undeclared ones in silence
    undeclared = sorted(supplied - set(declared))
    for name in undeclared:
        failures.append(
            f"reconciler at {tag} ({commit}) supplies '{name}', which the template does not "
            f"declare — Argo accepts it and discards it"
        )

    if appset_owned:
        print(
            f"note: {len(appset_owned)} parameter(s) filled by eks-gitops, not enforced here: "
            + ", ".join(sorted(appset_owned))
        )

    # C. the chart's CRDs and the binary's decode the same fields
    skew = crd_skew(chart_crds(), binary_crds(tag), tag)
    if release:
        failures.extend(skew)

    if failures:
        print(f"\nFAIL: chart {CHART_YAML.relative_to(ROOT)} and the image it names are different")
        print("      generations of the same contract.\n")
        for f in failures:
            print(f"  - {f}")
        print(
            "\n  Fix: publish an operator image from a commit that honours the current chart"
            "\n  (tag operator-v<next>), then point appVersion at it."
        )
        return 1

    if skew:
        print(f"\nHOLD: chart {chart_version} cannot be published while appVersion names {app_version} ({commit}):\n")
        for f in skew:
            print(f"  - {f}")
        print(
            "\n  This does not fail the merge. The charts-oci release job runs this gate with"
            "\n  --release and holds the operator chart until appVersion names an operator release"
            "\n  whose CRDs match the chart's: tag operator-v<next> from a commit carrying them,"
            "\n  then point appVersion at it.\n"
        )

    print(
        f"Runtime contract OK{' for the merge' if skew else ''}: chart names operator {app_version} ({commit}); "
        f"it supplies all {len(reconciler_owned)} reconciler-owned parameter(s) and "
        f"declares all {len(supplied)} it sends"
        + ("; publication is held above." if skew else ", and its CRDs decode every field the chart's declare.")
    )
    return 0


def self_test() -> int:
    """Check C against synthetic CRDs: each skew it must report, and each change it must not."""

    def crd(*fields: str, version: str = "v1alpha1", description: str = "", pattern: str = "") -> str:
        props = {}
        for f in fields:
            node = props
            parts = f.split(".")
            for part in parts[:-1]:
                node = node.setdefault(part, {"type": "object", "properties": {}})["properties"]
            leaf = {"type": "string"}
            if description:
                leaf["description"] = description
            if pattern:
                leaf["pattern"] = pattern
            node.setdefault(parts[-1], leaf)
        schema = {"type": "object", "properties": props}
        return yaml.safe_dump({"spec": {"versions": [{"name": version, "schema": {"openAPIV3Schema": schema}}]}})

    def spec_doc(spec_properties: dict) -> str:
        schema = {"type": "object", "properties": {"spec": {"type": "object", "properties": spec_properties}}}
        return yaml.safe_dump({"spec": {"versions": [{"name": "v1alpha1", "schema": {"openAPIV3Schema": schema}}]}})

    def agents(*fields: str) -> str:
        return spec_doc({"agents": {"type": "array", "items": {
            "type": "object", "properties": {f: {"type": "string"} for f in fields}}}})

    def selectors(*fields: str) -> str:
        return spec_doc({"selectors": {"type": "object", "additionalProperties": {
            "type": "object", "properties": {f: {"type": "string"} for f in fields}}}})

    base = crd("spec.tenant", "status.phase")
    cases: list[tuple[str, dict[str, str], dict[str, str], str | None]] = [
        ("identical CRDs", {"p.yaml": base}, {"p.yaml": base}, None),
        ("description changed", {"p.yaml": crd("spec.tenant", "status.phase", description="reworded")}, {"p.yaml": base}, None),
        ("validation changed", {"p.yaml": crd("spec.tenant", "status.phase", pattern="^[a-z]+$")}, {"p.yaml": base}, None),
        ("chart declares a field the binary lacks", {"p.yaml": crd("spec.tenant", "status.phase", "status.evalRoleArn")},
         {"p.yaml": base}, "declares status.evalRoleArn"),
        ("binary declares a field the chart prunes", {"p.yaml": base},
         {"p.yaml": crd("spec.tenant", "status.phase", "status.evalRoleArn")}, "status.evalRoleArn, which the chart's CRD prunes"),
        ("binary reconciles a kind the chart does not install", {"p.yaml": base},
         {"p.yaml": base, "q.yaml": base}, "q.yaml: the operator at"),
        ("chart installs a kind the binary does not reconcile", {"p.yaml": base, "q.yaml": base},
         {"p.yaml": base}, "q.yaml: the chart installs this CRD"),
        ("chart serves a version the binary does not", {"p.yaml": crd("spec.tenant", "status.phase", version="v1beta1")},
         {"p.yaml": base}, "the chart's CRD serves v1beta1"),
        ("binary serves a version the chart does not", {"p.yaml": base},
         {"p.yaml": crd("spec.tenant", "status.phase", version="v1beta1")}, "operator-vX serves v1beta1"),
        ("a list item gains a field", {"p.yaml": agents("name", "route")}, {"p.yaml": agents("name")},
         "declares spec.agents[].route"),
        ("a map value gains a field", {"p.yaml": selectors("key", "operator")}, {"p.yaml": selectors("key")},
         "declares spec.selectors{}.operator"),
    ]
    def parse(docs: dict[str, str]) -> dict[str, dict[str, set[str]]]:
        return {n: crd_fields(t, f"self-test {n}") for n, t in docs.items()}

    failures: list[str] = []
    for label, chart, binary, want in cases:
        found = crd_skew(parse(chart), parse(binary), "operator-vX")
        if want is None:
            ok = not found
        else:
            ok = any(want in f for f in found)
        print(f"  {'ok  ' if ok else 'BAD '}{label} -> {'reported: ' + '; '.join(found) if found else 'clean'}")
        if not ok:
            failures.append(f"{label}: expected {'clean' if want is None else repr(want)}, got {found or 'clean'}")
    if failures:
        print(f"\nFAIL  self-test: {len(failures)} case(s) behaved wrongly")
        for f in failures:
            print(f"  - {f}")
        return 1
    greens = sum(1 for c in cases if c[3] is None)
    print(f"\nOK    self-test: all {len(cases)} cases behaved as specified ({greens} of them must stay clean, and did).")
    return 0



# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--list', action='store_true', help='print every contract term checked, not only the failures')
    ap.add_argument('--release', action='store_true', help='fail on CRD skew too, as the charts-oci publication gate does')
    ap.add_argument('--self-test', action='store_true', help="prove the CRD comparison reports skew and ignores what it should")
    return ap.parse_args()


if __name__ == "__main__":
    args = _parse_args()
    if args.self_test:
        sys.exit(self_test())
    sys.exit(main(listing=args.list, release=args.release))
