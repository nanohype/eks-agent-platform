#!/usr/bin/env python3
"""No tracked shell script uses a construct newer than the bash it will run under.

macOS ships bash 3.2 and always will — its licence changed at bash 4 — so a
`#!/usr/bin/env bash` script that uses a bash 4 builtin fails on the machine most
likely to run a LOCAL developer tool. With `set -euo pipefail`, it fails at the
first use, before doing anything, and the error reads as a broken shell rather
than an unsupported one.

SHELLCHECK DOES NOT CATCH THIS. It parses; it does not run, and a static parser
has no idea which bash the reader has. That is the general shape worth carrying
away: a linter evaluates what it evaluates, which is rarely what you assumed. If
a rule depends on the RUNTIME's version, no parser can hold it and something like
this file has to.

The class was live here: scripts/local-kx/install.sh built its prerequisite table
with `declare -A`, so the local-kx onboarding installer aborted on any macOS
before checking a single prerequisite.

WHAT IS CHECKED, AND THE VIEW

Comments are stripped before matching, quote-aware, because the construct
appearing in prose that WARNS about it must not be a finding — otherwise the
first thing this gate does is flag the sentence explaining it. Strings are left
intact: a script echoing the word `mapfile` in a help message is harmless, and
blanking string bodies would also blank the code that uses them.

    scripts/check-shell-portability.py [--list]
"""

from __future__ import annotations

import argparse
import os
import pathlib
import re
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import require_binary

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _source_text import strip_comments  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent

# Each: (name, pattern, the bash it needs, what it does on 3.2).
# Derived PER FEATURE, not per spelling. The earlier table listed ${x^^} but not
# ${x^}, and `declare -A` but not -n/-l/-u — every gap an adjacent spelling of an
# absence already known about. Measured against /bin/bash 3.2.57: eleven
# constructs broke there while this gate accepted them, and it passed its
# positive control the whole time, because a control proves detection of the
# planted case and says nothing about the rest of the class.
#
# Each entry is verified in both directions by EXECUTION, not by reading: the
# construct fails under 3.2, and the 3.2-valid neighbour it could be confused
# with does not match. scripts/check-shell-portability.py --self-test runs that.
# TWO FLOORS, deliberately not one number.
#
# The UNCONDITIONAL floor is a law about any tree: at least one scanned script
# must live OUTSIDE the gates' own directory. It is what catches a tree holding
# only scripts/, where this check found the four shell scripts shipped beside it
# and certified its own presence. Collapsing the two into a single count is what
# lets a gate's own directory satisfy its floor.
#
# The REPO-ONLY floor is a fact about THIS repository, not a law: it catches a
# walk that has shrunk to almost nothing. It is set under the real count so a
# genuine deletion does not trip it.
MIN_SHELL_SCRIPTS = 4
GATE_DIR = "scripts"

# The execution half of --self-test needs a bash 3.2 to reach its verdict, which
# makes that interpreter an external authority. Absent, the self-test FAILS
# unless this is set, and the opt-out path prints what it did not do.
PARTIAL_OPTOUT = "SHELL_SELFTEST_ALLOW_PARTIAL"

BASH4_CONSTRUCTS: list[tuple[str, re.Pattern[str], str, str]] = [
    # Parameter expansion operators added in 4.x.
    ("${var^} / ${var^^} / ${var,} / ${var,,} case modification",
     re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*(?:\[[^]]*\])?(\^\^?|,,?)"), "4.0", "bad substitution"),
    ("${var@OP} parameter transformation",
     re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*(?:\[[^]]*\])?@[A-Za-z]\}"), "4.4", "bad substitution"),
    ("${arr[-1]} negative array index",
     re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*\[\s*-"), "4.3", "bad array subscript"),

    # declare/local/typeset options. Matched by OPTION LETTER so a new spelling
    # of the same absence cannot slip past: -A associative (4.0), -n nameref
    # (4.3), -l/-u case-forcing (4.0).
    ("declare/local -A associative array",
     re.compile(r"\b(?:declare|local|typeset)\s+(?:-[A-Za-z]*\s+)*-[A-Za-z]*A"), "4.0",
     "declare: -A: invalid option"),
    ("declare/local -n nameref",
     re.compile(r"\b(?:declare|local|typeset)\s+(?:-[A-Za-z]*\s+)*-[A-Za-z]*n"), "4.3",
     "declare: -n: invalid option"),
    ("declare/local -l or -u case forcing",
     re.compile(r"\b(?:declare|local|typeset)\s+(?:-[A-Za-z]*\s+)*-[A-Za-z]*[lu]"), "4.0",
     "declare: -l: invalid option"),

    # Builtins and syntax added in 4.x.
    ("mapfile", re.compile(r"\bmapfile\b"), "4.0", "command not found"),
    ("readarray", re.compile(r"\breadarray\b"), "4.0", "command not found"),
    ("coproc", re.compile(r"\bcoproc\b"), "4.0", "syntax error"),
    ("wait -n", re.compile(r"\bwait\s+(?:-[A-Za-z]*\s+)*-[A-Za-z]*n\b"), "4.3", "wait: -n: invalid option"),
    ("read -N", re.compile(r"\bread\s+(?:-[A-Za-z]*\s+)*-[A-Za-z]*N"), "4.1", "read: -N: invalid option"),
    ("shopt -s globstar", re.compile(r"\bshopt\s+(?:-[a-z]\s+)*globstar\b"), "4.0",
     "shopt: globstar: invalid shell option name"),
    ("printf -v into an array element",
     re.compile(r"\bprintf\s+(?:-[A-Za-z]*\s+)*-v\s+[\"\']?[A-Za-z_][A-Za-z0-9_]*\["), "4.1",
     "printf: `a[0]': not a valid identifier"),

    # Redirection forms added in 4.x. `&>` and `>&` are 3.2-valid; only the
    # APPEND form and the varname form are not.
    ("&>> append redirect", re.compile(r"&>>"), "4.0", "syntax error"),
    ("|& pipe-with-stderr", re.compile(r"\|&"), "4.0", "syntax error"),
    ("{fd}> varname redirect", re.compile(r"(?<![$\\])\{[A-Za-z_][A-Za-z0-9_]*\}\s*[<>]"), "4.1",
     "ambiguous redirect / syntax error"),
]


# Each construct paired with the 3.2-VALID neighbour it is most confusable with.
# The pattern must match the first and must not match the second — that pairing
# is what makes the rule a feature rather than a spelling. Where a bash 3.2 is
# on hand the pair is also EXECUTED, because a table asserting what a shell does
# is a second source of truth for the shell.
PROBES: list[tuple[str, str, str]] = [
    ("${var^} / ${var^^} / ${var,} / ${var,,} case modification", 'x=ab; echo "${x^}"', 'x=ab; echo "${x#a}"'),
    ("${var@OP} parameter transformation", 'x=ab; echo "${x@U}"', 'x=ab; echo "${x}"'),
    ("${arr[-1]} negative array index", 'a=(1 2); echo "${a[-1]}"', 'a=(1 2); echo "${a[0]}"'),
    ("declare/local -A associative array", "declare -A mm; mm[k]=v", "declare -r c=1; echo \"$c\""),
    ("declare/local -n nameref", 'v=1; declare -n r=v; echo "$r"', 'declare -i n=1; echo "$n"'),
    ("declare/local -l or -u case forcing", 'declare -l s=AB; echo "$s"', 'declare -i n=1; echo "$n"'),
    ("mapfile", "mapfile -t a < /dev/null", "cat /dev/null"),
    ("readarray", "readarray -t a < /dev/null", "cat /dev/null"),
    ("coproc", "coproc cat; exec 0<&-", "cat /dev/null"),
    ("wait -n", "sleep 0 & wait -n", "sleep 0 & wait"),
    ("read -N", 'printf ab | read -N2 v', 'printf "x\\n" | { read -r v; echo "$v"; }'),
    ("shopt -s globstar", "shopt -s globstar", "shopt -s nullglob"),
    ("printf -v into an array element", 'a=(x); printf -v "a[0]" %s y', "printf -v s %s hi; echo \"$s\""),
    ("&>> append redirect", "echo x &>> /dev/null", "echo x &> /dev/null"),
    ("|& pipe-with-stderr", "echo hi |& cat", "echo hi | cat"),
    ("{fd}> varname redirect", "exec {fd}>/dev/null", 'x=1; echo "${x}"'),
]


def self_test() -> int:
    """Every rule must reject its construct and accept its 3.2-valid neighbour."""
    import shutil
    import subprocess

    by_name = {name: pat for name, pat, _, _ in BASH4_CONSTRUCTS}
    problems: list[str] = []

    named = {n for n, _, _ in PROBES}
    for name in by_name:
        if name not in named:
            problems.append(f"{name}: declared as a rule with no probe, so nothing exercises it")

    for name, breaking, valid in PROBES:
        pat = by_name.get(name)
        if pat is None:
            problems.append(f"{name}: probe names a rule that does not exist")
            continue
        if not pat.search(breaking):
            problems.append(f"{name}: does not match its own construct {breaking!r}")
        if pat.search(valid):
            problems.append(f"{name}: also matches the 3.2-VALID neighbour {valid!r} — a false alarm")

    # Execution, when a bash 3.2 is actually available. Anything newer cannot
    # answer the question, and guessing from the version string is the table
    # again.
    # Look for a 3.2 rather than taking the first bash on PATH. A developer
    # machine commonly has a homebrew bash 5 ahead of the system one, and using
    # it makes the execution half skip on the very machine that HAS the shell
    # this table is about.
    candidates = [c for c in (shutil.which("bash"), "/bin/bash", "/usr/bin/bash") if c]
    bash, ver = "", ""
    for cand in candidates:
        try:
            v = subprocess.run([cand, "-c", "echo $BASH_VERSION"], capture_output=True, text=True).stdout.strip()
        except OSError:
            continue
        if not ver:
            bash, ver = cand, v
        if v.startswith("3.2"):
            bash, ver = cand, v
            break
    if ver.startswith("3.2"):
        for name, breaking, valid in PROBES:
            for body, want_fail in ((breaking, True), (valid, False)):
                rc = subprocess.run([bash, "-c", "set -euo pipefail\n" + body],
                                    capture_output=True, text=True).returncode
                if (rc != 0) != want_fail:
                    problems.append(
                        f"{name}: under bash {ver} the probe {body!r} exited {rc}, which contradicts "
                        "what this table claims about that shell"
                    )
        print(f"  probes executed against bash {ver} at {bash}")
    elif os.environ.get(PARTIAL_OPTOUT) == "1":
        # The permissive path is EXPLICIT and it prints what it did not do. A
        # skip that exits 0 on its own initiative is how an interpreter-shaped
        # authority disappears into a green job.
        print(
            f"  PARTIAL: bash here is {ver or '<unknown>'}, not 3.2, so the EXECUTION half did not "
            f"run. {len(PROBES)} rules were checked against their construct and their 3.2-valid "
            f"neighbour by pattern only. Set by {PARTIAL_OPTOUT}=1."
        )
    else:
        problems.append(
            f"no bash 3.2 is available (found {ver or '<unknown>'}), so the table's claims about "
            "that shell were not executed. An interpreter this check needs to REACH ITS VERDICT is "
            f"an authority like any other, and its absence is a refusal. Set {PARTIAL_OPTOUT}=1 to "
            "accept the pattern half alone, which prints PARTIAL and says what it skipped."
        )

    if problems:
        print(f"\n{len(problems)} self-test failure(s):\n", file=sys.stderr)
        for pr in problems:
            print(f"  - {pr}", file=sys.stderr)
        return 1
    print(f"✓ {len(PROBES)} constructs: each rule matches its construct and not its 3.2-valid neighbour")
    return 0


# For a script declaring #!/bin/sh. Debian's /bin/sh is dash; macOS's is bash 3.2
# in POSIX MODE, which ACCEPTS most of these — so a developer running one locally
# cannot surface what breaks for a Debian user. The local shell is not merely
# different, it actively agrees with you.
SH_BASHISMS: list[tuple[str, re.Pattern[str], str]] = [
    ("[[ ]] test", re.compile(r"\[\["), "dash: [[: not found"),
    ("${var^^} / ${var,,}", re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*(\^\^?|,,?)"), "dash: Bad substitution"),
    ("${var/pat/rep} substitution", re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*/"), "dash: Bad substitution"),
    ("arrays", re.compile(r"\b[A-Za-z_][A-Za-z0-9_]*=\("), "dash: Syntax error"),
    ("function keyword", re.compile(r"^\s*function\s+[A-Za-z_]"), "dash: Syntax error", ),
    # Anchored at a word boundary. `$'` also occurs where a `$` ends a regex
    # inside single quotes and the `'` closes it — grep -E '\.tf$' is the case
    # that produced a false alarm here, and a gate whose first finding is a false
    # one is a gate people stop reading.
    ("$'...' ANSI-C quoting", re.compile(r"(?:^|[\s=(|&;])\$'"), "dash: treats it literally, silently"),
    ("source builtin", re.compile(r"^\s*source\s+"), "dash: source: not found"),
]


def tracked_shell_scripts() -> list[pathlib.Path]:
    """Every shell script git tracks, plus the husky hooks it does not glob."""
    try:
        out = subprocess.run(
            ["git", "ls-files", "*.sh", "*.bash"], capture_output=True, text=True, cwd=ROOT, timeout=60
        ).stdout.split()
    except (subprocess.SubprocessError, OSError) as e:
        sys.exit(f"check-shell-portability: cannot list tracked files: {e}")

    paths = [ROOT / p for p in out]
    hooks = ROOT / ".husky"
    if hooks.is_dir():
        paths += [p for p in sorted(hooks.iterdir()) if p.is_file() and not p.name.startswith(".")]

    found = [p for p in paths if p.is_file()]
    if not found:
        sys.exit(
            "check-shell-portability: found no shell scripts at all. Finding no violation and "
            "reading no files produce the same silence, so this is an error rather than a pass."
        )
    return found


def declared_shell(path: pathlib.Path) -> str:
    first = path.read_text(encoding="utf-8", errors="replace").split("\n", 1)[0]
    if "bash" in first:
        return "bash"
    if first.startswith("#!") and ("/sh" in first or first.endswith("sh")):
        return "sh"
    return "unknown"


def main() -> int:

    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", action="store_true", help="print every script scanned and its declared shell")
    ap.add_argument("--self-test", action="store_true",
                    help="check every rule against its construct and its 3.2-valid neighbour")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    require_binary("git", "enumerate the committed shell scripts this parses")

    scripts = tracked_shell_scripts()
    findings: list[str] = []
    scanned = 0

    for path in scripts:
        rel = path.relative_to(ROOT)
        shell = declared_shell(path)
        raw = path.read_text(encoding="utf-8", errors="replace")
        # Comments stripped: the sentence WARNING about a construct must not be
        # the thing that trips the gate.
        body = strip_comments(raw, "shell")
        scanned += 1
        if args.list:
            print(f"  {rel} ({shell})")

        if shell == "bash":
            for name, pattern, needs, symptom in BASH4_CONSTRUCTS:
                for m in pattern.finditer(body):
                    line = body[: m.start()].count("\n") + 1
                    findings.append(
                        f"{rel}:{line}  {name} needs bash {needs}\n"
                        f"      macOS ships bash 3.2, where this is: {symptom}\n"
                        f"      Rewrite for 3.2, or make the script check BASH_VERSINFO and say so."
                    )
        elif shell == "sh":
            for name, pattern, symptom in SH_BASHISMS:
                for m in pattern.finditer(body):
                    line = body[: m.start()].count("\n") + 1
                    findings.append(
                        f"{rel}:{line}  {name} is a bashism in a #!/bin/sh script\n"
                        f"      On Debian /bin/sh is dash: {symptom}\n"
                        f"      macOS /bin/sh is bash in POSIX mode and ACCEPTS it, so running it "
                        f"locally cannot surface this."
                    )

    # A FLOOR ON WHAT WAS EXAMINED, not an at-least-one check. Run against a tree
    # holding only these gate scripts, this check found the shell scripts shipped
    # BESIDE it and reported success — "scanned nothing" was never true, and the
    # count it printed had nothing acting on it. The floor sits under the number
    # this repository carries, so it catches a walk that matched almost nothing
    # rather than a single deletion.
    outside_gate_dir = sum(
        1 for q in tracked_shell_scripts() if q.relative_to(ROOT).parts[0] != GATE_DIR
    )
    if outside_gate_dir == 0:
        print(
            f"check-shell-portability: every one of the {scanned} script(s) scanned lives under "
            f"{GATE_DIR}/. A tree containing only the gates satisfies a count floor with the gates' "
            "own scripts, so a pass here says nothing about the repository.",
            file=sys.stderr,
        )
        return 1

    if scanned < MIN_SHELL_SCRIPTS:
        print(
            f"check-shell-portability: scanned {scanned} shell script(s), fewer than the "
            f"{MIN_SHELL_SCRIPTS} this repository carries. Finding almost nothing and finding "
            "nothing wrong are not the same result, and only one of them is a pass.",
            file=sys.stderr,
        )
        return 1

    if findings:
        print(f"\n{len(findings)} bash-4-only construct(s) in scripts declaring bash:\n", file=sys.stderr)
        for f in findings:
            print(f"  - {f}", file=sys.stderr)
        print(
            "\nshellcheck does not catch these: it parses, it does not run, and it cannot know which "
            "bash the reader has.",
            file=sys.stderr,
        )
        return 1

    print(
        f"✓ {scanned} shell script(s) use nothing newer than bash 3.2 "
        f"({outside_gate_dir} of them outside {GATE_DIR}/)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
