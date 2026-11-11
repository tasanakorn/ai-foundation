# Specs

Technical and functional specs: the problem, the design, and the decisions.

## 1. File names

1. Pattern: `spec-<nnnn>-<slug>.<ext>` (for example `spec-0001-example-slug.md`).
2. `<nnnn>` is the next free 4-digit number in this folder. Never reuse a number.
3. `<slug>` is lowercase kebab-case, 2–6 words.

## 2. Suggested header

```markdown
# spec-<nnnn>: <Title>

- Status: draft | review | accepted | superseded
- Owner: <name>
- Related: <links to backlog items / tasks>
```

## 3. Index

List every file in [INDEX.md](INDEX.md). Update it whenever you add a file or change a status.
