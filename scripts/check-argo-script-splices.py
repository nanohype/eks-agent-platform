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
  * the program argument of a container that runs inline code: the argument
    after `-c` for a shell or python, after `-e`/`--eval`/`-p`/`--print` for
    node, wherever it falls across `command` and `args`, behind `env` or
    `busybox` included. Arguments after the program are positional data (`$1`,
    `sys.argv`) and are not code.

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
  rather than by path. A file that names one of those kinds but does not parse as
  YAML is exit 3, not a skip: a Helm template wrapping a WorkflowTemplate in Go
  templating is where a splice would hide from a walk that passed over what it
  could not read. A Workflow a program assembles at run time is outside this
  check.

Exit status: 0 no splice, 1 a splice, 2 usage error, 3 nothing could be checked
(git or PyYAML missing, no workflow manifest found, a manifest that does not
parse, or manifests holding no code at all).

    scripts/check-argo-script-splices.py [--list] [--self-test]
"""

from __future__ import annotations

import argparse
import pathlib
import re
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import EXIT_CANNOT_EVALUATE, require_binary  # noqa: E402

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
NODE_INLINE = ("-e", "--eval", "-p", "--print")


def interpreter(word: str) -> str | None:
    base = word.rsplit("/", 1)[-1]
    if base in SHELLS:
        return "shell"
    if re.fullmatch(r"python[0-9.]*", base):
        return "python"
    if base in ("node", "nodejs"):
        return "node"
    return None


def program_index(argv: list[str]) -> int | None:
    """Index of the argument a container runs as inline code, or None."""
    i = 0
    while i < len(argv):
        base = argv[i].rsplit("/", 1)[-1]
        if base == "env":
            i += 1
            while i < len(argv) and (argv[i].startswith("-") or "=" in argv[i]):
                i += 1
            continue
        if base == "busybox":
            i += 1
            continue
        break
    if i >= len(argv):
        return None
    kind = interpreter(argv[i])
    if kind is None:
        return None
    start = i + 1

    if kind == "node":
        for j in range(start, len(argv)):
            if argv[j] in NODE_INLINE:
                return j + 1 if j + 1 < len(argv) else None
            if argv[j].startswith(("--eval=", "--print=")):
                return j
        return None

    inline = False
    j = start
    while j < len(argv):
        a = argv[j]
        if a == "--":
            j += 1
            break
        if kind == "shell" and a in ("-o", "+o"):
            j += 2
            continue
        if kind == "python" and a in ("-W", "-X"):
            j += 2
            continue
        if kind == "python" and a == "-m":
            return None
        if a.startswith("--"):
            j += 1
            continue
        if len(a) > 1 and a[0] in "-+":
            if "c" in a[1:]:
                inline = True
                if kind == "python":
                    # python stops reading options at -c; what follows is the program.
                    j += 1
                    break
            j += 1
            continue
        break
    if inline and j < len(argv):
        return j
    return None


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
        idx = program_index(argv)
        if idx is not None:
            out.append((f"{label} program argument", argv[idx]))
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


class Splice:
    """One expression found in code: where, and the field text it sits in."""

    def __init__(self, template: str, field: str, line: int, expr: str, field_text: str):
        self.template = template
        self.field = field
        self.line = line
        self.expr = expr
        self.field_text = field_text

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
    try:
        r = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.SubprocessError) as e:
        print(f"check-argo-script-splices: `{' '.join(cmd)}` could not run ({e}); nothing was checked.",
              file=sys.stderr)
        sys.exit(EXIT_CANNOT_EVALUATE)
    if r.returncode != 0:
        print(f"check-argo-script-splices: `{' '.join(cmd)}` exited {r.returncode}: "
              f"{r.stderr.strip()[:300]}; nothing was checked.", file=sys.stderr)
        sys.exit(EXIT_CANNOT_EVALUATE)
    return [ROOT / p for p in r.stdout.split("\0") if p]


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
        ("a python -c program", _wft(_container(["python3", "-c", f"print('{x}')"])), x, 1),
        ("a node --eval program", _wft(_container(["node", "--eval", f"console.log('{x}')"])), x, 1),
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
    for name, why in ALLOWED.items():
        if not why.strip():
            failures.append(f"ALLOWED[{name!r}] carries no reason")

    if failures:
        for f in failures:
            print(f"self-test: {f}", file=sys.stderr)
        return 1
    print(
        f"✓ check-argo-script-splices self-test: {len(reject)} splices rejected, {len(accept)} "
        "data deliveries accepted, an unparseable workflow manifest refused"
    )
    return 0


# ── main ─────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--list", action="store_true",
                    help="print every manifest checked, its code-field count and the allowed expressions it uses")
    ap.add_argument("--self-test", action="store_true",
                    help="prove the rule rejects known splices and accepts data delivery, without reading the tree")
    args = ap.parse_args()

    if yaml is None:
        print("check-argo-script-splices: PRECONDITION FAILED — PyYAML is not importable "
              "(pip install pyyaml). Nothing was checked.", file=sys.stderr)
        return EXIT_CANNOT_EVALUATE
    if args.self_test:
        return self_test()

    require_binary("git", "list the tracked files Argo workflow manifests can live in")

    manifests: list[tuple[pathlib.Path, dict, str]] = []
    unparseable: list[pathlib.Path] = []
    for path in tracked_yaml():
        try:
            text = path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            continue
        verdict, docs = classify(text)
        if verdict == "unparseable":
            unparseable.append(path)
        manifests.extend((path, d, text) for d in docs)

    if unparseable:
        for p in unparseable:
            print(f"  UNDECIDED  {p.relative_to(ROOT)} names a workflow kind but does not parse as YAML",
                  file=sys.stderr)
        print("check-argo-script-splices: a workflow manifest that cannot be read is not one without a "
              "splice. Nothing about those files was checked.", file=sys.stderr)
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
