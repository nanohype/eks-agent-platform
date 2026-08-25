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
BASH4_CONSTRUCTS: list[tuple[str, re.Pattern[str], str, str]] = [
    ("declare -A / local -A", re.compile(r"\b(?:declare|local|typeset)\s+-[A-Za-z]*A\b"), "4.0",
     "declare: -A: invalid option"),
    ("mapfile", re.compile(r"\bmapfile\b"), "4.0", "command not found"),
    ("readarray", re.compile(r"\breadarray\b"), "4.0", "command not found"),
    ("coproc", re.compile(r"\bcoproc\b"), "4.0", "syntax error"),
    ("${var^^} / ${var,,} case modification", re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*(\^\^?|,,?)"), "4.0",
     "bad substitution"),
    ("&>> append redirect", re.compile(r"&>>"), "4.0", "syntax error"),
]


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
    args = ap.parse_args()

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

    if scanned == 0:
        print("check-shell-portability: scanned nothing", file=sys.stderr)
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

    print(f"✓ {scanned} shell script(s) use nothing newer than bash 3.2")
    return 0


if __name__ == "__main__":
    sys.exit(main())
