#!/usr/bin/env python3
"""A workflow that gates on pull-request metadata must subscribe to the event
that changes it.

The failure this exists for: `ci.yaml` ran `amannn/action-semantic-pull-request`
— an action whose entire job is to gate the PR *title* — under a bare
`on: pull_request`. GitHub defaults that to `[opened, synchronize, reopened]`,
which excludes `edited`, and `edited` is exactly what changing a title emits. So
the check went red on a bad title and then stayed red after the title was fixed,
because it never ran again. The only ways out were a manual re-run or an
unrelated push. Observed live on PR #170.

It is the same shape as everything else this repo gates against: a control that
cannot observe the thing it controls. Nothing failed. The check was present,
correctly configured for what it examined, and structurally unable to notice the
correction.

The rule this enforces: if a job reads `github.event.pull_request.<field>` — or
runs an action known to gate that field — the workflow's `pull_request` trigger
must include the activity types that can change it.

    title / body / base   ->  edited
    labels                ->  labeled, unlabeled
    milestone             ->  milestoned, demilestoned
    assignees             ->  assigned, unassigned
    draft                 ->  ready_for_review, converted_to_draft

Adding those types to a large workflow is the wrong fix — it re-runs every job
on every description tweak. Split the metadata gate into its own small workflow
and give that one the types.

For a job that reads a field without gating on it (logging a title into a
summary, say), put the marker on the job:

    my-job:
      # trigger-exempt: reads the title for the run summary; gates nothing
      name: ...

Usage:
    scripts/check-workflow-triggers.py            # fail on the first gap
    scripts/check-workflow-triggers.py --list     # print what it examined
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent
WORKFLOWS = REPO / ".github" / "workflows"

# GitHub's default activity types when a `pull_request` trigger names none.
# https://docs.github.com/actions/reference/events-that-trigger-workflows#pull_request
DEFAULT_TYPES = {"opened", "synchronize", "reopened"}

PR_TRIGGERS = ("pull_request", "pull_request_target")

# Which activity types can carry a change to each field. A gate that reads the
# field but subscribes to none of these can never see it change.
FIELD_EVENTS: dict[str, set[str]] = {
    "title": {"edited"},
    "body": {"edited"},
    "base": {"edited"},
    "labels": {"labeled", "unlabeled"},
    "milestone": {"milestoned", "demilestoned"},
    "assignees": {"assigned", "unassigned"},
    "draft": {"ready_for_review", "converted_to_draft"},
}

# Actions whose purpose is to gate a field, which therefore imply a read of it
# even though the field name appears nowhere in the workflow text.
GATING_ACTIONS: dict[str, str] = {
    "amannn/action-semantic-pull-request": "title",
}

FIELD_REF = re.compile(r"github\.event\.pull_request\.(\w+)")
EXEMPT = re.compile(r"#\s*trigger-exempt:\s*(?P<why>.+?)\s*$", re.M)


def load(path: Path) -> dict:
    doc = yaml.safe_load(path.read_text())
    if not isinstance(doc, dict):
        print(f"ERROR {path.name}: not a mapping; this check cannot read it", file=sys.stderr)
        sys.exit(2)
    return doc


def triggers(doc: dict, path: Path) -> dict:
    """The `on:` block, normalised.

    YAML 1.1 — which PyYAML implements — resolves a bare `on` key to the boolean
    True. Reading `doc["on"]` therefore returns nothing for every workflow file
    in existence, and a check built on it reports that all of them are clean.
    That is the exact class of defect this script exists to catch, so it is
    worth being explicit: look under both keys, and refuse to continue if
    neither is there.
    """
    for key in ("on", True):
        if key in doc:
            block = doc[key]
            break
    else:
        print(f"ERROR {path.name}: no `on:` block found; the parse is wrong", file=sys.stderr)
        sys.exit(2)

    # `on: pull_request` and `on: [pull_request, push]` are both legal and
    # neither can name activity types, so both mean "the defaults".
    if isinstance(block, str):
        return {block: None}
    if isinstance(block, list):
        return {name: None for name in block}
    if isinstance(block, dict):
        return block
    print(f"ERROR {path.name}: unreadable `on:` block of type {type(block).__name__}", file=sys.stderr)
    sys.exit(2)


def subscribed_types(on: dict) -> set[str]:
    """Every PR activity type this workflow actually receives."""
    types: set[str] = set()
    for name in PR_TRIGGERS:
        if name not in on:
            continue
        config = on[name]
        if isinstance(config, dict) and config.get("types"):
            types |= set(config["types"])
        else:
            types |= DEFAULT_TYPES
    return types


def fields_read(job: dict) -> set[str]:
    """Which PR fields this job depends on.

    The job is re-serialised rather than string-matched against the raw file, so
    a reference is attributed to the job that contains it — `if:`, `run:`,
    `with:` and `env:` are all covered by one pass, and a reference in a
    neighbouring job is not blamed on this one.
    """
    text = yaml.safe_dump(job)
    found = {m.group(1) for m in FIELD_REF.finditer(text)}

    for step in job.get("steps") or []:
        uses = (step or {}).get("uses", "")
        for action, field in GATING_ACTIONS.items():
            if uses.startswith(action):
                found.add(field)

    return {f for f in found if f in FIELD_EVENTS}


def job_exemption(source: str, job_id: str) -> str | None:
    """A `# trigger-exempt:` marker inside the job's block, if there is one."""
    start = re.search(rf"^  {re.escape(job_id)}:\s*$", source, re.M)
    if not start:
        return None
    rest = source[start.end():]
    # The job's block ends at the next top-level job key.
    end = re.search(r"^  \S.*:\s*$", rest, re.M)
    body = rest[: end.start()] if end else rest
    m = EXEMPT.search(body)
    return m.group("why") if m else None


def main() -> int:
    show = "--list" in sys.argv

    paths = sorted(list(WORKFLOWS.glob("*.yml")) + list(WORKFLOWS.glob("*.yaml")))
    if not paths:
        print(f"ERROR: no workflows under {WORKFLOWS}; the walk is not seeing the tree", file=sys.stderr)
        return 2

    gaps: list[tuple[str, str, str, set[str], set[str]]] = []
    examined = 0
    exempt = 0

    for path in paths:
        doc = load(path)
        on = triggers(doc, path)
        if not any(name in on for name in PR_TRIGGERS):
            continue

        have = subscribed_types(on)
        source = path.read_text()

        for job_id, job in (doc.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            for field in sorted(fields_read(job)):
                need = FIELD_EVENTS[field]
                why = job_exemption(source, job_id)
                if why:
                    exempt += 1
                    if show:
                        print(f"skip  {path.name}:{job_id} reads .{field} — exempt: {why}")
                    continue
                examined += 1
                if need <= have:
                    if show:
                        print(f"ok    {path.name}:{job_id} gates .{field}, subscribes to {sorted(need)}")
                    continue
                gaps.append((path.name, job_id, field, need - have, have))

    if not examined and not exempt:
        print(
            "no job in any workflow reads pull-request metadata — either that is true, "
            "or this check's detection is broken. Verify before trusting it.",
            file=sys.stderr,
        )

    if gaps:
        print()
        print("These jobs gate on pull-request metadata they cannot observe changing:")
        print()
        for name, job_id, field, missing, have in gaps:
            print(f"  {name}  job `{job_id}`  gates pull_request.{field}")
            print(f"    subscribes to: {sorted(have) or 'nothing'}")
            print(f"    missing:       {sorted(missing)}")
        print()
        print("A gate that cannot see the correction stays red after the correction. Do not")
        print("simply add the types to a large workflow — that re-runs every job on every")
        print("description edit. Split the metadata gate into its own workflow and give that")
        print("one the types it needs. If the job reads the field without gating on it, mark")
        print("it inside the job block:")
        print()
        print("      # trigger-exempt: reads the title for the run summary; gates nothing")
        print()
        return 1

    tail = f", {exempt} exempted and therefore unverified" if exempt else ""
    print(
        f"✓ {examined} pull-request metadata gate(s) subscribed to the events "
        f"that change what they read{tail}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
