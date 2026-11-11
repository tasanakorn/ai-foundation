# Tasks

Concrete, scoped units of work with clear done criteria.

## 1. File names

1. Pattern: `task-<nnnn>-<slug>.<ext>` (for example `task-0001-example-slug.md`).
2. `<nnnn>` is the next free 4-digit number in this folder. Never reuse a number.
3. `<slug>` is lowercase kebab-case, 2–6 words.

## 2. Suggested header

```markdown
# task-<nnnn>: <Title>

- Status: todo | doing | done | dropped
- Spec: <link to spec, if any>
- Done when: <acceptance criteria>
```

## 3. Index

List every file in [INDEX.md](INDEX.md). Update it whenever you add a file or change a status.
