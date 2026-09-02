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
in the code records.

Extras are not failures by themselves. A ClusterRole is consumed by more than the
reconcilers — leader election needs a grant no controller declares — so the rule
is that every unmarked grant is ACCOUNTED FOR: either a marker is added, or the
grant is recorded below, verb by verb, with what is known about who needs it. The
recorded verbs are compared against the render, so a verb added to a resource
that already has a record is itself a new grant and fails until the record covers
it. What that forbids is the silent extra.

WHAT GETS RENDERED

Which grants exist is read from the RENDERED chart rather than from the template
text. The template is Helm source; what a cluster receives is the render, and a
grant introduced by a conditional or a partial is invisible to a pattern over the
source.

One render is one cluster, though. The chart is rendered at its defaults and once
more for each boolean in values.yaml with that boolean flipped, and the set of
booleans is read out of values.yaml, so a toggle added to the chart enters the
matrix without being named here.

HOLDS: a grant that one boolean away from the defaults reaches is seen, in
whichever direction that boolean moves. The two directions read different sides
of the matrix. A marked permission has to be in the INTERSECTION, because the
operator relies on it and a role that carries it in some clusters and not others
is the drift this gate exists to catch. An unmarked grant has to be accounted for
if it appears in the UNION, because one cluster receiving it is enough.

DOES NOT HOLD: a grant that needs two booleans away from their defaults at once,
and a grant behind a value that is not a boolean — a name, a count, a list. Both
would be rendered by a matrix this does not build.

Compares at (apiGroup, resource, verb) granularity rather than per-rule, because
the two files legitimately group resources differently — only the effective
permission set has to match.

Exits non-zero and names every uncovered permission.
"""
import argparse
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
CHART_DIR = ROOT / "charts" / "operator"
CHART = CHART_DIR / "templates" / "rbac.yaml"
VALUES = CHART_DIR / "values.yaml"


# Grants the rendered manager ClusterRole carries that no kubebuilder marker
# generates, keyed by (apiGroup, resource) and holding the verbs each covers
# alongside what is known about them.
#
# This is a record, not an approval. Every entry below was searched for in the
# operator's own source and found unused; whether something OUTSIDE this repo
# depends on the role — the landing zone, or a gitops sync that reuses it — is
# not answerable from here, which is why they are recorded rather than removed.
# A verb set narrower than the code needs breaks the operator loudly; one wider
# breaks nothing until it is used by something nobody expected.
#
# The verbs are the record's content and are checked: a verb the render carries
# that the entry does not list is unaccounted for, and a verb the entry lists
# that no render carries is stale.
UNMARKED_GRANTS = {
    ("", "configmaps"): (
        "create delete get list patch update watch",
        "The operator makes no ConfigMap API call. The only ConfigMap symbol in the source is "
        "corev1.ResourceConfigMaps, a ResourceQuota quantity name; the remaining mentions are "
        "comments, one of them recording that the operator embeds a template rather than reading "
        "a chart-installed ConfigMap. Nothing in this repo breaks without it.",
    ),
    ("", "events"): (
        "delete get list update watch",
        "On top of the create;patch a marker does generate. The event recorder the manager builds "
        "emits events; it reads and removes none. Nothing in this repo breaks without the five.",
    ),
    ("apps", "statefulsets"): (
        "create delete get list patch update watch",
        "The vcluster control plane is a StatefulSet, and the vcluster Helm release creates it "
        "under its own identity — the operator names the workload only in a label selector and "
        "never touches the API type. Nothing in this repo breaks without it.",
    ),
    ("argoproj.io", "analysisruns"): (
        "create delete patch update",
        "On top of the get;list;watch a marker generates. The operator exposes a metric an "
        "AnalysisTemplate reads; it writes no analysis object.",
    ),
    ("argoproj.io", "analysistemplates"): (
        "create delete patch update",
        "On top of the get;list;watch a marker generates. Same reason: the templates are installed "
        "by the eval-runtime component, not by the operator.",
    ),
    ("agents.nanohype.dev", "agentfleets"): (
        "create delete",
        "The operator reconciles fleets and creates or deletes none.",
    ),
    ("agents.nanohype.dev", "agentsandboxes"): (
        "create",
        "Deletion IS marked — the TTL collector deletes a finished sandbox — but nothing in the "
        "operator creates one; sandboxes are pushed in by a dispatcher.",
    ),
    ("agents.nanohype.dev", "modelgateways"): (
        "create delete",
        "Reconciled, never created or deleted by the operator.",
    ),
    ("agents.nanohype.dev", "sandboxpools"): (
        "create delete",
        "Reconciled, never created or deleted by the operator.",
    ),
    ("governance.nanohype.dev", "budgetpolicies"): (
        "create delete",
        "Reconciled, never created or deleted by the operator.",
    ),
    ("governance.nanohype.dev", "evalsuites"): (
        "create delete",
        "Reconciled, never created or deleted by the operator.",
    ),
    ("governance.nanohype.dev", "slopolicies"): (
        "create delete",
        "Reconciled, never created or deleted by the operator.",
    ),
    ("platform.nanohype.dev", "platforms"): (
        "create delete",
        "Reconciled, never created or deleted by the operator.",
    ),
    ("platform.nanohype.dev", "tenants"): (
        "create delete",
        "Reconciled, never created or deleted by the operator.",
    ),
}


def recorded_verbs(group, resource):
    entry = UNMARKED_GRANTS.get((group, resource))
    return set(entry[0].split()) if entry else set()


def triples(rules):
    """Expand rules to the set of (group, resource, verb) they actually permit."""
    out = set()
    for rule in rules or []:
        for group in rule.get("apiGroups", []):
            for resource in rule.get("resources", []):
                for verb in rule.get("verbs", []):
                    out.add((group, resource, verb))
    return out


def boolean_settings():
    """Every boolean leaf in values.yaml, as a dotted path and its default."""
    found = []

    def walk(node, path):
        if isinstance(node, dict):
            for key, child in node.items():
                walk(child, path + [key])
        elif isinstance(node, bool):
            found.append((".".join(path), node))

    walk(yaml.safe_load(VALUES.read_text()), [])
    return sorted(found)


def render(overrides):
    """helm template the operator chart and return the manager ClusterRole's rules."""
    argv = ["helm", "template", "rbac-check", str(CHART_DIR)]
    for path, value in overrides:
        argv += ["--set", f"{path}={str(value).lower()}"]
    out = subprocess.run(argv, capture_output=True, text=True, timeout=180)
    if out.returncode != 0:
        sys.exit(f"helm template failed, so there is no rendered role to read:\n{out.stderr.strip()}")
    for doc in yaml.safe_load_all(out.stdout):
        if doc and doc.get("kind") == "ClusterRole" and "manager" in str(doc["metadata"]["name"]):
            return doc.get("rules", [])
    sys.exit(f"this render carries no manager ClusterRole: {' '.join(argv[4:]) or 'the chart defaults'}")


def render_matrix():
    """The defaults, and each boolean in values.yaml flipped away from them."""
    matrix = [("the chart's defaults", [])]
    for path, default in boolean_settings():
        matrix.append((f"{path}={str(not default).lower()}", [(path, not default)]))
    return matrix


def main():
    for p in (GENERATED, CHART, VALUES):
        if not p.exists():
            sys.exit(f"missing: {p}")

    generated = triples(yaml.safe_load(GENERATED.read_text()).get("rules"))
    rendered = {label: triples(render(overrides)) for label, overrides in render_matrix()}
    every_render = set.intersection(*rendered.values())
    any_render = set.union(*rendered.values())

    missing = sorted(generated - every_render)
    if missing:
        print(f"✗ the chart's manager ClusterRole is missing {len(missing)} permission(s)")
        print("  that controller-gen generates from the kubebuilder markers:\n")
        for group, resource, verb in missing:
            absent = [label for label, carried in rendered.items() if (group, resource, verb) not in carried]
            print(f"    {group or '(core)':32} {resource:28} {verb}")
            print(f"      absent under: {', '.join(absent)}")
        print("\n  The operator will fail at runtime on exactly these. Add them to")
        print(f"  {CHART.relative_to(ROOT)} — a kubebuilder marker is a promise the chart has to keep,")
        print("  in every cluster the chart can render, not in some of them.")
        return 1

    unmarked = sorted(any_render - generated)
    unaccounted = [t for t in unmarked if t[2] not in recorded_verbs(t[0], t[1])]
    if unaccounted:
        print(f"✗ the chart's manager ClusterRole carries {len(unaccounted)} grant(s) that no")
        print("  kubebuilder marker generates and nothing accounts for:\n")
        for group, resource, verb in unaccounted:
            known = recorded_verbs(group, resource)
            note = f"(recorded for {'/'.join(sorted(known))}, not {verb})" if known else ""
            print(f"    {group or '(core)':32} {resource:28} {verb:12} {note}")
        print("\n  A grant the code does not ask for is one nobody can remove safely later,")
        print("  because nothing records why it is there. Either declare it with a marker")
        print("  beside the code that needs it, or record it in UNMARKED_GRANTS with the")
        print("  verbs it permits and who is known to rely on it.")
        return 1

    carried = {}
    for group, resource, verb in unmarked:
        carried.setdefault((group, resource), set()).add(verb)
    stale = sorted(
        (group, resource, verb)
        for (group, resource), (verbs, _) in UNMARKED_GRANTS.items()
        for verb in set(verbs.split()) - carried.get((group, resource), set())
    )
    if stale:
        print(f"✗ {len(stale)} recorded unmarked verb(s) are in no render of the role:\n")
        for group, resource, verb in stale:
            print(f"    {group or '(core)':32} {resource:28} {verb}")
        print("\n  Delete the verb, and the entry with its last verb, rather than leaving a")
        print("  record of a grant nothing carries.")
        return 1

    print(f"✓ chart covers all {len(generated)} generated permissions in every one of "
          f"{len(rendered)} render(s), and all {len(unmarked)} unmarked grant(s) are accounted for")
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
