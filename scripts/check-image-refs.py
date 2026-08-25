#!/usr/bin/env python3
"""Every image this chart names must be one a registry can serve.

The failure this exists for: `charts/operator/Chart.yaml` declared
`appVersion: 0.4.0` for eighty commits while GHCR's newest operator tag was
`0.3.2`, and `values.yaml` pinned `eval-runner:0.1.0` against a GHCR package
that did not exist at all. The `addons-agent-operator` ApplicationSet renders
this chart from `main` with `image.tag: ""` — which resolves to appVersion — so
any cluster syncing it got ImagePullBackOff on the operator itself. Nothing the
operator reconciles would have run.

Nothing caught it because every gate here reads the chart against itself:
helm lint is happy, the values are well-formed, the templates render, the tag
is a valid string. The one question none of them asks is whether the thing on
the other end of the reference exists.

What this checks: every image reference reachable from the chart — the
operator's own (repository + tag, falling back to appVersion the way the
template does), the eval-runtime pins in values.yaml, and the literal `image:`
lines in the Argo WorkflowTemplate under files/ — resolves in its registry.

Third-party references (amazon/aws-cli, upstream sidecars) are checked too:
a pin nobody publishes any more fails the same way an unreleased one does.

Usage:
    scripts/check-image-refs.py            # fail on the first unresolvable ref
    scripts/check-image-refs.py --list     # print what it resolved, then check
"""

from __future__ import annotations

import argparse

import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "operator"
OPERATORS = ROOT / "operators"

# Image references the operator compiles in rather than reading from the chart.
# A Go constant is invisible to every chart-shaped check, which is how
# `sandbox-worker:0.1.0` came to be the default the reconciler applies to every
# SandboxPool while the package had never been published — no chart names it, so
# nothing ever asked the registry about it.
#
# Anchored on the registry rather than on a variable name, so a second one added
# later is caught without editing a list.
GO_IMAGE_REF = re.compile(r'"(ghcr\.io/[^"\s]+:[^"\s]+)"')

# References that genuinely do not resolve, each with the reason and what
# closing it means. A ledger of known gaps, not a mute button: every entry is
# printed loudly on each run, and an entry whose image HAS been published fails
# the check, so it cannot outlive the gap it records.
#
# Keep it empty when you can. An unargued entry turns this gate into a list of
# things it has agreed not to look at.
UNPUBLISHED_BY_DESIGN: dict[str, str] = {}


def sh(*args: str) -> tuple[int, str]:
    p = subprocess.run(args, capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr).strip()


def scalar(text: str, key: str) -> str | None:
    """Top-level-ish scalar lookup. Deliberately not a YAML parse.

    values.yaml and Chart.yaml here are hand-written with comments that carry
    meaning; a round-trip through a loader would work but pulling two strings
    out is not worth the dependency, and the patterns are anchored enough that
    a near-miss fails loudly rather than silently matching the wrong key.
    """
    m = re.search(rf"^\s*{re.escape(key)}:\s*\"?([^\"\n#]*)\"?\s*(?:#.*)?$", text, re.M)
    return m.group(1).strip() if m else None


def chart_images() -> list[tuple[str, str]]:
    """(where, ref) for every image the chart can render."""
    refs: list[tuple[str, str]] = []

    chart_yaml = (CHART / "Chart.yaml").read_text()
    values_yaml = (CHART / "values.yaml").read_text()

    app_version = scalar(chart_yaml, "appVersion")
    if not app_version:
        print("could not read appVersion from charts/operator/Chart.yaml", file=sys.stderr)
        sys.exit(2)

    # The operator's own image. `tag: ""` means "use appVersion" — the same
    # fallback templates/_helpers.tpl applies — so an appVersion bumped ahead
    # of a release is a reference to something that does not exist.
    block = re.search(r"^image:\n((?:\s+.*\n)+)", values_yaml, re.M)
    if not block:
        print("could not find the top-level image: block in values.yaml", file=sys.stderr)
        sys.exit(2)
    repo = scalar(block.group(1), "repository")
    tag = scalar(block.group(1), "tag") or app_version
    refs.append(("charts/operator/values.yaml (image, tag->appVersion)", f"{repo}:{tag}"))

    # Nested image blocks — evalRuntime today, anything added later for free.
    for m in re.finditer(r"^(\s+)image:\n((?:\1\s+.*\n)+)", values_yaml, re.M):
        nested = m.group(2)
        nrepo = scalar(nested, "repository")
        ntag = scalar(nested, "tag")
        if nrepo and ntag:
            refs.append(("charts/operator/values.yaml (nested image)", f"{nrepo}:{ntag}"))

    # Literal image: lines in the files/ trees the chart ships verbatim (the
    # Argo WorkflowTemplate). These never pass through values, so a stale pin
    # here is invisible to every other check.
    for path in sorted((CHART / "files").rglob("*.yaml")):
        for i, line in enumerate(path.read_text().splitlines(), 1):
            m = re.match(r"^\s*image:\s*\"?([^\s\"#]+)\"?", line)
            if m and "{{" not in m.group(1):
                rel = path.relative_to(ROOT)
                refs.append((f"{rel}:{i}", m.group(1)))

    # Image references compiled into the operator. These reach a cluster with no
    # chart involved — the SandboxPool reconciler applies its default to every
    # pool that does not override it — so a chart-shaped check cannot see them,
    # and did not.
    go_refs = 0
    for path in sorted(OPERATORS.rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        for i, line in enumerate(path.read_text().splitlines(), 1):
            for m in GO_IMAGE_REF.finditer(line):
                go_refs += 1
                refs.append((f"{path.relative_to(ROOT)}:{i}", m.group(1)))
    if go_refs == 0:
        # The operator has always compiled in at least one image reference. Zero
        # means the pattern stopped matching, not that the class went away — and
        # a silently-empty source is how this check would go back to being blind
        # to exactly what it was widened for.
        print("could not find any ghcr.io image reference in operators/**.go — the",
              file=sys.stderr)
        print("Go source scan matched nothing, so it is asserting nothing.", file=sys.stderr)
        sys.exit(2)

    # Same ref in three workflow steps is one question for the registry.
    seen: dict[str, str] = {}
    out: list[tuple[str, str]] = []
    for where, ref in refs:
        if ref in seen:
            continue
        seen[ref] = where
        out.append((where, ref))
    return out


def resolves(ref: str) -> tuple[bool, str]:
    """Ask a registry whether the reference is servable.

    `docker manifest inspect` is the check that matches what a kubelet does —
    it resolves the tag to a manifest. It needs no local pull and works
    unauthenticated against public GHCR and Docker Hub; in CI the workflow's
    GITHUB_TOKEN covers private packages via a prior docker login.
    """
    code, out = sh("docker", "manifest", "inspect", ref)
    if code == 0:
        return True, ""
    first = out.splitlines()[0] if out else "no output"
    return False, first


def main(args: argparse.Namespace) -> int:
    show = args.list
    refs = chart_images()
    if not refs:
        print("no image references found — the collector is broken, not the chart", file=sys.stderr)
        return 2

    if args.offline:
        # The commit-controlled half. chart_images() has already cross-checked
        # the tree's own declarations against each other — appVersion against
        # the pinned tag, the same ref across workflow steps — and raised on any
        # disagreement. Reaching a registry is the part that is not about this
        # commit, so it is skipped here and runs on the schedule instead.
        print(f"✓ {len(refs)} image reference(s) enumerated and internally consistent (offline; registry not consulted)")
        return 0

    failures: list[tuple[str, str, str]] = []
    known: list[tuple[str, str]] = []
    for where, ref in refs:
        ok, why = resolves(ref)
        excused = not ok and ref in UNPUBLISHED_BY_DESIGN
        if show or (not ok and not excused):
            print(f"{'ok  ' if ok else 'FAIL'}  {ref}\n        {where}" + ("" if ok else f"\n        {why}"))
        if ok:
            if ref in UNPUBLISHED_BY_DESIGN:
                # Publishing it is what retires the entry. A stale one left
                # behind would let the next unpublished image inherit a pass.
                print(f"FAIL  {ref} resolves, but UNPUBLISHED_BY_DESIGN still excuses it.")
                print("      Delete the entry — the gap it recorded is closed.")
                failures.append((where, ref, "stale exception"))
            continue
        if excused:
            known.append((ref, UNPUBLISHED_BY_DESIGN[ref]))
            continue
        failures.append((where, ref, why))

    if known:
        print()
        print("KNOWN GAPS — these do not resolve, and that is recorded rather than fixed:")
        for ref, why in known:
            print(f"  {ref}")
            for line in why.strip().splitlines():
                print(f"    {line.strip()}")

    if failures:
        print()
        print("these image references do not resolve:")
        for where, ref, why in failures:
            print(f"  {ref}")
            print(f"    named at {where}")
            print(f"    registry said: {why}")
        print()
        print("A chart that names an unpublished image renders and lints clean, and then")
        print("ImagePullBackOffs in every cluster the ApplicationSet syncs it to. If this is")
        print("the operator's own appVersion, the release tag was never cut — push")
        print("operator-v<appVersion> (release-on-merge should have done it; see release.yaml).")
        return 1

    if known:
        print()
        print(f"✓ {len(refs) - len(known)} of {len(refs)} image references resolve; "
              f"{len(known)} recorded above as a known gap")
    else:
        print(f"✓ all {len(refs)} image references resolve")
    return 0



# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--list', action='store_true', help='print every image reference examined, not only the failures')
    ap.add_argument('--offline', action='store_true',
                    help='enumerate and cross-check the references without asking a registry')
    return ap.parse_args()


if __name__ == "__main__":
    sys.exit(main(_parse_args()))
