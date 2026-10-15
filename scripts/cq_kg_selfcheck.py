#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/cq_kg_selfcheck.py —— CQ / 发布图谱 / 任务列表 交叉一致性自检

依据（D-012 / D-013 / 版本规划.html）：
  - CQ 的 release 标签必须与任务列表 release 兼容：
    v0.1 CQ 只能引用 release=v0.1 或 持续 的任务。
  - CQ kg 引用必须存在于 v0.1发布图谱.html 的 NODES。
  - CQ / 任务列表的页面行必须与内嵌 JSON 一致。
  - 图谱不得再出现旧口径词（全量预演/全量重放/角色迁入/P1 待裁决）。

退出码 0 = 一致；非 0 = 存在漂移。本脚本只做机械判定，不做语义裁决。
"""
import collections
import glob
import json
import re
import sys

import yaml

CQ = "v0.1_CQ.html"
KG = "v0.1发布图谱.html"
TASKS = "v0.1发布过程任务列表.html"

CQ_JSON_RE = re.compile(r'<script type="application/json"[^>]*>(.*?)</script>', re.S)
TASK_JSON_RE = re.compile(
    r'<script type="application/json" id="v01-release-task-registry">(.*?)</script>', re.S)
NODES_RE = re.compile(r'const NODES\s*=\s*')


def fail(msg: str) -> None:
    print("[FAIL]", msg)


def main() -> int:
    errors = 0

    # 1. load sources
    try:
        kg_text = open(KG, encoding="utf-8").read()
        m = NODES_RE.search(kg_text)
        nodes = json.loads(kg_text[m.end():kg_text.find("];", m.end()) + 1])
    except Exception as e:
        print(f"[FAIL] 图谱 NODES 解析失败: {e}")
        return 1
    node_ids = {n["id"] for n in nodes}
    print(f"[PASS] 图谱 NODES 解析：{len(nodes)} 节点")

    try:
        cq_text = open(CQ, encoding="utf-8").read()
        cq = json.loads(CQ_JSON_RE.search(cq_text).group(1))
        cqs = cq["cqs"]
    except Exception as e:
        print(f"[FAIL] CQ JSON 解析失败: {e}")
        return 1
    print(f"[PASS] CQ JSON 解析：{len(cqs)} 条")

    try:
        task_text = open(TASKS, encoding="utf-8").read()
        tasks = json.loads(TASK_JSON_RE.search(task_text).group(1))["tasks"]
    except Exception as e:
        print(f"[FAIL] 任务 JSON 解析失败: {e}")
        return 1
    task_by_id = {t["id"]: t for t in tasks}
    print(f"[PASS] 任务 JSON 解析：{len(tasks)} 条")

    # 2. all tasks have release
    no_rel = [t["id"] for t in tasks if "release" not in t]
    if no_rel:
        fail(f"任务缺 release 字段: {no_rel}")
        errors += 1
    else:
        print("[PASS] 全部任务含 release 字段")

    # 3. CQ visible rows == JSON
    rows = re.findall(
        r'<tr class="cq-row" data-id="([^"]+)" data-gid="([^"]+)" '
        r'data-status="([^"]+)" data-release="([^"]+)">', cq_text)
    row_map = {r[0]: r for r in rows}
    mism = []
    for c in cqs:
        r = row_map.get(c["id"])
        if not r or r[2] != c["status"] or r[3] != c["release"]:
            mism.append(c["id"])
    if mism:
        fail(f"CQ 页面行与 JSON 不一致: {mism}")
        errors += 1
    else:
        print(f"[PASS] CQ 页面 {len(rows)} 行与 JSON 一致")

    # 4. task visible rows == JSON
    trows = re.findall(
        r'<tr class="task-row" data-task-id="([^"]+)" data-release="([^"]+)" '
        r'data-lane="([^"]+)" data-action="([^"]+)" data-status="([^"]+)" '
        r'data-owner="([^"]+)">', task_text)
    trow_map = {r[0]: r for r in trows}
    tmism = []
    for t in tasks:
        r = trow_map.get(t["id"])
        if not r or r[1] != t["release"] or r[4] != t["status"]:
            tmism.append(t["id"])
    if tmism:
        fail(f"任务页面行与 JSON 不一致: {tmism}")
        errors += 1
    else:
        print(f"[PASS] 任务页面 {len(trows)} 行与 JSON 一致")

    # 5. CQ kg refs exist in graph
    bad_kg = sorted({k for c in cqs for k in c.get("kg", []) if k not in node_ids})
    if bad_kg:
        fail(f"CQ 引用不存在的图谱节点: {bad_kg}")
        errors += 1
    else:
        print("[PASS] CQ kg 引用全部存在")

    # 6. CQ task refs exist in task list
    bad_task = sorted({t for c in cqs for t in (c.get("task") or "").split("/")
                       if t and t != "—" and t not in task_by_id})
    if bad_task:
        fail(f"CQ 引用不存在的任务: {bad_task}")
        errors += 1
    else:
        print("[PASS] CQ task 引用全部存在")

    # 7. task kg refs exist in graph
    bad_tkg = sorted({k for t in tasks for k in t.get("kg", []) if k not in node_ids})
    if bad_tkg:
        fail(f"任务引用不存在的图谱节点: {bad_tkg}")
        errors += 1
    else:
        print("[PASS] 任务 kg 引用全部存在")

    # 8. v0.1 CQ only depends on v0.1 or 持续 tasks
    v01_mism = []
    for c in cqs:
        if c["release"] != "v0.1":
            continue
        for t in (c.get("task") or "").split("/"):
            t = t.strip()
            if t and t != "—" and t in task_by_id \
                    and task_by_id[t]["release"] not in ("v0.1", "持续"):
                v01_mism.append((c["id"], t, task_by_id[t]["release"]))
    if v01_mism:
        fail(f"v0.1 CQ 引用后置版本任务: {v01_mism}")
        errors += 1
    else:
        print("[PASS] v0.1 CQ 未引用 v0.2/v0.3 任务")

    # 9. graph stale wording
    stale = []
    for n in nodes:
        blob = n.get("text", "") + n.get("verify", "")
        for pat in ("全量预演", "全量重放", "角色迁入", "P1 待裁决"):
            if pat in blob:
                stale.append((n["id"], pat))
    if stale:
        fail(f"图谱存在旧口径措辞: {stale}")
        errors += 1
    else:
        print("[PASS] 图谱无旧口径措辞")

    # 10. CQ group counts == group-head labels
    heads = re.findall(
        r'<tr class="group-head" data-gid="([^"]+)">.*?（(\d+) 条）', cq_text, re.S)
    cnt = collections.Counter(c["gid"] for c in cqs)
    gmism = [(g, n, cnt.get(g)) for g, n in heads if cnt.get(g) != int(n)]
    if gmism:
        fail(f"CQ 分组计数不一致: {gmism}")
        errors += 1
    else:
        print(f"[PASS] CQ {len(heads)} 组计数与 JSON 一致")

    # 11. demo-boundary scenes -> claims refs all exist
    try:
        with open("project/views/demo-boundary.yaml", encoding="utf-8") as f:
            boundary = yaml.safe_load(f)
        claim_ids = set()
        for cf in glob.glob("project/claims/*.yaml"):
            with open(cf, encoding="utf-8") as f:
                doc = yaml.safe_load(f) or {}
            for c in doc.get("claims", []):
                claim_ids.add(c.get("claim_id"))
        missing_claims = sorted({cid for sc in boundary.get("scenes", [])
                                 for cid in sc.get("claims", []) if cid not in claim_ids})
        if missing_claims:
            fail(f"demo-boundary 引用不存在的 claim: {missing_claims}")
            errors += 1
        else:
            print(f"[PASS] demo-boundary 场景 claim 引用全部存在（{len(claim_ids)} claims）")
        yes_count = sum(1 for sc in boundary.get("scenes", []) if sc.get("backend_api") == "yes")
        if yes_count != 16:
            fail(f"demo-boundary backend_api=yes 计数 {yes_count} != 16")
            errors += 1
        else:
            print("[PASS] demo-boundary backend_api=yes 计数 16/17（XS5 除外）")
    except Exception as e:
        fail(f"demo-boundary/claims 校验失败: {e}")
        errors += 1

    print(f"\n结果: {'全部通过' if errors == 0 else f'{errors} 项失败'}")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
