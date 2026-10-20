#!/usr/bin/env python3
"""Fail when any authored `skill:` node has no resolvable body.

Mirrors the loader: sdk/deploykit/skills_collect.go resolves a body at
filepath.Join(GetSourceDir(), ".agents/skills", <name>, "SKILL.md"), where GetSourceDir()
is the directory of the candy whose manifest declares the node. Frontmatter splitting
mirrors sdk/candywalk/skillfile.go splitFrontmatter.

Composition-independent on purpose: CollectSkills only reads bodies for skills whose owner
is in a composed box chain, so a repo that authors no box has no gate at all
(opencharly/opencharly#530).
"""
import sys, pathlib, yaml

def walk(node, path, out):
    if isinstance(node, dict):
        for k, v in node.items():
            if k == "skill" and isinstance(v, dict) and "name" in v:
                out.append((path, v["name"], "content" in v))
            else:
                walk(v, path, out)
    elif isinstance(node, list):
        for v in node:
            walk(v, path, out)

def main(root):
    root = pathlib.Path(root)
    nodes, bad = [], []
    for m in sorted(root.glob("charly.yml")) + sorted(root.glob("candy/*/charly.yml")):
        doc = yaml.safe_load(m.read_text()) or {}
        walk(doc, m.parent, nodes)
    for base, name, inline in nodes:
        f = base / ".agents" / "skills" / name / "SKILL.md"
        if not f.is_file():
            bad.append(f"{name}: no body at {f.relative_to(root)}" + (" (inline content: also absent)" if not inline else ""))
            continue
        txt = f.read_text()
        if not txt.startswith("---\n"):
            bad.append(f"{name}: {f.relative_to(root)} has no frontmatter")
            continue
        rest = txt[4:]; end = rest.find("\n---\n")
        if end < 0:
            bad.append(f"{name}: {f.relative_to(root)} has unterminated frontmatter")
            continue
        fm = yaml.safe_load(rest[:end + 1]) or {}
        if fm.get("name") != name:
            bad.append(f"{name}: frontmatter name={fm.get('name')!r} != directory {name!r}")
    print(f"checked {len(nodes)} skill node(s)")
    for b in bad:
        print("FAIL", b)
    return 1 if bad else 0

if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "."))
