#!/usr/bin/env python3
"""Every job in a merge-gated workflow must be required by that gate.

Branch protection watches ONE required check per workflow — the merge gate — and
that gate passes when everything in its `needs:` list passed. A job absent from
that list still runs, still goes red, and still does not block the merge. The
red X sits in the checks tab next to a green required check, and the merge
button is enabled.

That is worse than not having the job. A check nobody added to the list is a
check that reports a real failure into a UI where the failure has no effect, and
its presence is why nobody looks for the gap: the job exists, it ran, it found
the problem. It just was not asked.

NO EXEMPTIONS, BECAUSE THE ENFORCER HAS NONE

An earlier version of this gate excused any job that could not run on a pull
request, reasoning that such a job cannot be red-but-ignored there. The shared
merge-gate action offers no such excuse: it requires EVERY job in the workflow
to appear in the gate's needs list, and it counts a skipped dependency as a
failure. A local rule more permissive than the enforcer describes a state the
enforcer is not in, and the pull request is where the two meet.

So a job whose work belongs to the post-merge tier does not get an `if:` inside
a gated workflow — it gets its own workflow. That is a structural answer rather
than an exemption, and it is why .github/workflows/image-refs-registry.yaml
exists.

    scripts/check-merge-gate.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"

# workflow file -> (job id, the CONTEXT STRING branch protection requires).
#
# Protection matches a required check by the job's DISPLAY NAME, not its id, so
# renaming `name:` silently detaches the job from the rule that requires it: the
# workflow still runs, the gate still passes, and the required context never
# reports. Both halves are asserted here so the rename fails locally instead.
GATES = {
    "ci.yaml": ("merge-gate", "merge gate"),
    "security.yaml": ("merge-gate-security", "merge gate (security)"),
}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print each workflow's reachable/total counts")
    args = ap.parse_args()

    findings: list[str] = []
    conditional: list[str] = []
    checked = 0

    for fname, (gate, context) in GATES.items():
        path = WORKFLOWS / fname
        if not path.is_file():
            sys.exit(f"check-merge-gate: {path.relative_to(ROOT)} is missing; this gate names it by hand")
        wf = yaml.safe_load(path.read_text(encoding="utf-8"))
        jobs = wf.get("jobs") or {}
        if gate not in jobs:
            sys.exit(f"check-merge-gate: {fname} has no job named {gate!r} — the required check is gone")
        declared = jobs[gate].get("name")
        if declared != context:
            sys.exit(
                f"check-merge-gate: {fname} job {gate!r} declares name {declared!r}, but branch "
                f"protection requires the context {context!r}. A required check matches by display "
                "name; renaming the job detaches it from the rule without failing anything."
            )
        needs = jobs[gate].get("needs") or []
        needs = [needs] if isinstance(needs, str) else list(needs)
        for name, job in jobs.items():
            if name == gate:
                continue
            checked += 1
            # A conditional job still has to be in the list; the condition only
            # decides whether it runs, and a skipped dependency fails the gate.
            # Reported so the combination is visible, never excused.
            if job.get("if"):
                conditional.append(f"{fname}:{name}  if: {job['if']}")
            if name not in needs:
                findings.append(f"{fname}:{name}")
        if args.list:
            print(f"  {fname:16} {len(needs)}/{len(jobs) - 1} jobs required by {gate}")

    if checked == 0:
        sys.exit(
            "check-merge-gate: enumerated no jobs at all. An empty enumeration and a fully wired "
            "set of workflows produce the same silence, so this is an error."
        )

    for c in conditional:
        print(f"  required AND conditional — it will be skipped, and a skip fails the gate: {c}")

    if findings:
        print(f"\n{len(findings)} job(s) in a gated workflow that the gate does not require:\n", file=sys.stderr)
        for f in findings:
            print(f"  - {f}", file=sys.stderr)
        print(
            "\nEach of these goes red in the checks tab beside a green required check, and the merge "
            "button stays enabled. Add the job to its merge gate's needs list.",
            file=sys.stderr,
        )
        return 1

    print(f"✓ {checked} jobs across {len(GATES)} workflows: every one is required by its merge gate")
    return 0


if __name__ == "__main__":
    sys.exit(main())
