#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/verify_release_artifacts.py —— v0.1 发布物 digest 清单复核器

读取 docs/history/evidence/v0.1-release-artifacts-20260820.md：
  - 文件 artifact：以清单基线 commit 的 git blob 内容重算 sha256；
  - 目录 artifact：以清单基线 commit 的 git tree 对象重算 `git rev-parse`；
  - migrations：与文件 artifact 同样重算 sha256。

发布纪律：任一不匹配即退出非零；发布前必须全绿。
"""
import hashlib
import re
import subprocess
import sys

MANIFEST = "docs/history/evidence/v0.1-release-artifacts-20260820.md"
FILE_RE = re.compile(r"^- `([^`]+)` : `([0-9a-f]{64})`$", re.M)
DIR_RE = re.compile(r"^- `([^`]+)` : `([0-9a-f]{40,64})`$", re.M)
BASE_RE = re.compile(r"^- 基线 commit：`([0-9a-f]{7,40})`", re.M)
SECTION_RE = re.compile(r"^## (.+)$", re.M)


def fail(msg: str) -> None:
    print("[FAIL]", msg)


def git_show(commit: str, path: str) -> bytes:
    out = subprocess.run(
        ["git", "show", f"{commit}:{path}"],
        check=True, capture_output=True)
    return out.stdout


def sha256_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def git_tree(commit: str, path: str) -> str:
    out = subprocess.run(
        ["git", "rev-parse", f"{commit}:{path}"],
        check=True, capture_output=True, text=True)
    return out.stdout.strip()


def main() -> int:
    with open(MANIFEST, encoding="utf-8") as f:
        text = f.read()
    m = BASE_RE.search(text)
    if not m:
        fail("清单缺少基线 commit")
        return 1
    commit = m.group(1)
    print(f"[PASS] 基线 commit: {commit}")

    # split sections
    headings = list(SECTION_RE.finditer(text))
    sections = {}
    for i, sm in enumerate(headings):
        start = sm.end()
        end = headings[i + 1].start() if i + 1 < len(headings) else len(text)
        sections[sm.group(1)] = text[start:end]
    body = sections

    errors = 0
    checks = 0
    # 文件 artifact + 迁移
    for section in ("文件 artifact（sha256）", "迁移文件（sha256）"):
        raw = body.get(section, "")
        entries = FILE_RE.findall(raw)
        if not entries:
            fail(f"清单段落为空: {section}")
            errors += 1
            continue
        for path, want in entries:
            checks += 1
            try:
                got = sha256_bytes(git_show(commit, path))
            except Exception as e:
                fail(f"{path}: 无法从 git 读取 {commit}: {e}")
                errors += 1
                continue
            if got != want:
                fail(f"{path}: sha256 {got} != 清单 {want}")
                errors += 1
            else:
                print(f"[PASS] {path}")
    # 目录 artifact
    raw = body.get("目录 artifact（git tree）", "")
    entries = DIR_RE.findall(raw)
    if not entries:
        fail("清单缺少目录 artifact")
        errors += 1
    for path, want in entries:
        checks += 1
        try:
            got = git_tree(commit, path.rstrip("/"))
        except Exception as e:
            fail(f"{path}: git tree 读取失败 {e}")
            errors += 1
            continue
        if got != want:
            fail(f"{path}: tree {got} != 清单 {want}")
            errors += 1
        else:
            print(f"[PASS] {path}")

    print(f"\n结果: {checks - errors}/{checks} 项匹配，{errors} 项失败")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
