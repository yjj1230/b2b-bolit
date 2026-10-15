#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/verify_scene_routes.py —— 17 场景 route-level 追踪证据复核

读取 project/views/scene-route-evidence.yaml，在指定源码范围内静态搜索 probes。
全部命中且 backend_api 标注与边界表一致 → PASS；否则非零退出。
"""
import glob
import os
import sys

import yaml

VIEW = "project/views/scene-route-evidence.yaml"
BOUNDARY = "project/views/demo-boundary.yaml"


def load_source(scope: list[str]) -> str:
    chunks = []
    for d in scope:
        for p in glob.glob(os.path.join(d, "**", "*.go"), recursive=True):
            with open(p, encoding="utf-8") as f:
                chunks.append(f.read())
    return "\n".join(chunks)


def main() -> int:
    try:
        with open(VIEW, encoding="utf-8") as f:
            view = yaml.safe_load(f)
        with open(BOUNDARY, encoding="utf-8") as f:
            boundary = yaml.safe_load(f)
    except Exception as e:
        print(f"[FAIL] 配置读取失败: {e}")
        return 1

    scope = view.get("search_scope", [])
    src = load_source(scope)
    bmap = {s["id"]: s.get("backend_api") for s in boundary.get("scenes", [])}

    errors = 0
    checks = 0
    for sc in view.get("scenes", []):
        sid = sc["id"]
        missing = [p for p in sc.get("probes", []) if p not in src]
        checks += len(sc.get("probes", []))
        if missing:
            print(f"[FAIL] {sid}: 探针未命中 {missing}")
            errors += 1
            continue
        if sc.get("backend_api") != bmap.get(sid):
            print(f"[FAIL] {sid}: scene-route backend_api={sc.get('backend_api')} "
                  f"!= demo-boundary={bmap.get(sid)}")
            errors += 1
            continue
        note = f"（{sc.get('note')}）" if sc.get("note") else ""
        print(f"[PASS] {sid}: {len(sc.get('probes', []))} probes{note}")

    print(f"\n结果: {'全部通过' if errors == 0 else f'{errors} 项失败'} "
          f"（{checks} probes）")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
