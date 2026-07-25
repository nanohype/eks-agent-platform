#!/usr/bin/env python3
"""Assert charts/tenant stays faithful to the Platform CRD, in both directions.

WHY THIS EXISTS

The Platform CRD is mirrored in four places. Three of them are gated: the Go
types generate config/crd + docs/crd-reference (diffed by the crd-docs job),
packages/core/src/schemas.ts is diffed against the generated OpenAPI schema by
check-schema-drift.mts, and the operator chart's RBAC is diffed against the
kubebuilder markers by check-chart-rbac.py.

charts/tenant was the ungated one, and it drifted. The CRD grew
spec.datastores, spec.identity.capabilities, spec.identity.directSecretReads and
spec.attribution; the chart could express none of them. Every hand-written
Platform CR in the org used the full vocabulary while the chart every vending
path renders — agentctl, portal, ArgoCD — silently could not, so a tenant vended
through the chart got a Platform missing its stateful substrate and its
attribution, with nothing failing anywhere.

A CRD field the chart cannot emit is a field no vending path can reach. This
asserts the chart can reach all of them, and that it emits nothing the CRD would
reject.

WHAT IT CHECKS

1. COVERAGE. The union of the renders under charts/tenant/ci/ must reach every
   property path in the CRD's Platform spec schema. One render cannot cover
   everything — allowedModels and allowedModelFamilies are mutually exclusive,
   and a datastore's config block must match its kind — so coverage is a union
   over several committed values files, each of which is also a readable
   worked example.

2. NO EXCESS. Nothing in a rendered spec may be absent from the CRD schema. A
   mistyped key in a template renders perfectly valid YAML and is dropped (or
   rejected) at admission; this names it at build time.

3. REJECTION. Every values file under charts/tenant/tests/reject/ must make
   `helm template` fail, with the message its `# expect:` header names. These
   are the CRD constraints the chart enforces early — CEL rules, required
   fields, MaxItems caps, and the AWS naming limits the CRD's own ceiling does
   not cover. Without this, a guard can be silently weakened to a no-op.

Paths are array-transparent: `datastores[].name` is compared as
`datastores.name`, since a chart emits a list whose element shape is what has to
match.

Usage: ./scripts/check-chart-crd-parity.py
"""
import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

ROOT = Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "tenant"
CRD = ROOT / "charts" / "operator" / "crds" / "platform.nanohype.dev_platforms.yaml"
CRD_VERSION = "v1alpha1"

# Fields the chart deliberately does not surface, each with the reason. Keep this
# empty unless a field genuinely cannot be set at vend time; the point of the
# gate is that the list stays short and every entry is argued.
COVERAGE_EXCEPTIONS: dict[str, str] = {}


def crd_spec_schema():
    doc = yaml.safe_load(CRD.read_text())
    for version in doc["spec"]["versions"]:
        if version["name"] == CRD_VERSION:
            return version["schema"]["openAPIV3Schema"]["properties"]["spec"]
    sys.exit(f"{CRD}: no {CRD_VERSION} version")


def schema_paths(node, prefix=""):
    """Every property path in an OpenAPI schema node, descending through arrays."""
    out = set()
    if not isinstance(node, dict):
        return out
    if "items" in node:
        return schema_paths(node["items"], prefix)
    for name, child in (node.get("properties") or {}).items():
        path = f"{prefix}.{name}" if prefix else name
        out.add(path)
        out |= schema_paths(child, path)
    return out


def value_paths(node, prefix=""):
    """Every path present in a rendered value, unioned across list elements."""
    out = set()
    if isinstance(node, dict):
        for name, child in node.items():
            path = f"{prefix}.{name}" if prefix else name
            out.add(path)
            out |= value_paths(child, path)
    elif isinstance(node, list):
        for item in node:
            out |= value_paths(item, prefix)
    return out


def render(values_files, extra_args=()):
    """helm template the tenant chart; returns (ok, stdout_or_stderr)."""
    cmd = ["helm", "template", "parity", str(CHART)]
    for f in values_files:
        cmd += ["-f", str(f)]
    cmd += list(extra_args)
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        return False, proc.stderr
    return True, proc.stdout


def platform_spec(rendered, source):
    specs = [
        doc["spec"]
        for doc in yaml.safe_load_all(rendered)
        if doc and doc.get("kind") == "Platform"
    ]
    if len(specs) != 1:
        sys.exit(f"{source}: expected exactly one Platform document, got {len(specs)}")
    return specs[0]


def check_coverage(problems):
    coverage_files = sorted((CHART / "ci").glob("*-values.yaml"))
    if not coverage_files:
        sys.exit(f"no coverage renders found under {CHART / 'ci'}")

    crd_paths = schema_paths(crd_spec_schema())
    emitted = set()
    for f in coverage_files:
        ok, out = render([f])
        if not ok:
            problems.append(f"coverage render {f.name} failed to template:\n{out}")
            continue
        emitted |= value_paths(platform_spec(out, f.name))

    for path in sorted(crd_paths - emitted):
        if path in COVERAGE_EXCEPTIONS:
            continue
        problems.append(
            f"spec.{path} is in the Platform CRD but no charts/tenant/ci render emits it"
            " — the chart cannot express it, so no vending path can set it"
        )
    for path in sorted(emitted - crd_paths):
        problems.append(
            f"spec.{path} is emitted by the chart but is not in the Platform CRD"
            " — the API server will reject or silently drop it"
        )
    for path in sorted(COVERAGE_EXCEPTIONS):
        if path not in crd_paths:
            problems.append(
                f"COVERAGE_EXCEPTIONS names spec.{path}, which is no longer in the CRD"
                " — drop the entry"
            )
    return len(coverage_files)


def check_rejections(problems):
    reject_dir = CHART / "tests" / "reject"
    fixtures = sorted(reject_dir.glob("*.yaml"))
    if not fixtures:
        sys.exit(f"no rejection fixtures found under {reject_dir}")

    for f in fixtures:
        expect = None
        for line in f.read_text().splitlines():
            if line.startswith("# expect:"):
                expect = line[len("# expect:") :].strip()
                break
        if not expect:
            problems.append(
                f"{f.name} has no '# expect: <substring>' header naming the message"
                " it must fail with"
            )
            continue
        ok, out = render([f])
        if ok:
            problems.append(
                f"{f.name} rendered successfully but must be rejected"
                f" (expected message containing {expect!r})"
            )
        elif expect not in out:
            problems.append(
                f"{f.name} was rejected, but not for the stated reason."
                f"\n  expected message to contain: {expect}\n  got: {out.strip()}"
            )
    return len(fixtures)


def main():
    problems: list[str] = []
    renders = check_coverage(problems)
    rejections = check_rejections(problems)

    if problems:
        print("charts/tenant has drifted from the Platform CRD:\n", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        print(
            "\nTo fix: add the field to charts/tenant/values.yaml +"
            " templates/platform.yaml, then set it in a charts/tenant/ci render so"
            " the gate sees it. If a field genuinely cannot be set at vend time,"
            " add it to COVERAGE_EXCEPTIONS in this script with the reason.",
            file=sys.stderr,
        )
        sys.exit(1)

    print(
        f"charts/tenant covers every Platform CRD spec field across {renders}"
        f" render(s), emits nothing outside the CRD, and rejects all"
        f" {rejections} invalid declaration(s)."
    )


if __name__ == "__main__":
    main()
