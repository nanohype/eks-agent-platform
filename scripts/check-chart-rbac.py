#!/usr/bin/env python3
"""Assert the operator chart's manager ClusterRole covers every permission that
controller-gen generates from the kubebuilder markers.

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

A marker is a promise the chart has to keep. This checks that it does.

Compares at (apiGroup, resource, verb) granularity rather than per-rule, because
the two files legitimately group resources differently — only the effective
permission set has to match.

Exits non-zero and names every uncovered permission.
"""
import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
GENERATED = ROOT / "operators" / "config" / "rbac" / "role.yaml"
CHART = ROOT / "charts" / "operator" / "templates" / "rbac.yaml"


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


def main():
    for p in (GENERATED, CHART):
        if not p.exists():
            sys.exit(f"missing: {p}")

    generated = triples(yaml.safe_load(GENERATED.read_text()).get("rules"))
    chart = triples(manager_rules(CHART))

    missing = sorted(generated - chart)
    if missing:
        print(f"✗ the chart's manager ClusterRole is missing {len(missing)} permission(s)")
        print("  that controller-gen generates from the kubebuilder markers:\n")
        for group, resource, verb in missing:
            print(f"    {group or '(core)':32} {resource:28} {verb}")
        print("\n  The operator will fail at runtime on exactly these. Add them to")
        print(f"  {CHART.relative_to(ROOT)} — a kubebuilder marker is a promise the chart has to keep.")
        return 1

    print(f"✓ chart covers all {len(generated)} generated permissions "
          f"({len(chart)} granted; extras are allowed, gaps are not)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
