---
name: bilingual-docs
description: BarPay keeps every Markdown document in English with a Japanese translation beside it. Use when creating, editing, renaming, or deleting any .md file in this repository (README, PRD, design docs, docs/, Devpost text or video script drafts), so the English file and its .ja.md translation stay in sync.
---

# bilingual-docs — English source, Japanese translation

Every Markdown document in this repository is written in English, and has a Japanese translation next to it named `<name>.ja.md`.
The hackathon requires English materials and the repository will be public for judging; the Japanese copy is for the author.

## Rules

- **English is the source of truth.** Write or change `<name>.md` first, then bring `<name>.ja.md` in line with it.
- **Change both in the same commit.** A commit that touches one without the other leaves them out of sync.
- **Rename and delete together.** `PRD.md` → `docs/PRD.md` also moves `PRD.ja.md` → `docs/PRD.ja.md`.
- **No Japanese-only documents in the repository.** A draft written in Japanese first is either turned into an English `.md` with its `.ja.md`, or kept outside the repository.
- **Each file points to the other**, right under the title:
  - English: ``` `<name>.ja.md` is the Japanese translation of this file. ```
  - Japanese:

    ```markdown
    > `<name>.md` の日本語訳。内容が食い違う場合は `<name>.md` が正。
    ```

- **Translate, do not rewrite.** Keep the same sections, order, tables, diagrams and links so a reader can move between the two. Translate prose, headings, table cells and diagram labels; keep code, commands, identifiers, file names and API paths as they are.
- **UI strings stay in English.** The UI is English only, so a string the user sees on screen is kept as is, with the Japanese in parentheses: `"Please see staff"（スタッフにお声がけください）`.
- **Links to other documents point to the same language.** In a `.ja.md`, link to the other `.ja.md`. Anchors follow the translated heading using GitHub's rule: lowercase, spaces → `-`, punctuation such as `.` removed, Japanese characters kept. `## 13. 未決事項` → `#13-未決事項`.
- When the Japanese side is edited first (for example, the author fixed wording there), carry the change into the English file and treat the English wording as final.

## Check after editing

List the headings of both files (outside code blocks) and confirm they match one to one, in the same order. Set `f` to the English file:

```sh
f=PRD.md; h() { awk '/^```/{c=!c} !c && /^#/' "$1"; }; paste -d'|' <(h "$f") <(h "${f%.md}.ja.md")
```

The heading check does not catch the pointer lines, UI strings or meaning drift; read the changed passages side by side too.

Then confirm every non-exempt `.md` has its pair:

```sh
git ls-files --cached --others --exclude-standard '*.md' | grep -v '\.ja\.md$' | grep -v -E '^(\.claude/|\.github/|mock/)|(^|/)(CLAUDE|AGENTS|CHANGELOG)\.md$' | while read f; do [ -f "${f%.md}.ja.md" ] || echo "missing: ${f%.md}.ja.md"; done
```

## Exceptions

These files are not translated:

- Files read by agents, not people: `CLAUDE.md`, `AGENTS.md`, and everything under `.claude/` (including this skill).
- Templates under `.github/` (issue and PR templates).
- Everything under `mock/` (the throwaway HTML/CSS/JS prototype; it should not contain `.md` files).
- Vendored or generated Markdown (e.g. under `node_modules/`, a tool-generated `CHANGELOG.md`).
- `LICENSE` and other legal texts.
- `*.ja.md` files themselves — never create `<name>.ja.ja.md`.
