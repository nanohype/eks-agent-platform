"""
invocation_cost_publisher
─────────────────────────

CloudWatch Logs subscription handler. Bedrock invocation logs land in the
log group as JSON records (one per invocation). We:

  1. gunzip + base64-decode the CloudWatch payload,
  2. parse each invocation record,
  3. compute estimated USD cost from input/output token counts plus the
     per-model pricing table below,
  4. emit a PutMetricData call to CloudWatch under
     ``namespace=agents/Bedrock`` / ``MetricName=EstimatedInvocationCostUsd``,
     dimensioned by PlatformId (extracted from the invocation's tags or
     headers, falling back to "unknown").

The Budget reconciler reads this metric to estimate in-flight cost
incurred since the most recent CUR partition (which lags by ~24h).

Pricing table is conservative and rounded; the metric is intended for
threshold/alerting decisions, not finance-grade reporting. The CUR-based
view is authoritative for billing.
"""

from __future__ import annotations

import base64
import gzip
import json
import logging
import os
from collections import defaultdict
from typing import Any

import boto3

logger = logging.getLogger()
logger.setLevel(logging.INFO)

cloudwatch = boto3.client("cloudwatch")
NAMESPACE = "agents/Bedrock"
METRIC_NAME = "EstimatedInvocationCostUsd"


# USD per 1K tokens. Rough; rounded conservatively upward so the
# metric trips alerts slightly before the CUR confirms.
# Bedrock-specific identifiers (cross-region inference profile prefixes
# like 'us.' get stripped before lookup).
PRICING: dict[str, dict[str, float]] = {
    # Anthropic
    "anthropic.claude-3-5-sonnet-20241022-v2:0": {"input": 0.003,  "output": 0.015},
    "anthropic.claude-3-5-haiku-20241022-v1:0":  {"input": 0.001,  "output": 0.005},
    "anthropic.claude-3-opus-20240229-v1:0":     {"input": 0.015,  "output": 0.075},
    "anthropic.claude-3-haiku-20240307-v1:0":    {"input": 0.00025, "output": 0.00125},
    # Amazon Nova
    "amazon.nova-pro-v1:0":   {"input": 0.0008,  "output": 0.0032},
    "amazon.nova-lite-v1:0":  {"input": 0.00006, "output": 0.00024},
    "amazon.nova-micro-v1:0": {"input": 0.000035, "output": 0.00014},
    # Meta Llama
    "meta.llama3-1-70b-instruct-v1:0": {"input": 0.00072, "output": 0.00072},
    "meta.llama3-1-8b-instruct-v1:0":  {"input": 0.00022, "output": 0.00022},
}

# Fallback when modelId isn't in the table. Tuned to be roughly mid-range —
# alerts on unknown models will be slightly off but not silent.
FALLBACK_PRICING = {"input": 0.003, "output": 0.015}


def _model_pricing(model_id: str) -> dict[str, float]:
    # Strip cross-region prefixes like 'us.' / 'eu.' that AWS prepends to
    # inference profile IDs.
    bare = model_id.split(".", 1)[1] if "." in model_id and len(model_id.split(".", 1)[0]) <= 3 else model_id
    return PRICING.get(bare, FALLBACK_PRICING)


def _estimate_cost(record: dict[str, Any]) -> tuple[float, str]:
    """
    Returns (cost_usd, platform_id) for one invocation log record.

    Bedrock invocation log shape (abridged):
      {
        "modelId": "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
        "input": {"inputTokenCount": 142, ...},
        "output": {"outputTokenCount": 87, ...},
        "requestId": "...",
        "identity": {"arn": "arn:aws:sts:::assumed-role/<env>-<platform>-tenant/..."}
      }

    The PlatformId is recovered from the assumed-role ARN's session name
    — the tenant role naming contract (see ADR 0004) embeds the platform
    name there. The role-name itself is the source of truth.
    """
    model = record.get("modelId") or record.get("model") or "unknown"
    in_tokens = (record.get("input") or {}).get("inputTokenCount", 0) or 0
    out_tokens = (record.get("output") or {}).get("outputTokenCount", 0) or 0
    pricing = _model_pricing(model)
    cost = (in_tokens / 1000.0) * pricing["input"] + (out_tokens / 1000.0) * pricing["output"]

    platform_id = "unknown"
    identity_arn = (record.get("identity") or {}).get("arn", "")
    # Tenant role pattern: ${env}-${platform}-tenant
    # ARN looks like arn:aws:sts::123:assumed-role/dev-acme-tenant/<session>
    if ":assumed-role/" in identity_arn:
        role_part = identity_arn.split(":assumed-role/", 1)[1].split("/", 1)[0]
        env_prefix = os.environ.get("AGENTS_ENVIRONMENT", "")
        suffix = "-tenant"
        if role_part.endswith(suffix):
            stripped = role_part[: -len(suffix)]
            if env_prefix and stripped.startswith(env_prefix + "-"):
                platform_id = stripped[len(env_prefix) + 1 :]
            else:
                platform_id = stripped
    return cost, platform_id


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


def handler(event: dict[str, Any], _context: Any) -> dict[str, Any]:
    payload = _decode_payload(event["awslogs"]["data"])
    if payload.get("messageType") != "DATA_MESSAGE":
        return {"status": "skipped", "reason": payload.get("messageType")}

    per_platform: dict[str, float] = defaultdict(float)
    parsed = 0
    skipped = 0
    for log_event in payload.get("logEvents", []):
        try:
            record = json.loads(log_event["message"])
        except (KeyError, json.JSONDecodeError):
            skipped += 1
            continue
        cost, platform_id = _estimate_cost(record)
        if cost <= 0:
            skipped += 1
            continue
        per_platform[platform_id] += cost
        parsed += 1

    if per_platform:
        _emit_metrics(dict(per_platform))

    logger.info(
        "processed log batch parsed=%d skipped=%d platforms=%d",
        parsed,
        skipped,
        len(per_platform),
    )
    return {"status": "ok", "parsed": parsed, "skipped": skipped, "platforms": len(per_platform)}
