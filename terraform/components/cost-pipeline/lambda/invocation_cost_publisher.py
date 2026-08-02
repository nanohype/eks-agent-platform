"""
invocation_cost_publisher
─────────────────────────

CloudWatch Logs subscription handler. Bedrock invocation logs land in the
log group as JSON records (one per invocation). We:

  1. gunzip + base64-decode the CloudWatch payload,
  2. parse each invocation record,
  3. compute estimated USD cost from input/output token counts plus the
     per-model pricing table (``pricing_data.PRICING``),
  4. resolve the spending tenant by reading the ``PlatformId`` tag off the IAM
     role in ``identity.arn`` — a lookup, not a derivation, see
     ``_extract_platform`` — falling back to "unknown",
  5. emit a PutMetricData call to CloudWatch under
     ``namespace=agents/Bedrock`` / ``MetricName=EstimatedInvocationCostUsd``,
     dimensioned by PlatformId.

The identity in step 4 is minted by the operator and is cluster-qualified, because
a CloudWatch namespace is account+region global and a CUR covers a whole account,
while two co-located clusters may each host a Platform of the same name. Nothing
here composes or decomposes that value; the tag is read and passed through.

An invocation on a model that isn't in the pricing table is **not** priced
against a borrowed rate — that would silently undercount spend on a new or
mistyped model id. Instead it is published as an explicit ``UnpricedInvocations``
count dimensioned by PlatformId + ModelId, so unpriced traffic is observable
and the pricing table can be extended before the next billing cycle.

Imported (Custom Model Import, open-weight) models are capacity-billed rather
than per-token, so they carry no pricing-table row. They surface as
``UnpricedInvocations`` by default, and are priced at the optional
``IMPORTED_MODEL_ESTIMATE_USD_PER_MTOKENS`` per-token governance estimate when
one is configured, so imported spend can reach the kill-switch signal (see
``IMPORTED_ESTIMATE_USD_PER_M``).

The pricing table is generated from the same JSON source of truth the
TypeScript ``@eks-agent/pricing`` package imports
(``packages/pricing/src/data/bedrock-pricing.json``); a CI drift gate keeps the
two in lockstep. Prices are USD per 1,000,000 tokens.

The Budget reconciler reads the cost metric to estimate in-flight cost
incurred since the most recent CUR partition (which lags by ~24h). The metric
is intended for threshold/alerting decisions, not finance-grade reporting; the
CUR-based view is authoritative for billing.
"""

from __future__ import annotations

import base64
import gzip
import json
import logging
import os
import uuid
from collections import defaultdict
from datetime import datetime, timezone
from typing import Any

import boto3
from botocore.exceptions import ClientError

from pricing_data import PRICING

logger = logging.getLogger()
logger.setLevel(logging.INFO)

cloudwatch = boto3.client("cloudwatch")
s3 = boto3.client("s3")
iam = boto3.client("iam")
NAMESPACE = "agents/Bedrock"
METRIC_NAME = "EstimatedInvocationCostUsd"
TOKENS_IN_METRIC = "TokensIn"
TOKENS_OUT_METRIC = "TokensOut"
UNPRICED_METRIC = "UnpricedInvocations"
UNATTRIBUTED_METRIC = "UnattributedInvocations"

# The tag the operator stamps on every role it mints, carrying the
# cluster-qualified cost identity (operators/internal/controller/budget_reconcile.go,
# platformCostID). This Lambda READS that tag rather than reconstructing the value
# from the role name — see _extract_platform for why that distinction is the whole
# point. Case matters: Cost Explorer treats tag keys as case-sensitive.
PLATFORM_ID_TAG = "PlatformId"

# Estimate export → S3 (Hive-partitioned NDJSON under usage_date=<d>/) feeds
# the Athena `invocation_cost_estimates` table, which the
# `invocation_cost_reconciliation` view SUMs against CUR truth. An empty
# ESTIMATE_BUCKET disables the export (the CloudWatch metric path is
# unaffected) so the handler degrades cleanly if the env wiring is absent.
ESTIMATE_BUCKET = os.environ.get("ESTIMATE_BUCKET", "")
ESTIMATE_PREFIX = os.environ.get("ESTIMATE_PREFIX", "estimates")
ESTIMATE_KMS_KEY_ID = os.environ.get("ESTIMATE_KMS_KEY_ID", "")

# Custom Model Import (open-weight) models are capacity-billed (per active model
# copy per minute, i.e. CMUs), not per-token, so no per-token rate is derivable
# and they carry no pricing-table row. Imported invocations are always surfaced —
# as UnpricedInvocations when no estimate is configured, so they are never a
# silent zero — and when IMPORTED_MODEL_ESTIMATE_USD_PER_MTOKENS is set they are
# priced at that per-token GOVERNANCE estimate (applied to input+output tokens),
# which brings imported spend into the EstimatedInvocationCostUsd signal the
# kill-switch reads and splits it across the tenants sharing the model by token
# share. It is a threshold knob, not finance-grade; CUR stays authoritative for
# the actual CMU bill.
IMPORTED_ESTIMATE_USD_PER_M = float(
    os.environ.get("IMPORTED_MODEL_ESTIMATE_USD_PER_MTOKENS", "0") or 0
)


def _bare_model(model_id: str) -> str:
    """
    Strip a Bedrock cross-region inference-profile prefix (``us.``, ``eu.``,
    ``jp.``, ``ap.``, ``apac.``, ``global.`` …) from a model id, leaving the
    bare ``<provider>.<model>`` id used as the pricing-table key.

    Removes exactly one leading lowercase-alpha segment, and only when a
    ``<provider>.<model>`` id remains (the remainder still contains a dot), so
    a bare provider id like ``anthropic.claude-3-opus-20240229-v1:0`` is left
    untouched. Pattern-based — no hardcoded prefix list — so future regional
    shorts of any length resolve automatically.
    """
    head, sep, tail = model_id.partition(".")
    if sep and head.isalpha() and head.islower() and "." in tail:
        return tail
    return model_id


def _price_for(model_id: str) -> dict[str, float] | None:
    """Return the price row for a model id, or None when it isn't priced."""
    return PRICING.get(_bare_model(model_id))


def _is_imported(model_id: str) -> bool:
    """True for a Bedrock Custom Model Import model, keyed off its ARN resource type."""
    return ":imported-model/" in model_id


def _imported_key(model_id: str) -> str:
    """Compact metric/estimate id for an imported model: imported/<model-id>."""
    return "imported/" + model_id.rsplit("imported-model/", 1)[-1]


# PlatformId lookups, memoized for the life of the execution environment. A log
# batch is overwhelmingly the same few roles, and IAM's endpoint is global and
# throttled, so an uncached lookup per record would be the bottleneck. Only
# definitive answers are cached: a transient IAM failure must not pin a role to
# "unknown" for the rest of the container's life.
_ROLE_PLATFORM_CACHE: dict[str, str] = {}
_ROLE_CACHE_MAX = 512

# IAM errors that are a settled answer rather than a bad moment. These ARE cached:
# the grant's scope and a role's existence do not change under a running container,
# and every principal in the account appears in this log group — not just tenants —
# so the unreadable ones are the common case, not the exception.
_TERMINAL_LOOKUP_ERRORS = frozenset({"AccessDenied", "AccessDeniedException", "NoSuchEntity"})

# Roles seen in this batch that yielded no PlatformId, so the gap is published
# rather than absorbed into the "unknown" bucket silently. Reset per invocation.
_UNTAGGED_ROLES: dict[str, int] = defaultdict(int)

# Roles whose lookup failed TRANSIENTLY in this batch. Two scopes are in play and
# conflating them is what makes this dangerous:
#
#   across invocations — a fresh look is right, so these are never written to
#     _ROLE_PLATFORM_CACHE. Pinning a real tenant to "unknown" for the life of a
#     warm container would turn a blip into hours of unattributed spend.
#   within one batch — a role that just threw will throw again, and re-asking is
#     pure amplification.
#
# The amplification is not one extra request. botocore's default retry mode is
# legacy: up to 5 attempts with `rand() * 2^(n-1)` backoff, which is several
# seconds of sleeping per sustained-throttled lookup, and this function's timeout
# is 30s. A handful of such records exhausts it — and because every _emit_* call
# happens AFTER the record loop, a timeout mid-loop publishes nothing at all: the
# batch's cost metric, token metrics and estimate export are all lost, CloudWatch
# Logs re-invokes, and the retry re-enters the same throttle. A degraded record
# becomes a silent zero for the whole batch, which is the failure this Lambda is
# supposed to have stopped having.
_BATCH_FAILED_ROLES: set[str] = set()


def _platform_id_for_role(role_name: str) -> str:
    """The PlatformId tag on an IAM role, or "unknown"."""
    cached = _ROLE_PLATFORM_CACHE.get(role_name)
    if cached is not None:
        if cached == "unknown":
            _UNTAGGED_ROLES[role_name] += 1
        return cached
    if role_name in _BATCH_FAILED_ROLES:
        # Already failed transiently in this batch; do not pay the retry storm again.
        _UNTAGGED_ROLES[role_name] += 1
        return "unknown"

    try:
        # Paginated, and NOT because a role can carry more tags than a page holds —
        # IAM caps roles at 50 tags and defaults MaxItems to 100. AWS documents that
        # "IAM might return fewer results, even when there are more results
        # available", with IsTruncated set, and recommends checking it after every
        # call. So a single-page read is not merely tight, it is wrong whenever the
        # service decides to split — and the failure is this Lambda's whole defect
        # class again: PlatformId on page two reads as absent, the invocation
        # attributes to "unknown", and the number is quietly low.
        tags = [
            t
            for page in iam.get_paginator("list_role_tags").paginate(RoleName=role_name)
            for t in page["Tags"]
        ]
    except ClientError as e:
        code = e.response.get("Error", {}).get("Code", "")
        if code in _TERMINAL_LOOKUP_ERRORS:
            # A settled "no". AccessDenied means the role is outside the grant's IAM
            # path — every principal in the account shows up in this log group, not
            # just tenants, and none of the others are readable by design.
            # NoSuchEntity means the role is gone. Neither answer changes while this
            # container lives, and NOT caching them would mean one IAM call per log
            # RECORD for every non-tenant Bedrock caller in the account, against a
            # global throttled endpoint, in the hot path of an account-wide firehose.
            logger.warning("cannot read tags for role %s (%s); attributing to 'unknown'", role_name, code)
            _UNTAGGED_ROLES[role_name] += 1
            if len(_ROLE_PLATFORM_CACHE) < _ROLE_CACHE_MAX:
                _ROLE_PLATFORM_CACHE[role_name] = "unknown"
            return "unknown"
        # Transient (throttle, timeout, 5xx). NOT cached — a retry gets a fresh look,
        # because pinning a real tenant to "unknown" for the life of a warm container
        # would turn a blip into hours of spend attributed to nobody.
        logger.warning("could not read tags for role %s (%s); attributing to 'unknown'", role_name, code or "unknown error")
        _UNTAGGED_ROLES[role_name] += 1
        _BATCH_FAILED_ROLES.add(role_name)
        return "unknown"
    except Exception:
        logger.warning("could not read tags for role %s; attributing to 'unknown'", role_name, exc_info=True)
        _UNTAGGED_ROLES[role_name] += 1
        _BATCH_FAILED_ROLES.add(role_name)
        return "unknown"

    platform_id = next((t["Value"] for t in tags if t["Key"] == PLATFORM_ID_TAG), "")
    if not platform_id:
        # A definitive absence: the role exists and carries no PlatformId. Worth
        # caching, and worth publishing — it means something is invoking Bedrock
        # under a role the operator did not mint, or minted before the tag existed.
        logger.warning("role %s carries no %s tag; attributing to 'unknown'", role_name, PLATFORM_ID_TAG)
        platform_id = "unknown"
        _UNTAGGED_ROLES[role_name] += 1

    if len(_ROLE_PLATFORM_CACHE) < _ROLE_CACHE_MAX:
        _ROLE_PLATFORM_CACHE[role_name] = platform_id
    return platform_id


def _role_name(identity_arn: str) -> str:
    """The role name out of an assumed-role ARN, or "" when it isn't one.

    ``arn:aws:sts::<account>:assumed-role/<role-name>/<session-name>``
    """
    if ":assumed-role/" not in identity_arn:
        return ""
    return identity_arn.split(":assumed-role/", 1)[1].split("/", 1)[0]


def _extract_platform(identity_arn: str) -> str:
    """Read the PlatformId the operator stamped on the invoking role.

    This is a LOOKUP, deliberately, and not a derivation. The value is minted by
    the operator (``platformCostID``) and written as a tag on every role it
    creates, so reading the tag is the only way to get it that cannot disagree
    with the writer.

    The previous version reconstructed it by taking apart the role name, and it
    was wrong for the entire life of the code: it stripped an environment token
    (``staging``) off a role the operator names with the CLUSTER (
    ``staging-platform-acme-tenant``), yielding ``platform-acme`` where every
    reader queried ``staging-platform-acme``. The metric was published to a
    dimension nobody read and the in-flight cost signal was a permanent zero,
    with nothing red anywhere. It also returned "unknown" for the whole
    ``-session`` role family, because that suffix is not ``-tenant``.

    Both failures share one cause — a name assembled in Go, in another repo
    layer, taken apart here by string surgery, with no check that could observe
    the disagreement. Reading the tag deletes the contract instead of restating
    it: role families need no enumeration, the cluster prefix needs no parsing,
    and a rename on the writer's side cannot silently change what this returns.

    Returns "unknown" when the identity is not an assumed role, when the role
    carries no PlatformId tag, or when IAM cannot be reached. "unknown" is a
    real, published dimension — spend attributable to nobody is visible rather
    than dropped — and ``_UNTAGGED_ROLES`` records which roles caused it.
    """
    role = _role_name(identity_arn)
    if not role:
        return "unknown"
    return _platform_id_for_role(role)


def _estimate_cost(record: dict[str, Any]) -> tuple[float, str, str, int, int, bool]:
    """
    Returns (cost_usd, platform_id, model_id, in_tokens, out_tokens, priced)
    for one invocation log record.

    Bedrock invocation log shape (abridged):
      {
        "modelId": "us.anthropic.claude-sonnet-4-6",
        "input": {"inputTokenCount": 142, ...},
        "output": {"outputTokenCount": 87, ...},
        "requestId": "...",
        "identity": {"arn": "arn:aws:sts:::assumed-role/<cluster>-<platform>-tenant/..."}
      }

    ``priced`` is False when the model id has no pricing-table entry; the cost
    is then an unmetered 0 rather than a borrowed rate, and the caller surfaces
    it as an UnpricedInvocations count.
    """
    model = record.get("modelId") or record.get("model") or "unknown"
    in_tokens = (record.get("input") or {}).get("inputTokenCount", 0) or 0
    out_tokens = (record.get("output") or {}).get("outputTokenCount", 0) or 0
    platform_id = _extract_platform((record.get("identity") or {}).get("arn", ""))

    # Imported (Custom Model Import) models carry no pricing-table row (capacity-
    # billed, not per-token). Price them at the configured per-token governance
    # estimate when one is set, so imported spend is visible to the kill-switch;
    # otherwise leave them unpriced, which the caller surfaces as an
    # UnpricedInvocations count rather than a silent zero. See IMPORTED_ESTIMATE_USD_PER_M.
    if _is_imported(model):
        priced = IMPORTED_ESTIMATE_USD_PER_M > 0
        cost = (
            ((in_tokens + out_tokens) / 1_000_000.0) * IMPORTED_ESTIMATE_USD_PER_M
            if priced
            else 0.0
        )
        return cost, platform_id, _imported_key(model), int(in_tokens), int(out_tokens), priced

    bare = _bare_model(model)
    price = PRICING.get(bare)
    priced = price is not None
    cost = 0.0
    if price is not None:
        cost = (in_tokens / 1_000_000.0) * price["input"] + (
            out_tokens / 1_000_000.0
        ) * price["output"]
    return cost, platform_id, bare, int(in_tokens), int(out_tokens), priced


def _decode_payload(awslogs_data: str) -> dict[str, Any]:
    """
    Decode the CloudWatch Logs subscription envelope.

    Returns an empty 'CONTROL_MESSAGE'-shaped dict when the payload is
    corrupt rather than raising — a single bad envelope from CloudWatch
    (which AWS can produce during service events) should not poison the
    whole subscription with infinite retries until the retention window
    expires. The handler treats the empty messageType as 'skipped'.
    """
    try:
        raw = base64.b64decode(awslogs_data)
        unz = gzip.decompress(raw)
        return json.loads(unz)
    except (ValueError, OSError, json.JSONDecodeError) as exc:
        logger.warning("decode failed; treating as control message: %s", exc)
        return {"messageType": "CONTROL_MESSAGE", "logEvents": []}


def _emit_metrics(per_platform: dict[str, float]) -> None:
    # PutMetricData caps at 20 metric data values per call.
    items = list(per_platform.items())
    while items:
        chunk = items[:20]
        items = items[20:]
        cloudwatch.put_metric_data(
            Namespace=NAMESPACE,
            MetricData=[
                {
                    "MetricName": METRIC_NAME,
                    "Dimensions": [{"Name": "PlatformId", "Value": pid}],
                    "Unit": "None",
                    "Value": round(cost, 6),
                }
                for pid, cost in chunk
            ],
        )


def _emit_token_metrics(aggregates: dict[tuple[str, str], dict[str, float]]) -> None:
    # TokensIn/TokensOut per (PlatformId, ModelId), same agents/Bedrock
    # namespace. Distinct dimensions from EstimatedInvocationCostUsd (which the
    # Budget reconciler reads by PlatformId only), so this is purely additive.
    data: list[dict[str, Any]] = []
    for (platform_id, model_id), agg in aggregates.items():
        dims = [
            {"Name": "PlatformId", "Value": platform_id},
            {"Name": "ModelId", "Value": model_id},
        ]
        data.append({"MetricName": TOKENS_IN_METRIC, "Dimensions": dims, "Unit": "Count", "Value": float(agg["in"])})
        data.append({"MetricName": TOKENS_OUT_METRIC, "Dimensions": dims, "Unit": "Count", "Value": float(agg["out"])})
    while data:
        chunk = data[:20]
        data = data[20:]
        cloudwatch.put_metric_data(Namespace=NAMESPACE, MetricData=chunk)


def _emit_unpriced(counts: dict[tuple[str, str], int]) -> None:
    # One UnpricedInvocations count per (PlatformId, ModelId) for models with no
    # pricing entry. Surfaces unpriced traffic as an observable miss instead of
    # letting a borrowed price silently undercount spend. Alarm on this metric
    # to catch a new or mistyped model id before it accrues unmetered cost.
    data: list[dict[str, Any]] = [
        {
            "MetricName": UNPRICED_METRIC,
            "Dimensions": [
                {"Name": "PlatformId", "Value": platform_id},
                {"Name": "ModelId", "Value": model_id},
            ],
            "Unit": "Count",
            "Value": float(count),
        }
        for (platform_id, model_id), count in counts.items()
    ]
    while data:
        chunk = data[:20]
        data = data[20:]
        cloudwatch.put_metric_data(Namespace=NAMESPACE, MetricData=chunk)


def _emit_unattributed(counts: dict[str, int]) -> None:
    # One UnattributedInvocations count per role whose PlatformId could not be
    # read. That spend is still published, under PlatformId="unknown", but it
    # reaches no Platform's budget — so it has to be visible as its own signal
    # rather than as a quiet shortfall in somebody's number. RoleName is the
    # dimension because it is the only thing that says WHICH identity to go fix,
    # and it is bounded by the number of roles minted in the account.
    #
    # Alarm on this: a non-zero value means either something is invoking Bedrock
    # under a role the operator did not mint, or IAM could not be read, and both
    # make every budget in the account read low.
    data: list[dict[str, Any]] = [
        {
            "MetricName": UNATTRIBUTED_METRIC,
            "Dimensions": [{"Name": "RoleName", "Value": role}],
            "Unit": "Count",
            "Value": float(count),
        }
        for role, count in counts.items()
    ]
    while data:
        chunk = data[:20]
        data = data[20:]
        cloudwatch.put_metric_data(Namespace=NAMESPACE, MetricData=chunk)


def _write_estimates(aggregates: dict[tuple[str, str], dict[str, float]], usage_date: str) -> None:
    """
    Write one NDJSON object per log batch to the Hive-partitioned estimate
    prefix (s3://$ESTIMATE_BUCKET/$ESTIMATE_PREFIX/usage_date=<d>/<uuid>.json),
    one record per (platform_id, model_id). The Athena table partitions on
    usage_date only (date projection) and treats platform_id as a data column,
    so the reconciliation view can SUM across all platforms without a
    per-partition predicate.

    A failed put is logged and swallowed — an S3 hiccup must never poison the
    CloudWatch metric path or trigger log-subscription retries.
    """
    if not ESTIMATE_BUCKET or not aggregates:
        return
    records = [
        {
            "platform_id": platform_id,
            "model_id": model_id,
            "estimate_usd": round(agg["cost"], 6),
            "input_tokens": int(agg["in"]),
            "output_tokens": int(agg["out"]),
            "invocation_count": int(agg["count"]),
        }
        for (platform_id, model_id), agg in aggregates.items()
    ]
    body = "\n".join(json.dumps(r) for r in records).encode("utf-8")
    key = f"{ESTIMATE_PREFIX}/usage_date={usage_date}/{uuid.uuid4().hex}.json"
    put_kwargs: dict[str, Any] = {
        "Bucket": ESTIMATE_BUCKET,
        "Key": key,
        "Body": body,
        "ContentType": "application/x-ndjson",
    }
    if ESTIMATE_KMS_KEY_ID:
        put_kwargs["ServerSideEncryption"] = "aws:kms"
        put_kwargs["SSEKMSKeyId"] = ESTIMATE_KMS_KEY_ID
    try:
        s3.put_object(**put_kwargs)
    except Exception as exc:  # telemetry export must never block the metric path
        logger.warning("estimate export failed: %s", exc)


def handler(event: dict[str, Any], _context: Any) -> dict[str, Any]:
    payload = _decode_payload(event["awslogs"]["data"])
    if payload.get("messageType") != "DATA_MESSAGE":
        return {"status": "skipped", "reason": payload.get("messageType")}

    # Attribution gaps are counted per batch, not per container — a role that
    # went untagged once should not keep re-reporting on every later invocation
    # the warm container serves.
    _UNTAGGED_ROLES.clear()
    _BATCH_FAILED_ROLES.clear()

    per_platform: dict[str, float] = defaultdict(float)
    aggregates: dict[tuple[str, str], dict[str, float]] = {}
    unpriced: dict[tuple[str, str], int] = defaultdict(int)
    parsed = 0
    skipped = 0
    for log_event in payload.get("logEvents", []):
        try:
            record = json.loads(log_event["message"])
        except (KeyError, json.JSONDecodeError):
            skipped += 1
            continue
        cost, platform_id, model_id, in_tokens, out_tokens, priced = _estimate_cost(record)
        if not priced:
            unpriced[(platform_id, model_id)] += 1
            parsed += 1
            continue
        if cost <= 0:
            skipped += 1
            continue
        per_platform[platform_id] += cost
        agg = aggregates.setdefault((platform_id, model_id), {"cost": 0.0, "in": 0.0, "out": 0.0, "count": 0.0})
        agg["cost"] += cost
        agg["in"] += in_tokens
        agg["out"] += out_tokens
        agg["count"] += 1
        parsed += 1

    if per_platform:
        _emit_metrics(dict(per_platform))
        _emit_token_metrics(aggregates)
        _write_estimates(aggregates, datetime.now(timezone.utc).strftime("%Y-%m-%d"))
    if unpriced:
        _emit_unpriced(dict(unpriced))
    if _UNTAGGED_ROLES:
        _emit_unattributed(dict(_UNTAGGED_ROLES))

    logger.info(
        "processed log batch parsed=%d skipped=%d unpriced=%d unattributed=%d platforms=%d",
        parsed,
        skipped,
        sum(unpriced.values()),
        sum(_UNTAGGED_ROLES.values()),
        len(per_platform),
    )
    return {
        "status": "ok",
        "parsed": parsed,
        "skipped": skipped,
        "unpriced": sum(unpriced.values()),
        "unattributed": sum(_UNTAGGED_ROLES.values()),
        "platforms": len(per_platform),
    }
