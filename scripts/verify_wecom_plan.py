#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/verify_wecom_plan.py —— real_wecom 现场证据计划完整性校验。

- 计划必须覆盖 demo-boundary 的全部 17 个场景；
- 每个计划项必须有 owner / evidence_type / task_ref / steps / acceptance；
- summary.total_scenes 与状态一致。
"""
import sys

import yaml

PLAN = "project/views/real-wecom-evidence-plan.yaml"
BOUNDARY = "project/views/demo-boundary.yaml"


def main() -> int:
    with open(PLAN, encoding="utf-8") as f:
        plan = yaml.safe_load(f)
    with open(BOUNDARY, encoding="utf-8") as f:
        boundary = yaml.safe_load(f)

    errors = 0
    scenes = plan.get("scenes", [])
    boundary_ids = {s["id"] for s in boundary.get("scenes", [])}
    plan_ids = {s["scene"] for s in scenes}
    if plan_ids != boundary_ids:
        print(f"[FAIL] 计划覆盖不一致：缺 {sorted(boundary_ids - plan_ids)}，多 {sorted(plan_ids - boundary_ids)}")
        errors += 1
    for s in scenes:
        for k in ("scene", "owner", "evidence_type", "task_ref", "steps", "acceptance"):
            if not s.get(k):
                print(f"[FAIL] {s.get('scene')}: 缺字段 {k}")
                errors += 1
        if s.get("status", "UNCOLLECTED") != "UNCOLLECTED":
            print(f"[FAIL] {s.get('scene')}: 未采集前不得改状态")
            errors += 1
    summary = plan.get("summary", {})
    if int(str(summary.get("total_scenes", 0))) != len(scenes):
        print("[FAIL] summary.total_scenes 与实际场景数不一致")
        errors += 1
    if summary.get("status") != "ALL_UNCOLLECTED":
        print("[FAIL] summary.status 必须为 ALL_UNCOLLECTED")
        errors += 1
    if errors:
        print(f"结果: {errors} 项失败")
        return 1
    print(f"[PASS] real_wecom 证据计划完整（{len(scenes)} 场景全部未采集）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
