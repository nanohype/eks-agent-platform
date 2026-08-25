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


# The floor's own blind spot, named rather than left to be discovered.
#
# Probing this file from this file re-invokes it once per gate, recursively, and
# never returns — so check-gates.py is the one gate its own floor does not hold.
# Nothing here proves this script parses argv strictly; that rests on reading it
# and on its --self-test, which is testimony about itself by exactly the standard
# this file exists to enforce elsewhere.
SELF = "check-gates.py"

# The gate scripts exist in any tree that holds this file, so their count proves
# nothing about the repository. The CI invocations do: run against a tree with no
# .github/, this check probed every gate and passed while reading zero workflows.
# Set under the real count, for the reason check-shell-portability states.
MIN_CI_INVOCATIONS = 15


def gates() -> list[pathlib.Path]:
    found = sorted(p for p in SCRIPTS.glob("check-*.py") if p.name != SELF)
    if not found:
        sys.exit(
            f"check-gates: no check-*.py under {SCRIPTS.relative_to(ROOT)}. "
            "Finding no gate and having no gates to find produce the same silence, "
            "so this is an error rather than a pass."
        )
    return found


# argparse exits 2 on an unrecognized argument. That number is the OBSERVATION;
# anything the gate prints about itself is testimony.
#
# The distinction is not pedantic — it is the whole floor. A four-line script
# that runs no check, prints "unrecognized argument" and exits 1 satisfied the
# previous form of this function, because that form read the gate's own stderr
# for words like "unrecognized". The subject was being asked to describe its own
# behaviour and believed. Verified by writing exactly that script and watching it
# pass as one of twenty-one healthy gates.
ARGPARSE_USAGE_EXIT = 2


def parses_argv_strictly(gate: pathlib.Path, declared: set[str]) -> tuple[bool, str]:
    """Does the gate distinguish an argument it declares from one it does not?

    Two halves, and they cannot be satisfied by the same evidence — that is the
    point of testing both. A gate that exits 2 on everything fails the accept
    half; one that exits 2 on nothing fails the reject half. Neither can be
    faked by a script that does no work, because a script that does no work
    cannot tell the two invocations apart.

    Only exit status is read. Nothing the gate writes is consulted.
    """
    def run(args: list[str]) -> int:
        try:
            return subprocess.run(
                [sys.executable, str(gate), *args],
                capture_output=True, text=True, timeout=300, cwd=ROOT,
            ).returncode
        except subprocess.TimeoutExpired:
            return -1

    # REJECT half: an argument nothing declares must be refused as a usage error.
    if run([NONSENSE]) != ARGPARSE_USAGE_EXIT:
        return False, (
            f"an unrecognized argument did not exit {ARGPARSE_USAGE_EXIT}. A gate that ignores argv "
            "cannot tell a renamed flag from a correct one, so a CI step naming a mode the script "
            "no longer has keeps passing"
        )

    # ACCEPT half: a flag it DOES declare must not be refused as a usage error.
    # Without this, a gate that refuses every argument would pass the half above
    # while being unusable — the reject test alone is one-sided.
    # One declared flag, not all of them. The shape being caught is a gate that
    # refuses EVERYTHING, which shows up on any flag — and several gates here
    # shell out to helm or a registry, so a run per flag makes the floor
    # expensive enough that it stops being run, which is its own failure mode.
    if declared:
        flag = sorted(declared)[0]
        rc = run([flag])
        if rc == ARGPARSE_USAGE_EXIT:
            return False, f"it declares {flag} but refuses it as a usage error, so the flag is unreachable"
        if rc == -1:
            return False, f"timed out running its own declared flag {flag}"
    return True, ""


def declared_flags(gate: pathlib.Path, boolean_only: bool = False) -> set[str]:
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
        # A flag taking a VALUE cannot be probed by passing it alone — argparse
        # rightly calls that a usage error, which says nothing about whether the
        # gate parses strictly. Only store_true flags are self-contained enough
        # to use as the accept-half fixture.
        if boolean_only:
            store_true = any(
                kw.arg == "action" and isinstance(kw.value, ast.Constant) and kw.value.value == "store_true"
                for kw in node.keywords
            )
            if not store_true:
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
    """Try to fool the probe. A probe nobody has attacked is one being trusted."""
    ok = True
    tmp = SCRIPTS / "_check-gates-selftest-tmp.py"

    def try_gate(body: str, declared: set[str]) -> bool:
        tmp.write_text(body, encoding="utf-8")
        try:
            passed, _ = parses_argv_strictly(tmp, declared)
            return passed
        finally:
            tmp.unlink(missing_ok=True)

    # A gate that does nothing but CLAIM to reject. This is the exact shape that
    # defeated the previous stdout-reading form of the probe.
    if try_gate(
        "#!/usr/bin/env python3\nimport sys\nprint('unrecognized argument', file=sys.stderr)\nsys.exit(1)\n",
        set(),
    ):
        print("self-test: a gate that only CLAIMS to reject was reported as strict", file=sys.stderr)
        ok = False

    # A gate that refuses everything, including flags it declares. Passes the
    # reject half and must fail the accept half.
    if try_gate("#!/usr/bin/env python3\nimport sys\nsys.exit(2)\n", {"--list"}):
        print("self-test: a gate that refuses its own declared flag was reported as strict", file=sys.stderr)
        ok = False

    # A gate that accepts everything.
    if try_gate("#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n", set()):
        print("self-test: a gate that ignores argv was reported as strict", file=sys.stderr)
        ok = False

    # A genuinely strict gate must pass, or the probe rejects everything and the
    # floor is vacuous in the other direction.
    if not try_gate(
        "#!/usr/bin/env python3\nimport argparse\nap = argparse.ArgumentParser()\n"
        "ap.add_argument('--list', action='store_true')\nap.parse_args()\n",
        {"--list"},
    ):
        print("self-test: a genuinely strict gate was rejected", file=sys.stderr)
        ok = False

    if not ok:
        return 1
    print("✓ check-gates self-test: the probe reads exit status only, and cannot be satisfied by a gate that merely claims to reject")
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
    if len(invocations) < MIN_CI_INVOCATIONS:
        print(
            f"check-gates: found {len(invocations)} CI invocation(s) of a gate, fewer than the "
            f"{MIN_CI_INVOCATIONS} this repository is known to wire. The gate scripts are present "
            "in any tree holding this file, so without the workflows this check probes argument "
            "handling and proves nothing about the repository.",
            file=sys.stderr,
        )
        return 1
    failures: list[str] = []

    for gate in found:
        passed, why = parses_argv_strictly(gate, declared_flags(gate, boolean_only=True))
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

    print(
        f"✓ {len(found)} gates probed: each refuses an unrecognized argument and accepts a flag it "
        f"declares, observed from exit status alone. Every flag {len(invocations)} CI invocations "
        f"pass is declared.\n"
        f"  NOT probed: {SELF} (probing it from itself recurses); flags taking a VALUE "
        f"(bare, they are a usage error whatever the gate does)."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
