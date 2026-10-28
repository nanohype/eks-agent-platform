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

  Any option outside those named is read as taking no argument. Arguments after
  the program are positional data (`$1`, `sys.argv`) and are not code.

  Comments inside a source count as code. Argo substitutes inside them too, and
  a value carrying a newline ends the comment.

  The containers read are a template's `script` and `container`, every
  container of a `containerSet`, and its `initContainers` and `sidecars`.

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
  rather than by path. A file the walk cannot judge is exit 3, not a skip:

    * a file that names one of those kinds but does not parse as YAML. A Helm
      template wrapping a WorkflowTemplate in Go templating is where a splice
      would hide from a walk that passed over what it could not read;
    * a tracked *.yaml or *.yml that cannot be read, or whose bytes are not
      UTF-8, whatever they hold. A search of the raw bytes for argoproj.io
      cannot rule such a file out: the same manifest in UTF-16 holds no such
      byte run.

  A path git lists that is gone from the working tree holds no bytes, and is
  passed over. A Workflow a program assembles at run time is outside this check.

Exit status: 0 no splice, 1 a splice, 2 usage error, 3 nothing could be checked
(git or PyYAML missing, a git call that did not finish or start, no workflow
manifest found, a file the walk cannot judge, or manifests holding no code at
all).

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

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import EXIT_CANNOT_EVALUATE, require_binary, run_tool  # noqa: E402

# Imported here but asserted after argument parsing, so --help and a usage error
# still behave on a machine without it.
try:
    import yaml
except ImportError:  # pragma: no cover - environment guard
    yaml = None

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


def code_arguments(argv: list[str]) -> list[int]:
    """Indices of the arguments in argv that a container runs as code."""
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
            for k in code_arguments(["env", *words, *argv[i:]]):
                if k > len(words):
                    out.append(i + k - 1 - len(words))
            return out
    if i >= len(argv):
        return []
    program = {"shell": _shell_program, "python": _python_program, "node": _node_program}.get(
        interpreter(argv[i]) or "", lambda _argv, _j: None
    )(argv, i + 1)
    return [] if program is None else [program]


def _containers(tmpl: dict) -> list[tuple[str, dict]]:
    out: list[tuple[str, dict]] = []
    for key in ("script", "container"):
        if isinstance(tmpl.get(key), dict):
            out.append((key, tmpl[key]))
    container_set = tmpl.get("containerSet")
    if isinstance(container_set, dict):
        for i, c in enumerate(container_set.get("containers") or []):
            if isinstance(c, dict):
                out.append((f"containerSet.containers[{c.get('name', i)}]", c))
    for key in ("initContainers", "sidecars"):
        for i, c in enumerate(tmpl.get(key) or []):
            if isinstance(c, dict):
                out.append((f"{key}[{c.get('name', i)}]", c))
    return out


def code_fields(tmpl: dict) -> list[tuple[str, str]]:
    """(field, text) for every string in a template that a step executes as code."""
    out: list[tuple[str, str]] = []
    script = tmpl.get("script")
    if isinstance(script, dict) and isinstance(script.get("source"), str):
        out.append(("script.source", script["source"]))
    for label, c in _containers(tmpl):
        argv = [str(a) for a in (c.get("command") or [])] + [str(a) for a in (c.get("args") or [])]
        out.extend((f"{label} program argument", argv[idx]) for idx in code_arguments(argv))
    return out


def templates_of(doc: dict) -> list[tuple[str, dict]]:
    """(name, template) for every template a workflow manifest defines."""
    spec = doc.get("spec") or {}
    if doc.get("kind") == "CronWorkflow":
        spec = spec.get("workflowSpec") or {}
    out: list[tuple[str, dict]] = []
    if isinstance(spec.get("templateDefaults"), dict):
        out.append(("templateDefaults", spec["templateDefaults"]))
    for t in spec.get("templates") or []:
        if isinstance(t, dict):
            out.append((str(t.get("name", "?")), t))
    return out


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


def check_manifest(doc: dict) -> tuple[list[Splice], int, list[str]]:
    """(splices, code fields examined, allowed expressions seen) for one manifest."""
    found: list[Splice] = []
    allowed: list[str] = []
    examined = 0
    for name, tmpl in templates_of(doc):
        for field, text in code_fields(tmpl):
            examined += 1
            allowed.extend(allowed_uses(text))
            for line, expr in splices(text):
                found.append(Splice(name, field, line, expr, text))
    return found, examined, allowed


def tracked_yaml() -> list[pathlib.Path]:
    cmd = ["git", "ls-files", "-z", "--", "*.yaml", "*.yml"]
    r = run_tool(cmd, timeout=120, cwd=ROOT)
    if r.returncode != 0:
        print(f"check-argo-script-splices: `{' '.join(cmd)}` exited {r.returncode}: "
              f"{r.stderr.strip()[:300]}; nothing was checked.", file=sys.stderr)
        sys.exit(EXIT_CANNOT_EVALUATE)
    return [ROOT / p for p in r.stdout.split("\0") if p]


def survey(paths: list[pathlib.Path]) -> tuple[list[tuple[pathlib.Path, dict, str]], list[tuple[pathlib.Path, str]]]:
    """(manifests, undecided): each workflow manifest among paths with its file's
    text, and each path this gate owes a verdict on and cannot give one, with why."""
    manifests: list[tuple[pathlib.Path, dict, str]] = []
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
        manifests.extend((path, d, text) for d in docs)
    return manifests, undecided


# ── self-test ────────────────────────────────────────────────────────────────

ORIGINAL_SPLICE = "echo '{{inputs.parameters.results-json}}' > /tmp/results.json"


def _wft(template: dict, kind: str = "WorkflowTemplate") -> dict:
    spec = {"templates": [template]}
    if kind == "CronWorkflow":
        spec = {"schedule": "0 2 * * *", "workflowSpec": spec}
    return {"apiVersion": "argoproj.io/v1alpha1", "kind": kind, "metadata": {"name": "t"}, "spec": spec}


def _script(source: str, command: list[str] | None = None, **extra) -> dict:
    return {"name": "step", "script": {"image": "x", "command": command or ["/bin/sh", "-c"],
                                       "source": source, **extra}}


def _container(command: list[str], args: list[str] | None = None) -> dict:
    c: dict = {"image": "x", "command": command}
    if args is not None:
        c["args"] = args
    return {"name": "step", "container": c}


def self_test() -> int:
    failures: list[str] = []
    x = "{{inputs.parameters.x}}"
    reject = [
        ("the score step's original results line", _wft(_script(f"set -eu\n{ORIGINAL_SPLICE}\n")),
         "{{inputs.parameters.results-json}}", 2),
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
        ("an expression inside a comment", _wft(_script(f"set -eu\n# was {x}\ntrue\n")), x, 2),
        ("a python script source", _wft(_script(f"print('{x}')", command=["python3"])), x, 1),
        ("a container's sh -c program", _wft(_container(["sh", "-c"], [f"echo {x}"])), x, 1),
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
        ("a python -c program", _wft(_container(["python3", "-c", f"print('{x}')"])), x, 1),
        ("a python program attached to -c in a cluster", _wft(_container(["python3", f"-Bcprint('{x}')"])), x, 1),
        ("a node --eval program", _wft(_container(["node", "--eval", f"console.log('{x}')"])), x, 1),
        ("a node -pe program", _wft(_container(["node", "-pe", f"'{x}'"])), x, 1),
        ("a node -ep program", _wft(_container(["node", "-ep", f"'{x}'"])), x, 1),
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
        found, examined, _ = check_manifest(doc)
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
        ("an argument to a binary that runs no inline code",
         _wft(_container(["kubectl"], ["get", "evalsuite", x])), False),
        ("python running a module", _wft(_container(["python3", "-m", "http.server", x])), False),
    ]
    for what, doc, holds_code in accept:
        found, examined, _ = check_manifest(doc)
        if found:
            failures.append(f"{what} was flagged: {[s.describe() for s in found]!r}")
        if holds_code and not examined:
            failures.append(f"{what}: no code field was examined, so the acceptance proves nothing")
        if not holds_code and examined:
            failures.append(f"{what}: read as code, so a data argument would be held to the code rule")

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
        "notes.yml": b"title: not a workflow\n",
    }
    undecidable = {"latin-1.yaml", "utf-16.yaml", "helm.yaml", "directory.yaml"}
    with tempfile.TemporaryDirectory() as d:
        tmp = pathlib.Path(d)
        for name, data in files.items():
            (tmp / name).write_bytes(data)
        (tmp / "directory.yaml").mkdir()
        manifests, undecided = survey([tmp / n for n in [*files, "directory.yaml", "deleted.yaml"]])
    if {p.name for p, _ in undecided} != undecidable:
        failures.append(f"files undecided: {sorted(p.name for p, _ in undecided)}, want {sorted(undecidable)}")
    if [p.name for p, _, _ in manifests] != ["clean.yaml"]:
        failures.append(f"manifests found: {[p.name for p, _, _ in manifests]}, want only clean.yaml")
    for name, why in ALLOWED.items():
        if not why.strip():
            failures.append(f"ALLOWED[{name!r}] carries no reason")

    if failures:
        for f in failures:
            print(f"self-test: {f}", file=sys.stderr)
        return 1
    print(
        f"✓ check-argo-script-splices self-test: {len(reject)} splices rejected, {len(accept)} "
        f"data deliveries accepted, {len(undecidable)} files the walk cannot judge refused"
    )
    return 0


# ── main ─────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--list", action="store_true",
                    help="print every manifest checked, its code-field count and the allowed expressions it uses")
    ap.add_argument("--self-test", action="store_true",
                    help="prove the rule rejects known splices, accepts data delivery and refuses files it "
                    "cannot judge, without reading the tree")
    args = ap.parse_args()

    if yaml is None:
        print("check-argo-script-splices: PRECONDITION FAILED — PyYAML is not importable "
              "(pip install pyyaml). Nothing was checked.", file=sys.stderr)
        return EXIT_CANNOT_EVALUATE
    if args.self_test:
        return self_test()

    require_binary("git", "list the tracked files Argo workflow manifests can live in")

    manifests, undecided = survey(tracked_yaml())
    if undecided:
        for p, why in undecided:
            print(f"  UNDECIDED  {p.relative_to(ROOT)} {why}", file=sys.stderr)
        print("check-argo-script-splices: a file this gate cannot read could be a workflow manifest, and "
              "one it cannot read is not one without a splice. Nothing about those files was checked.",
              file=sys.stderr)
        return EXIT_CANNOT_EVALUATE
    if not manifests:
        print("check-argo-script-splices: found no tracked argoproj.io workflow manifest, so nothing was "
              "checked. Finding none and having none to find read the same.", file=sys.stderr)
        return EXIT_CANNOT_EVALUATE

    findings: list[str] = []
    examined = 0
    for path, doc, text in manifests:
        rel = path.relative_to(ROOT)
        found, doc_examined, allowed = check_manifest(doc)
        examined += doc_examined
        if args.list:
            name = (doc.get("metadata") or {}).get("name", "?")
            uses = ", ".join(sorted(set(allowed))) or "none"
            print(f"  {rel}: {doc['kind']} {name}: {doc_examined} code field(s); allowed expressions: {uses}")
        findings.extend(f"{rel}: {s.describe(text)}" for s in found)

    if examined == 0:
        print(f"check-argo-script-splices: {len(manifests)} workflow manifest(s) parsed but none holds a "
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
          f"{len(manifests)} workflow manifest(s); allowed: {', '.join(sorted(ALLOWED))}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
