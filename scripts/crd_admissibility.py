#!/usr/bin/env python3
"""One reading of "would the API server accept this custom resource?".

WHY THIS EXISTS

The question is asked in several places — a chart render is checked here, a
catalog tree is checked where the catalog lives, a tenant repository checks its
own declaration — and each place that answered it separately answered a
different question. A custom resource was then only as validated as whichever
reading happened to see it, and the classes one reading missed were not the
classes another missed. Two `spec.datastores` both named `main` carry every
required property and declare nothing undeclared; a reading built from
`required` and pruning alone admits them, and the API server answers
`spec.datastores: Duplicate value` and refuses the whole object.

Splitting the answer is what produces that. This module is the answer, and a
caller asks it rather than reimplementing it. What a caller still owns is the
population — which documents it feeds in, and which CustomResourceDefinitions
it resolves them against. Those differ legitimately: what a cluster installs is
not what a repository has on its main branch.

TWO VERDICTS, BECAUSE THE API SERVER HAS TWO

A declaration the API server rejects and a declaration it accepts after silently
dropping part of it are different answers, and a caller that collapses them
reports a field as working when it has never reached a cluster.

    REFUSED   the object is rejected and nothing is created
    PRUNED    the object is created with the property removed

WHAT IT HOLDS

Against the structural schema of the resolved CustomResourceDefinition version:

    required        a property listed in `required` and absent is REFUSED,
                    unless it carries a `default` — structural defaulting runs
                    before validation, so the API server fills it in and admits
                    the object. Reading `required` alone reports a Tenant whose
                    `spec.primaryPersona` defaults as refused, on a declaration
                    that has been admitted for as long as the field has existed.
    pruning         a property the schema does not declare is PRUNED, except
                    under `additionalProperties`, which declares the keys free,
                    and under `x-kubernetes-preserve-unknown-fields`, where the
                    API server keeps what it does not describe.
    list-type       `map` requires the tuple of `x-kubernetes-list-map-keys` to
                    be unique across entries, and `set` requires the entries
                    themselves to be. An absent key is a value and collides with
                    another entry that also omits it. `atomic` imposes no
                    uniqueness and is not flagged: an entry repeated inside an
                    atomic list is admitted, and reporting it teaches an author
                    to work around the reading.
    type            a value of the wrong JSON type is REFUSED. `boolean` and
                    `integer` are distinguished, which Python does not do on its
                    own. A node marked `x-kubernetes-int-or-string` accepts both.
    enum            a value outside `enum` is REFUSED.
    pattern         a string not matching `pattern` is REFUSED.
    bounds          `minimum`, `maximum` and their exclusive forms, `minLength`,
                    `maxLength`, `minItems` and `maxItems`.
    scope           a cluster-scoped kind carrying `metadata.namespace` is
                    PRUNED. The API server does not refuse it — it drops the
                    namespace and creates the object cluster-wide, so the
                    declaration reads as scoped and is not.

WHAT IT DOES NOT HOLD

    `x-kubernetes-validations` — the CEL rules. Seventeen of them are declared
    across these CustomResourceDefinitions and this evaluates none: a rule is a
    program, and running it needs the CEL environment the API server builds,
    including the `oldSelf` of a transition rule, which does not exist before an
    object does. A declaration this admits can still be refused by a rule. The
    conformance oracle beside this module puts every corpus case through a real
    API server, so a case whose only defect is a CEL rule is recorded as refused
    by the authority and not by this — the gap is measured rather than assumed.

    `format`. `int32`, `int64` and `date-time` are declared here and none is
    enforced against the value.

    Anything the request carries rather than the object: the namespace a
    namespaced object is created in comes from the request, so a namespaced kind
    with no `metadata.namespace` is not a defect in the document.

    Admission webhooks, quota, and any rule that is not in the schema.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - the callers all report this themselves
    raise SystemExit("PyYAML required: pip install pyyaml")

REFUSED = "refused"
PRUNED = "pruned"

# The keys the API server itself owns on every object. `metadata` is not pruned
# against the schema — a structural schema may not constrain it beyond name and
# generateName — so it is walked for scope alone.
OBJECT_KEYS = {"apiVersion", "kind", "metadata", "spec", "status"}


@dataclass(frozen=True)
class Finding:
    """One thing the API server would do to this document, where, and why."""

    verdict: str
    rule: str
    path: str
    detail: str

    def __str__(self) -> str:
        return f"[{self.verdict} {self.rule}] {self.path}: {self.detail}"


@dataclass(frozen=True)
class CRD:
    kind: str
    group: str
    scope: str
    schemas: dict[str, dict]

    @property
    def namespaced(self) -> bool:
        return self.scope == "Namespaced"


def load_crds(crd_dir: Path) -> dict[str, CRD]:
    """kind -> CRD, for every CustomResourceDefinition in a directory.

    Keyed by kind because a document names its kind and its apiVersion, and the
    kind is what selects the schema. A directory holding two definitions of one
    kind is a directory whose answer depends on file order, so it raises.
    """
    out: dict[str, CRD] = {}
    for path in sorted(Path(crd_dir).glob("*.yaml")):
        for doc in yaml.safe_load_all(path.read_text()):
            if not doc or doc.get("kind") != "CustomResourceDefinition":
                continue
            spec = doc["spec"]
            kind = spec["names"]["kind"]
            if kind in out:
                raise ValueError(
                    f"{path.name} defines {kind}, which another file in "
                    f"{crd_dir} also defines"
                )
            out[kind] = CRD(
                kind=kind,
                group=spec["group"],
                scope=spec["scope"],
                schemas={
                    v["name"]: v["schema"]["openAPIV3Schema"]
                    for v in spec["versions"]
                    if v.get("schema")
                },
            )
    return out


def admissibility(doc: dict, crds: dict[str, CRD]) -> list[Finding]:
    """What the API server would do to one document. Empty means it is admitted.

    A document naming a kind or an apiVersion no definition here declares raises
    rather than returning empty: nothing validated it, and an empty list from
    this function means admitted.
    """
    kind = doc.get("kind")
    crd = crds.get(kind)
    if crd is None:
        raise KeyError(f"no CustomResourceDefinition here declares kind {kind!r}")
    api_version = doc.get("apiVersion", "")
    group, _, version = api_version.partition("/")
    if group != crd.group:
        raise KeyError(
            f"{kind} names apiVersion {api_version!r}, and the definition here "
            f"is group {crd.group!r}"
        )
    schema = crd.schemas.get(version)
    if schema is None:
        raise KeyError(
            f"{kind} names version {version!r}, and the definition here serves "
            f"{sorted(crd.schemas)}"
        )

    out: list[Finding] = []
    if not crd.namespaced and (doc.get("metadata") or {}).get("namespace"):
        out.append(
            Finding(
                PRUNED,
                "scope",
                "metadata.namespace",
                f"{kind} is a cluster-scoped kind, so the API server drops the "
                "namespace and creates the object cluster-wide — the declaration "
                "reads as scoped to one namespace and is not",
            )
        )

    props = schema.get("properties") or {}
    for name, value in doc.items():
        if name == "metadata":
            continue
        if name not in OBJECT_KEYS and name not in props:
            out.append(
                Finding(
                    PRUNED,
                    "property",
                    name,
                    "is not declared at the top level of the schema, so the API "
                    "server drops it and the object is created without it",
                )
            )
            continue
        child = props.get(name)
        if child is not None and name in ("spec", "status"):
            _walk(value, child, name, out)
    return out


def _walk(value, schema, path: str, out: list[Finding], *, prune: bool = True) -> None:
    if not isinstance(schema, dict):
        return

    prune = prune and not schema.get("x-kubernetes-preserve-unknown-fields")

    _scalar_rules(value, schema, path, out)

    if isinstance(value, list):
        _list_rules(value, schema, path, out)
        items = schema.get("items")
        if isinstance(items, dict):
            for i, item in enumerate(value):
                _walk(item, items, f"{path}[{i}]", out, prune=prune)
        return

    if not isinstance(value, dict):
        return

    props = schema.get("properties") or {}
    additional = schema.get("additionalProperties")

    for name in schema.get("required") or []:
        if name in value:
            continue
        # Defaulting runs before validation, so a required property carrying a
        # default is filled in by the API server and the object is admitted.
        if "default" in (props.get(name) or {}):
            continue
        out.append(
            Finding(
                REFUSED,
                "required",
                f"{path}.{name}",
                "is required by the schema, carries no default, and is absent, "
                "so the API server refuses the whole object with `Required value`",
            )
        )

    for name, child in value.items():
        child_schema = props.get(name)
        if child_schema is None and isinstance(additional, dict):
            child_schema = additional
        if child_schema is None:
            if prune and additional is not True:
                out.append(
                    Finding(
                        PRUNED,
                        "property",
                        f"{path}.{name}",
                        "is not declared by the schema, so the API server drops "
                        "it and the object is created without it",
                    )
                )
            continue
        _walk(child, child_schema, f"{path}.{name}", out, prune=prune)


def _list_rules(value: list, schema: dict, path: str, out: list[Finding]) -> None:
    """Uniqueness, for the two list types that impose it.

    `atomic` imposes none. Flagging a repeat inside an atomic list would name a
    declaration the API server admits, which is the kind of false answer that
    teaches an author to work around the reading rather than to fix the object.
    """
    list_type = schema.get("x-kubernetes-list-type")
    if list_type == "set":
        seen: dict = {}
        for i, item in enumerate(value):
            key = _hashable(item)
            if key in seen:
                out.append(
                    Finding(
                        REFUSED,
                        "list-set",
                        f"{path}[{i}]",
                        f"repeats the value at index {seen[key]} in a list "
                        "declared `x-kubernetes-list-type: set`, so the API "
                        "server refuses the object with `Duplicate value`",
                    )
                )
                continue
            seen[key] = i
        return

    if list_type != "map":
        return

    keys = schema.get("x-kubernetes-list-map-keys") or []
    if not keys:
        return
    seen = {}
    for i, item in enumerate(value):
        if not isinstance(item, dict):
            continue
        # An unset key is a value: two entries that both omit it collide, which
        # is the shape a reading built on "compare the entries" misses, because
        # the entries themselves differ.
        identity = tuple(_hashable(item.get(k)) for k in keys)
        if identity in seen:
            named = ", ".join(
                f"{k}={item.get(k)!r}" for k in keys
            )
            out.append(
                Finding(
                    REFUSED,
                    "list-map",
                    f"{path}[{i}]",
                    f"has the same {' + '.join(keys)} as the entry at index "
                    f"{seen[identity]} ({named}) in a list keyed on "
                    f"{keys}, so the API server refuses the object with "
                    "`Duplicate value`",
                )
            )
            continue
        seen[identity] = i


_JSON_TYPES: dict[str, tuple] = {
    "string": (str,),
    "boolean": (bool,),
    "integer": (int,),
    "number": (int, float),
    "array": (list,),
    "object": (dict,),
}


def _scalar_rules(value, schema: dict, path: str, out: list[Finding]) -> None:
    declared = schema.get("type")
    if declared and not schema.get("x-kubernetes-int-or-string"):
        expected = _JSON_TYPES.get(declared)
        # bool is a subclass of int in Python and is not an integer to the API
        # server, so the two are separated before the check rather than after.
        actual_is_bool = isinstance(value, bool)
        if expected is not None and value is not None:
            wrong = not isinstance(value, expected)
            if declared in ("integer", "number") and actual_is_bool:
                wrong = True
            if declared == "boolean" and not actual_is_bool:
                wrong = True
            if wrong:
                out.append(
                    Finding(
                        REFUSED,
                        "type",
                        path,
                        f"is declared `type: {declared}` and this is "
                        f"{type(value).__name__}, so the API server refuses the "
                        "object with `Invalid value`",
                    )
                )
                return

    enum = schema.get("enum")
    if enum is not None and value not in enum:
        out.append(
            Finding(
                REFUSED,
                "enum",
                path,
                f"is {value!r}, which is outside the declared enum {enum}, so "
                "the API server refuses the object with `Unsupported value`",
            )
        )

    if isinstance(value, str):
        pattern = schema.get("pattern")
        if pattern is not None and re.search(pattern, value) is None:
            out.append(
                Finding(
                    REFUSED,
                    "pattern",
                    path,
                    f"is {value!r}, which does not match the declared pattern "
                    f"{pattern!r}, so the API server refuses the object",
                )
            )
        _bound(len(value), schema, "minLength", "maxLength", path, "characters", out)

    if isinstance(value, list):
        _bound(len(value), schema, "minItems", "maxItems", path, "items", out)

    if isinstance(value, (int, float)) and not isinstance(value, bool):
        minimum, maximum = schema.get("minimum"), schema.get("maximum")
        if minimum is not None:
            exclusive = schema.get("exclusiveMinimum")
            if value < minimum or (exclusive and value == minimum):
                out.append(
                    Finding(
                        REFUSED,
                        "minimum",
                        path,
                        f"is {value}, below the declared minimum {minimum}, so "
                        "the API server refuses the object",
                    )
                )
        if maximum is not None:
            exclusive = schema.get("exclusiveMaximum")
            if value > maximum or (exclusive and value == maximum):
                out.append(
                    Finding(
                        REFUSED,
                        "maximum",
                        path,
                        f"is {value}, above the declared maximum {maximum}, so "
                        "the API server refuses the object",
                    )
                )


def _bound(size, schema, low_key, high_key, path, unit, out) -> None:
    low, high = schema.get(low_key), schema.get(high_key)
    if low is not None and size < low:
        out.append(
            Finding(
                REFUSED,
                low_key,
                path,
                f"carries {size} {unit}, below the declared {low_key} {low}, so "
                "the API server refuses the object",
            )
        )
    if high is not None and size > high:
        out.append(
            Finding(
                REFUSED,
                high_key,
                path,
                f"carries {size} {unit}, above the declared {high_key} {high}, "
                "so the API server refuses the object",
            )
        )


def _hashable(value):
    """A comparable identity for a JSON value, including unhashable shapes."""
    if isinstance(value, dict):
        return tuple(sorted((k, _hashable(v)) for k, v in value.items()))
    if isinstance(value, list):
        return tuple(_hashable(v) for v in value)
    # A bool and the integer it equals are different values to the API server.
    return (type(value).__name__, value)


def schema_declares(schema: dict, path: str) -> bool:
    """Whether a dotted path resolves to a declared property of a schema.

    Callers use this to hold a fixture's own header honest: a header naming a
    path the schema does not declare is a header that can never be matched by
    anything but a typo.
    """
    node = schema
    for segment in path.split("."):
        if not isinstance(node, dict):
            return False
        while "items" in node and "properties" not in node:
            node = node["items"]
        props = node.get("properties") or {}
        if segment not in props:
            return False
        node = props[segment]
    return True


# The rules this module applies. A caller holding a corpus honest asks which of
# these the schemas in front of it make reachable, rather than keeping its own
# list: a rule added here with nothing to exercise it is the gap that made three
# readings of one property disagree in the first place.
RULES = frozenset(
    {
        "required",
        "property",
        "list-map",
        "list-set",
        "type",
        "enum",
        "pattern",
        "minLength",
        "maxLength",
        "minItems",
        "maxItems",
        "minimum",
        "maximum",
        "scope",
    }
)

_KEYWORD_RULES = (
    "required",
    "type",
    "enum",
    "pattern",
    "minLength",
    "maxLength",
    "minItems",
    "maxItems",
    "minimum",
    "maximum",
)


def reachable_rules(crds: dict[str, CRD]) -> set[str]:
    """Which rules these definitions can actually produce a finding for.

    Derived from the schemas rather than declared. A definition that grows its
    first `x-kubernetes-list-type: set` makes that rule reachable, and a corpus
    that does not exercise it becomes incomplete on the same commit — without
    anyone remembering to widen a list.
    """
    found: set[str] = set()
    if any(not crd.namespaced for crd in crds.values()):
        found.add("scope")

    def visit(node) -> None:
        if not isinstance(node, dict):
            return
        for rule in _KEYWORD_RULES:
            if rule in node:
                found.add(rule)
        list_type = node.get("x-kubernetes-list-type")
        if list_type == "map" and node.get("x-kubernetes-list-map-keys"):
            found.add("list-map")
        elif list_type == "set":
            found.add("list-set")
        if node.get("properties"):
            # Pruning needs a described object to have something undescribed
            # beside it, which any node carrying properties permits.
            found.add("property")
            for child in node["properties"].values():
                visit(child)
        if isinstance(node.get("items"), dict):
            visit(node["items"])
        if isinstance(node.get("additionalProperties"), dict):
            visit(node["additionalProperties"])

    for crd in crds.values():
        for schema in crd.schemas.values():
            visit(schema)
    return found & RULES


def cel_rules(crds: dict[str, CRD]) -> int:
    """How many `x-kubernetes-validations` rules these definitions declare.

    This module evaluates none of them. A caller uses the count to require that
    the gap be exercised by a fixture the API server refuses and this admits, so
    the limit is measured on every run rather than asserted in a paragraph that
    nothing checks.
    """
    total = 0

    def visit(node) -> None:
        nonlocal total
        if not isinstance(node, dict):
            return
        total += len(node.get("x-kubernetes-validations") or [])
        for child in (node.get("properties") or {}).values():
            visit(child)
        if isinstance(node.get("items"), dict):
            visit(node["items"])
        if isinstance(node.get("additionalProperties"), dict):
            visit(node["additionalProperties"])

    for crd in crds.values():
        for schema in crd.schemas.values():
            visit(schema)
    return total
