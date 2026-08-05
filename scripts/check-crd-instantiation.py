#!/usr/bin/env python3
"""Every CRD kind this operator ships must be instantiable, and every CR it
renders must resolve its platformRef.

WHY THIS EXISTS

SLOPolicy shipped with a Go type, a generated CRD, a registered controller,
conformance tests, a kube-state-metrics projection and three Grafana alerts —
and no CR anywhere in the org. Not in charts/tenant, not in the eks-gitops
reference stack, not in a tenant repo, not in a test fixture that reaches a
cluster. Every existing gate passed, because every existing gate asks whether
the artifact is well-formed. None of them asked whether anything ever creates
one.

That is this org's recurring failure: a healthy control plane over a dead data
path. check-chart-crd-parity.py already holds charts/tenant faithful to the
Platform CRD in both directions — but it is scoped to Platform, so a kind the
chart cannot emit at all is invisible to it. A kind nothing instantiates is a
feature that has never run.

WHAT IT CHECKS

1. INSTANTIATION. Every spec.names.kind in charts/operator/crds/ appears as a
   `kind:` in the union of the charts/tenant/ci/ renders, or carries an argued
   entry in INSTANTIATION_EXCEPTIONS. An exception naming a kind that no longer
   ships is itself a failure — a stale exception is how a gate quietly stops
   covering the thing it was written for.

2. RESOLUTION. Every rendered CR carrying spec.platformRef.name must find a
   Platform in the SAME render with that metadata.name AND the same
   metadata.namespace. This is the part a `grep 'kind: SLOPolicy'` cannot do,
   and it is the failure that actually bites: platformRef resolves
   same-namespace by name (slo_reconcile.go resolveSLOPlatform ->
   getReferencedPlatform), so a CR rendered into the workload namespace instead
   of the control-plane namespace reports PlatformNotFound and nothing else.
   The CR exists, the controller runs, and the objective is never evaluated.

3. VACUITY. Fail if no CRD was parsed, if a render produced no CR, or if the
   exceptions cover every shipped kind. A gate whose healthy output matches its
   did-nothing output is the failure it exists to catch.

Usage: ./scripts/check-crd-instantiation.py
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
CRD_DIR = ROOT / "charts" / "operator" / "crds"
CHART = ROOT / "charts" / "tenant"
CI_VALUES = sorted((CHART / "ci").glob("*.yaml"))

# Kinds no tenant chart can declare, each with the reason it cannot. Keep this
# short and keep every entry argued — an unargued entry turns this gate into a
# list of things it has agreed not to look at.
INSTANTIATION_EXCEPTIONS: dict[str, str] = {
    "AgentSandbox": (
        "push-dispatched at runtime, one object per agent role-session, created "
        "by the operator and garbage-collected after a TTL. There is no "
        "deploy-time declaration to render — a committed AgentSandbox would be "
        "a single session frozen into GitOps."
    ),
    "SandboxPool": (
        "NOT a clean exception, and recorded as a gap rather than a decision. "
        "SandboxPool is declarative and Platform-scoped, so it belongs in this "
        "chart on the same argument SLOPolicy did — but nothing in the org "
        "instantiates one, and its worker image "
        "(ghcr.io/nanohype/eks-agent-platform/sandbox-worker) has never been "
        "published, so a rendered SandboxPool would reference a tag that does "
        "not exist. Deleting this entry is the definition of done for that "
        "work; it is not meant to live here."
    ),
}


def crd_kinds() -> set[str]:
    """spec.names.kind for every CRD the operator chart ships."""
    kinds: set[str] = set()
    for path in sorted(CRD_DIR.glob("*.yaml")):
        doc = yaml.safe_load(path.read_text())
        if not doc or doc.get("kind") != "CustomResourceDefinition":
            continue
        kind = (doc.get("spec") or {}).get("names", {}).get("kind")
        if kind:
            kinds.add(kind)
    return kinds


def render(values: Path) -> list[dict]:
    """helm template the tenant chart against one ci/ values file."""
    proc = subprocess.run(
        ["helm", "template", "instantiation", str(CHART), "-f", str(values)],
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        print(f"FAIL  helm template failed for {values.name}")
        for line in proc.stderr.strip().splitlines()[:8]:
            print(f"      {line}")
        return []
    return [d for d in yaml.safe_load_all(proc.stdout) if isinstance(d, dict)]


def main() -> int:
    if not CRD_DIR.is_dir():
        print(f"FAIL  {CRD_DIR} does not exist")
        return 1
    if not CI_VALUES:
        print(f"FAIL  no values files under {(CHART / 'ci').relative_to(ROOT)} —")
        print("      there is nothing to render, so this check asserts nothing.")
        return 1

    shipped = crd_kinds()
    if not shipped:
        print(f"FAIL  no CRD in {CRD_DIR.relative_to(ROOT)} names a kind — the parse")
        print("      matched nothing, so this check is comparing an empty set.")
        return 1

    ok = True
    rendered_kinds: set[str] = set()
    docs_by_values: dict[str, list[dict]] = {}

    for values in CI_VALUES:
        docs = render(values)
        if not docs:
            print(f"FAIL  {values.name} rendered no manifests.")
            ok = False
            continue
        docs_by_values[values.name] = docs
        rendered_kinds |= {d["kind"] for d in docs if d.get("kind")}

    if not rendered_kinds:
        print("FAIL  the ci/ renders produced no manifests at all.")
        return 1

    # 1. INSTANTIATION
    missing = sorted(shipped - rendered_kinds - set(INSTANTIATION_EXCEPTIONS))
    for kind in missing:
        ok = False
        print(f"FAIL  {kind} is a shipped CRD kind that no ci/ render instantiates.")
        print("      A CRD with a controller and no CR anywhere is a control plane")
        print("      over a dead data path. Add a template, or add an argued entry")
        print("      to INSTANTIATION_EXCEPTIONS saying why it cannot be declared.")

    # A stale exception is how this gate quietly stops covering a kind.
    for kind in sorted(set(INSTANTIATION_EXCEPTIONS) - shipped):
        ok = False
        print(f"FAIL  INSTANTIATION_EXCEPTIONS names {kind}, which this operator no")
        print("      longer ships. Remove the entry — a stale exception hides the")
        print("      next kind that lands with the same name.")

    if set(INSTANTIATION_EXCEPTIONS) >= shipped:
        ok = False
        print("FAIL  INSTANTIATION_EXCEPTIONS covers every shipped kind, so this")
        print("      check would pass having asserted nothing.")

    # 2. RESOLUTION
    checked_refs = 0
    for name, docs in docs_by_values.items():
        platforms = {
            (d["metadata"]["name"], d["metadata"].get("namespace"))
            for d in docs
            if d.get("kind") == "Platform"
        }
        for doc in docs:
            ref = ((doc.get("spec") or {}).get("platformRef") or {}).get("name")
            if not ref:
                continue
            checked_refs += 1
            ns = doc["metadata"].get("namespace")
            if (ref, ns) in platforms:
                continue
            ok = False
            print(
                f"FAIL  {name}: {doc['kind']}/{doc['metadata']['name']} in namespace "
                f"{ns!r} references Platform {ref!r},"
            )
            print("      and no Platform with that name exists in that namespace in the")
            print("      same render. platformRef resolves same-namespace by name, so")
            print("      this reports PlatformNotFound on the cluster and nothing else.")
            if any(p == ref for p, _ in platforms):
                elsewhere = sorted({n for p, n in platforms if p == ref})
                print(f"      That Platform renders into {elsewhere} — this is a")
                print("      namespace mismatch, not a missing Platform.")

    if checked_refs == 0:
        ok = False
        print("FAIL  no rendered manifest carries a spec.platformRef.name, so the")
        print("      resolution check asserted nothing.")

    if ok:
        covered = sorted(shipped & rendered_kinds)
        print(
            f"CRD instantiation ok — {len(covered)} of {len(shipped)} shipped kinds "
            f"are instantiated across {len(docs_by_values)} render(s), and all "
            f"{checked_refs} platformRef(s) resolve in-namespace:"
        )
        print(f"  instantiated: {', '.join(covered)}")
        for kind, why in sorted(INSTANTIATION_EXCEPTIONS.items()):
            print(f"  excepted:     {kind} — {why.split('.')[0]}.")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
