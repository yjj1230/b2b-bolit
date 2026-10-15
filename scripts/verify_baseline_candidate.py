#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/verify_baseline_candidate.py —— baseline candidate DRAFT 一致性复核

- 重新计算 37 claims / D-006..D-013 decisions 的 canonical sha256；
- 校验 candidate.publishable == false（当前机制 PROPOSED 且存在外部阻塞）；
- 校验 scope 与 includes 键一致。
"""
import glob
import hashlib
import json
import sys

import yaml

VIEW = "project/views/baseline-candidate-20260820.yaml"


def canon(obj):
    def dflt(o):
        return o.isoformat() if hasattr(o, "isoformat") else str(o)
    return json.dumps(obj, ensure_ascii=False, sort_keys=True,
                      separators=(",", ":"), allow_nan=False, default=dflt)


def sha(obj):
    return "sha256:" + hashlib.sha256(canon(obj).encode("utf-8")).hexdigest()


def main() -> int:
    with open(VIEW, encoding="utf-8") as f:
        view = yaml.safe_load(f)
    errors = 0

    claims = {}
    for f in sorted(glob.glob("project/claims/*.yaml")):
        with open(f, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh) or {}
        for c in doc.get("claims", []):
            claims[c["claim_id"]] = sha(c)
    if len(claims) != 37:
        print(f"[FAIL] claims 数量 {len(claims)} != 37")
        errors += 1
    decisions = {}
    for i in range(6, 14):
        did = f"D-{i:03d}"
        with open(f"project/decisions/{did}.yaml", encoding="utf-8") as fh:
            decisions[did] = sha(yaml.safe_load(fh))
    want_c = view["includes"]["claims"]
    want_d = view["includes"]["decisions"]
    for cid, digest in claims.items():
        if want_c.get(cid) != digest:
            print(f"[FAIL] claim digest 不一致: {cid}")
            errors += 1
    for did, digest in decisions.items():
        if want_d.get(did) != digest:
            print(f"[FAIL] decision digest 不一致: {did}")
            errors += 1
    if set(view["scope"]["claims"]) != set(want_c):
        print("[FAIL] scope.claims 与 includes.claims 不一致")
        errors += 1
    if set(view["scope"]["decisions"]) != set(want_d):
        print("[FAIL] scope.decisions 与 includes.decisions 不一致")
        errors += 1
    pred = view["publish_predicate_evaluation"]
    if pred.get("publishable") is not False:
        print("[FAIL] DRAFT candidate publishable 必须为 false")
        errors += 1
    if not view.get("publish_blockers"):
        print("[FAIL] DRAFT candidate 缺少 publish_blockers")
        errors += 1

    if errors:
        print(f"结果: {errors} 项失败")
        return 1
    print(f"[PASS] baseline candidate DRAFT 一致（{len(claims)} claims / {len(decisions)} decisions / publishable=false）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
