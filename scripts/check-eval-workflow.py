#!/usr/bin/env python3
"""Assert the eval WorkflowTemplate's pods can be admitted, and its scripts can run.

WHY THIS EXISTS

The eval pipeline was dead twice over, and both failures were invisible to every
gate this repo had, because nothing was wrong with the YAML.

    1. ADMISSION. templates/eval-runtime/namespace.yaml labels the namespace
       `pod-security.kubernetes.io/enforce: restricted`, and the WorkflowTemplate
       carried no securityContext at all. PSA is a built-in admission plugin with
       no exclusion list, so every step pod was REJECTED:

         pods "…-resolve-cases-…" is forbidden: violates PodSecurity
         "restricted:latest": allowPrivilegeEscalation != false (containers
         "init", "wait", "main" must set …), unrestricted capabilities …,
         runAsNonRoot != true …, seccompProfile …

       helm lint, helm template, kubeconform and trivy all pass on that
       WorkflowTemplate. It is a valid manifest. It just cannot produce a pod the
       cluster will accept.

    2. EXECUTION. The score step ran `run_id="$(date …)-$RANDOM"` under
       `command: ["/bin/sh", "-c"]`. /bin/sh in the eval-runner image is dash,
       which has no $RANDOM, and `set -eu` turns an unset parameter into a fatal
       error on that line:

         /argo/staging/script: 3: RANDOM: parameter not set

       So scoring exited 2 before it scored anything, on every run, on every
       cluster — and no linter looks inside a `source:` block.

Both are the same shape: a manifest that is perfectly valid and cannot work. This
checks the two properties that make it work.

WHAT IT DOES
    A. PSA — renders the chart, reads the profile the eval namespace ENFORCES
       (rather than hardcoding "restricted"), and asserts every container the
       WorkflowTemplate defines satisfies it, counting pod-level fields the
       container inherits.
    B. SHELL — for every script declaring a POSIX shell, flags bash-only
       constructs. A bashism here is a runtime failure in a container no test
       instantiates.

WHAT IT DOES NOT COVER, DELIBERATELY
    Argo injects `init` and `wait` containers into every workflow pod, and their
    allowPrivilegeEscalation/capabilities are per-container fields no
    WorkflowTemplate can set. They come from the workflow controller's
    `executor.securityContext`, which lives in eks-gitops
    (addons/argo-platform/argo-workflows/values.yaml). This gate cannot see that
    repo; it asserts the half this repo owns and says so, rather than implying
    the pod is admissible on that basis alone.
"""

from __future__ import annotations

import argparse

import pathlib
import re
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from _tooling import require_binary

try:
    import yaml
except ImportError:
    sys.exit("PyYAML required: pip install pyyaml")

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
CHART = REPO_ROOT / "charts" / "operator"
PSA_ENFORCE_LABEL = "pod-security.kubernetes.io/enforce"

# The restricted profile's container-level requirements, as (path, predicate,
# human description). Only `restricted` is modelled: it is what the namespace
# enforces, and a gate that silently accepted a weaker profile would be the very
# thing this file exists to prevent.
RESTRICTED_CONTAINER = [
    ("allowPrivilegeEscalation", lambda v: v is False,
     "allowPrivilegeEscalation must be false"),
    ("capabilities", lambda v: isinstance(v, dict) and "ALL" in (v.get("drop") or []),
     "capabilities.drop must include ALL"),
]
# These may come from the pod-level securityContext instead.
RESTRICTED_INHERITABLE = [
    ("runAsNonRoot", lambda v: v is True, "runAsNonRoot must be true"),
    ("seccompProfile",
     lambda v: isinstance(v, dict) and v.get("type") in ("RuntimeDefault", "Localhost"),
     "seccompProfile.type must be RuntimeDefault or Localhost"),
]

# Bash-only constructs that dash accepts at parse time (or fails only at
# runtime), so `sh -n` would not catch them. $RANDOM is the one that shipped.
BASHISMS = [
    (re.compile(r"\$RANDOM\b"), "$RANDOM (bash only; dash + set -u exits on it)"),
    (re.compile(r"\[\["), "[[ ]] test (bash only)"),
    (re.compile(r"<<<"), "<<< here-string (bash only)"),
    (re.compile(r"^\s*function\s+\w+", re.M), "`function` keyword (bash only)"),
    (re.compile(r"\$\{\w+:\d+(:\d+)?\}"), "${var:offset:length} slicing (bash only)"),
    (re.compile(r"^\s*\w+=\(", re.M), "array assignment (bash only)"),
    (re.compile(r"\$\{\w+\[[^\]]+\]\}"), "array subscript (bash only)"),
]
POSIX_SHELLS = ("/bin/sh", "sh", "/usr/bin/sh")


def render() -> list[dict]:
    proc = subprocess.run(
        ["helm", "template", "op", str(CHART),
         "--set", "evalRuntime.enabled=true",
         "--set", "evalRuntime.evalReportsBucket=ci-gate-bucket"],
        capture_output=True, text=True,
    )
    if proc.returncode != 0:
        sys.exit(f"FAIL  helm template failed:\n{proc.stderr}")
    return [d for d in yaml.safe_load_all(proc.stdout) if isinstance(d, dict) and d.get("kind")]


def containers(tmpl: dict):
    """(name, container-spec) for every container a template defines."""
    for key in ("script", "container"):
        if key in tmpl:
            yield tmpl["name"], tmpl[key]


def check_psa(docs: list[dict]) -> bool:
    print("── Pod Security Admission ──")
    namespaces = [d for d in docs if d["kind"] == "Namespace"]
    wfts = [d for d in docs if d["kind"] == "WorkflowTemplate"]
    if not wfts:
        print("  FAIL  the chart rendered no WorkflowTemplate to check")
        return False

    ok = True
    for wft in wfts:
        ns_name = wft["metadata"].get("namespace")
        ns = next((n for n in namespaces if n["metadata"]["name"] == ns_name), None)
        if ns is None:
            print(f"  FAIL  {wft['metadata']['name']} lands in namespace "
                  f"'{ns_name}', which this chart does not render — its enforced "
                  f"PSA profile is unknown, so compliance cannot be asserted")
            ok = False
            continue
        profile = (ns["metadata"].get("labels") or {}).get(PSA_ENFORCE_LABEL)
        if profile is None:
            print(f"  FAIL  namespace {ns_name} sets no {PSA_ENFORCE_LABEL} — "
                  f"either enforce a profile or this gate is checking nothing")
            ok = False
            continue
        if profile != "restricted":
            print(f"  FAIL  namespace {ns_name} enforces '{profile}'; this gate "
                  f"only models 'restricted'. Update it deliberately rather than "
                  f"letting a weaker profile pass unchecked.")
            ok = False
            continue

        pod_sc = wft["spec"].get("securityContext") or {}
        print(f"  {ns_name} enforces {profile}; checking "
              f"{wft['metadata']['name']}")

        # The pod-level block is asserted on its own, not merely as a fallback
        # for the containers below. Argo injects `init` and `wait` into every
        # workflow pod, and pod-level securityContext is the ONLY thing this
        # repo can set that reaches them. Containers we define could each carry
        # their own runAsNonRoot/seccompProfile and satisfy the loop below while
        # the injected pair still fails admission — so this has to fail
        # separately, or removing it looks harmless.
        for field, pred, desc in RESTRICTED_INHERITABLE:
            if not pred(pod_sc.get(field)):
                print(f"  FAIL  pod securityContext: {desc} — Argo's injected "
                      f"init/wait containers inherit only from here, and "
                      f"nothing else can set it for them")
                ok = False

        found = 0
        for tmpl in wft["spec"].get("templates", []):
            for name, spec in containers(tmpl):
                found += 1
                sc = spec.get("securityContext") or {}
                for field, pred, desc in RESTRICTED_CONTAINER:
                    if not pred(sc.get(field)):
                        print(f"  FAIL  {name}: {desc} "
                              f"(container securityContext.{field} = "
                              f"{sc.get(field)!r})")
                        ok = False
                for field, pred, desc in RESTRICTED_INHERITABLE:
                    if not (pred(sc.get(field)) or pred(pod_sc.get(field))):
                        print(f"  FAIL  {name}: {desc} — set on neither the "
                              f"container nor the pod securityContext")
                        ok = False
        if not found:
            print(f"  FAIL  {wft['metadata']['name']} defines no containers — "
                  f"nothing was checked")
            ok = False
        elif ok:
            print(f"  ok    all {found} containers satisfy {profile}")

    print("  note  Argo's injected init/wait containers are governed by the "
          "workflow controller's")
    print("        executor.securityContext (eks-gitops), which this repo "
          "cannot see. Both halves")
    print("        are required for the pod to be admitted.")
    print()
    return ok


def _strip_comments(source: str) -> str:
    """Blank out `#` comments, preserving line numbering.

    Scanned text must be what the shell EXECUTES. Without this the check flags
    itself: the fix for the $RANDOM defect explains the defect in a comment, and
    naming a bashism is not using one. Quote state is tracked so a `#` inside a
    string — common in an s3:// URL or a jq program — is not mistaken for the
    start of a comment.
    """
    out = []
    for line in source.split("\n"):
        quote = None
        for i, ch in enumerate(line):
            if quote:
                if ch == quote:
                    quote = None
            elif ch in ("'", '"'):
                quote = ch
            elif ch == "#" and (i == 0 or line[i - 1].isspace()):
                line = line[:i]
                break
        out.append(line)
    return "\n".join(out)


def check_shell(docs: list[dict]) -> bool:
    print("── POSIX shell scripts ──")
    ok, checked = True, 0
    for wft in (d for d in docs if d["kind"] == "WorkflowTemplate"):
        for tmpl in wft["spec"].get("templates", []):
            script = tmpl.get("script")
            if not script or "source" not in script:
                continue
            cmd = script.get("command") or []
            if not any(c in POSIX_SHELLS for c in cmd):
                continue  # a bash/other interpreter may use its own features
            checked += 1
            code = _strip_comments(script["source"])
            for pattern, desc in BASHISMS:
                m = pattern.search(code)
                if m:
                    line = code[:m.start()].count("\n") + 1
                    print(f"  FAIL  {tmpl['name']} runs under {cmd} but uses "
                          f"{desc} at line {line} of its source")
                    ok = False
    if not checked:
        print("  FAIL  no POSIX-shell scripts found to check — the gate would "
              "pass vacuously")
        ok = False
    elif ok:
        print(f"  ok    {checked} scripts use no bash-only construct")
    print()
    return ok


def main() -> int:
    require_binary("helm", "render the chart holding the eval workflow")

    docs = render()
    results = [check_psa(docs), check_shell(docs)]
    if all(results):
        print("Eval workflow gate PASSED.")
        return 0
    print("Eval workflow gate FAILED.")
    return 1



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
