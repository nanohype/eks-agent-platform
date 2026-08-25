#!/usr/bin/env python3
"""Every gate must refuse a tree that contains only the gates.

A gate reports success for two reasons a reader cannot tell apart: it examined
the repository and found nothing wrong, or it examined nothing. The second is a
fail-open, and it does not announce itself — there need be no skip branch, no
"could not resolve", no warning. A gate can RUN, enumerate, match zero, and
report success on zero.

Reading the source does not settle it. Detectors written to find this class come
back narrower than the class: a skip branch is greppable, an empty enumeration is
not, and a gate whose walk silently rebases onto the wrong root looks identical
to one whose subject is genuinely clean.

WHAT THIS DOES

Copies the gate scripts into an empty git repository — no charts, no terraform,
no operator source, no workflows — and runs each one. Every gate must exit
non-zero. A gate that passes there is passing on nothing, and would pass on
nothing in this repository the moment its walk stopped matching.

WHY IT FOUND THINGS A FLOOR DID NOT

Two gates here passed that test while carrying an at-least-one floor. Both
floors counted what MATCHED rather than what the repository holds, and the
scripts copied in to run the gates were themselves enough to satisfy them: one
reported "2 shell scripts" that were its own, the other probed every gate while
reading zero workflows. A floor has to be set against the tree the gate is meant
to examine, and well under its real size, or it certifies its own presence.

    scripts/check-empty-tree.py [--list]
"""

from __future__ import annotations

import argparse
import pathlib
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import require_binary  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = ROOT / "scripts"
SELF = "check-empty-tree.py"

# A gate whose SUBJECT is the scripts directory has its complete corpus in a
# gates-only tree, so passing there is correct rather than vacuous. Exempted by
# name with the reason, and each carries a different obligation instead.
EXEMPT = {
    "check-controls.py": (
        "its corpus IS the gates, so a gates-only tree is a complete corpus rather than an empty "
        "one. Its own vacuity obligation is different and separately enforced: emptying the control "
        "registry must indict every gate, which it does because membership is derived from the "
        "gates on disk rather than from the registry's length."
    ),
}

# Per-gate budget. A gate that never finishes has not reported success, so a
# timeout counts as a refusal — but it is NAMED, because "refused" and "never
# answered" are different facts.
GATE_TIMEOUT = 120


def build_empty_tree(dest: pathlib.Path) -> None:
    """A repository holding the gates and nothing else."""
    (dest / "scripts").mkdir(parents=True)
    for p in sorted(SCRIPTS.iterdir()):
        if p.is_file():
            shutil.copy2(p, dest / "scripts" / p.name)
        elif p.is_dir():
            shutil.copytree(p, dest / "scripts" / p.name)
    subprocess.run(["git", "init", "-q", "."], cwd=dest, check=True, capture_output=True)
    subprocess.run(["git", "add", "scripts"], cwd=dest, check=True, capture_output=True)
    subprocess.run(
        ["git", "-c", "user.email=gate@local", "-c", "user.name=gate", "commit", "-qm", "gates only"],
        cwd=dest, check=True, capture_output=True,
    )


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print each gate and how it refused")
    args = ap.parse_args()

    require_binary("git", "build the empty repository every gate is run against")

    gates = sorted(
        p.name for p in SCRIPTS.glob("check-*.py") if p.name != SELF and p.name not in EXEMPT
    )
    for name, why in sorted(EXEMPT.items()):
        print(f"  exempt: {name} — {why}")
    if not gates:
        sys.exit(
            "check-empty-tree: found no gates to run. Enumerating nothing and finding every gate "
            "well-behaved produce the same silence, so this is an error."
        )

    passed_on_nothing: list[tuple[str, str]] = []
    with tempfile.TemporaryDirectory() as td:
        dest = pathlib.Path(td) / "tree"
        build_empty_tree(dest)
        for gate in gates:
            try:
                r = subprocess.run(
                    [sys.executable, f"scripts/{gate}"],
                    cwd=dest, capture_output=True, text=True, timeout=GATE_TIMEOUT,
                )
            except subprocess.TimeoutExpired:
                # Not a pass, and not a decision either. Letting this escape made
                # THIS gate crash rather than report — the shape it screens for.
                if args.list:
                    print(f"  refused (timeout)  {gate} — did not finish in {GATE_TIMEOUT}s")
                continue
            last = (r.stdout + r.stderr).strip().splitlines()
            summary = last[-1][:100] if last else "<no output>"
            if r.returncode == 0:
                passed_on_nothing.append((gate, summary))
            elif args.list:
                print(f"  refused ({r.returncode})  {gate}")

    if passed_on_nothing:
        print(
            f"\n{len(passed_on_nothing)} gate(s) reported SUCCESS against a tree holding no "
            "repository content:\n",
            file=sys.stderr,
        )
        for gate, summary in passed_on_nothing:
            print(f"  - {gate}\n      {summary}", file=sys.stderr)
        print(
            "\nEach of these would report the same success on this repository the moment its walk "
            "stopped matching. Give it a floor on what it EXAMINED, set under the count this tree "
            "carries — not an at-least-one check, which the gate's own scripts can satisfy.",
            file=sys.stderr,
        )
        return 1

    print(f"✓ {len(gates)} gates: every one refuses a tree that contains only the gates")
    return 0


if __name__ == "__main__":
    sys.exit(main())
