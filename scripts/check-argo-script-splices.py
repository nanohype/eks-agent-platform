#!/usr/bin/env python3
"""No Argo template expression is spliced into code a workflow step runs.

Argo resolves `{{...}}` by rewriting a template's text before the pod exists.
Inside a script's `source`, or inside the program a shell is handed with `-c`,
the resolved value becomes part of the program: a value carrying a quote, a `$(`
or a newline runs as code in the step's pod, under the step's credentials. A
line like

    echo '{{inputs.parameters.results-json}}' > /tmp/results.json

over a parameter that carries model replies runs any reply holding a single
quote as shell. helm lint, kubeconform and the PodSecurity gate all pass such a
template, because none of them looks at what a substitution lands in.

WHAT COUNTS AS CODE

  * a script template's `source`, whatever its interpreter;
  * the program argument of a container that runs inline code, wherever it
    falls across `command` and `args`:
      - a shell (sh, bash, dash, ash, zsh, ksh, mksh): the first word after its
        options when one of them is `-c`, alone or in a cluster. The walk skips
        the argument of `-o`/`+o` and `-O`/`+O`, alone or in a cluster, and of
        `--rcfile` and `--init-file`;
      - python: the word after `-c`, or the rest of the word when the program is
        attached (`-cCODE`, `-BcCODE`). The walk skips the argument of `-W`, `-X`
        and `--check-hash-based-pycs`; `-m` names a module, and the walk stops there;
      - node: the word after `-e`, `--eval`, `-p`, `--print` or a cluster holding
        e or p (`-pe`), and the value of `--eval=` or `--print=`.
    The interpreter may stand behind `busybox` or `env`. The env walk skips
    NAME=VALUE assignments and env's options, with the argument of `-u`, `-C`,
    `-a` and their long forms, attached or separate. The string handed to
    `env -S` or `--split-string` is code in its own right, because env splits it
    into the command line it runs, and the program that command line names is
    found the same way.

  Behind any other launcher (`tini --`, `timeout 60`, `nice`, `xargs`, an
  entrypoint script) the launcher's own options are unknown, so every later word
  that names an interpreter, `env` or `busybox` is read as the start of a
  command line it runs. That reading errs toward code: a data argument that
  spells an interpreter and its inline flag is held to the code rule.

  Any option outside those named is read as taking no argument. For a shell and
  for python, arguments after the program are positional data (`$1`,
  `sys.argv`) and are not code. node's walk does not stop at a script path, so a
  word after one that reads as an inline-program flag is held to the code rule.

  Comments inside a source count as code. Argo substitutes inside them too, and
  a value carrying a newline ends the comment.

  The templates read are those in `templates` and `templateDefaults`, and every
  template a step (`steps[][].inline`) or a DAG task (`dag.tasks[].inline`)
  defines inline, at any depth. The containers read are a template's `script`
  and `container`, every container of a `containerSet`, and its
  `initContainers` and `sidecars`.

  A container that sets no `command` runs its image's ENTRYPOINT, which the
  manifest does not show. Under a shell entrypoint `args: [-c, PROGRAM]` is code,
  and under an entrypoint of `sh -c` a lone PROGRAM is code with no `-c` to
  read, so nothing in `args` alone separates code from data. Such a container
  whose `args` hold an expression not on ALLOWED is undecided (exit 3), never
  read as data.

WHAT IS FLAGGED

  Every expression, `{{=...}}` included, whose name is not on ALLOWED. That is
  wider than step inputs and outputs (`inputs.parameters.*`, `steps.*.outputs.*`,
  `tasks.*`): a workflow parameter carries whatever its submitter put there, and
  a gate scoped to the narrower set passes `{{workflow.parameters.x}}` spliced
  directly.

THE ALLOWLIST

  An expression belongs on ALLOWED only when Argo or the API server generates its
  value in a fixed format that nothing a submitter, a tenant or a model supplies
  can reach. A parameter never belongs there, however it is validated: the
  validation lives in another file, and a splice keeps no record of depending on
  it.

  A value that has to reach a script travels as data instead: a document as a raw
  input artifact the script reads from a file, a scalar as an environment
  variable the script expands inside double quotes. The header of
  charts/operator/files/eval-runtime/workflow-template.yaml states the rule
  where that template's authors read it.

WHERE TEMPLATES ARE FOUND

  Every tracked *.yaml and *.yml holding an argoproj.io Workflow,
  WorkflowTemplate, ClusterWorkflowTemplate or CronWorkflow, found by content
  rather than by path. A file or manifest the walk cannot judge is exit 3, not a
  skip:

    * a file that names one of those kinds but does not parse as YAML. A Helm
      template wrapping a WorkflowTemplate in Go templating is where a splice
      would hide from a walk that passed over what it could not read;
    * a tracked *.yaml or *.yml that cannot be read, or whose bytes are not
      UTF-8, whatever they hold. A search of the raw bytes for argoproj.io
      cannot rule such a file out: the same manifest in UTF-16 holds no such
      byte run;
    * a manifest whose structure the walk cannot follow: a `spec`,
      `workflowSpec`, `dag`, `containerSet`, template, step, task or container
      that is not a mapping, a `templates`, `steps`, step group, `tasks`,
      container list, `command` or `args` that is not a list, or a `source` that
      is not a string. A walk that stepped past such a field would read it as
      holding no splice;
    * a container that hands an expression to its image's entrypoint, above.

  A path git lists that is gone from the working tree holds no bytes, and is
  passed over. A Workflow a program assembles at run time is outside this check.

Exit status: 0 no splice, 1 a splice, 2 usage error, 3 nothing could be checked
(git or PyYAML missing, a git call that did not finish or start, no workflow
manifest found, a file or manifest the walk cannot judge, or manifests holding
no code at all).

    scripts/check-argo-script-splices.py [--list] [--self-test]
"""

from __future__ import annotations

import argparse
import dataclasses
import os
import pathlib
import re
import shlex
import sys
import tempfile
from typing import Any

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import EXIT_CANNOT_EVALUATE, require_binary, run_tool  # noqa: E402

# Imported here but asserted after argument parsing, so --help and a usage error
# still behave on a machine without it.
try:
    import yaml

    HAVE_YAML = True
except ImportError:  # pragma: no cover - environment guard
    HAVE_YAML = False

ROOT = pathlib.Path(__file__).resolve().parent.parent

# Expression name -> why nothing outside Argo and the API server reaches it.
ALLOWED = {
    "workflow.uid": "the Workflow's metadata.uid, a UUID the API server assigns on create",
    "workflow.creationTimestamp.RFC3339": (
        "the Workflow's metadata.creationTimestamp, which the API server sets on create, "
        "in the layout Argo formats it to"
    ),
}

WORKFLOW_KINDS = ("Workflow", "WorkflowTemplate", "ClusterWorkflowTemplate", "CronWorkflow")

# Recognises a workflow manifest from its text alone, so a file that does not
# parse is still known to be one this gate owes a verdict on.
KIND_LINE = re.compile(
    r"^\s*kind:\s*[\"']?(?:" + "|".join(WORKFLOW_KINDS) + r")[\"']?\s*(?:#.*)?$", re.M
)

EXPRESSION = re.compile(r"\{\{(.*?)\}\}", re.S)

SHELLS = {"sh", "bash", "dash", "ash", "zsh", "ksh", "mksh"}

# Options that take the next argument, or the rest of their own word where the
# program allows it attached. Every option outside these sets is read as taking
# none, so the first word after it that is not an option ends the walk.
SHELL_SHORT_WITH_ARGUMENT = frozenset("oO")  # set -o/+o, bash's shopt -O/+O
SHELL_LONG_WITH_ARGUMENT = frozenset({"--rcfile", "--init-file"})
PYTHON_SHORT_WITH_ARGUMENT = frozenset("WX")
PYTHON_LONG_WITH_ARGUMENT = frozenset({"--check-hash-based-pycs"})
NODE_INLINE_LONG = ("--eval", "--print")
ENV_SHORT_WITH_ARGUMENT = frozenset("uCSa")
ENV_LONG_WITH_ARGUMENT = frozenset({"--unset", "--chdir", "--split-string", "--argv0"})


def interpreter(word: str) -> str | None:
    base = word.rsplit("/", 1)[-1]
    if base in SHELLS:
        return "shell"
    if re.fullmatch(r"python[0-9.]*", base):
        return "python"
    if base in ("node", "nodejs"):
        return "node"
    return None


def _env_options(argv: list[str], i: int) -> tuple[int, tuple[int, str] | None]:
    """Walk env's options and NAME=VALUE assignments from argv[i].

    Returns (index, split). Without -S, split is None and index is the command
    env runs. At -S, split is (index of the argument holding the string, the
    string) and index is the argument after it: env splits the string into
    words in the option's place and reads on from there.
    """
    while i < len(argv):
        a = argv[i]
        if a == "--":
            return i + 1, None
        if a.startswith("--"):
            name, eq, value = a.partition("=")
            if name in ENV_LONG_WITH_ARGUMENT and not eq:
                if name == "--split-string" and i + 1 < len(argv):
                    return i + 2, (i + 1, argv[i + 1])
                i += 2
                continue
            if name == "--split-string":
                return i + 1, (i, value)
            i += 1
            continue
        if len(a) > 1 and a[0] == "-":
            i += 1
            for k, ch in enumerate(a[1:], 1):
                if ch not in ENV_SHORT_WITH_ARGUMENT:
                    continue
                attached = a[k + 1:]
                if ch == "S":
                    if attached:
                        return i, (i - 1, attached)
                    return (i + 1, (i, argv[i])) if i < len(argv) else (i, None)
                if not attached:
                    i += 1
                break
            continue
        if a == "-" or "=" in a:
            i += 1
            continue
        return i, None
    return i, None


def _shell_program(argv: list[str], j: int) -> int | None:
    inline = False
    while j < len(argv):
        a = argv[j]
        if a == "--":
            j += 1
            break
        if a.startswith("--"):
            j += 2 if a in SHELL_LONG_WITH_ARGUMENT else 1
            continue
        if len(a) > 1 and a[0] in "-+":
            if "c" in a[1:]:
                inline = True
            j += 1 + sum(ch in SHELL_SHORT_WITH_ARGUMENT for ch in a[1:])
            continue
        break
    return j if inline and j < len(argv) else None


def _python_program(argv: list[str], j: int) -> int | None:
    while j < len(argv):
        a = argv[j]
        if a.startswith("--") and a != "--":
            j += 2 if a in PYTHON_LONG_WITH_ARGUMENT else 1
            continue
        if len(a) < 2 or a[0] != "-" or a == "--":
            # A script path, `-` for stdin, or the end of options: no inline program.
            return None
        for k, ch in enumerate(a[1:], 1):
            attached = a[k + 1:]
            if ch == "c":
                # python stops reading options at -c; what follows is the program.
                if attached:
                    return j
                return j + 1 if j + 1 < len(argv) else None
            if ch == "m":
                return None
            if ch in PYTHON_SHORT_WITH_ARGUMENT:
                j += 0 if attached else 1
                break
        j += 1
    return None


def _node_program(argv: list[str], j: int) -> int | None:
    for k in range(j, len(argv)):
        a = argv[k]
        if a in NODE_INLINE_LONG or (len(a) > 1 and a[0] == "-" and a[1] != "-" and set(a[1:]) & {"e", "p"}):
            return k + 1 if k + 1 < len(argv) else None
        if a.startswith(tuple(f"{o}=" for o in NODE_INLINE_LONG)):
            return k
    return None


def code_arguments(argv: list[str], scan: bool = True) -> list[int]:
    """Indices of the arguments in argv that a container runs as code.

    scan reads past a launcher at the head of the command line. A call made from
    that scan passes False, since the scan already starts a reading at every
    later word; letting each reading scan again repeats the work at every depth.
    """
    i = 0
    while i < len(argv):
        base = argv[i].rsplit("/", 1)[-1]
        if base == "busybox":
            i += 1
            continue
        if base != "env":
            break
        i, split = _env_options(argv, i + 1)
        if split is not None:
            # The string is code in its own right: env splits it into the command
            # line it runs. The program that command line names may still be a
            # word after it, so env's walk is read again over the split words.
            at, text = split
            out = [at]
            try:
                words = shlex.split(text)
            except ValueError:
                return out
            for k in code_arguments(["env", *words, *argv[i:]], scan):
                if k > len(words):
                    out.append(i + k - 1 - len(words))
            return out
    if i >= len(argv):
        return []
    kind = interpreter(argv[i])
    if kind is None:
        return _behind_launcher(argv, i) if scan else []
    program = {"shell": _shell_program, "python": _python_program, "node": _node_program}[kind](argv, i + 1)
    return [] if program is None else [program]


def _behind_launcher(argv: list[str], i: int) -> list[int]:
    """Code arguments of the command lines a launcher at argv[i] may run.

    Which word starts that command line depends on the launcher's own options,
    which are unknown here, so each later word naming an interpreter, env or
    busybox is read as the start of one.
    """
    out: set[int] = set()
    for k in range(i + 1, len(argv)):
        if interpreter(argv[k]) or argv[k].rsplit("/", 1)[-1] in ("env", "busybox"):
            out.update(k + idx for idx in code_arguments(argv[k:], scan=False))
    return sorted(out)


class Undecidable(Exception):
    """A manifest the walk cannot judge: a shape it cannot follow, or code it cannot see."""


def _shape(value: object) -> str:
    if isinstance(value, dict):
        return "a mapping"
    if isinstance(value, list):
        return "a list"
    return "a scalar"


def _mapping(value: object, where: str) -> dict[str, Any] | None:
    """value when it is a mapping, None when it is absent."""
    if value is None:
        return None
    if not isinstance(value, dict):
        raise Undecidable(f"{where} is {_shape(value)}, not a mapping")
    return value


def _mappings(value: object, where: str) -> list[dict[str, Any]]:
    """value when it is a list of mappings, [] when it is absent."""
    if value is None:
        return []
    if not isinstance(value, list):
        raise Undecidable(f"{where} is {_shape(value)}, not a list")
    for i, item in enumerate(value):
        if not isinstance(item, dict):
            raise Undecidable(f"{where}[{i}] is {_shape(item)}, not a mapping")
    return value


def _words(container: dict[str, Any], key: str, where: str) -> list[str]:
    """A container's `command` or `args` as strings, [] when it is absent."""
    value = container.get(key)
    if value is None:
        return []
    if not isinstance(value, list):
        raise Undecidable(f"{where}.{key} is {_shape(value)}, not a list")
    for i, word in enumerate(value):
        if isinstance(word, (dict, list)):
            raise Undecidable(f"{where}.{key}[{i}] is {_shape(word)}, not a string")
    return [str(word) for word in value]


def _containers(tmpl: dict[str, Any]) -> list[tuple[str, dict[str, Any]]]:
    out: list[tuple[str, dict[str, Any]]] = []
    for key in ("script", "container"):
        c = _mapping(tmpl.get(key), key)
        if c is not None:
            out.append((key, c))
    container_set = _mapping(tmpl.get("containerSet"), "containerSet")
    if container_set is not None:
        for i, c in enumerate(_mappings(container_set.get("containers"), "containerSet.containers")):
            out.append((f"containerSet.containers[{c.get('name', i)}]", c))
    for key in ("initContainers", "sidecars"):
        for i, c in enumerate(_mappings(tmpl.get(key), key)):
            out.append((f"{key}[{c.get('name', i)}]", c))
    return out


def code_fields(name: str, tmpl: dict[str, Any]) -> list[tuple[str, str]]:
    """(field, text) for every string in a template that a step executes as code.

    Raises Undecidable at a shape the walk cannot follow, and at a container
    that hands an expression to an entrypoint the manifest does not show.
    """
    out: list[tuple[str, str]] = []
    try:
        script = _mapping(tmpl.get("script"), "script")
        if script is not None and "source" in script:
            if not isinstance(script["source"], str):
                raise Undecidable(f"script.source is {_shape(script['source'])}, not a string")
            out.append(("script.source", script["source"]))
        for label, c in _containers(tmpl):
            command, args = _words(c, "command", label), _words(c, "args", label)
            if not command:
                held = [expr for arg in args for _, expr in splices(arg)]
                if held:
                    raise Undecidable(
                        f"{label} sets no command, so its image's entrypoint runs args holding {held[0]}, "
                        "and whether that is code depends on an entrypoint the manifest does not show"
                    )
                continue
            argv = command + args
            out.extend((f"{label} program argument", argv[idx]) for idx in code_arguments(argv))
    except Undecidable as e:
        raise Undecidable(f"template {name}: {e}") from None
    return out


def templates_of(doc: dict[str, Any]) -> list[tuple[str, dict[str, Any]]]:
    """(name, template) for every template a workflow manifest defines, inline
    templates included. Raises Undecidable at a shape the walk cannot follow."""
    spec = _mapping(doc.get("spec"), "spec") or {}
    if doc.get("kind") == "CronWorkflow":
        spec = _mapping(spec.get("workflowSpec"), "spec.workflowSpec") or {}
    out: list[tuple[str, dict[str, Any]]] = []
    # A YAML alias can hand the walk one template twice, or a template inside
    # itself; each is read once.
    seen: set[int] = set()

    def visit(name: str, tmpl: dict[str, Any]) -> None:
        if id(tmpl) in seen:
            return
        seen.add(id(tmpl))
        out.append((name, tmpl))
        for i, group in enumerate(_step_groups(tmpl.get("steps"), f"{name}.steps")):
            for j, step in enumerate(group):
                where = f"{name}.steps[{i}][{step.get('name', j)}].inline"
                inline = _mapping(step.get("inline"), where)
                if inline is not None:
                    visit(where, inline)
        dag = _mapping(tmpl.get("dag"), f"{name}.dag")
        if dag is not None:
            for j, task in enumerate(_mappings(dag.get("tasks"), f"{name}.dag.tasks")):
                where = f"{name}.dag.tasks[{task.get('name', j)}].inline"
                inline = _mapping(task.get("inline"), where)
                if inline is not None:
                    visit(where, inline)

    defaults = _mapping(spec.get("templateDefaults"), "spec.templateDefaults")
    if defaults is not None:
        visit("templateDefaults", defaults)
    for i, t in enumerate(_mappings(spec.get("templates"), "spec.templates")):
        visit(str(t.get("name", f"spec.templates[{i}]")), t)
    return out


def _step_groups(value: object, where: str) -> list[list[dict[str, Any]]]:
    """A template's `steps`: a list of step groups, each a list of steps."""
    if value is None:
        return []
    if not isinstance(value, list):
        raise Undecidable(f"{where} is {_shape(value)}, not a list")
    return [_mappings(group, f"{where}[{i}]") for i, group in enumerate(value)]


def splices(text: str) -> list[tuple[int, str]]:
    """(line within text, expression) for every expression not on ALLOWED."""
    out: list[tuple[int, str]] = []
    for m in EXPRESSION.finditer(text):
        if m.group(1).strip() in ALLOWED:
            continue
        out.append((text.count("\n", 0, m.start()) + 1, m.group(0)))
    return out


def allowed_uses(text: str) -> list[str]:
    return [m.group(1).strip() for m in EXPRESSION.finditer(text) if m.group(1).strip() in ALLOWED]


def classify(text: str) -> tuple[str, list[dict]]:
    """("manifests", docs) | ("unparseable", []) | ("other", []) for one file's text."""
    if "argoproj.io" not in text:
        return "other", []
    try:
        docs = [d for d in yaml.safe_load_all(text) if isinstance(d, dict)]
    except yaml.YAMLError:
        return ("unparseable", []) if KIND_LINE.search(text) else ("other", [])
    found = [
        d for d in docs
        if str(d.get("apiVersion", "")).partition("/")[0] == "argoproj.io" and d.get("kind") in WORKFLOW_KINDS
    ]
    return ("manifests", found) if found else ("other", [])


def file_line(file_text: str, field_text: str, line: int) -> int | None:
    """The file line holding line `line` of a field, when that line is unique in the file."""
    lines = field_text.split("\n")
    if not 0 < line <= len(lines):
        return None
    needle = lines[line - 1].strip()
    hits = [n for n, ln in enumerate(file_text.split("\n"), 1) if ln.strip() == needle]
    return hits[0] if len(hits) == 1 else None


@dataclasses.dataclass(frozen=True)
class Splice:
    """One expression found in code: where, and the field text it sits in."""

    template: str
    field: str
    line: int
    expr: str
    field_text: str

    def describe(self, file_text: str | None = None) -> str:
        at = file_line(file_text, self.field_text, self.line) if file_text is not None else None
        where = f"line {at} of the file" if at is not None else f"line {self.line} of the field"
        return f"{self.expr} in template {self.template} {self.field} ({where})"


def check_manifest(doc: dict[str, Any]) -> tuple[list[Splice], int, list[str]]:
    """(splices, code fields examined, allowed expressions seen) for one manifest.

    Raises Undecidable when the walk cannot judge the manifest.
    """
    found: list[Splice] = []
    allowed: list[str] = []
    examined = 0
    for name, tmpl in templates_of(doc):
        for field, text in code_fields(name, tmpl):
            examined += 1
            allowed.extend(allowed_uses(text))
            for line, expr in splices(text):
                found.append(Splice(name, field, line, expr, text))
    return found, examined, allowed


@dataclasses.dataclass(frozen=True)
class Checked:
    """One workflow manifest the walk judged, with the file it came from."""

    path: pathlib.Path
    doc: dict[str, Any]
    text: str
    found: list[Splice]
    examined: int
    allowed: list[str]


def tracked_yaml() -> list[pathlib.Path]:
    cmd = ["git", "ls-files", "-z", "--", "*.yaml", "*.yml"]
    r = run_tool(cmd, timeout=120, cwd=ROOT)
    if r.returncode != 0:
        print(f"check-argo-script-splices: `{' '.join(cmd)}` exited {r.returncode}: "
              f"{r.stderr.strip()[:300]}; nothing was checked.", file=sys.stderr)
        sys.exit(EXIT_CANNOT_EVALUATE)
    return [ROOT / p for p in r.stdout.split("\0") if p]


def survey(paths: list[pathlib.Path]) -> tuple[list[Checked], list[tuple[pathlib.Path, str]]]:
    """(checked, undecided): each workflow manifest among paths, judged, and each
    path this gate owes a verdict on and cannot give one, with why."""
    checked: list[Checked] = []
    undecided: list[tuple[pathlib.Path, str]] = []
    for path in paths:
        if not os.path.lexists(path):
            # Listed by git, gone from the working tree: no bytes to hold a splice.
            continue
        try:
            text = path.read_bytes().decode("utf-8")
        except OSError as e:
            undecided.append((path, f"cannot be read ({e.strerror or e})"))
            continue
        except UnicodeDecodeError:
            undecided.append((path, "is not UTF-8 text"))
            continue
        verdict, docs = classify(text)
        if verdict == "unparseable":
            undecided.append((path, "names a workflow kind but does not parse as YAML"))
        for doc in docs:
            try:
                found, examined, allowed = check_manifest(doc)
            except Undecidable as e:
                undecided.append((path, f"holds {doc['kind']} {_manifest_name(doc)}, which the walk cannot judge: {e}"))
                continue
            checked.append(Checked(path, doc, text, found, examined, allowed))
    return checked, undecided


def _manifest_name(doc: dict[str, Any]) -> str:
    metadata = doc.get("metadata")
    return str(metadata.get("name", "?")) if isinstance(metadata, dict) else "?"


# ── self-test ────────────────────────────────────────────────────────────────

SCORE_RESULTS_ECHO_SPLICE = "echo '{{inputs.parameters.results-json}}' > /tmp/results.json"


def _manifest(spec: Any, kind: str = "WorkflowTemplate") -> dict[str, Any]:
    return {"apiVersion": "argoproj.io/v1alpha1", "kind": kind, "metadata": {"name": "t"}, "spec": spec}


def _wft(template: dict[str, Any], kind: str = "WorkflowTemplate") -> dict[str, Any]:
    spec: dict[str, Any] = {"templates": [template]}
    if kind == "CronWorkflow":
        spec = {"schedule": "0 2 * * *", "workflowSpec": spec}
    return _manifest(spec, kind)


def _script(source: str, command: list[str] | None = None, **extra: Any) -> dict[str, Any]:
    return {"name": "step", "script": {"image": "x", "command": command or ["/bin/sh", "-c"],
                                       "source": source, **extra}}


def _container(command: list[str], args: list[str] | None = None) -> dict[str, Any]:
    c: dict[str, Any] = {"image": "x", "command": command}
    if args is not None:
        c["args"] = args
    return {"name": "step", "container": c}


def _entrypoint(args: list[str]) -> dict[str, Any]:
    """A container that sets no command, so its image's entrypoint runs args."""
    return {"name": "step", "container": {"image": "x", "args": args}}


def _steps(inline: dict[str, Any]) -> dict[str, Any]:
    return {"name": "main", "steps": [[{"name": "run", "inline": inline}]]}


def _dag(inline: dict[str, Any]) -> dict[str, Any]:
    return {"name": "main", "dag": {"tasks": [{"name": "run", "inline": inline}]}}


def self_test() -> int:
    failures: list[str] = []
    x = "{{inputs.parameters.x}}"
    brace_call = '{{=toJson({"a": inputs.parameters.x})}}'
    brace_last = '{{={"a": inputs.parameters.x} }}'
    reject = [
        ("the score step's results written through a single-quoted echo",
         _wft(_script(f"set -eu\n{SCORE_RESULTS_ECHO_SPLICE}\n")), "{{inputs.parameters.results-json}}", 2),
        ("a task output", _wft(_script('r="{{tasks.run.outputs.parameters.results}}"')),
         "{{tasks.run.outputs.parameters.results}}", 1),
        ("a step output", _wft(_script("printf '%s' '{{steps.run.outputs.result}}'")),
         "{{steps.run.outputs.result}}", 1),
        ("a workflow parameter", _wft(_script("m='{{workflow.parameters.cases-manifest}}'")),
         "{{workflow.parameters.cases-manifest}}", 1),
        ("an expression with inner spaces", _wft(_script("v='{{ inputs.parameters.x }}'")),
         "{{ inputs.parameters.x }}", 1),
        ("an expr-language expression", _wft(_script("v='{{=inputs.parameters.x}}'")),
         "{{=inputs.parameters.x}}", 1),
        ("an allowed name in expr-language form", _wft(_script("v='{{=workflow.uid}}'")),
         "{{=workflow.uid}}", 1),
        ("an expr-language call holding a closing brace", _wft(_script(f"v='{brace_call}'")), brace_call, 1),
        ("a closing brace right before the tag's end", _wft(_script(f"v='{brace_last}'")), brace_last, 1),
        ("an expression inside a comment", _wft(_script(f"set -eu\n# was {x}\ntrue\n")), x, 2),
        ("a python script source", _wft(_script(f"print('{x}')", command=["python3"])), x, 1),
        ("a templateDefaults script source",
         _manifest({"templateDefaults": {"script": {"image": "x", "command": ["sh"], "source": f"echo '{x}'"}},
                    "templates": [_container(["true"])]}), x, 1),
        ("a script inline in a step", _wft(_steps(_script(f"echo '{x}'"))), x, 1),
        ("a container inline in a DAG task", _wft(_dag(_container(["sh", "-c"], [f"echo {x}"]))), x, 1),
        ("an inline template inside an inline template", _wft(_steps(_dag(_script(f"echo '{x}'")))), x, 1),
        ("a container's sh -c program", _wft(_container(["sh", "-c"], [f"echo {x}"])), x, 1),
        ("a zsh -c program", _wft(_container(["zsh", "-c", f"echo {x}"])), x, 1),
        ("a program behind env and an option cluster",
         _wft(_container(["/usr/bin/env", "LC_ALL=C", "bash", "-euc"], [f"echo {x}"])), x, 1),
        ("a program behind -o pipefail", _wft(_container(["bash", "-o", "pipefail", "-c", f"echo {x}"])), x, 1),
        ("a program behind -o inside a cluster",
         _wft(_container(["bash", "-eo", "pipefail", "-c", f"echo {x}"])), x, 1),
        ("a program behind -O and its argument", _wft(_container(["bash", "-O", "extglob", "-c", f"echo {x}"])), x, 1),
        ("a program behind --rcfile and its argument",
         _wft(_container(["bash", "--rcfile", "/etc/rc", "-c", f"echo {x}"])), x, 1),
        ("a program behind --init-file and its argument",
         _wft(_container(["bash", "--init-file", "/etc/rc", "-c", f"echo {x}"])), x, 1),
        ("a program behind env -u and its argument",
         _wft(_container(["env", "-u", "FOO", "sh", "-c", f"echo {x}"])), x, 1),
        ("a program behind env -u attached in a cluster",
         _wft(_container(["env", "-iuFOO", "sh", "-c", f"echo {x}"])), x, 1),
        ("a program behind env --unset and its argument",
         _wft(_container(["env", "--unset", "FOO", "sh", "-c", f"echo {x}"])), x, 1),
        ("a program behind env --unset=", _wft(_container(["env", "--unset=FOO", "sh", "-c", f"echo {x}"])), x, 1),
        ("a program behind env -C and its argument",
         _wft(_container(["env", "-C", "/work", "sh", "-c", f"echo {x}"])), x, 1),
        ("an expression in the command line env -S splits",
         _wft(_container(["env", "-S", f"kubectl get evalsuite {x}"])), x, 1),
        ("a program after the command line env -S splits",
         _wft(_container(["/usr/bin/env", "-S", "sh -c"], [f"echo {x}"])), x, 1),
        ("a program behind tini", _wft(_container(["tini", "--", "sh", "-c", f"echo {x}"])), x, 1),
        ("a program behind timeout and its duration",
         _wft(_container(["timeout", "60"], ["sh", "-c", f"echo {x}"])), x, 1),
        ("an env -S command line behind a launcher",
         _wft(_container(["tini", "--", "env", "-S", f"sh -c 'echo {x}'"])), x, 1),
        ("a python -c program", _wft(_container(["python3", "-c", f"print('{x}')"])), x, 1),
        ("a python program attached to -c in a cluster", _wft(_container(["python3", f"-Bcprint('{x}')"])), x, 1),
        ("a node --eval program", _wft(_container(["node", "--eval", f"console.log('{x}')"])), x, 1),
        ("a node -pe program", _wft(_container(["node", "-pe", f"'{x}'"])), x, 1),
        ("a node -ep program", _wft(_container(["node", "-ep", f"'{x}'"])), x, 1),
        ("an initContainer's program",
         _wft({"name": "s", "container": {"image": "x", "command": ["true"]},
               "initContainers": [{"name": "init", "image": "x", "command": ["sh", "-c", f"echo {x}"]}]}), x, 1),
        ("a sidecar's program",
         _wft({"name": "s", "container": {"image": "x", "command": ["true"]},
               "sidecars": [{"name": "side", "image": "x", "command": ["sh", "-c", f"echo {x}"]}]}), x, 1),
        ("a containerSet container",
         _wft({"name": "s", "containerSet": {"containers": [
             {"name": "main", "image": "x", "command": ["sh", "-c"], "args": [f"echo {x}"]}]}}), x, 1),
        ("a CronWorkflow's nested template", _wft(_script(f"echo '{x}'"), kind="CronWorkflow"), x, 1),
        ("a Workflow", _wft(_script(f"echo '{x}'"), kind="Workflow"), x, 1),
        ("a ClusterWorkflowTemplate", _wft(_script(f"echo '{x}'"), kind="ClusterWorkflowTemplate"), x, 1),
    ]
    for what, doc, expr, line in reject:
        try:
            found, examined, _ = check_manifest(doc)
        except Exception as e:  # noqa: BLE001 - an escape is a fixture the walk did not judge
            failures.append(f"{what}: raised {type(e).__name__}: {e}")
            continue
        if not examined:
            failures.append(f"{what}: no code field was examined")
        if not any(s.expr == expr and s.line == line for s in found):
            got = [s.describe() for s in found]
            failures.append(f"{what}: expected {expr} at line {line} of the field, got {got!r}")

    # (what, manifest, whether it holds code). A fixture holding code has to be
    # examined, or its acceptance is a field the walk never reached.
    accept = [
        ("a parameter in an env value the script quotes",
         _wft(_script('printf \'%s\' "$X" > /tmp/x', env=[{"name": "X", "value": x}])), True),
        ("a parameter in an env value inside an inline template",
         _wft(_dag(_script('printf \'%s\' "$X" > /tmp/x', env=[{"name": "X", "value": x}]))), True),
        ("a parameter in a raw input artifact",
         _wft({**_script("cat /tmp/x.json"), "inputs": {
             "parameters": [{"name": "x"}],
             "artifacts": [{"name": "x", "path": "/tmp/x.json", "raw": {"data": x}}]}}), True),
        ("an allowed Argo-generated value", _wft(_script('run_id="$(date -u +%s)-{{workflow.uid}}"')), True),
        ("the allowed timestamp", _wft(_script("started='{{workflow.creationTimestamp.RFC3339}}'")), True),
        ("a positional argument after the program",
         _wft(_container(["sh", "-c", 'printf "%s" "$1"', "sh", x])), True),
        ("a positional argument after the program env -S names",
         _wft(_container(["env", "-S", "sh -c 'printf \"%s\" \"$1\"' sh", x])), True),
        ("a positional argument after a program behind a launcher",
         _wft(_container(["tini", "--", "sh", "-c", 'printf "%s" "$1"', "sh", x])), True),
        ("an argument to a binary that runs no inline code",
         _wft(_container(["kubectl"], ["get", "evalsuite", x])), False),
        ("a launcher running a binary that runs no inline code",
         _wft(_container(["tini", "--", "kubectl", "get", "evalsuite", x])), False),
        ("python running a module", _wft(_container(["python3", "-m", "http.server", x])), False),
        ("an entrypoint handed an allowed value", _wft(_entrypoint(["--run-id", "{{workflow.uid}}"])), False),
    ]
    for what, doc, holds_code in accept:
        try:
            found, examined, _ = check_manifest(doc)
        except Exception as e:  # noqa: BLE001 - an escape is a fixture the walk did not judge
            failures.append(f"{what}: raised {type(e).__name__}: {e}")
            continue
        if found:
            failures.append(f"{what} was flagged: {[s.describe() for s in found]!r}")
        if holds_code and not examined:
            failures.append(f"{what}: no code field was examined, so the acceptance proves nothing")
        if not holds_code and examined:
            failures.append(f"{what}: read as code, so a data argument would be held to the code rule")

    # Manifests the walk cannot judge: each has to stop the gate as undecided,
    # neither passing as clean nor escaping as a traceback, which exits 1.
    refuse = [
        ("a shell entrypoint handed -c and a program", _wft(_entrypoint(["-c", f"echo {x}"]))),
        ("an entrypoint handed a lone program", _wft(_entrypoint([f"echo {x}"]))),
        ("an entrypoint inline in a DAG task", _wft(_dag(_entrypoint(["-c", f"echo {x}"])))),
        ("a spec that is a list", _manifest([1])),
        ("a spec that is a scalar", _manifest("templates")),
        ("a workflowSpec that is a scalar",
         _manifest({"schedule": "0 2 * * *", "workflowSpec": "x"}, kind="CronWorkflow")),
        ("templates that are a mapping", _manifest({"templates": {"step": _script(f"echo '{x}'")}})),
        ("a step group that is a mapping", _wft({"name": "main", "steps": [{"name": "run",
                                                                              "inline": _script(f"echo '{x}'")}]})),
        ("a DAG that is a list", _wft({"name": "main", "dag": [{"name": "run", "inline": _script(f"echo '{x}'")}]})),
        ("a command that is a string", _wft({"name": "step", "container": {"image": "x",
                                                                           "command": f"sh -c 'echo {x}'"}})),
        ("a script that is a list", _wft({"name": "step", "script": [f"echo '{x}'"]})),
        ("a source that is a list", _wft({"name": "step", "script": {"image": "x", "command": ["sh"],
                                                                     "source": [f"echo '{x}'"]}})),
    ]
    for what, doc in refuse:
        try:
            check_manifest(doc)
        except Undecidable:
            continue
        except Exception as e:  # noqa: BLE001 - an escape is the defect this fixture holds off
            failures.append(f"{what}: raised {type(e).__name__}, which exits 1 and reads as a splice")
            continue
        failures.append(f"{what}: judged, though the walk cannot follow it")

    unreadable = (
        "apiVersion: argoproj.io/v1alpha1\nkind: WorkflowTemplate\nspec:\n"
        "  templates: {{ .Values.templates | toYaml | nindent 4 }}\n  bad: [\n"
    )
    if classify(unreadable)[0] != "unparseable":
        failures.append("a workflow manifest that does not parse was not refused")
    rollout = "apiVersion: argoproj.io/v1alpha1\nkind: AnalysisTemplate\nspec: {}\n"
    if classify(rollout)[0] != "other":
        failures.append("an AnalysisTemplate was read as a workflow manifest")
    if classify(yaml.safe_dump(_wft(_script("true"))))[0] != "manifests":
        failures.append("a parseable WorkflowTemplate was not found")

    # The walk over files, on files of its own: each one it cannot judge is
    # undecided, and only the readable manifest is checked.
    spliced = yaml.safe_dump(_wft(_script(f"echo '{x}'")))
    files = {
        "clean.yaml": yaml.safe_dump(_wft(_script("true"))).encode(),
        "latin-1.yaml": spliced.replace("name: t\n", "name: caf\xe9\n").encode("latin-1"),
        "utf-16.yaml": spliced.encode("utf-16"),
        "helm.yaml": unreadable.encode(),
        "spec-list.yaml": yaml.safe_dump(_manifest([_script(f"echo '{x}'")])).encode(),
        "notes.yml": b"title: not a workflow\n",
    }
    undecidable = {"latin-1.yaml", "utf-16.yaml", "helm.yaml", "spec-list.yaml", "directory.yaml"}
    with tempfile.TemporaryDirectory() as d:
        tmp = pathlib.Path(d)
        for name, data in files.items():
            (tmp / name).write_bytes(data)
        (tmp / "directory.yaml").mkdir()
        try:
            checked, undecided = survey([tmp / n for n in [*files, "directory.yaml", "deleted.yaml"]])
        except Exception as e:  # noqa: BLE001 - an escape is a file the walk did not judge
            failures.append(f"the walk over files raised {type(e).__name__}: {e}")
            checked, undecided = [], []
    if {p.name for p, _ in undecided} != undecidable:
        failures.append(f"files undecided: {sorted(p.name for p, _ in undecided)}, want {sorted(undecidable)}")
    if [c.path.name for c in checked] != ["clean.yaml"]:
        failures.append(f"manifests checked: {[c.path.name for c in checked]}, want only clean.yaml")
    for name, why in ALLOWED.items():
        if not why.strip():
            failures.append(f"ALLOWED[{name!r}] carries no reason")

    if failures:
        for f in failures:
            print(f"self-test: {f}", file=sys.stderr)
        return 1
    print(
        f"✓ check-argo-script-splices self-test: {len(reject)} splices rejected, {len(accept)} "
        f"data deliveries accepted, {len(refuse)} manifests and {len(undecidable)} files the walk cannot "
        "judge refused"
    )
    return 0


# ── main ─────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--list", action="store_true",
                    help="print every manifest checked, its code-field count and the allowed expressions it uses")
    ap.add_argument("--self-test", action="store_true",
                    help="prove the rule rejects known splices, accepts data delivery and refuses manifests and "
                    "files it cannot judge, without reading the tree")
    args = ap.parse_args()

    if not HAVE_YAML:
        print("check-argo-script-splices: PRECONDITION FAILED — PyYAML is not importable (install "
              "the version .github/workflows/ci.yaml pins as PYYAML_VERSION). Nothing was checked.",
              file=sys.stderr)
        return EXIT_CANNOT_EVALUATE
    if args.self_test:
        return self_test()

    require_binary("git", "list the tracked files Argo workflow manifests can live in")

    checked, undecided = survey(tracked_yaml())
    if undecided:
        for p, why in undecided:
            print(f"  UNDECIDED  {p.relative_to(ROOT)} {why}", file=sys.stderr)
        print("check-argo-script-splices: a file this gate cannot read could be a workflow manifest, and a "
              "manifest it cannot follow could hold a splice; neither is one without a splice. Nothing "
              "about those files was checked.", file=sys.stderr)
        return EXIT_CANNOT_EVALUATE
    if not checked:
        print("check-argo-script-splices: found no tracked argoproj.io workflow manifest, so nothing was "
              "checked. Finding none and having none to find read the same.", file=sys.stderr)
        return EXIT_CANNOT_EVALUATE

    findings: list[str] = []
    examined = 0
    for c in checked:
        rel = c.path.relative_to(ROOT)
        examined += c.examined
        if args.list:
            uses = ", ".join(sorted(set(c.allowed))) or "none"
            print(f"  {rel}: {c.doc['kind']} {_manifest_name(c.doc)}: {c.examined} code field(s); "
                  f"allowed expressions: {uses}")
        findings.extend(f"{rel}: {s.describe(c.text)}" for s in c.found)

    if examined == 0:
        print(f"check-argo-script-splices: {len(checked)} workflow manifest(s) parsed but none holds a "
              "script source or an inline program, so nothing was checked.", file=sys.stderr)
        return EXIT_CANNOT_EVALUATE

    if findings:
        print("A template expression is spliced into code a step runs. Argo substitutes it into the "
              "program text, so a value carrying a quote, a $( or a newline runs as code:\n")
        for f in findings:
            print(f"  FAIL  {f}")
        print("\nDeliver the value as data: a document as an inputs.artifacts raw artifact the script "
              "reads from its path, a scalar as an env value the script expands inside double quotes.")
        return 1

    print(f"✓ no template expression is spliced into code: {examined} code field(s) across "
          f"{len(checked)} workflow manifest(s); allowed: {', '.join(sorted(ALLOWED))}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
