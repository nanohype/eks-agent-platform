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

import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "operator"


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


def main() -> int:
    show = "--list" in sys.argv
    refs = chart_images()
    if not refs:
        print("no image references found — the collector is broken, not the chart", file=sys.stderr)
        return 2

    failures: list[tuple[str, str, str]] = []
    for where, ref in refs:
        ok, why = resolves(ref)
        if show or not ok:
            print(f"{'ok  ' if ok else 'FAIL'}  {ref}\n        {where}" + ("" if ok else f"\n        {why}"))
        if not ok:
            failures.append((where, ref, why))

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

    print(f"✓ all {len(refs)} image references resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
