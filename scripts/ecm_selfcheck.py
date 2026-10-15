#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/ecm_selfcheck.py — ECM bootstrap 自检器（bootstrap 阶段工具）

用途：
  - --fill   回填 project/policies/mechanism/ECM-1.0.0.yaml 的 components 摘要
             （对五个组件块做 canonical-json（键按 UTF-8 字节序）后 sha256，ECM-CANON-003/005）。
  - --check  校验（默认）：附录 A.5 RESOLVE_MECHANISM 的机械可判定部分 +
             project.yaml / current.yaml / ECM-1.0.0.yaml 三处 digest 一致性 + HTML 身份一致性 +
             事件流合法性 + bootstrap handoff 契约。

注意：本脚本是 bootstrap 自检工具，不是 ECM-ACT-001 要求的正式 validator
（正式 validator 需随激活发布并纳入校准回放）。同一 Agent 的自检不得冒充独立接手验证（A.10 第 6 条）。
"""
import hashlib
import json
import re
import sys
from datetime import datetime, timezone

try:
    import yaml
except ImportError:
    sys.exit("需要 PyYAML: pip install pyyaml")

REPO = "."
COMPONENTS = ["ontology", "authority_matrix", "state_machine", "evidence_policy", "handoff_policy"]
SPEC = "project/policies/mechanism/ECM-1.0.0.yaml"
GUIDE = "project/policies/mechanism/ECM-1.0.0.html"
CURRENT = "project/policies/mechanism/current.yaml"
PROJECT = "project/project.yaml"
EVENTS = "project/events/project-events.jsonl"
HANDOFF_DIR = "project/handoffs"
M0_MANIFEST = "project/views/artifact-manifest.yaml"

RESULTS = []


def sha256_bytes(raw: bytes) -> str:
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as f:
        return sha256_bytes(f.read())


def canonical_json(obj) -> str:
    """canonical-json：compact、ensure_ascii=False、键按 UTF-8 字节升序（ECM-CANON-003/004）。
    UTF-8 字节序与 Unicode 码位序一致，json.dumps(sort_keys=True) 即满足。
    datetime 统一 isoformat（PyYAML 会把 RFC3339 字符串解析为 datetime）。"""
    def _default(o):
        return o.isoformat() if hasattr(o, "isoformat") else str(o)
    return json.dumps(obj, ensure_ascii=False, sort_keys=True,
                      separators=(",", ":"), allow_nan=False, default=_default)


def check(name: str, ok: bool, detail: str = ""):
    RESULTS.append((name, ok, detail))
    tag = "PASS" if ok else "FAIL"
    print(f"[{tag}] {name}" + (f" — {detail}" if detail else ""))


def rel(path: str) -> str:
    import os
    return os.path.normpath(path).replace("\\", "/")


def load_yaml(path: str):
    with open(path, "r", encoding="utf-8") as f:
        return yaml.safe_load(f)


def rfc3339_ok(s) -> bool:
    if not isinstance(s, str):
        return False
    try:
        dt = datetime.fromisoformat(s)
        return dt.tzinfo is not None
    except ValueError:
        return False


def fill():
    data = load_yaml(SPEC)
    for name in COMPONENTS:
        block = data.get(name)
        if block is None:
            sys.exit(f"FAIL: ECM-1.0.0.yaml 缺少组件块 {name}")
        data["components"][name] = sha256_bytes(canonical_json(block).encode("utf-8"))
    with open(SPEC, "w", encoding="utf-8") as f:
        yaml.safe_dump(data, f, allow_unicode=True, sort_keys=False,
                       default_flow_style=False, width=1000)
    print("已回填 components 摘要:")
    for name in COMPONENTS:
        print(f"  {name}: {data['components'][name]}")


def main():
    if "--fill" in sys.argv:
        fill()
        return

    # 1. project.yaml 存在且可解析（A.5 02-03）
    try:
        project = load_yaml(PROJECT)
        check("project.yaml 存在且 schema 可解析", True)
    except Exception as e:
        check("project.yaml 存在且 schema 可解析", False, str(e))
        project = {}

    # 2. 指针解析（A.5 04-05）
    ptr = (project.get("mechanism") or {}).get("pointer")
    check("mechanism.pointer 为单一相对路径", isinstance(ptr, str) and not ptr.startswith(("/", "..")),
          str(ptr))
    try:
        current = load_yaml(CURRENT) if ptr else {}
        check("current.yaml 存在且可解析", bool(current))
    except Exception as e:
        check("current.yaml 存在且可解析", False, str(e))
        current = {}

    # 3. id/version 一致（A.5 07）
    pmech = project.get("mechanism") or {}
    check("pointer.id/version == project.yaml mechanism.id/version",
          current.get("mechanism_id") == pmech.get("id")
          and str(current.get("version")) == str(pmech.get("version")),
          f"{current.get('mechanism_id')}@{current.get('version')} vs {pmech.get('id')}@{pmech.get('version')}")

    # 4. spec/guide 路径在 repo 内且 digest 匹配（A.5 08-09）
    spec_rel, guide_rel = current.get("spec"), current.get("guide")
    for label, p in (("spec", spec_rel), ("guide", guide_rel)):
        ok = isinstance(p, str) and not p.startswith(("/", ".."))
        check(f"{label} 路径在 repo 内", ok, str(p))
    try:
        spec_digest = sha256_file(spec_rel) if spec_rel else ""
        guide_digest = sha256_file(guide_rel) if guide_rel else ""
        check("spec digest == current.spec_digest", spec_digest == current.get("spec_digest"))
        check("spec digest == project.spec_digest", spec_digest == pmech.get("spec_digest"))
        check("guide digest == current.guide_digest", guide_digest == current.get("guide_digest"))
    except OSError as e:
        check("spec/guide digest 可计算", False, str(e))

    # 5. YAML 规范源 schema 合法 + 身份（A.5 10）
    try:
        spec = load_yaml(SPEC)
        check("ECM-1.0.0.yaml 可解析（schema 合法）", True)
        check("YAML 身份 id/version/status",
              spec.get("mechanism_id") == current.get("mechanism_id")
              and str(spec.get("version")) == str(current.get("version"))
              and spec.get("status") == current.get("status"),
              f"{spec.get('mechanism_id')}@{spec.get('version')} {spec.get('status')}")
    except Exception as e:
        check("ECM-1.0.0.yaml 可解析（schema 合法）", False, str(e))
        spec = {}

    # 6. HTML 声明同 id/version/status（A.5 10）
    try:
        html = open(GUIDE, "r", encoding="utf-8").read()
        id_ok = "ECM 1.0.0" in html
        st_ok = "PROPOSED" in html and "v1.0 Proposal" in html
        check("HTML 声明 id/version", id_ok)
        check("HTML 声明 status=PROPOSED", st_ok)
    except OSError as e:
        check("HTML 指南可读", False, str(e))

    # 7. policy components digest（A.5 11）
    comps = spec.get("components") or {}
    for name in COMPONENTS:
        block = spec.get(name)
        if block is None:
            check(f"组件 {name} 存在", False)
            continue
        got = sha256_bytes(canonical_json(block).encode("utf-8"))
        check(f"组件 {name} digest 匹配", got == comps.get(name),
              f"{got} vs {comps.get(name)}")

    # 8. 事件流（§11 / ECM-CANON-002）
    try:
        lines = [l for l in open(EVENTS, "r", encoding="utf-8") if l.strip()]
        ok_all = True
        kinds = set()
        for i, line in enumerate(lines, 1):
            try:
                ev = json.loads(line)
                if not (rfc3339_ok(ev.get("at")) and ev.get("event_id") and ev.get("kind")):
                    ok_all = False
                kinds.add(ev.get("kind"))
            except json.JSONDecodeError:
                ok_all = False
        check("project-events.jsonl 每行合法且含 RFC3339 时间", ok_all, f"{len(lines)} 事件")
        check("事件流含 ArtifactChanged 事件（§11 每次变化必须产出）",
              "ArtifactChanged" in kinds, ",".join(sorted(kinds)))
    except OSError:
        check("project-events.jsonl 可读", False)

    # 9. M1 退出条件（§09：交付物 + 能够表达 SIM-01..06）
    import os as _os
    check("M1 基线 schema 存在（project/baselines/schema.yaml）",
          _os.path.isfile("project/baselines/schema.yaml"))
    try:
        spec2 = load_yaml(SPEC)
        routes = (spec2.get("evidence_policy") or {}).get("entry_routes") or []
        check("规范源含 §06 五种入口差异化路由", len(routes) == 5,
              f"{len(routes)} 条")
        cal = load_yaml("project/policies/mechanism/calibration-cases.yaml")
        sim = cal.get("m1_exit_validation") or {}
        missing = [f"SIM-0{i}" for i in range(1, 7) if f"SIM-0{i}" not in sim]
        check("SIM-01..06 表达性映射完整（§09 M1 退出条件）", not missing,
              f"缺失: {missing}")
    except Exception as e:
        check("M1 退出条件可解析", False, str(e))

    # 10. bootstrap handoff 契约（ECM-REC-004 / A.8）
    import glob as _glob
    hofs = sorted(_glob.glob(f"{HANDOFF_DIR}/HOF-*/manifest.yaml"))
    check("存在 handoff manifest", bool(hofs), ";".join(hofs))
    if hofs:
        man = load_yaml(hofs[-1])
        check("handoff.status == OFFERED", man.get("status") == "OFFERED",
              str(man.get("status")))
        acc_path = hofs[-1].replace("manifest.yaml", "acceptance.yaml")
        try:
            acc = load_yaml(acc_path)
            d = acc.get("decision")
            check("acceptance.decision 未写 ACCEPTED（bootstrap，ECM-REC-004）",
                  d in (None, "", "NOT_APPLICABLE"), str(d))
            check("acceptance.bootstrap_mode == true（A.8 required-boolean）",
                  acc.get("bootstrap_mode") is True)
        except Exception as e:
            check("acceptance.yaml 可读", False, str(e))

    # 11. claims 库（§04 / §09 M2 首批）
    SEM = {"PROPOSED", "ACCEPTED", "DEFERRED", "REJECTED", "SUPERSEDED", "CHALLENGED"}
    IMP = {"UNPLANNED", "PLANNED", "PARTIAL", "IMPLEMENTED", "DRIFTED", "NOT_APPLICABLE"}
    VER = {"UNVERIFIED", "PASS", "FAIL", "STALE", "INVALID", "EXPIRED", "NOT_APPLICABLE"}
    claim_files = ["project/claims/ordering.yaml", "project/claims/fulfillment.yaml",
                   "project/claims/authorization.yaml", "project/claims/wecom-interaction.yaml"]
    ids, errs = set(), []
    for cf in claim_files:
        try:
            data = load_yaml(cf)
        except Exception as e:
            check(f"claims 文件可解析: {cf}", False, str(e))
            continue
        for c in (data.get("claims") or []):
            cid = c.get("claim_id")
            if cid in ids:
                errs.append(f"duplicate {cid}")
            ids.add(cid)
            st = c.get("statement") or {}
            if not all(k in st for k in ("subject", "predicate", "object")):
                errs.append(f"{cid}: statement 缺三元组")
            if c.get("semantic_status") not in SEM:
                errs.append(f"{cid}: semantic_status 非法")
            if c.get("risk") in ("R2", "R3") and not c.get("owner"):
                errs.append(f"{cid}: R2/R3 缺 owner")
            if not (c.get("authority") or {}).get("sources"):
                errs.append(f"{cid}: 缺 source")
    check("claims 库 37 条且全部合法（唯一 ID/三元组/状态词表/R2-R3 owner/source）",
          len(ids) == 37 and not errs, f"{len(ids)} claims; errors={errs}")

    # 10. M0 manifest 存在（§09 M0 退出条件）
    check("M0 artifact-manifest.yaml 存在", __import__("os").path.isfile(M0_MANIFEST))

    n_fail = sum(1 for _, ok, _ in RESULTS if not ok)
    print(f"\n结果: {len(RESULTS) - n_fail}/{len(RESULTS)} 通过，{n_fail} 失败")
    print("注意: 本自检由同一 Agent 完成，不构成独立接手验证（A.10 第 6 条）；"
          "机制仍为 PROPOSED（ECM-ACT-001 未满足），current.yaml 不得视为生效指针。")
    sys.exit(1 if n_fail else 0)


if __name__ == "__main__":
    main()
