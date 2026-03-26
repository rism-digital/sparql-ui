#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$PROJECT_ROOT"

if ! command -v jq >/dev/null 2>&1; then
  echo "Error: jq is required to compile examples." >&2
  exit 1
fi

shopt -s nullglob
files=(examples/*.rq)
if [ ${#files[@]} -eq 0 ]; then
  echo "Error: no .rq files found in examples/." >&2
  exit 1
fi

tmp_jsonl="$(mktemp)"
cleanup() {
  rm -f "$tmp_jsonl"
}
trap cleanup EXIT

while IFS= read -r file; do
  line_no=0
  title=""
  title_line_no=0

  while IFS= read -r line || [ -n "$line" ]; do
    line_no=$((line_no + 1))
    trimmed="${line#"${line%%[![:space:]]*}"}"
    if [ -z "$trimmed" ]; then
      continue
    fi

    if [[ "$trimmed" =~ ^#\ Title:[[:space:]]*(.+)$ ]]; then
      title="${BASH_REMATCH[1]}"
      title_line_no="$line_no"
      break
    fi

    echo "Error: $file first non-empty line must be '# Title: ...'" >&2
    exit 1
  done < "$file"

  if [ -z "$title" ]; then
    echo "Error: missing '# Title: ...' in $file" >&2
    exit 1
  fi

  query="$(awk -v skip="$title_line_no" 'NR == skip { next } { print }' "$file")"
  id="$(basename "$file" .rq)"

  jq -n \
    --arg id "$id" \
    --arg title "$title" \
    --arg query "$query" \
    --arg sourceFile "$file" \
    '{id: $id, title: $title, query: $query, sourceFile: $sourceFile}' >> "$tmp_jsonl"
done < <(printf "%s\n" "${files[@]}" | sort)

jq -s . "$tmp_jsonl" > examples.json
echo "Wrote examples.json with $(jq length examples.json) examples."
