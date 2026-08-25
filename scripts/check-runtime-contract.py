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

The appVersion is resolved to source through the release tag (`operator-v<X>`),
which is the only thing that builds an image — see `.github/workflows/release.yaml`.
A missing tag is a failure, not a skip: it means the chart names a version this
repo cannot account for.

WHAT IT DOES NOT COVER, DELIBERATELY

    Parameters marked REPLACE_BY_APPLICATIONSET are filled by eks-gitops, in
    another repo this gate cannot read. They are reported and not enforced.

    Conditional supply. `cases-inline` and `cases-manifest` are appended inside
    branches, and this counts them as supplied. Deciding whether a branch is
    reachable for a given EvalSuite is beyond static reach, and a gate that
    claimed to do it would be claiming more than it delivers.

Usage:
    scripts/check-runtime-contract.py           # fail on any mismatch
    scripts/check-runtime-contract.py --list    # print both sides, then check
"""

from __future__ import annotations

import argparse

import re
import subprocess
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import require_binary
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - environment guard
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
CHART_YAML = ROOT / "charts" / "operator" / "Chart.yaml"
WORKFLOW_TEMPLATE = ROOT / "charts" / "operator" / "files" / "eval-runtime" / "workflow-template.yaml"
RECONCILER = "operators/internal/controller/eval_reconcile.go"

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


def chart_app_version() -> str:
    chart = yaml.safe_load(CHART_YAML.read_text())
    app_version = str(chart.get("appVersion", "")).strip()
    if not app_version:
        sys.exit(f"FAIL: {CHART_YAML.relative_to(ROOT)} declares no appVersion.")
    return app_version


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


def main() -> int:
    require_binary("git", "read the committed tree this compares the runtime contract against")

    listing = "--list" in sys.argv

    app_version = chart_app_version()
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

    if failures:
        print(f"\nFAIL: chart {CHART_YAML.relative_to(ROOT)} and the image it names are different")
        print("      generations of the same contract.\n")
        for f in failures:
            print(f"  - {f}")
        print(
            f"\n  Fix: publish an operator image from a commit that honours the current template "
            f"\n  (tag operator-v<next>), then point appVersion at it."
        )
        return 1

    print(
        f"Runtime contract OK: chart names operator {app_version} ({commit}); "
        f"it supplies all {len(reconciler_owned)} reconciler-owned parameter(s) and "
        f"declares all {len(supplied)} it sends."
    )
    return 0



# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--list', action='store_true', help='print every contract term checked, not only the failures')
    return ap.parse_args()


if __name__ == "__main__":
    _parse_args()
    sys.exit(main())
