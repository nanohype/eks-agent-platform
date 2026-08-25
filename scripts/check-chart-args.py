#!/usr/bin/env python3
"""Every --flag the operator chart passes must be a flag the operator binary defines.

Go's `flag` package builds `flag.CommandLine` with `ExitOnError`, so an argument
the binary does not recognise makes `flag.Parse()` print "flag provided but not
defined" and call `os.Exit(2)` — before the manager starts, before the health
probes bind, before a single reconcile. The pod CrashLoopBackOffs and the only
signal is an exit code.

Nothing else in either repository can see this. The chart renders, `helm lint`
passes, `helm template` passes, kubeconform validates the rendered Deployment,
and the binary compiles and tests clean — because both artifacts are individually
correct and the only thing that joins them is a process nobody starts until an
install is already under way. Deleting a reconciler tier removes its flags from
main.go while the chart goes on passing them; that is exactly how the batch tier
left, taking its types, controller, CRD and RBAC with it and leaving two arg
lines behind.

The chart side is scoped to the container `args:` block rather than the whole
file. A bare `- --x` and a quoted `- "--x=1"` render identically to Helm, so a
whole-file scan can be defeated by quoting, and a `--flag` mentioned in a comment
elsewhere in the template would otherwise be read as passed.

Out of scope, deliberately and worth stating: `.Values.extraArgs` is rendered
verbatim into the same args list (templates/deployment.yaml, values.yaml), so
`--set extraArgs[0]=--batch-workers=2` reproduces this failure and passes this
gate. A values schema is the control for that path; this one covers what the
chart itself hardcodes.
"""

from __future__ import annotations

import argparse

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DEPLOYMENT = ROOT / "charts" / "operator" / "templates" / "deployment.yaml"
MAIN = ROOT / "operators" / "cmd" / "main.go"

# The container's args list: the `args:` key and every line indented deeper than
# it, so the scan stops at the sibling `env:` key rather than running to EOF.
ARGS_BLOCK = re.compile(r"^(\s*)args:\s*$\n((?:\1[ \t].*\n|[ \t]*\n)*)", re.M)

# A long flag as passed on the command line. Lowercase-and-dashes matches the
# convention every flag in this binary already follows.
PASSED_FLAG = re.compile(r"--([a-z][a-z0-9-]*)")

# The name argument of a flag registration: flag.StringVar(&x, "name", ...) and
# every other flag.<Type>Var in the package.
DEFINED_FLAG = re.compile(r'flag\.\w+Var\([^,]+,\s*"([a-z][a-z0-9-]*)"')


def main() -> int:
    for path in (DEPLOYMENT, MAIN):
        if not path.is_file():
            print(f"FAIL  {path.relative_to(ROOT)} does not exist")
            return 1

    block_match = ARGS_BLOCK.search(DEPLOYMENT.read_text(encoding="utf-8"))
    if not block_match:
        print(f"FAIL  no container args: block in {DEPLOYMENT.relative_to(ROOT)} —")
        print("      the parse matched nothing, so this check is asserting nothing.")
        return 1

    passed = sorted(set(PASSED_FLAG.findall(block_match.group(2))))
    defined = set(DEFINED_FLAG.findall(MAIN.read_text(encoding="utf-8")))

    # Neither side may be empty. Both are produced by regexes that a formatting
    # change could silently stop matching, and an empty set on either side makes
    # the comparison vacuously true.
    if not passed:
        print("FAIL  parsed zero flags out of the args block — this check evaluated")
        print("      nothing. The args list shape changed; fix the parse.")
        return 1
    if not defined:
        print(f"FAIL  parsed zero flag definitions out of {MAIN.relative_to(ROOT)} —")
        print("      this check evaluated nothing. The registration shape changed.")
        return 1

    undefined = [flag for flag in passed if flag not in defined]
    for flag in undefined:
        print(f"FAIL  the chart passes --{flag} and {MAIN.relative_to(ROOT)} defines")
        print("      no such flag. flag.Parse() exits 2 on an unknown argument, so")
        print("      the operator pod cannot start and never reports Available.")
    if undefined:
        print()
        print(f"{len(undefined)} of {len(passed)} flags the chart passes are undefined.")
        return 1

    print(f"chart args ok — all {len(passed)} flags the chart passes are defined.")
    return 0



# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__)
    # This gate takes no arguments; argparse rejects anything passed.
    return ap.parse_args()


if __name__ == "__main__":
    _parse_args()
    sys.exit(main())
