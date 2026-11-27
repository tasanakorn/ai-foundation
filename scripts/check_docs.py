"""Check the docs folder: indexes, statuses and links (docs/workflow.md, CLAUDE.md section 4).

Run: python scripts/check_docs.py [--chars]
Exit 1 on any problem. --chars also reports characters a normal keyboard does not
type (em dash, smart quotes, arrows); that part only reports and never fails.
Standard library only.
"""

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DOCS = ROOT / "docs"
KINDS = {"changes": "change", "specs": "spec", "tasks": "task", "backlogs": "backlog"}
ROW = re.compile(r"^\|\s*\[([a-z]+-\d{4})\]\(([^)]+)\)\s*\|[^|]*\|\s*([a-z]+)\s*\|")
STATUS = re.compile(r"^- Status:\s*([a-z]+)", re.M)
LINK = re.compile(r"\]\(([^)\s]+)\)")


def check_index(folder: str, kind: str) -> list[str]:
    d = DOCS / folder
    errors = []
    docs = {p.name: p for p in d.glob(f"{kind}-*")}
    for name in docs:
        if not re.fullmatch(rf"{kind}-\d{{4}}-[a-z0-9]+(-[a-z0-9]+)*\.[a-z]+", name):
            errors.append(f"{folder}/{name}: name does not match {kind}-<nnnn>-<slug>.<ext>")
    index = d / "INDEX.md"
    if not index.exists():
        return errors + [f"{folder}/INDEX.md: missing"]
    listed = {}
    for line in index.read_text().splitlines():
        m = ROW.match(line)
        if m:
            listed[m.group(2)] = m.group(3)
    for name in sorted(set(docs) - set(listed)):
        errors.append(f"{folder}/{name}: not listed in INDEX.md")
    for name in sorted(set(listed) - set(docs)):
        errors.append(f"{folder}/INDEX.md: lists {name}, which does not exist")
    for name, status in listed.items():
        if name not in docs:
            continue
        m = STATUS.search(docs[name].read_text())
        if m and m.group(1) != status:
            errors.append(f"{folder}/{name}: status {m.group(1)}, but INDEX.md says {status}")
    return errors


def check_links(path: Path) -> list[str]:
    errors = []
    text = re.sub(r"```.*?```", "", path.read_text(), flags=re.S)
    text = re.sub(r"`[^`\n]*`", "", text)
    for target in LINK.findall(text):
        if re.match(r"[a-z]+:", target) or target.startswith("#"):
            continue
        file = target.split("#", 1)[0]
        if file and not (path.parent / file).exists():
            errors.append(f"{path.relative_to(ROOT)}: broken link {target}")
    return errors


def report_chars(paths: list[Path]) -> None:
    for path in paths:
        for n, line in enumerate(path.read_text().splitlines(), 1):
            odd = sorted({c for c in line if ord(c) > 126})
            if odd:
                print(f"note: {path.relative_to(ROOT)}:{n}: {' '.join(odd)}")


def main() -> int:
    errors = []
    for folder, kind in KINDS.items():
        errors += check_index(folder, kind)
    paths = sorted(DOCS.rglob("*.md")) + [ROOT / "CLAUDE.md", ROOT / "README.md", ROOT / "CHANGELOG.md"]
    for path in paths:
        errors += check_links(path)
    if "--chars" in sys.argv:
        report_chars(paths)
    for e in errors:
        print(f"error: {e}")
    print(f"docs check: {len(errors)} problem(s)")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
