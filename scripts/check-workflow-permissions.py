#!/usr/bin/env python3
"""No workflow may grant a write permission at the top level.

A `permissions:` block at workflow level applies to every job in the file,
including every job added to it later. `id-token: write` declared there hands a
job written months afterwards the right to mint an OIDC token and assume a cloud
role, and the diff that acquires that right is the diff that adds the job — it
contains no permission change at all, so nothing in review looks like a
privilege grant.

Per-job permissions invert this: a job gets what its own block names, and
widening it is an edit to the line that names it.

WHY THIS IS NOT LEFT TO THE THIRD-PARTY SCANNER

zizmor covers this class as `excessive-permissions`, but only under
`--persona=auditor`; the persona is a separate axis from `--min-severity`, and
the default persona does not run the audit at all. Measured on a workflow
carrying `id-token: write` at the top level: the default persona exits 0 and
auditor exits 14, at identical severity. A gate can be green because the tree is
clean or because the check never ran, and those two are indistinguishable from
the outside.

So the CI job runs auditor AND this gate asserts the property directly. The
duplication is the point: this file states the rule in terms a reader can check
against the workflows, and it keeps holding if the scanner is unavailable,
changes its default persona again, or reclassifies the finding below the
threshold.

READ permissions are unrestricted here. `contents: read` at workflow level is
the recommended baseline — it narrows the default token rather than widening it.

    scripts/check-workflow-permissions.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"

# `write-all` and a bare `write` are the same grant by another spelling.
WRITE_VALUES = {"write", "write-all"}


def offending(perms: object) -> list[str]:
    """Which top-level permissions grant write? Returns scope names."""
    if isinstance(perms, str):
        # `permissions: write-all` — every scope, every job.
        return [f"<all scopes>: {perms}"] if perms in WRITE_VALUES else []
    if isinstance(perms, dict):
        return [f"{k}: {v}" for k, v in perms.items() if str(v) in WRITE_VALUES]
    return []


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print every workflow and its top-level permissions")
    args = ap.parse_args()

    if not WORKFLOWS.is_dir():
        sys.exit(f"check-workflow-permissions: {WORKFLOWS.relative_to(ROOT)} does not exist")

    files = sorted(p for p in WORKFLOWS.iterdir() if p.suffix in (".yaml", ".yml"))
    if not files:
        sys.exit(
            "check-workflow-permissions: no workflow files found. Reading nothing and finding "
            "nothing clean produce the same silence, so this is an error."
        )

    findings: list[tuple[str, str]] = []
    for p in files:
        try:
            doc = yaml.safe_load(p.read_text(encoding="utf-8"))
        except yaml.YAMLError as e:
            sys.exit(f"check-workflow-permissions: {p.name} is not parseable YAML: {e}")
        if not isinstance(doc, dict):
            sys.exit(f"check-workflow-permissions: {p.name} does not parse to a mapping")
        perms = doc.get("permissions")
        if args.list:
            print(f"  {p.name:24} workflow-level: {perms!r}")
        for scope in offending(perms):
            findings.append((p.name, scope))

    if findings:
        print(f"\n{len(findings)} workflow-level write permission(s):\n", file=sys.stderr)
        for name, scope in findings:
            print(f"  - .github/workflows/{name}  permissions.{scope}", file=sys.stderr)
        print(
            "\nA write permission at workflow level applies to every job in the file, including "
            "jobs added later — so the diff that acquires the right contains no permission change. "
            "Move it to the one job that needs it.",
            file=sys.stderr,
        )
        return 1

    print(f"✓ {len(files)} workflows grant no write permission at the top level")
    return 0


if __name__ == "__main__":
    sys.exit(main())
