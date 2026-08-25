#!/usr/bin/env python3
"""Every job that can run on a pull request must be reachable from its merge gate.

Branch protection watches ONE required check per workflow — the merge gate — and
that gate passes when everything in its `needs:` list passed. A job absent from
that list still runs, still goes red, and still does not block the merge. The
red X sits in the checks tab next to a green required check, and the merge
button is enabled.

That is worse than not having the job. A check nobody added to the list is a
check that reports a real failure into a UI where the failure has no effect, and
its presence is why nobody looks for the gap: the job exists, it ran, it found
the problem. It just was not asked.

WHY THE PULL-REQUEST QUALIFIER

A job gated behind `if: github.event_name == 'push'` cannot run on a pull
request, so it cannot be red-but-ignored there; it belongs to the post-merge
tier by construction. Requiring it in the merge gate would make the gate depend
on a job that is always skipped, which turns the whole gate green for a reason
unrelated to the tree. Those jobs are reported, not failed — an exemption that
prints is an exemption a reader can audit.

    scripts/check-merge-gate.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"

# workflow file -> the job branch protection requires.
GATES = {"ci.yaml": "merge-gate", "security.yaml": "merge-gate-security"}


def runs_on_pull_request(wf: dict, job: dict) -> bool:
    """Can this job execute in a pull_request run?"""
    on = wf.get("on") or wf.get(True)  # YAML 1.1 parses a bare `on:` as True
    triggers = set(on) if isinstance(on, dict) else {on} if isinstance(on, str) else set(on or ())
    if "pull_request" not in triggers and "pull_request_target" not in triggers:
        return False
    cond = str(job.get("if", ""))
    # A push-only condition cannot be true in a pull_request run. Matching the
    # literal rather than evaluating the expression: this gate reports what it
    # cannot decide instead of guessing at GitHub's expression semantics.
    return "github.event_name == 'push'" not in cond


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print each workflow's reachable/total counts")
    args = ap.parse_args()

    findings: list[str] = []
    exempt: list[str] = []
    checked = 0

    for fname, gate in GATES.items():
        path = WORKFLOWS / fname
        if not path.is_file():
            sys.exit(f"check-merge-gate: {path.relative_to(ROOT)} is missing; this gate names it by hand")
        wf = yaml.safe_load(path.read_text(encoding="utf-8"))
        jobs = wf.get("jobs") or {}
        if gate not in jobs:
            sys.exit(f"check-merge-gate: {fname} has no job named {gate!r} — the required check is gone")
        needs = jobs[gate].get("needs") or []
        needs = [needs] if isinstance(needs, str) else list(needs)
        for name, job in jobs.items():
            if name == gate:
                continue
            checked += 1
            if not runs_on_pull_request(wf, job):
                exempt.append(f"{fname}:{name} (cannot run on a pull request)")
                continue
            if name not in needs:
                findings.append(f"{fname}:{name}")
        if args.list:
            print(f"  {fname:16} {len(needs)}/{len(jobs) - 1} jobs required by {gate}")

    if checked == 0:
        sys.exit(
            "check-merge-gate: enumerated no jobs at all. An empty enumeration and a fully wired "
            "set of workflows produce the same silence, so this is an error."
        )

    for e in exempt:
        print(f"  not required, by construction: {e}")

    if findings:
        print(f"\n{len(findings)} job(s) run on pull requests but do not block the merge:\n", file=sys.stderr)
        for f in findings:
            print(f"  - {f}", file=sys.stderr)
        print(
            "\nEach of these goes red in the checks tab beside a green required check, and the merge "
            "button stays enabled. Add the job to its merge gate's needs list.",
            file=sys.stderr,
        )
        return 1

    print(f"✓ {checked} jobs across {len(GATES)} workflows: every pull-request job blocks the merge")
    return 0


if __name__ == "__main__":
    sys.exit(main())
