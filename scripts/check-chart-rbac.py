#!/usr/bin/env python3
"""Assert the operator chart's manager ClusterRole and the kubebuilder markers
describe the same permission set, in both directions.

WHY THIS EXISTS

operators/config/rbac/role.yaml IS generated (`make manifests`). The Helm chart's
manager ClusterRole is NOT — it is maintained by hand, while carrying a comment
claiming it was generated. The two drifted, and nothing caught it:

    platform_controller.go declared
        +kubebuilder:rbac:groups="",resources=users,verbs=impersonate
        +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;...

    the chart shipped neither.

The operator could therefore not create the impersonation RBAC its Platform
controller exists to create, its ClusterRole informer never synced, and every
Platform CR hung in phase=Provisioning — on a cluster where every pod was Running
and every ArgoCD Application was Healthy. The failure was invisible to kustomize,
kubeconform, helm lint and trivy, because the manifests were all perfectly valid.
The only thing wrong was that they granted less than the code needs.

A marker is a promise the chart has to keep. This checks that it does — and the
converse, which the first direction alone leaves open: a grant the chart carries
that no marker generates is a permission the operator holds for a reason nothing
in the code records. That direction found fifty-one of them.

Extras are not failures by themselves. A ClusterRole is consumed by more than the
reconcilers — leader election needs a grant no controller declares — so the rule
is that every unmarked grant is ACCOUNTED FOR: either a marker is added, or the
grant is recorded below with what it permits and what is known about who needs
it. What that forbids is the silent extra, which is how fifty-one accumulated.

Which grants exist is read from the RENDERED chart rather than from the template
text. The template is Helm source; what a cluster receives is the render, and a
grant introduced by a conditional or a partial is invisible to a pattern over the
source.

Compares at (apiGroup, resource, verb) granularity rather than per-rule, because
the two files legitimately group resources differently — only the effective
permission set has to match.

Exits non-zero and names every uncovered permission.
"""
import argparse
import re
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _tooling import require_binary  # noqa: E402

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
GENERATED = ROOT / "operators" / "config" / "rbac" / "role.yaml"
CHART = ROOT / "charts" / "operator" / "templates" / "rbac.yaml"


# Grants the rendered manager ClusterRole carries that no kubebuilder marker
# generates, keyed by (apiGroup, resource) with what is known about them.
#
# This is a record, not an approval. Every entry below was searched for in the
# operator's own source and found unused; whether something OUTSIDE this repo
# depends on the role — the landing zone, or a gitops sync that reuses it — is
# not answerable from here, which is why they are recorded rather than removed.
# A verb set narrower than the code needs breaks the operator loudly; one wider
# breaks nothing until it is used by something nobody expected.
UNMARKED_GRANTS = {
    ("", "configmaps"): (
        "create/delete/get/list/patch/update/watch. The operator reads and writes no ConfigMap "
        "through the API: the one mention in the source is corev1.ResourceConfigMaps, a "
        "ResourceQuota quantity name rather than a call. Nothing in this repo breaks without it."
    ),
    ("", "events"): (
        "delete/get/list/update/watch, on top of the create;patch a marker does generate. The "
        "event recorder the manager builds emits events; it reads and removes none. Nothing in "
        "this repo breaks without the five."
    ),
    ("apps", "statefulsets"): (
        "create/delete/get/list/patch/update/watch. The vcluster control plane is a StatefulSet, "
        "and the vcluster Helm release creates it under its own identity — the operator names the "
        "workload only in a label selector and never touches the API type. Nothing in this repo "
        "breaks without it."
    ),
    ("argoproj.io", "analysisruns"): (
        "create/delete/patch/update, on top of the get;list;watch a marker generates. The operator "
        "exposes a metric an AnalysisTemplate reads; it writes no analysis object."
    ),
    ("argoproj.io", "analysistemplates"): (
        "create/delete/patch/update, on top of the get;list;watch a marker generates. Same reason: "
        "the templates are installed by the eval-runtime component, not by the operator."
    ),
    ("agents.nanohype.dev", "agentfleets"): ("create/delete. The operator reconciles fleets and creates or deletes none."),
    ("agents.nanohype.dev", "agentsandboxes"): (
        "create. Deletion IS marked — the TTL collector deletes a finished sandbox — but nothing "
        "in the operator creates one; sandboxes are pushed in by a dispatcher."
    ),
    ("agents.nanohype.dev", "modelgateways"): ("create/delete. Reconciled, never created or deleted by the operator."),
    ("agents.nanohype.dev", "sandboxpools"): ("create/delete. Reconciled, never created or deleted by the operator."),
    ("governance.nanohype.dev", "budgetpolicies"): ("create/delete. Reconciled, never created or deleted by the operator."),
    ("governance.nanohype.dev", "evalsuites"): ("create/delete. Reconciled, never created or deleted by the operator."),
    ("governance.nanohype.dev", "slopolicies"): ("create/delete. Reconciled, never created or deleted by the operator."),
    ("platform.nanohype.dev", "platforms"): ("create/delete. Reconciled, never created or deleted by the operator."),
    ("platform.nanohype.dev", "tenants"): ("create/delete. Reconciled, never created or deleted by the operator."),
}


def triples(rules):
    """Expand rules to the set of (group, resource, verb) they actually permit."""
    out = set()
    for rule in rules or []:
        for group in rule.get("apiGroups", []):
            for resource in rule.get("resources", []):
                for verb in rule.get("verbs", []):
                    out.add((group, resource, verb))
    return out


def manager_rules(chart_path):
    # Strip Helm templating so the manifest parses as plain YAML. We only need the
    # rules, which contain no templating.
    raw = re.sub(r"\{\{-?.*?-?\}\}", "x", chart_path.read_text(), flags=re.S)
    for doc in yaml.safe_load_all(raw):
        if doc and doc.get("kind") == "ClusterRole" and "manager" in str(doc["metadata"]["name"]):
            return doc.get("rules", [])
    sys.exit(f"no manager ClusterRole found in {chart_path}")


def rendered_manager_rules():
    """helm template the operator chart and return the manager ClusterRole's rules."""
    out = subprocess.run(
        ["helm", "template", "rbac-check", str(ROOT / "charts" / "operator")],
        capture_output=True, text=True, timeout=180,
    )
    if out.returncode != 0:
        sys.exit(f"helm template failed, so there is no rendered role to read:\n{out.stderr.strip()}")
    for doc in yaml.safe_load_all(out.stdout):
        if doc and doc.get("kind") == "ClusterRole" and "manager" in str(doc["metadata"]["name"]):
            return doc.get("rules", [])
    sys.exit("the rendered chart contains no manager ClusterRole")


def main():
    for p in (GENERATED, CHART):
        if not p.exists():
            sys.exit(f"missing: {p}")

    generated = triples(yaml.safe_load(GENERATED.read_text()).get("rules"))
    chart = triples(rendered_manager_rules())

    missing = sorted(generated - chart)
    if missing:
        print(f"✗ the chart's manager ClusterRole is missing {len(missing)} permission(s)")
        print("  that controller-gen generates from the kubebuilder markers:\n")
        for group, resource, verb in missing:
            print(f"    {group or '(core)':32} {resource:28} {verb}")
        print("\n  The operator will fail at runtime on exactly these. Add them to")
        print(f"  {CHART.relative_to(ROOT)} — a kubebuilder marker is a promise the chart has to keep.")
        return 1

    unmarked = sorted(chart - generated)
    unaccounted = [t for t in unmarked if (t[0], t[1]) not in UNMARKED_GRANTS]
    if unaccounted:
        print(f"✗ the chart's manager ClusterRole carries {len(unaccounted)} grant(s) that no")
        print("  kubebuilder marker generates and nothing accounts for:\n")
        for group, resource, verb in unaccounted:
            print(f"    {group or '(core)':32} {resource:28} {verb}")
        print("\n  A grant the code does not ask for is one nobody can remove safely later,")
        print("  because nothing records why it is there. Either declare it with a marker")
        print("  beside the code that needs it, or record it in UNMARKED_GRANTS with what")
        print("  it permits and who is known to rely on it.")
        return 1

    stale = sorted(k for k in UNMARKED_GRANTS if k not in {(g, r) for g, r, _ in unmarked})
    if stale:
        print(f"✗ {len(stale)} recorded unmarked grant(s) are no longer in the rendered role:\n")
        for group, resource in stale:
            print(f"    {group or '(core)':32} {resource}")
        print("\n  Delete the entry rather than leaving a record of a grant nothing carries.")
        return 1

    print(f"✓ chart covers all {len(generated)} generated permissions, and all "
          f"{len(unmarked)} unmarked grant(s) are accounted for")
    return 0



# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__)
    # This gate takes no arguments; argparse rejects anything passed.
    return ap.parse_args()


if __name__ == "__main__":
    _parse_args()
    require_binary("helm", "render the chart whose ClusterRole this reads")
    sys.exit(main())
