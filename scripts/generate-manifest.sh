#!/usr/bin/env bash
# Generate a machine-readable release manifest from a GoReleaser dist/
# directory, so a downstream consumer (e.g. a Nix overlay) can pin per-target
# sha256 automatically instead of hand-copying them out of a release page.
#
# Usage: scripts/generate-manifest.sh <dist-dir> <git-ref> <status>
# Must be run from the repository root — artifacts.json's "path" fields are
# relative to it. Writes the manifest JSON to stdout.
set -euo pipefail

dist="${1:?usage: generate-manifest.sh <dist-dir> <git-ref> <status>}"
git_ref="${2:?usage: generate-manifest.sh <dist-dir> <git-ref> <status>}"
status="${3:-ok}"

metadata="$dist/metadata.json"
artifacts_json="$dist/artifacts.json"

version=$(jq -r '.version' "$metadata")
git_sha=$(jq -r '.commit' "$metadata")
built_at=$(jq -r '.date' "$metadata")

artifacts="[]"
while IFS= read -r entry; do
  name=$(jq -r '.name' <<<"$entry")
  path=$(jq -r '.path' <<<"$entry")
  goos=$(jq -r '.goos' <<<"$entry")
  goarch=$(jq -r '.goarch' <<<"$entry")
  format=$(jq -r '.extra.Format // "tar.gz"' <<<"$entry")
  sha256=$(sha256sum "$path" | cut -d' ' -f1)
  size_bytes=$(stat -c%s "$path")
  artifact=$(jq -n \
    --arg name "$name" \
    --arg target "${goos}_${goarch}" \
    --arg sha256 "$sha256" \
    --argjson size_bytes "$size_bytes" \
    --arg archive_format "$format" \
    '{name: $name, target: $target, sha256: $sha256, size_bytes: $size_bytes, archive_format: $archive_format}')
  artifacts=$(jq --argjson a "$artifact" '. + [$a]' <<<"$artifacts")
done < <(jq -c '.[] | select(.type == "Archive")' "$artifacts_json")

jq -n \
  --argjson schema_version 1 \
  --arg tool "portitor" \
  --arg version "$version" \
  --arg git_sha "$git_sha" \
  --arg git_ref "$git_ref" \
  --arg built_at "$built_at" \
  --arg status "$status" \
  --argjson artifacts "$artifacts" \
  '{schema_version: $schema_version, tool: $tool, version: $version, git_sha: $git_sha, git_ref: $git_ref, built_at: $built_at, status: $status, artifacts: $artifacts}'
