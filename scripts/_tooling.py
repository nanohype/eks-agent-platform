"""Preconditions for gates that shell out to a binary.

A gate that runs `helm template` and lets the missing-binary case escape as a
FileNotFoundError traceback exits non-zero, and non-zero is how a gate says it
REJECTED something. So a machine without helm reports the same verdict as a
chart that genuinely violates the rule, while the message on stderr blames the
Python interpreter for a missing executable.

Both halves are wrong in the same direction as a fail-open, but louder: the tree
is called broken because the toolbox is empty.

THE EXIT CODE HAS TO BE ITS OWN

1 means the check ran and the tree failed it. 2 is argparse's usage code, and the
floor in check-gates.py reads exit status ALONE to decide whether a gate refuses
a nonsense argument — so a precondition sharing 2 would let a missing binary be
credited as a gate rejecting an argument it never parsed, on every machine that
lacks the tool. 3 is neither, which is what makes the three outcomes separable
from outside the process.

Call this AFTER parse_args, so `--help` and a usage error still work on a machine
without the tool. Argument handling does not depend on the toolbox.

WHY PRESENCE IS NOT ENOUGH

`shutil.which` proves a name resolves on PATH, not that it runs. A shim that
exits non-zero on every invocation, a wrapper pointing at an uninstalled
version manager, or a binary for the wrong architecture all resolve and all fail
at the first real call — where the failure is again indistinguishable from a
rejection. So the probe RUNS the tool with a harmless argument and requires it
to succeed.

THE PROBE IS PER-TOOL, AND GUESSING ONE IS THE SAME BUG

`--version` is not universal. `helm --version` is an unknown flag and exits 1;
`go --version` is an undefined flag and exits 2. A shared default would report
both working tools as broken preconditions — a wrong instrument producing a
confident verdict about the wrong thing, which is the failure this file exists
to prevent, one layer up. So PROBES names the invocation per binary and a tool
with no entry is an error rather than a guess.
"""

from __future__ import annotations

import shutil
import subprocess
import sys

# 0 pass, 1 a finding about the tree, 2 argparse's own usage error, 3 the gate
# could not evaluate. 3 covers a missing tool AND a parse or discovery that came
# back empty: both mean nothing was checked, which is a different statement from
# "checked and clean" and from "checked and failed". Sharing 2 with argparse
# would make a gate that bailed indistinguishable from one handed a bad flag,
# and a floor reading exit status alone cannot tell them apart.
EXIT_PRECONDITION = 3
EXIT_CANNOT_EVALUATE = EXIT_PRECONDITION

# The invocation each tool actually accepts, measured rather than assumed.
PROBES = {
    "git": ["git", "--version"],
    "helm": ["helm", "version", "--short"],
    "go": ["go", "version"],
}


def require_binary(name: str, why: str, probe: list[str] | None = None) -> None:
    """Assert a binary exists AND runs, or exit 2 naming it.

    The probe comes from PROBES. A binary with no entry there is a programming
    error in the caller, not a finding about the machine.
    """
    if shutil.which(name) is None:
        print(
            f"{sys.argv[0]}: PRECONDITION FAILED — {name!r} is not on PATH, and it is needed to "
            f"{why}. This is not a finding about the tree: nothing was checked. Install {name} and "
            "re-run.\n",
            file=sys.stderr,
        )
        sys.exit(EXIT_PRECONDITION)
    cmd = probe or PROBES.get(name)
    if cmd is None:
        raise KeyError(
            f"require_binary({name!r}): no probe known. Add one to PROBES after measuring what "
            f"{name!r} accepts — `--version` is not universal, and guessing reports a working "
            "tool as a broken precondition."
        )
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as e:
        print(
            f"{sys.argv[0]}: PRECONDITION FAILED — {name!r} resolves on PATH but could not be run "
            f"({e}). Nothing was checked.\n",
            file=sys.stderr,
        )
        sys.exit(EXIT_PRECONDITION)
    if r.returncode != 0:
        print(
            f"{sys.argv[0]}: PRECONDITION FAILED — `{' '.join(cmd)}` exited {r.returncode}, so "
            f"{name!r} resolves but does not run. Nothing was checked.\n"
            f"  stderr: {r.stderr.strip()[:300]}\n",
            file=sys.stderr,
        )
        sys.exit(EXIT_PRECONDITION)
