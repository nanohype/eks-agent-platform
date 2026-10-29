#!/usr/bin/env bash
# Holds a trivy JSON image report to the two things the scan's exit status
# cannot show:
#
#   1. The image trivy scanned is the platform the caller named. trivy picks a
#      platform's manifest out of an index, but scans a local image or a
#      single-platform manifest as whatever it is, so the image config in the
#      report is the only record of which platform was scanned.
#   2. Every suppressed finding was suppressed by the image's acknowledgement
#      file, every entry in that file carries an expired_at date, and every
#      entry suppressed a finding in this scan. With no acknowledgement file,
#      nothing may be suppressed.
#
# The report has to be written with --show-suppressed. Without it a suppressed
# finding is absent from the report rather than listed, so an acknowledged
# entry reads as unused and the second check fails.
#
# .github/actions/image-scan runs --self-test and then this against the report
# of every scan it makes.
#
#   verify-report.sh <report> <image> <platform> [<acknowledged>]
#   verify-report.sh --self-test
#
# Exit status: 0 the report holds, 1 it does not, 2 usage, 3 jq or yq is
# missing, so nothing was checked.
set -euo pipefail

need() {
  command -v "$1" >/dev/null 2>&1 && return 0
  echo "verify-report: $1 is not on PATH; nothing was checked" >&2
  exit 3
}

fail() {
  echo "::error::$*"
  exit 1
}

verify() {
  local report=$1 image=$2 platform=$3 acknowledged=${4:-}
  need jq

  [ -s "$report" ] || fail "no trivy report at $report, so nothing says what was scanned"
  jq -e '.Metadata' "$report" >/dev/null 2>&1 || fail "$report is not a trivy image report"

  local want_os want_arch want_variant os arch variant id
  IFS=/ read -r want_os want_arch want_variant <<< "$platform"
  # A placeholder for every absent field: read collapses adjacent tabs, so an
  # empty field would shift the ones after it.
  read -r os arch variant id < <(jq -r '
    .Metadata | [.ImageConfig.os // "?", .ImageConfig.architecture // "?",
     .ImageConfig.variant // "-", .ImageID // "?"] | @tsv
  ' "$report")
  [ "$variant" = "-" ] && variant=""
  local scanned="$os/$arch${variant:+/$variant}"
  if [ "$os" != "$want_os" ] || [ "$arch" != "$want_arch" ] \
    || { [ -n "$want_variant" ] && [ "$variant" != "$want_variant" ]; }; then
    fail "trivy scanned $image as $scanned, not $platform"
  fi
  echo "$image scanned as $scanned, image config $id"

  # One line per suppressed finding: the vulnerability, then the file that
  # suppressed it.
  local suppressed
  suppressed=$(jq -r '
    .Results[]? | .ExperimentalModifiedFindings[]?
    | [.Finding.VulnerabilityID // "?", .Source // "?"] | @tsv
  ' "$report")

  if [ -z "$acknowledged" ]; then
    [ -z "$suppressed" ] || fail "findings were suppressed for an image that acknowledges none: $(tr '\t\n' ' ;' <<< "$suppressed")"
    echo "no finding suppressed"
    return 0
  fi

  local line foreign=""
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    [ "${line#*$'\t'}" = "$acknowledged" ] || foreign="$foreign ${line%%$'\t'*} (${line#*$'\t'})"
  done <<< "$suppressed"
  [ -z "$foreign" ] || fail "findings were suppressed by a file other than $acknowledged:$foreign"

  need yq
  local entries
  entries=$(yq -r '.vulnerabilities[] | [.id, .expired_at // ""] | @tsv' "$acknowledged")
  [ -n "$entries" ] || fail "$acknowledged names no vulnerabilities; an image that acknowledges nothing passes no file"

  local found entry expires stale=0
  found=$(cut -f1 <<< "$suppressed")
  while IFS=$'\t' read -r entry expires; do
    if [ -z "$expires" ]; then
      echo "::error::$acknowledged acknowledges $entry with no expired_at, which is a permanent waiver"
      stale=1
    elif ! grep -qxF -- "$entry" <<< "$found"; then
      echo "::error::$acknowledged acknowledges $entry, which this scan of $image did not find"
      stale=1
    else
      echo "acknowledged until $expires: $entry"
    fi
  done <<< "$entries"
  return "$stale"
}

# A report carrying only what verify reads: the scanned platform, and the
# suppressed findings given as id:source pairs.
fixture_report() {
  local out=$1 arch=$2 variant=$3
  shift 3
  local findings="[]" pair
  for pair in "$@"; do
    findings=$(jq -c --arg id "${pair%%:*}" --arg src "${pair#*:}" \
      '. + [{Type: "vulnerability", Status: "ignored", Source: $src, Finding: {VulnerabilityID: $id}}]' \
      <<< "$findings")
  done
  jq -n --arg arch "$arch" --arg variant "$variant" --argjson findings "$findings" '{
    ArtifactName: "fixture",
    Metadata: {
      ImageID: "sha256:fixture",
      ImageConfig: ({os: "linux", architecture: $arch} + (if $variant == "" then {} else {variant: $variant} end))
    },
    Results: [{Target: "fixture", ExperimentalModifiedFindings: $findings}]
  }' > "$out"
}

self_test() {
  need jq
  need yq
  local dir self
  self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
  dir=$(mktemp -d)
  # shellcheck disable=SC2064 # expand now: dir is local to this function
  trap "rm -rf '$dir'" EXIT
  cd "$dir"

  fixture_report amd64.json amd64 ""
  fixture_report arm64v8.json arm64 v8
  fixture_report plain.json amd64 "" "CVE-1:.trivyignore"
  fixture_report acked.json amd64 "" "CVE-1:ack.yaml"
  fixture_report acked-and-plain.json amd64 "" "CVE-1:ack.yaml" "CVE-2:.trivyignore"
  printf 'vulnerabilities:\n  - id: CVE-1\n    expired_at: 2099-01-01\n    statement: fixture\n' > ack.yaml
  printf 'vulnerabilities:\n  - id: CVE-1\n    statement: fixture\n' > noexpiry.yaml
  printf 'vulnerabilities:\n  - id: CVE-1\n    expired_at: 2099-01-01\n  - id: CVE-9\n    expired_at: 2099-01-01\n' > stale.yaml
  printf 'vulnerabilities: []\n' > empty.yaml
  printf 'not json\n' > garbage.json
  fixture_report noexpiry.json amd64 "" "CVE-1:noexpiry.yaml"
  fixture_report stale.json amd64 "" "CVE-1:stale.yaml"

  # want-exit|what is checked|output the verdict must contain|arguments
  local cases='0|the platform named, nothing suppressed|no finding suppressed|amd64.json img linux/amd64
1|another platform|scanned img as linux/amd64, not linux/arm64|amd64.json img linux/arm64
0|a variant the caller does not name|scanned as linux/arm64/v8|arm64v8.json img linux/arm64
1|a variant the caller names differently|not linux/arm64/v7|arm64v8.json img linux/arm64/v7
1|a suppression with no acknowledgement file|acknowledges none: CVE-1|plain.json img linux/amd64
0|a suppression the acknowledgement file made|acknowledged until 2099-01-01: CVE-1|acked.json img linux/amd64 ack.yaml
1|a suppression from a second file beside the acknowledgement|other than ack.yaml: CVE-2 (.trivyignore)|acked-and-plain.json img linux/amd64 ack.yaml
1|an acknowledgement with no expiry|CVE-1 with no expired_at|noexpiry.json img linux/amd64 noexpiry.yaml
1|an acknowledgement this scan did not use|acknowledges CVE-9, which this scan|stale.json img linux/amd64 stale.yaml
1|an acknowledgement file naming nothing|names no vulnerabilities|amd64.json img linux/amd64 empty.yaml
1|no report|no trivy report at absent.json|absent.json img linux/amd64
1|a report that is not trivy JSON|is not a trivy image report|garbage.json img linux/amd64
2|too few arguments|usage:|amd64.json img'

  local failures=0 ran=0 want what expect args out rc
  while IFS='|' read -r want what expect args; do
    rc=0
    # shellcheck disable=SC2086 # args is a word list by construction
    out=$("$BASH" "$self" $args 2>&1) || rc=$?
    ran=$((ran + 1))
    if [ "$rc" != "$want" ]; then
      echo "self-test: $what exited $rc, want $want: $out" >&2
      failures=$((failures + 1))
    elif ! grep -qF -- "$expect" <<< "$out"; then
      echo "self-test: $what exited $rc without saying '$expect': $out" >&2
      failures=$((failures + 1))
    fi
  done <<< "$cases"

  if [ "$failures" -ne 0 ]; then
    echo "verify-report self-test: $failures of $ran cases failed" >&2
    return 1
  fi
  echo "verify-report self-test: $ran cases, each with the exit status and the verdict expected"
}

if [ "${1:-}" = "--self-test" ] && [ "$#" -eq 1 ]; then
  self_test
  exit $?
fi
if [ "$#" -lt 3 ] || [ "$#" -gt 4 ]; then
  echo "usage: $0 <report> <image> <platform> [<acknowledged>]" >&2
  echo "       $0 --self-test" >&2
  exit 2
fi
verify "$@"
