---
name: bilingual-docs
description: BarPay keeps every Markdown document in English with a Japanese translation beside it. Use when creating, editing, renaming, or deleting any .md file in this repository (README, PRD, design docs, docs/), so the English file and its .ja.md translation stay in sync.
---

# bilingual-docs — English source, Japanese translation

Every Markdown document in this repository is written in English, and has a Japanese translation next to it named `<name>.ja.md`.
The repository is public and read in English; the Japanese copy is for the author.

## Rules

- **English is the source of truth.** Write or change `<name>.md` first, then bring `<name>.ja.md` in line with it.
- **Change both in the same commit.** A commit that touches one without the other leaves them out of sync. Exception: when an outside contributor changes only the English file, the maintainer adds the translation in a separate commit on the same branch before merging; the pair must be in sync when it is merged.
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
- **Links to other documents point to the same language.** In a `.ja.md`, link to the other `.ja.md`. Anchors follow the translated heading using GitHub's rule: lowercase, spaces → `-`, punctuation such as `.` removed, Japanese characters kept. `## 12. 未決事項` → `#12-未決事項`.
- When the Japanese side is edited first (for example, the author fixed wording there), carry the change into the English file and treat the English wording as final.

## Check after editing

Run the check; CI runs the same script on every pull request and push to main:

```sh
scripts/check-bilingual-docs.sh
```

It fails when a tracked non-exempt `.md` has no `.ja.md` (or a `.ja.md` has no `.md`), or when a pair's headings outside code blocks differ in count or in level at any position. It does not compare wording, pointer lines or UI strings, so read the changed passages side by side too.

## Exceptions

These files are not translated. The list of exempt paths in `scripts/check-bilingual-docs.sh` matches this one; change both together.

- Files read by agents, not people: `CLAUDE.md` and `AGENTS.md` in any directory, and everything under `.claude/` (including this skill).
- Everything under `.github/` (issue and PR templates).
- Everything under `mock/` (the throwaway HTML/CSS/JS prototype; it should not contain `.md` files).
- Vendored or generated Markdown: anything under `node_modules/`, and `CHANGELOG.md`.
- `LICENSE`, which is not Markdown and so never reaches the script.
- `*.ja.md` files themselves — never create `<name>.ja.ja.md`.
