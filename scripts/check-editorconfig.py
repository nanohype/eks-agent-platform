#!/usr/bin/env python3
"""The rules .editorconfig declares that no formatter owns, checked from the tree.

WHY THIS EXISTS

.editorconfig declares charset, line endings, a final newline and no trailing
whitespace for every file in the tree. The three formatters each own one
language — tofu fmt for Terraform, biome for TypeScript and JSON, gofmt for Go —
so YAML, HCL, Markdown, shell and the chart templates had nothing observing
those four rules until something read the whole tree against them.

WHY IT IS HERE RATHER THAN A DEPENDENCY

The reader before this one resolved a release through the GitHub API and
downloaded a binary at check time. Every published version of that package does
this; none ships the binary. So the verdict on a merge depended on an
unauthenticated call to a third party, at the moment a merge was waiting for it,
and it failed both ways that call can fail: a rate limit, and an asset lookup
that came back empty. Both printed as a failing format check on a tree with no
formatting defect.

A gate on the merge path decides from the tree. That is the whole reason this is
a script here and not an install step.

WHAT IT CHECKS

For every file `git ls-files` names, the properties the matching sections of
.editorconfig resolve to:

    charset                   utf-8, decoded rather than guessed at
    end_of_line               lf, so a CR before a LF is a finding
    insert_final_newline      the last byte is a newline
    trim_trailing_whitespace  no space or tab before a line ending

Sections are applied in file order and later ones win, which is how `[*.md]`
turns trailing-whitespace off for Markdown alone — two trailing spaces are that
language's hard-break idiom.

WHAT IT REFUSES TO DECIDE, RATHER THAN PASSING

Indentation is delegated: indent_style and indent_size are the three formatters'
business, and a Makefile needs tabs in its recipes and spaces in the
continuation lines inside them. Delegated is not the same as ignored — it is
recorded here, and a property that is neither implemented nor delegated stops
the run.

The same for syntax. This decides a subset of the pattern language: a literal
name, a `*` wildcard within one path segment, and a brace list of those. A
section naming a directory, a `**`, a character class or a `?` exits without a
verdict rather than matching approximately, because a pattern read wrongly
checks a different set of files than the one declared and reports success over
the difference.

THE EXIT CODES ARE THE POINT

    0  every file matches what .editorconfig declares
    1  a file does not, named with its line
    2  argparse — an argument this does not take
    3  NOTHING WAS CHECKED

Three is the code the reader before this one could not produce. Its download
failed and the step went red, so the tree was reported malformed by a check that
never ran. Anything this cannot evaluate — no .editorconfig, no section, a
property or a pattern outside what it decides, no files — exits 3 and says so.

Usage: ./scripts/check-editorconfig.py
"""

from __future__ import annotations

import argparse
import fnmatch
import pathlib
import re
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import EXIT_CANNOT_EVALUATE, require_binary

from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CONFIG = ROOT / ".editorconfig"

# Decided here, against the file's bytes.
IMPLEMENTED = {
    "charset": {"utf-8"},
    "end_of_line": {"lf"},
    "insert_final_newline": {"true", "false"},
    "trim_trailing_whitespace": {"true", "false"},
}

# Owned elsewhere, with the owner named. An entry here is a decision not to
# check, which is different from a property nobody thought about — and the
# difference is enforced: a property in neither table stops the run.
DELEGATED = {
    "indent_style": "tofu fmt, biome and gofmt each own indentation for their language, and a "
    "Makefile needs tabs in a recipe and spaces inside its continuation lines",
    "indent_size": "same owners; the formatters disagree with each other by language, which is "
    "correct, and a single declared width cannot be true for all of them",
}

# The pattern syntax this resolves. Anything else is refused rather than
# approximated: a `/` or a `**` makes a section path-relative, and matching it on
# the basename would check a different set of files than the section names.
UNDECIDED_SYNTAX = re.compile(r"\*\*|/|\?|\[")

SECTION = re.compile(r"^\[(?P<glob>.+)\]$")
ASSIGNMENT = re.compile(r"^(?P<key>[A-Za-z0-9_]+)\s*[=:]\s*(?P<value>.*)$")


class CannotEvaluate(Exception):
    """Nothing was checked. Distinct from a finding, and it must stay distinct.

    Raised rather than exited so the self-test can put a tree in front of the
    same code path and read the outcome, instead of mutating the repository this
    gate is meant to be reading.
    """


def cannot_evaluate(*lines: str) -> None:
    raise CannotEvaluate("\n".join(lines))


def parse_config(text: str) -> list[tuple[str, dict[str, str]]]:
    """Sections in file order, each with its properties. Preamble keys are dropped."""
    sections: list[tuple[str, dict[str, str]]] = []
    current: dict[str, str] | None = None
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line[0] in "#;":
            continue
        found = SECTION.match(line)
        if found:
            current = {}
            sections.append((found.group("glob"), current))
            continue
        assignment = ASSIGNMENT.match(line)
        if assignment and current is not None:
            current[assignment.group("key").lower()] = assignment.group("value").strip().lower()
    return sections


def alternatives(glob: str) -> list[str]:
    """A brace list becomes its members; anything else is itself.

    Braces are expanded once, which is the shape .editorconfig uses to name a
    file with no extension alongside a suffix. A nested brace is not decided.
    """
    if "{" not in glob and "}" not in glob:
        return [glob]
    found = re.fullmatch(r"\{([^{}]*)\}", glob)
    if not found:
        return []
    return [part.strip() for part in found.group(1).split(",") if part.strip()]


def resolve(path: str, sections) -> dict[str, str]:
    """Every matching section merged in order, later winning."""
    out: dict[str, str] = {}
    name = path.rsplit("/", 1)[-1]
    for glob, properties in sections:
        if any(fnmatch.fnmatchcase(name, alt) for alt in alternatives(glob)):
            out.update(properties)
    return out


def tracked_files(root: Path) -> list[str]:
    proc = subprocess.run(
        ["git", "ls-files", "-z"], cwd=root, capture_output=True, text=True, check=False
    )
    if proc.returncode != 0:
        cannot_evaluate(
            "`git ls-files` failed, so the set of files to read is unknown.",
            f"git said: {proc.stderr.strip()[:200]}",
        )
    return [p for p in proc.stdout.split("\0") if p]


def inspect(path: Path, rules: dict[str, str], name: str, problems: list[str]) -> bool:
    """Read one file against its resolved rules. False means it was not read."""
    data = path.read_bytes()
    if not data:
        return False
    if b"\0" in data[:8192]:
        return False

    if rules.get("charset") == "utf-8":
        try:
            data.decode("utf-8")
        except UnicodeDecodeError as exc:
            problems.append(f"{name}: is not valid utf-8 ({exc.reason} at byte {exc.start})")
            return True

    text = data.decode("utf-8", errors="replace")
    lines = text.split("\n")

    if rules.get("end_of_line") == "lf":
        for number, line in enumerate(lines[:-1], start=1):
            if line.endswith("\r"):
                problems.append(f"{name}:{number}: ends with CRLF, and end_of_line is lf")
                break

    if rules.get("insert_final_newline") == "true" and not data.endswith(b"\n"):
        problems.append(f"{name}:{len(lines)}: has no final newline")

    if rules.get("trim_trailing_whitespace") == "true":
        for number, line in enumerate(lines, start=1):
            stripped = line.rstrip("\r")
            if stripped and stripped[-1] in " \t":
                problems.append(f"{name}:{number}: has trailing whitespace")
                break

    return True


def evaluate(root: Path, problems: list[str], counted: list[int]) -> int:
    """0 clean, 1 findings in `problems`. Raises CannotEvaluate for anything else.

    Reporting belongs to the caller: the self-test drives this against eleven
    crafted trees and a function that printed its own verdict would bury the one
    the run is about.
    """
    config = root / ".editorconfig"
    if not config.is_file():
        cannot_evaluate(
            ".editorconfig does not exist, so there are no rules to check against.",
        )
    sections = parse_config(config.read_text(encoding="utf-8"))
    if not sections:
        cannot_evaluate(
            ".editorconfig declares no section, so it matches no file.",
        )

    for glob, properties in sections:
        if UNDECIDED_SYNTAX.search(glob) or not alternatives(glob):
            cannot_evaluate(
                f"section [{glob}] uses pattern syntax this does not decide.",
                "A literal name, a `*` inside one path segment, and a brace list of those are what",
                "it resolves. Matching anything else on the basename would read a different set of",
                "files than the section names, and report success over the difference.",
            )
        for key in properties:
            if key in IMPLEMENTED or key in DELEGATED:
                continue
            cannot_evaluate(
                f"section [{glob}] declares `{key}`, which this neither checks nor delegates.",
                "Passing over it would report the tree as matching a rule nothing read.",
                "Implement it, or record who owns it in DELEGATED with the reason.",
            )
        for key, value in properties.items():
            allowed = IMPLEMENTED.get(key)
            if allowed is not None and value not in allowed:
                cannot_evaluate(
                    f"section [{glob}] sets `{key} = {value}`, and this decides {sorted(allowed)}.",
                    "A value it cannot check is not a value it may skip.",
                )

    files = tracked_files(root)
    if not files:
        cannot_evaluate("`git ls-files` named no file, so nothing was read.")

    read = 0
    for name in files:
        path = root / name
        if not path.is_file():
            continue  # a submodule or a deleted-but-staged path
        rules = resolve(name, sections)
        if not rules:
            continue
        if inspect(path, rules, name, problems):
            read += 1

    if not read:
        cannot_evaluate(
            f"{len(files)} tracked path(s), and none was readable text this could compare.",
        )

    if problems:
        return 1
    counted.append(read)
    return 0


def main() -> int:
    require_binary("git", "list the files under version control, which is the set this reads")
    problems: list[str] = []
    counted: list[int] = []
    try:
        code = evaluate(ROOT, problems, counted)
    except CannotEvaluate as why:
        print(f"{sys.argv[0]}: NOTHING WAS CHECKED —", file=sys.stderr)
        for line in str(why).splitlines():
            print(f"  {line}", file=sys.stderr)
        print(
            "\n  This is not a finding about the tree. No file was compared against "
            "anything.",
            file=sys.stderr,
        )
        return EXIT_CANNOT_EVALUATE
    if code:
        print(
            f".editorconfig declares rules {len(problems)} file(s) do not match:\n",
            file=sys.stderr,
        )
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        print(
            "\nEach is charset, line ending, final newline or trailing whitespace — the rules no "
            "formatter owns. Fix the file; the declaration is in .editorconfig.",
            file=sys.stderr,
        )
        return code
    delegated = ", ".join(sorted(DELEGATED))
    print(
        f"{counted[0]} tracked file(s) match what .editorconfig declares for them: "
        f"{', '.join(sorted(IMPLEMENTED))}."
    )
    print(f"  delegated to the language formatters, and not checked here: {delegated}")
    return code


CASES: list[tuple[str, str, dict[str, str], int, str]] = [
    (
        "a tree that matches its own declaration",
        "[*]\ncharset = utf-8\nend_of_line = lf\ninsert_final_newline = true\n"
        "trim_trailing_whitespace = true\n",
        {"a.yaml": "key: value\n"},
        0,
        "",
    ),
    (
        "trailing whitespace",
        "[*]\ntrim_trailing_whitespace = true\n",
        {"a.yaml": "key: value \n"},
        1,
        "a.yaml:1: has trailing whitespace",
    ),
    (
        "no final newline",
        "[*]\ninsert_final_newline = true\n",
        {"a.yaml": "key: value"},
        1,
        "has no final newline",
    ),
    (
        "a CRLF ending",
        "[*]\nend_of_line = lf\n",
        {"a.yaml": "key: value\r\n"},
        1,
        "ends with CRLF",
    ),
    (
        "a later section turning a rule off, which is how markdown keeps its hard breaks",
        "[*]\ntrim_trailing_whitespace = true\n\n[*.md]\ntrim_trailing_whitespace = false\n",
        {"a.md": "line  \n"},
        0,
        "",
    ),
    (
        "a brace list, the form that names a file with no extension",
        "[*]\ntrim_trailing_whitespace = false\n\n[{Makefile,*.mk}]\n"
        "trim_trailing_whitespace = true\n",
        {"Makefile": "all: \n"},
        1,
        "Makefile:1: has trailing whitespace",
    ),
    (
        "a property the gate neither checks nor delegates",
        "[*]\nmax_line_length = 80\n",
        {"a.yaml": "key: value\n"},
        EXIT_CANNOT_EVALUATE,
        "neither checks nor delegates",
    ),
    (
        "a value the gate does not decide",
        "[*]\nend_of_line = crlf\n",
        {"a.yaml": "key: value\n"},
        EXIT_CANNOT_EVALUATE,
        "and this decides",
    ),
    (
        "a path-relative section, which would read a different set of files",
        "[*]\ntrim_trailing_whitespace = true\n\n[charts/**/*.yaml]\ncharset = utf-8\n",
        {"a.yaml": "key: value\n"},
        EXIT_CANNOT_EVALUATE,
        "pattern syntax this does not decide",
    ),
    (
        "no declaration at all",
        None,
        {"a.yaml": "key: value\n"},
        EXIT_CANNOT_EVALUATE,
        "does not exist",
    ),
    (
        "a declaration matching nothing",
        "root = true\n",
        {"a.yaml": "key: value\n"},
        EXIT_CANNOT_EVALUATE,
        "declares no section",
    ),
]


def self_test() -> int:
    """Each verdict, against a tree built for it.

    The two that matter are the last group. A gate whose checker is unavailable
    and a gate whose tree is malformed both exit non-zero, and the reader of a red
    build cannot act on either until they are told apart. The reader this replaces
    could not tell them apart — it downloaded its checker when the check ran, and
    when the download failed the step said `format`. So the cases where nothing
    was checked are asserted here by exit code AND by what they say, and the
    positive-control harness deliberately refuses to score them as catches: an
    exit that means "could not evaluate" is not a rejection, and
    check-controls.py says so itself.

    Crafted trees, not the repository. A gate that mutates the tree it is meant
    to be reading cannot be run beside anything else.
    """
    require_binary("git", "list the files under version control, which is the set this reads")
    import tempfile

    failures = 0
    for label, config, files, want_code, want_text in CASES:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            if config is not None:
                (root / ".editorconfig").write_text(config, encoding="utf-8")
            for name, body in files.items():
                # Bytes, not text: a case whose whole subject is a CR before a
                # LF cannot go through a layer that translates line endings.
                (root / name).write_bytes(body.encode("utf-8"))
            for command in (["git", "init", "-q"], ["git", "add", "-A"]):
                subprocess.run(command, cwd=root, capture_output=True, check=True)

            problems: list[str] = []
            try:
                code = evaluate(root, problems, [])
                said = "\n".join(problems)
            except CannotEvaluate as why:
                code = EXIT_CANNOT_EVALUATE
                said = str(why)

        if code != want_code:
            failures += 1
            print(f"FAIL  {label}: exit {code}, expected {want_code}", file=sys.stderr)
            continue
        if want_text and want_text not in said:
            failures += 1
            print(f"FAIL  {label}: exit {code} was right and it said the wrong thing", file=sys.stderr)
            print(f"      expected to contain: {want_text}", file=sys.stderr)
            print(f"      said: {said or '(nothing)'}", file=sys.stderr)
            continue
        if want_code == EXIT_CANNOT_EVALUATE and problems:
            failures += 1
            print(
                f"FAIL  {label}: reported {len(problems)} finding(s) about the tree while saying "
                "nothing was checked",
                file=sys.stderr,
            )

    kinds = {code for _, _, _, code, _ in CASES}
    for code, why in (
        (0, "a self-test with no passing case cannot tell a gate that refuses everything from a correct one"),
        (1, "with no failing case it cannot tell one that accepts everything"),
        (EXIT_CANNOT_EVALUATE, "and with no could-not-evaluate case the distinction this gate exists to keep is untested"),
    ):
        if code not in kinds:
            failures += 1
            print(f"FAIL  no case expects exit {code} — {why}", file=sys.stderr)

    if failures:
        print(f"\n{failures} self-test case(s) failed.", file=sys.stderr)
        return 1
    verdicts = sorted(kinds)
    print(f"self-test: {len(CASES)} case(s), verdicts {verdicts}, each with the text it must say.")
    return 0


# Argument parsing is strict on purpose: a gate that ignores argv cannot tell a
# renamed flag from a correct one, so a CI step naming a mode this script does
# not have would keep exiting 0. scripts/check-gates.py asserts this for every
# gate here.
def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--self-test",
        action="store_true",
        help="run this gate against crafted trees and require each verdict, including the two it "
        "must keep apart: a tree that violates a rule, and a ruleset it cannot decide",
    )
    return ap.parse_args()


if __name__ == "__main__":
    args = _parse_args()
    if args.self_test:
        sys.exit(self_test())
    sys.exit(main())
