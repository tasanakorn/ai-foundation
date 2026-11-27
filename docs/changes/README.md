# Change

One record per change: what changes, why, the impact, and how to roll back. Write it **first**, as `draft`, before any code; set it to `applied` when it ships ([workflow](../workflow.md)).

## 1. File names

1. Pattern: `change-<nnnn>-<slug>.<ext>` (for example `change-0001-example-slug.md`).
2. `<nnnn>` is the next free 4-digit number in this folder. Never reuse a number.
3. `<slug>` is lowercase kebab-case, 2–6 words.

## 2. Suggested header

```markdown
# change-<nnnn>: <Title>

- Status: draft | applied | reverted
- Date: YYYY-MM-DD
- Related: <links to specs / tasks>
```

## 3. Index

List every file in [INDEX.md](INDEX.md). Update it whenever you add a file or change a status.
