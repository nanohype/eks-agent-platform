#!/usr/bin/env python3
"""operators/PROJECT must list exactly the kinds this operator actually ships.

PROJECT is kubebuilder's scaffolding record, and nothing reads it at runtime —
which is precisely why it drifts. Deleting a tier removes its types, its
controller, its CRD and its RBAC, and every gate goes green while PROJECT still
declares the kind. The batch tier was deleted whole and its PROJECT entry
survived, so the file kept announcing a `BatchJob` API with a controller behind
it, at the front door of the repo an agent reads to learn what this operator is.

The comparison is against the generated CRD bases rather than the Go types,
because `config/crd/bases` is what actually gets applied — a kind can have types
and no CRD, and only the CRD decides whether the API server serves it.

Two directions, and they fail for opposite reasons:

  * PROJECT declares a kind with no CRD — the file promises an API that does not
    exist. That is the drift above.
  * A CRD exists that PROJECT does not declare — controller-gen ran against
    types nobody recorded, so `kubebuilder create api` was skipped and the next
    scaffolding command may not know about it.

    scripts/check-project-resources.py
"""

from __future__ import annotations

import argparse

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
PROJECT = ROOT / "operators" / "PROJECT"
CRD_DIR = ROOT / "operators" / "config" / "crd" / "bases"

# `kind: X` under a `resources:` entry. Parsed textually so this runs with no
# third-party YAML on the runner, matching the other gates in this directory.
PROJECT_KIND = re.compile(r"^\s+kind:\s*([A-Za-z][A-Za-z0-9]*)\s*$", re.M)

# The `kind:` a CRD names for the resource it defines, i.e. spec.names.kind —
# NOT the document's own `kind: CustomResourceDefinition`. Anchored on the
# indentation controller-gen emits for spec.names.
CRD_KIND = re.compile(r"^\s{4}kind:\s*([A-Za-z][A-Za-z0-9]*)\s*$", re.M)


def main() -> int:
    if not PROJECT.is_file():
        print(f"FAIL  {PROJECT} does not exist")
        return 1
    if not CRD_DIR.is_dir():
        print(f"FAIL  {CRD_DIR} does not exist")
        return 1

    declared = set(PROJECT_KIND.findall(PROJECT.read_text(encoding="utf-8")))
    shipped: set[str] = set()
    for crd in sorted(CRD_DIR.glob("*.yaml")):
        shipped.update(CRD_KIND.findall(crd.read_text(encoding="utf-8")))

    # Neither side may be empty. An empty set on either makes the comparison
    # vacuously true in one direction, and both are produced by regexes that a
    # formatting change upstream could silently stop matching.
    if not declared:
        print("FAIL  PROJECT declares no kinds — the parse matched nothing, so this")
        print("      check is comparing an empty set and asserting nothing.")
        return 1
    if not shipped:
        print(f"FAIL  no CRD in {CRD_DIR.relative_to(ROOT)} names a kind — the parse")
        print("      matched nothing, so this check is asserting nothing.")
        return 1

    ok = True
    for kind in sorted(declared - shipped):
        ok = False
        print(f"FAIL  PROJECT declares {kind}, but no CRD in config/crd/bases defines it.")
        print("      The file is announcing an API this operator does not serve.")
    for kind in sorted(shipped - declared):
        ok = False
        print(f"FAIL  config/crd/bases defines {kind}, but PROJECT does not declare it.")
        print("      Record it with `kubebuilder create api`, or the next scaffolding")
        print("      command will not know the kind exists.")

    if ok:
        print(f"PROJECT ok — {len(declared)} declared kinds match the shipped CRDs exactly:")
        print(f"  {', '.join(sorted(declared))}")
    return 0 if ok else 1



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
