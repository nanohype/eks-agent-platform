#!/usr/bin/env python3
"""A gate over the gates: every check script must be able to reject.

A check that cannot fail is worse than no check, because it reports success and
the reported success is what everything downstream reads. This repo's evidence
that a dimension holds is largely "a gate covers it", so the gates themselves
are load-bearing and nothing was holding them to a standard.

Two failure modes, both silent, both found by inspection rather than by anything
failing:

  * A gate that ACCEPTS an unknown argument. CI invokes several gates with a
    flag. If the script parses argv by scanning for the flags it knows and
    ignores the rest, then a renamed flag, a typo, or a flag deleted from the
    script leaves the CI step passing an argument to nothing. The step still
    runs, still exits 0, and the reader of the workflow believes the mode named
    in the step is the mode that ran.

  * A flag CI passes that the gate does not DECLARE. The same defect from the
    other side, and the one worth checking separately: reading the workflow
    tells you what CI intends, reading the script tells you what it does, and
    nobody reads both.

WHAT THIS ASSERTS

  1. Every scripts/check-*.py rejects an unrecognized argument with a non-zero
     exit. This is what argparse does by default; the gates that hand-roll an
     argv scan are the ones that do not.

  2. Every flag any workflow passes to a gate is a flag that gate declares.
     Parsed out of the workflow files, so a step added later is covered without
     editing this list.

WHAT IT DOES NOT ASSERT, AND WHY THAT IS STATED HERE

Not every gate carries a --self-test. Two do, and a self-test is the stronger
property — it proves the gate rejects a case it is meant to reject, where this
only proves it rejects nonsense. The weaker invariant is enforced because it is
enforceable for every gate today; a --self-test asserting nothing in particular
would itself be a check that cannot fail, which is the defect this file exists
to prevent. Gates gaining real self-tests is the next increment, and this script
is where that assertion goes when they do.

    scripts/check-gates.py [--list] [--self-test]
"""

from __future__ import annotations

import argparse
import ast
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = ROOT / "scripts"
WORKFLOWS = ROOT / ".github" / "workflows"

# An argument no gate will ever declare. Long enough that a substring match
# against a real flag is not possible.
NONSENSE = "--check-gates-nonsense-argument"

# `./scripts/check-foo.py --flag --other` inside a workflow `run:` line.
INVOCATION = re.compile(r"(?:\./)?scripts/(check-[a-z0-9-]+\.py)((?:\s+--[a-z0-9-]+(?:=\S+)?)*)")
FLAG = re.compile(r"--[a-z0-9-]+")


def gates() -> list[pathlib.Path]:
    found = sorted(SCRIPTS.glob("check-*.py"))
    if not found:
        sys.exit(
            f"check-gates: no check-*.py under {SCRIPTS.relative_to(ROOT)}. "
            "Finding no gate and having no gates to find produce the same silence, "
            "so this is an error rather than a pass."
        )
    return found


def rejects_nonsense(gate: pathlib.Path) -> tuple[bool, str]:
    """Run the gate with an argument nothing declares and report whether it refused."""
    try:
        proc = subprocess.run(
            [sys.executable, str(gate), NONSENSE],
            capture_output=True,
            text=True,
            timeout=120,
            cwd=ROOT,
        )
    except subprocess.TimeoutExpired:
        return False, "timed out"
    if proc.returncode == 0:
        return False, "exited 0 — the argument was ignored"
    combined = (proc.stdout + proc.stderr).lower()
    # A non-zero exit is necessary but not sufficient: a gate can fail for its
    # own reasons (a missing dependency, a real finding) and look like it
    # rejected the argument. Require it to SAY so.
    if any(w in combined for w in ("unrecognized", "unknown", "invalid", "usage:")):
        return True, ""
    return False, f"exited {proc.returncode} without naming the argument — indistinguishable from failing for another reason"


def declared_flags(gate: pathlib.Path) -> set[str]:
    """Flags this gate actually declares, read from the AST.

    The question is whether a CALL happens, so the view has to be one where text
    that merely mentions the call cannot answer. Regex over raw source cannot
    give that: a docstring containing `add_argument("--ghost")` matches, and a
    gate documenting a mode it does not implement is precisely the case this
    check exists to catch. Blanking comments does not help either — a Python
    docstring is a string literal, not a comment.

    The AST resolves it exactly. A mention inside a docstring is a Str node, not
    a Call, so it cannot be mistaken for a declaration; and the flag name is
    itself a string literal, so a view with string bodies blanked would delete
    the thing being read.
    """
    try:
        tree = ast.parse(gate.read_text(encoding="utf-8"))
    except SyntaxError as e:
        sys.exit(f"check-gates: {gate.name} does not parse, so its flags cannot be read: {e}")
    out: set[str] = set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        fn = node.func
        if not (isinstance(fn, ast.Attribute) and fn.attr == "add_argument"):
            continue
        for arg in node.args:
            if isinstance(arg, ast.Constant) and isinstance(arg.value, str) and arg.value.startswith("--"):
                out.add(arg.value)
    return out


def workflow_invocations() -> list[tuple[str, str, str]]:
    """(workflow, gate, flag) for every flag a workflow passes to a gate."""
    out: list[tuple[str, str, str]] = []
    for wf in sorted(WORKFLOWS.glob("*.y*ml")):
        text = wf.read_text(encoding="utf-8")
        for gate_name, flags in INVOCATION.findall(text):
            for flag in FLAG.findall(flags):
                out.append((wf.name, gate_name, flag))
    return out


def self_test() -> int:
    """Prove the two probes can distinguish a passing gate from a failing one.

    Without this the probes are themselves unfalsified — a rejects_nonsense that
    always returned True would report every gate healthy, which is the exact
    shape this file exists to catch.
    """
    ok = True

    strict = SCRIPTS / "check-leaf-input-parity.py"  # argparse-based
    passed, why = rejects_nonsense(strict)
    if not passed:
        print(f"self-test: {strict.name} is argparse-based and should reject; got: {why}", file=sys.stderr)
        ok = False

    # A gate that ignores everything must be caught. Written to a temp path
    # rather than shipped, so the repo carries no permanently-failing gate.
    lax = SCRIPTS / "_check-gates-selftest-lax.py"
    lax.write_text("#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n", encoding="utf-8")
    try:
        passed, _ = rejects_nonsense(lax)
        if passed:
            print("self-test: a gate that exits 0 on any argument was reported as rejecting", file=sys.stderr)
            ok = False
    finally:
        lax.unlink(missing_ok=True)

    if not ok:
        return 1
    print("✓ check-gates self-test: the probe distinguishes a strict gate from a permissive one")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print every gate and the flags CI passes it")
    ap.add_argument("--self-test", action="store_true", help="prove this script's own probes can fail")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    found = gates()
    invocations = workflow_invocations()
    failures: list[str] = []

    for gate in found:
        passed, why = rejects_nonsense(gate)
        if not passed:
            failures.append(
                f"{gate.relative_to(ROOT)} accepts an unrecognized argument ({why}).\n"
                "    A gate that ignores argv cannot tell a renamed flag from a correct one, so a CI step\n"
                "    naming a mode the script no longer has keeps passing. Parse with argparse, which\n"
                "    refuses an unknown argument for you."
            )

    by_gate: dict[str, set[str]] = {}
    for _wf, gate_name, flag in invocations:
        by_gate.setdefault(gate_name, set()).add(flag)

    for wf, gate_name, flag in invocations:
        gate = SCRIPTS / gate_name
        if not gate.is_file():
            failures.append(f"{wf} invokes scripts/{gate_name}, which does not exist")
            continue
        if flag not in declared_flags(gate):
            failures.append(
                f"{wf} passes {flag} to scripts/{gate_name}, which declares no such flag.\n"
                "    The workflow names a mode the script does not have; the step runs and exits 0,\n"
                "    and the reader believes the named mode is what ran."
            )

    if args.list:
        for gate in found:
            flags = sorted(by_gate.get(gate.name, set()))
            print(f"  {gate.name}: CI passes {', '.join(flags) if flags else '(no flags)'}")

    if failures:
        print(f"\n{len(failures)} gate problem(s):\n", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        return 1

    print(f"✓ {len(found)} gates reject an unknown argument; every flag {len(invocations)} CI invocations pass is declared")
    return 0


if __name__ == "__main__":
    sys.exit(main())
