#!/bin/sh
# Smoke-tests one built eval-runner image on one platform, against what the
# eval WorkflowTemplate relies on:
#
#   1. `score` turns testdata/score.results.json into testdata/score.golden.json.
#      contract.test.ts holds the scorer to that same pair, so a mismatch here
#      is the image (the bundle, its entry shim, node) rather than the scoring.
#   2. node, aws, kubectl and jq each run: the WorkflowTemplate's steps exec them.
#   3. npm, npx, corepack, yarn and yarnpkg are not on PATH.
#
# Needs docker and jq on the host. .github/actions/eval-runner-image-check runs
# it on every platform the ci.yaml and release.yaml eval-runner-image jobs
# build; the release runs it against the pushed digest before that digest is
# tagged or signed.
#
#   packages/eval-runner/scripts/smoke-image.sh <image> <platform>
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <image> <platform>    e.g. $0 eval-runner:ci-arm64 linux/arm64" >&2
  exit 2
fi
image=$1
platform=$2

# 3, not 1: a missing tool means nothing was checked, which is not a finding
# about the image.
for tool in docker jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "smoke-image: $tool is not on PATH; nothing was checked" >&2
    exit 3
  fi
done

pkg=$(cd "$(dirname "$0")/.." && pwd)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
# The image runs as uid 1000, which need not be the uid that owns the mount.
chmod 0777 "$out"

echo "── $image ($platform): score reproduces testdata/score.golden.json"
docker run --rm --platform "$platform" \
  -v "$pkg/testdata:/in:ro" -v "$out:/out" \
  "$image" score \
  --results /in/score.results.json --pass-threshold 0.85 \
  --report /out/report.html --junit /out/junit.xml --score-out /out/score.json
for f in score.json report.html junit.xml; do
  if [ ! -s "$out/$f" ]; then
    echo "score wrote no $f" >&2
    exit 1
  fi
done
want=$(jq -cS . "$pkg/testdata/score.golden.json")
got=$(jq -cS . "$out/score.json")
if [ "$got" != "$want" ]; then
  echo "score.json does not match testdata/score.golden.json" >&2
  echo "  want: $want" >&2
  echo "  got:  $got" >&2
  exit 1
fi
echo "   $got"

echo "── $image ($platform): the userland the WorkflowTemplate execs, and no package manager"
docker run --rm --platform "$platform" "$image" /bin/sh -euc '
  node --version
  aws --version
  kubectl version --client
  jq --version
  for pm in npm npx corepack yarn yarnpkg; do
    if command -v "$pm" >/dev/null 2>&1; then
      echo "package manager on PATH: $pm ($(command -v "$pm"))" >&2
      exit 1
    fi
  done
  echo "not on PATH: npm npx corepack yarn yarnpkg"
'
