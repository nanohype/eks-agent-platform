#!/usr/bin/env python3
"""Assert the pinned vcluster chart ACCEPTS the values the operator emits, and
renders the object set the isolation tier depends on.

WHY THIS EXISTS

buildVClusterValues returns a map[string]interface{}. Nothing type-checks it
against the chart: not the compiler, not `go vet`, not the unit tests. Those
tests assert what the operator EMITS — that sync.toHost.serviceAccounts.enabled
is true, that the control plane is PSS-restricted, that CoreDNS keeps
NET_BIND_SERVICE. Every one of them passes against a chart version that has
renamed the path out from under it.

So a chart bump that moved or removed a values key would ship green and break
the isolation tier at sync, on every vcluster-isolated tenant. That is not
hypothetical: 0.36.0 REMOVED sync.toHost.volumeSnapshots, and the chart fails
the render outright if a values file still sets it.

RENDERING IS NOT ENOUGH, EITHER

A bump can also SILENTLY DROP an object while exiting 0. Going 0.35.2 -> 0.36.1
drops the ClusterRole and ClusterRoleBinding: ten objects become eight, helm
exits 0, and no gate that only checks the exit code notices. That particular
drop is correct — the ClusterRole existed for sync.toHost.volumeSnapshots and
persistentVolumes, neither of which this configuration enables — but "correct"
was established by running a vcluster, not by reading the render. The next drop
might not be.

So the rendered object set is compared against a committed inventory. Any
addition or removal fails, and the reviewer decides which it is. The inventory
is the record of that decision.

WHAT IT DOES
    1. reads the pinned chart repo + version out of charts/operator/values.yaml
       — the same pin the operator hands ArgoCD, so the gate cannot drift from
       what ships;
    2. exports the operator's own values through TestExportVClusterValues, which
       calls buildVClusterValues rather than restating it;
    3. renders the chart against them, failing on a non-zero helm exit;
    4. diffs the rendered (apiVersion, kind, name) set against
       scripts/vcluster-render-inventory.txt.

Refresh the inventory with --write after reviewing the diff.
"""

from __future__ import annotations

import argparse
import os
import pathlib
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import require_binary
import tempfile

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
CHART_VALUES = REPO_ROOT / "charts" / "operator" / "values.yaml"
INVENTORY = REPO_ROOT / "scripts" / "vcluster-render-inventory.txt"
OPERATORS = REPO_ROOT / "operators"

# The release name and namespace the operator installs under. Both appear in
# rendered object names, so they are part of what the inventory pins.
RELEASE = "vcluster"
# TestExportVClusterValues builds its Platform from vclusterPlatform(), whose
# namespace resolves to tenants-demo. Rendering into any other namespace would
# produce names the exported values (extraSANs, exportKubeConfig.server) do not
# agree with.
NAMESPACE = "tenants-demo"


def pinned_chart() -> tuple[str, str]:
    """(repoURL, version) as the operator chart declares them."""
    values = yaml.safe_load(CHART_VALUES.read_text())
    chart = (values.get("vcluster") or {}).get("chart") or {}
    repo, version = chart.get("repoURL"), chart.get("version")
    if not repo or not version:
        sys.exit(f"FAIL  {CHART_VALUES.relative_to(REPO_ROOT)} declares no "
                 f"vcluster.chart.repoURL/version")
    return repo, str(version)


def export_values(dest: pathlib.Path) -> None:
    """Run the export seam. Its output IS buildVClusterValues' output."""
    proc = subprocess.run(
        ["go", "test", "./internal/controller/", "-run",
         "TestExportVClusterValues", "-count=1"],
        cwd=OPERATORS, capture_output=True, text=True,
        env={**os.environ, "VCLUSTER_VALUES_OUT": str(dest)},
    )
    if proc.returncode != 0:
        print(proc.stdout)
        sys.exit(f"FAIL  could not export vcluster values:\n{proc.stderr}")
    if not dest.exists() or not dest.stat().st_size:
        sys.exit("FAIL  TestExportVClusterValues wrote nothing — the export seam "
                 "is gone or was skipped")


def render(repo: str, version: str, values: pathlib.Path) -> list[str]:
    """helm template the pinned chart; exit non-zero if it rejects the values."""
    proc = subprocess.run(
        ["helm", "template", RELEASE, "vcluster", "--repo", repo,
         "--version", version, "-n", NAMESPACE, "-f", str(values)],
        capture_output=True, text=True,
    )
    if proc.returncode != 0:
        print(f"FAIL  vcluster chart {version} REJECTED the values the operator "
              f"emits.")
        print("      This is the isolation tier failing to install, caught before "
              "it ships.\n")
        print(proc.stderr.strip())
        sys.exit(1)

    objects = []
    for doc in yaml.safe_load_all(proc.stdout):
        if isinstance(doc, dict) and doc.get("kind"):
            objects.append(f"{doc['apiVersion']} {doc['kind']} "
                           f"{doc['metadata']['name']}")
    return sorted(objects)


def main() -> int:

    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--write", action="store_true",
                    help="rewrite the inventory from this render (review the "
                         "diff first)")
    args = ap.parse_args()

    require_binary("helm", "render the vcluster chart")
    require_binary("go", "resolve the operator constants the chart must agree with")

    repo, version = pinned_chart()
    print("── vcluster chart acceptance ──")
    print(f"  pinned {repo} vcluster@{version}")

    with tempfile.TemporaryDirectory() as tmp:
        values = pathlib.Path(tmp) / "values.yaml"
        export_values(values)
        rendered = render(repo, version, values)

    print(f"  ok    chart {version} accepts the operator's values "
          f"({len(rendered)} objects)")

    if args.write:
        # Preserve the review record; only the object list is regenerated.
        header = [ln for ln in INVENTORY.read_text().splitlines()
                  if ln.lstrip().startswith("#")] if INVENTORY.exists() else []
        INVENTORY.write_text("\n".join(header + rendered) + "\n")
        print(f"  wrote {INVENTORY.relative_to(REPO_ROOT)}")
        return 0

    if not INVENTORY.exists():
        sys.exit(f"FAIL  {INVENTORY.relative_to(REPO_ROOT)} is missing — run "
                 f"with --write to create it")

    # `#` lines carry the review record for the current object set — why an
    # object is present, and why an absent one is absent. Kept in this file so
    # the decision sits next to the thing it decided.
    expected = [ln.strip() for ln in INVENTORY.read_text().splitlines()
                if ln.strip() and not ln.lstrip().startswith("#")]
    dropped = [o for o in expected if o not in rendered]
    added = [o for o in rendered if o not in expected]
    if not dropped and not added:
        print(f"  ok    rendered object set matches the committed inventory")
        print("\nvcluster chart gate PASSED.")
        return 0

    print(f"  FAIL  the rendered object set changed:")
    for o in dropped:
        print(f"          DROPPED  {o}")
    for o in added:
        print(f"          ADDED    {o}")
    print("        A dropped object is the dangerous direction — helm exits 0 "
          "and the")
    print("        isolation tier loses a permission or a resource at sync. "
          "Decide whether")
    print("        the change is correct, then refresh with "
          "scripts/check-vcluster-chart.py --write.")
    print("\nvcluster chart gate FAILED.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
