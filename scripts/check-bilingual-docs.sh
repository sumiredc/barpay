#!/usr/bin/env bash
# Checks that every tracked Markdown document has its .ja.md translation and
# that both files have the same headings: same count, same level at each
# position. Wording is not compared because it is translated.
# Exits non-zero on any problem. Rules: .claude/skills/bilingual-docs/SKILL.md
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Files that have no translation. Keep in line with "Exceptions" in SKILL.md.
exempt='^(\.claude/|\.github/|mock/)|(^|/)node_modules/|(^|/)(CLAUDE|AGENTS|CHANGELOG)\.md$'

# Headings outside fenced code blocks, one per line: "<level> <heading>".
# No regex intervals such as {1,6}: mawk, the default awk on Ubuntu, mishandles them.
headings() {
  awk '{ f = $0; sub(/^[[:space:]]+/, "", f); h = $0; sub(/^ ? ? ?/, "", h) }
       fence == "" && match(f, /^(```+|~~~+)/) { fence = substr(f, 1, RLENGTH); next }
       fence != "" {
         # Closed only by the same character, at least as long, with nothing after it.
         if (index(f, fence) == 1) { r = f; c = substr(fence, 1, 1); while (substr(r, 1, 1) == c) r = substr(r, 2); if (r ~ /^[[:space:]]*$/) fence = "" }
         next }
       match(h, /^#+([[:space:]]|$)/) { n = h; sub(/[[:space:]].*/, "", n); if (length(n) <= 6) print length(n) " " h }' "$1"
}

status=0
docs=$(git -c core.quotePath=false ls-files --cached -- '*.md' | grep -Ev "$exempt" || true)

while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    *.ja.md)
      grep -Eq "$exempt" <<<"${f%.ja.md}.md" && { echo "unexpected translation of an exempt file: $f"; status=1; continue; }
      grep -Fqx -- "${f%.ja.md}.md" <<<"$docs" || { echo "missing: ${f%.ja.md}.md"; status=1; }
      continue ;;
  esac
  ja="${f%.md}.ja.md"
  if ! grep -Fqx -- "$ja" <<<"$docs"; then
    echo "missing: $ja"; status=1; continue
  fi
  # Captured first so that an awk failure stops the script under set -e.
  en_h=$(headings "$f")
  ja_h=$(headings "$ja")
  # Print the first position where the count or level differs.
  if ! paste -d '\n' <(printf '%s\n' "$en_h") <(printf '%s\n' "$ja_h") | awk -v en="$f" -v ja="$ja" '
      NR % 2 { a = $0; next }
      { n++; if ((a == "") != ($0 == "") || a + 0 != $0 + 0) {
          sub(/^[0-9]+ /, "", a); x = $0; sub(/^[0-9]+ /, "", x)
          printf "headings differ at #%d: %s: %s | %s: %s\n", n, en, (a == "" ? "(none)" : a), ja, (x == "" ? "(none)" : x)
          exit 1 } }'; then
    status=1
  fi
done <<<"$docs"

exit "$status"
