/* ============================================================
   demo-engine.js —— 产品演示引擎（docs/demo/ 共享）
   数据驱动：页面定义 window.DEMO_ROLES（角色数组，每个角色含子场景数组），
   引擎负责：顶部角色切换 / 场景切换 / 消息步进播放 / AI 时间线 / 注解面板 / 企微 mockup 渲染。
   纯前端、无外部依赖，file:// 直接打开即可运行。
   交互参照：AI-Sale scene-12-workbench（步进 + 播放 + 进度条）
   ============================================================ */

(function (global) {
  'use strict';

  var roles = [];          // 全部角色
  var demo = null;         // 当前角色配置
  var cur = null;          // 当前场景
  var idx = 0;             // 已显示消息数
  var playing = false;
  var playTimer = null;
  var clock = 0;           // 模拟时间（分钟偏移）
  var BASE_MIN = 9 * 60 + 24; // 09:24
  var viewMode = null;     // 当前实际渲染终端：mobile / pc
  var viewModePinned = false;

  var els = {};

  // ===== 工具 =====
  function $(id) { return document.getElementById(id); }

  function fmtClock(off) {
    var m = BASE_MIN + off;
    var h = Math.floor(m / 60) % 24, mi = m % 60;
    return (h < 10 ? '0' : '') + h + ':' + (mi < 10 ? '0' : '') + mi;
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  // `side` 同时承载两类数据：对象表示订单助手页面快照，字符串表示消息方向。
  // 只有对象快照才能驱动 H5 打开和内容渲染，避免 `side: 'right'` 清空应用页。
  function sideSnapshot(msg) {
    return msg && msg.side && typeof msg.side === 'object' ? msg.side : null;
  }

  function currentFormFactor() {
    return viewMode || (demo && demo.formFactor) || 'mobile';
  }

  function updateDeviceChrome() {
    var mode = currentFormFactor();
    var pcBtn = $('btn-device-pc');
    var mobileBtn = $('btn-device-mobile');
    [pcBtn, mobileBtn].forEach(function (button) {
      if (!button) return;
      var active = button === (mode === 'pc' ? pcBtn : mobileBtn);
      button.classList.toggle('on', active);
      button.setAttribute('aria-pressed', active ? 'true' : 'false');
    });
    var copy = $('demo-device-copy');
    if (copy) copy.textContent = '长安仁恒 × 广西天华 · 膨润土 AB-3 · ' + (mode === 'pc' ? 'PC 端演示' : '移动端演示');
    var stageTitle = $('stage-title');
    if (stageTitle) stageTitle.textContent = mode === 'mobile'
      ? '企微移动端 · 手机真机交互视图（群聊 ⇄ 订单助手 H5）'
      : '企微 PC 端 · 客户端交互视图（会话 + 群聊 + 订单助手侧边栏）';
    document.body.dataset.device = mode;
  }

  function setViewMode(mode, userTriggered) {
    if (mode !== 'pc' && mode !== 'mobile') return;
    if (userTriggered) viewModePinned = true;
    if (viewMode === mode && cur) {
      updateDeviceChrome();
      return;
    }
    stopPlay();
    viewMode = mode;
    try { localStorage.setItem('order-fulfill-device', mode); } catch (e) {}
    updateDeviceChrome();
    if (!cur) return;
    resetStage();
    renderChat();
    renderSide(latestSide(idx), true);
    renderTimeline(latestAnnotation(idx), true);
    renderAnnos(latestAnnotation(idx));
    updateProg();
  }

  // ===== 初始化 =====
  function init(roleList) {
    roles = roleList || [];
    if (!roles.length) return;
    try {
      var savedMode = localStorage.getItem('order-fulfill-device');
      if (savedMode === 'pc' || savedMode === 'mobile') {
        viewMode = savedMode;
        viewModePinned = true;
      }
    } catch (e) {}
    bindTop();
    renderRoleTabs();
    selectRole(roles[0].role);
  }

  // ===== 顶部角色条 =====
  function renderRoleTabs() {
    var wrap = $('role-tabs');
    if (!wrap) return;
    wrap.innerHTML = '';
    roles.forEach(function (r) {
      var b = document.createElement('button');
      b.className = 'role-tab' + (r === roles[0] ? ' on' : '');
      b.dataset.role = r.role;
      b.innerHTML =
        '<span class="rn">' + esc(r.roleName) + '</span>' +
        '<span class="rc">' + r.scenarios.length + '</span>';
      b.onclick = function () { selectRole(r.role); };
      wrap.appendChild(b);
    });
  }

  function selectRole(roleId) {
    var r = roles.filter(function (x) { return x.role === roleId; })[0];
    if (!r || !r.scenarios.length) return;
    demo = r;
    // 角色条高亮
    Array.prototype.forEach.call(document.querySelectorAll('.role-tab'), function (b) {
      b.classList.toggle('on', b.dataset.role === roleId);
    });
    // 按角色重建 mockup 的 Tab 条（PC 侧边栏 + 手机壳）
    renderStaticTabs();
    // 未主动选择过终端时沿用角色默认值；主动切换后跨角色保持选择。
    if (!viewModePinned) viewMode = demo.formFactor || 'mobile';
    updateDeviceChrome();
    // 舞台标题与手机壳标题
    var chatName = demo.chatName || (demo.appName || '订单助手') + ' · 企微群聊';
    var phTitle = document.querySelector('#stage-phone .nav .title');
    if (phTitle) phTitle.textContent = chatName;
    var appTitle = $('app-title');
    if (appTitle) appTitle.textContent = demo.appName || '订单助手';
    renderSceneTabs();
    renderSceneList();
    selectScenario(r.scenarios[0].id);
  }

  // 按当前角色的 tabs 配置重建 mockup 的 Tab 条
  function renderStaticTabs() {
    var sels = ['.wc-pc .tabs', '#app-page .aside-tabs'];
    sels.forEach(function (sel) {
      var el = document.querySelector(sel);
      if (!el) return;
      var isAside = el.classList.contains('aside-tabs');
      el.innerHTML = (demo.tabs || []).map(function (t) {
        return '<span class="' + (isAside ? 'aside-tab' : 'tab') + '">' + esc(t) + '</span>';
      }).join('');
    });
  }

  function bindTop() {
    els.back = $('btn-back');
    els.step = $('btn-step');
    els.play = $('btn-play');
    els.reset = $('btn-reset');
    els.prog = $('prog-text');
    els.fill = $('prog-fill');
    if (els.back) els.back.onclick = stepBack;
    if (els.step) els.step.onclick = stepForward;
    if (els.play) els.play.onclick = togglePlay;
    if (els.reset) els.reset.onclick = resetAll;
    var pcMode = $('btn-device-pc');
    var mobileMode = $('btn-device-mobile');
    if (pcMode) pcMode.onclick = function () { setViewMode('pc', true); };
    if (mobileMode) mobileMode.onclick = function () { setViewMode('mobile', true); };
    // 应用页关闭（回到聊天）
    var appClose = $('app-close');
    if (appClose) appClose.onclick = function () {
      var p = $('app-page');
      if (p) p.classList.remove('open');
    };
    // 演示卡片中的主操作不再是静态摆设：点击审批条或可用按钮即推进到下一步。
    document.addEventListener('click', function (e) {
      var target = e.target.closest('.wc-approval, .wc-po-actions .btn');
      if (!target || target.classList.contains('disabled')) return;
      if (!target.closest('#stage-pc, #stage-phone')) return;
      stopPlay();
      stepForward();
    });
    // 长表单「本次已核对」：全部勾选后才解锁提交
    document.addEventListener('change', function (e) {
      var t = e.target;
      if (!t || !t.classList || !t.classList.contains('lf-confirm')) return;
      var form = t.closest('.wc-long-form');
      if (!form) return;
      syncLongFormSubmit(form);
    });
  }

  function syncLongFormSubmit(form) {
    var required = form.querySelectorAll('.lf-confirm');
    if (!required.length) return;
    var all = true;
    Array.prototype.forEach.call(required, function (c) {
      if (!c.checked) all = false;
    });
    var btn = form.querySelector('.wc-po-actions .btn-confirm');
    if (!btn) return;
    if (all) btn.classList.remove('disabled');
    else btn.classList.add('disabled');
  }

  // ===== 场景切换 =====
  function renderSceneTabs() {
    var wrap = $('scene-tabs');
    if (!wrap) return;
    wrap.innerHTML = '';
    demo.scenarios.forEach(function (s, i) {
      var b = document.createElement('button');
      b.className = 'scene-tab' + (i === 0 ? ' on' : '');
      b.dataset.id = s.id;
      b.innerHTML = '<span class="si">' + esc(s.id) + '</span>' + esc(s.name);
      b.onclick = function () { selectScenario(s.id); };
      wrap.appendChild(b);
    });
  }

  function renderSceneList() {
    var wrap = $('scene-list');
    if (!wrap) return;
    wrap.innerHTML = '';
    demo.scenarios.forEach(function (s, i) {
      var b = document.createElement('button');
      b.className = 'scene-item' + (i === 0 ? ' on' : '');
      b.dataset.id = s.id;
      b.title = '内部场景编号：' + s.id;
      b.innerHTML =
        '<span style="flex:1;min-width:0;">' +
        '  <div class="nm">' + esc(sceneLabel(s)) + '</div>' +
        '</span>';
      b.onclick = function () { selectScenario(s.id); };
      wrap.appendChild(b);
    });
  }

  var SCENE_LABELS = {
    XS1: '查询订单并回复客户', XS2: '客户人工下单', XS3: 'AI 智能下单',
    XS4: '查看销售工作台', XS5: '查询物流并回复客户', XSZG1: '订单审价',
    SW1: '工作台确认履约', SW2: '查询全量订单', SW3: '处理换仓异常',
    XSZL1: '代销售创建订单', XSZL2: '群入口代销售创建订单', CW1: '确认开票', CW2: '无回单开票审批',
    CG1: '接受发货任务', CG2: '确认发货', CG3: '上传并核对回单',
    DSZ1: 'C/D 级订单终审'
  };

  var SCENE_NARRATIVES = {
    XS1: { need: '“帮我看看最近这两单的进度。”', problem: '查一笔订单，销售原本需要跨聊天记录、订单系统和物流信息人工核对。', chain: ['理解问题', '定位客户', '匹配最近订单', '聚合履约状态', '生成回复'], ai: '查询订单 · 聚合物流与状态 · 生成回复', human: '确认内容 · 决定是否发送给客户', checks: ['自然语言正确匹配订单', '多订单状态同时聚合', '物流与 ETA 信息完整', 'AI 不修改订单状态', '对客发送前需销售确认'], outcome: '人工 5–6 个操作 → 1 次提问 + 1 次确认' },
    XS2: { need: '销售需要为当前群客户手工创建订单。', problem: '客户、合同、价格和收货信息分散，人工录入耗时且容易遗漏。', chain: ['读取群客户', '预填客户档案', '匹配合同与价格', '标注字段来源', '提交订单'], ai: '读取上下文 · 预填字段 · 标注来源', human: '过目预填结果 · 确认提交', checks: ['客户归属不可误改', '预填信息来源可追溯', '地址与联系人来源标明', '正式下单由销售提交'], outcome: '重复录入 → 过目预填结果后提交' },
    XS3: { need: '客户在群里用自然语言提出下单需求。', problem: '销售需要把非结构化消息重新整理为完整订单。', chain: ['识别下单意图', '抽取订单字段', '关联历史订单', '检查风险', '生成确认卡'], ai: '抽取 · 关联 · 预填 · 风险提示', human: '修正识别结果 · 确认正式下单', checks: ['产品与数量识别正确', '历史地址仅作建议', '异常字段明确提示', '未经确认不生成正式订单'], outcome: '逐项建单 → 审核一张订单确认卡' },
    XS4: { need: '销售需要快速知道今天应优先处理什么。', problem: '订单、审批和客户跟进分散，销售难以判断优先级。', chain: ['汇总个人任务', '识别阻塞节点', '按紧急度排序', '生成处理建议'], ai: '聚合待办 · 标记风险 · 推荐顺序', human: '选择任务 · 处理客户与订单决策', checks: ['仅展示本人业务范围', '阻塞原因清晰', '任务状态与订单一致', '完成后明确下一责任人'], outcome: '多处巡检 → 一个工作台掌握重点' },
    XS5: { need: '客户询问订单物流与预计到达时间。', problem: '销售需要查订单、物流单号和轨迹后再组织客户回复。', chain: ['理解物流问询', '匹配订单', '查询物流轨迹', '计算 ETA', '生成客户卡片'], ai: '查物流 · 聚合轨迹 · 生成客户可读信息', human: '确认隐私与内容 · 发送给客户', checks: ['物流订单匹配正确', 'ETA 与最新轨迹一致', '客户卡片不泄露内部字段', '发送动作由销售触发'], outcome: '多系统查询 → 一键生成并确认回复' },
    XSZG1: { need: '主管需要判断特殊价格是否可以批准。', problem: '审价需要同时核对标准价、历史成交价、客户等级与申请原因。', chain: ['读取申请', '对比价格基线', '识别风险', '解释审批原因', '给出建议'], ai: '对比价格 · 计算差异 · 解释风险', human: '要求修改 · 拒绝或批准', checks: ['审批触发原因明确', '关键价格差异完整', 'AI 只建议不审批', '拒绝后可修改重提'], outcome: '人工拼接依据 → 聚合决策信息后审批' },
    SW1: { need: '终审通过后，商务需要确认订单能否按期履约。', problem: '库存口径、仓库和物流时效分散，人工确认容易超卖或承诺错误交期。', chain: ['接收工作台待办', '聚合库存口径', '评估仓库与交期', '人工确认', '预留库存并交接'], ai: '聚合库存与物流 · 提示履约风险 · 生成建议', human: '确认仓库和承诺交期 · 授权库存预留', checks: ['账面/预留/可分配口径一致', '提交时重新校验库存', '确认后只生成仓管任务', '销售收到承诺交期通知'], outcome: '跨系统核对 → 一次确认完成预留与角色交接' },
    SW2: { need: '商务需要跨客户定位订单及其履约状态。', problem: '订单数量多、状态分散，但全量查询不能扩大商务的业务权限。', chain: ['理解筛选条件', '查询履约字段', '定位目标订单', '查看只读详情', '跳转本人待办'], ai: '筛选 · 汇总 · 标记异常订单', human: '查看详情 · 进入本人待办处理', checks: ['仅展示履约相关字段', '查询不修改业务数据', '不可承接其他角色审批', '操作入口回到本人待办'], outcome: '逐单翻查 → 只读定位后进入正确任务' },
    SW3: { need: '仓管拒绝接单后，商务需要重新确定可执行方案。', problem: '拒绝任务若没有回流和新责任人，订单会停在无人处理状态。', chain: ['接收仓管拒绝', '保留拒绝原因', '比较备选方案', '人工选择换仓', '释放原预留并重派'], ai: '比较换仓/拆单/调期 · 解释成本与交期影响', human: '选择新履约方案 · 确认重新派发', checks: ['原拒绝记录可追溯', 'AI 不自动改派', '原仓预留正确释放', '新仓管和销售收到通知'], outcome: '人工重新协调多方 → 一次确认完成换仓闭环' },
    XSZL1: { need: '销售助理需要代指定销售创建订单。', problem: '代下单既要减少录入，又必须保证客户和销售归属正确。', chain: ['选择归属销售', '关联客户', '预填订单', '校验权限', '生成草稿'], ai: '关联客户 · 预填字段 · 校验归属', human: '选择归属销售 · 核对并提交', checks: ['代办人与归属销售分离', '客户权限正确', '预填字段可修改', '提交后进入正常审批链'], outcome: '重复代录 → 选定归属后核对提交' },
    XSZL2: { need: '销售助理需要从客户群代指定销售创建订单。', problem: '群入口的客户与归属销售已经确定，不应再让助理重新选择，避免选错归属。', chain: ['读取群上下文', '锁定客户', '自动带出归属销售', '预填订单', '校验权限'], ai: '读取群上下文 · 锁定客户/归属 · 预填字段', human: '核对预填结果 · 提交订单', checks: ['客户由群上下文锁定', '归属销售 readonly 不可改', '预填字段可修改', '提交后进入正常审批链'], outcome: '重复选客/选销售 → 群入口直接锁定后核对提交' },
    CW1: { need: '财务需要确认订单满足开票条件并登记凭证。', problem: '开票前要核对发货、回单和税务信息，字段分散且容易错漏。', chain: ['读取开票任务', '核验发货与回单', '预填税务信息', '检查异常', '生成开票确认'], ai: '核验条件 · 预填票面信息 · 提示异常', human: '复核票面 · 确认开票', checks: ['开票前置条件齐全', '税号与抬头匹配', 'AI 不自动开票', '凭证登记后状态闭环'], outcome: '多处核验 → 集中复核后开票' },
    CW2: { need: '财务需要判断无回单情况下是否允许例外开票。', problem: '缺少回单属于财务风险，必须有明确原因、材料和审批责任。', chain: ['识别回单缺失', '核对例外材料', '评估风险', '解释审批原因', '生成审批建议'], ai: '发现异常 · 聚合材料 · 提示风险', human: '批准或拒绝例外开票', checks: ['异常原因明确', '证明材料可追溯', 'AI 不越权放行', '审批结果更新开票状态'], outcome: '人工追材料 → 风险与依据集中呈现' },
    CG1: { need: '仓管需要确认是否接受新的发货任务。', problem: '仓管需要快速确认商品、数量、仓库和要求发货时间。', chain: ['接收发货任务', '核对仓库归属', '检查任务信息', '提示异常'], ai: '整理任务 · 校验仓库与信息完整性', human: '接受任务或反馈异常', checks: ['任务来源清晰', '仓库归属正确', '价格等无关信息隐藏', '接受后进入待发货'], outcome: '群内协调 → 一张任务卡完成接单' },
    CG2: { need: '仓管完成装车后需要登记物流并确认发货。', problem: '物流字段和发货凭证需要完整录入，否则后续无法追踪。', chain: ['读取发货任务', '预填物流信息', '检查必填字段', '生成发货确认'], ai: '预填承运信息 · 校验完整性', human: '核对实物与物流 · 确认发货', checks: ['数量与订单一致', '物流必填项完整', '确认前订单仍未发货', '完成后通知销售'], outcome: '手工同步多方 → 一次确认推进发货状态' },
    CG3: { need: '仓管需要上传回单并关联到正确订单。', problem: '回单图片质量和订单匹配度不稳定，错误绑定会影响开票。', chain: ['读取回单', 'OCR 提取信息', '匹配订单', '评估置信度', '生成绑定建议'], ai: '识别凭证 · 推荐匹配订单 · 标记低置信度', human: '确认或修正绑定结果', checks: ['订单匹配有依据', '低置信度必须人工确认', '错误图片可重新上传', '确认后才进入待开票'], outcome: '人工辨认录入 → 审核 AI 匹配结果' },
    DSZ1: { need: '董事长需要完成 C/D 级订单终审。', problem: '高风险订单需要快速理解触发规则、利润风险和前序审批结论。', chain: ['读取审批上下文', '解释触发规则', '聚合关键风险', '呈现前序结论', '生成决策建议'], ai: '解释为什么需终审 · 聚合风险依据', human: '要求修改 · 拒绝或批准', checks: ['终审触发规则明确', '只展示决策相关信息', 'AI 不代替终审', '通过后进入商务履约'], outcome: '重新调查背景 → 直接基于完整依据决策' }
  };

  function sceneLabel(s) { return SCENE_LABELS[s.id] || s.name; }

  function renderSceneNarrative(s) {
    var n = SCENE_NARRATIVES[s.id] || {};
    $('scene-need').textContent = n.need || '完成当前业务任务。';
    $('scene-problem').textContent = n.problem || String(s.painPoint || '').split('。')[0] + '。';
    $('scene-agent-chain').innerHTML = (n.chain || ['理解', '查询', '生成建议']).map(function (step, i, arr) {
      return '<span class="chain-step">' + esc(step) + '</span>' + (i < arr.length - 1 ? '<span class="chain-arrow">→</span>' : '');
    }).join('');
    $('scene-ai-boundary').textContent = n.ai || '查询 · 聚合 · 生成建议';
    $('scene-human-boundary').textContent = n.human || '核对结果 · 执行业务决定';
    $('scene-validation').innerHTML = (n.checks || s.highlights || []).map(function (item) {
      return '<li>' + esc(item) + '</li>';
    }).join('');
    $('scene-outcome').textContent = n.outcome || s.demoGoal || '减少查找和重复录入';
  }

  function selectScenario(id) {
    var s = demo.scenarios.filter(function (x) { return x.id === id; })[0];
    if (!s) return;
    cur = s;
    idx = 0;
    clock = 0;
    stopPlay();
    // 场景级群名覆盖（无群上下文场景可覆盖角色默认群名）
    var phTitle = document.querySelector('#stage-phone .nav .title');
    if (phTitle) phTitle.textContent = s.chatName || demo.chatName || (demo.appName || '订单助手') + ' · 企微群聊';
    // 高亮 Tab 与列表
    Array.prototype.forEach.call(document.querySelectorAll('.scene-tab'), function (b) {
      b.classList.toggle('on', b.dataset.id === id);
    });
    Array.prototype.forEach.call(document.querySelectorAll('.scene-item'), function (b) {
      b.classList.toggle('on', b.dataset.id === id);
    });
    // 左侧场景信息
    $('scene-name').textContent = s.name;
    $('scene-stage').textContent = s.stage || '';
    renderSceneNarrative(s);
    // 中央舞台：进度 0 不预开小程序/侧栏内容（PC 侧栏常驻，不能提前塞 initialSide）
    resetStage();
    renderChat();
    renderSide(null, true);
    renderTimeline();
    renderAnnos(null);
    updateProg();
  }

  // ===== 舞台重置 =====
  function resetStage() {
    var pc = $('stage-pc'), ph = $('stage-phone');
    if (pc) pc.style.display = 'none';
    if (ph) ph.style.display = 'none';
    // 手机端：单手机壳（聊天 ⇄ 应用页切换）；PC 端恢复 grid
    if (currentFormFactor() === 'mobile') {
      if (ph) ph.style.display = 'flex';
    } else if (pc) {
      pc.style.display = 'grid';
    }
    hidePip();
    var chatBody = currentFormFactor() === 'mobile'
      ? (ph ? ph.querySelector('.chat-msgs') : null)
      : (pc ? pc.querySelector('.ch-body') : null);
    if (chatBody) chatBody.innerHTML = '';
    var aside = currentFormFactor() === 'mobile'
      ? (ph ? ph.querySelector('.app-page .aside-scroll') : null)
      : (pc ? pc.querySelector('.aside-content') : null);
    if (aside) aside.innerHTML = '<div class="empty">等待演示开始…</div>';
    var appPage = $('app-page');
    if (appPage) appPage.classList.remove('open');
    syncPcAside(false);
  }

  // ===== 消息步进 =====
  function stepForward() {
    if (!cur || idx >= cur.messages.length) return;
    var msg = cur.messages[idx];
    var ann = cur.annotations && cur.annotations[idx] ? cur.annotations[idx] : null;
    idx++;
    clock += 1 + Math.floor(Math.random() * 2);
    renderChat();
    var snap = sideSnapshot(msg);
    if (snap) renderSide(snap, false);
    if (msg.pip) renderPip(msg.pip); else hidePip();
    renderTimeline(ann);
    if (ann) renderAnnos(ann);   // 无注解消息保留上次注解面板
    updateProg();
    if (idx >= cur.messages.length) { stopPlay(); if (els.play) els.play.textContent = '▶ 播放'; }
  }

  function latestSide(limit) {
    // 只认消息里的 side；不用 initialSide 兜底，避免 0 步就打开小程序内容
    for (var i = limit - 1; i >= 0; i--) {
      var snap = sideSnapshot(cur.messages[i]);
      if (snap) return snap;
    }
    return null;
  }

  function latestAnnotation(limit) {
    for (var i = limit - 1; i >= 0; i--) {
      if (cur.annotations && cur.annotations[i]) return cur.annotations[i];
    }
    return null;
  }

  function stepBack() {
    if (!cur || idx <= 0) return;
    idx--;
    renderChat();
    // 回退到当前最后一条消息的 side/pip/注解
    var msg = cur.messages[idx - 1];
    var ann = latestAnnotation(idx);
    renderSide(latestSide(idx), true);
    if (msg && msg.pip) renderPip(msg.pip); else hidePip();
    renderTimeline(ann, true);
    renderAnnos(ann);
    updateProg();
  }

  // ===== 播放/重置 =====
  function togglePlay() {
    if (playing) { stopPlay(); return; }
    if (idx >= cur.messages.length) { resetAll(); return; }
    playing = true;
    if (els.play) els.play.textContent = '⏸ 暂停';
    playTimer = setInterval(function () {
      stepForward();
      if (idx >= cur.messages.length) stopPlay();
    }, 1400);
  }

  function stopPlay() {
    playing = false;
    if (playTimer) { clearInterval(playTimer); playTimer = null; }
    if (els.play) els.play.textContent = '▶ 播放';
  }

  function resetAll() {
    stopPlay();
    idx = 0; clock = 0;
    resetStage();
    renderChat();
    renderSide(null, true);
    renderTimeline();
    renderAnnos(null);
    updateProg();
  }

  function updateProg() {
    if (!cur) return;
    var total = cur.messages.length;
    if (els.prog) els.prog.textContent = idx + ' / ' + total;
    if (els.fill) els.fill.style.width = (total ? (idx / total * 100) : 0) + '%';
  }

  // ===== 聊天区渲染 =====
  function chatContainer() {
    var t = currentFormFactor() === 'mobile' ? $('stage-phone') : $('stage-pc');
    return t ? t.querySelector('.chat-msgs, .ch-body') : null;
  }

  function renderChat() {
    var box = chatContainer();
    if (!box || !cur) return;
    box.innerHTML = '';
    var msgs = cur.messages.slice(0, idx);
    msgs.forEach(function (m, i) {
      var el = buildMsg(m, i);
      if (el) box.appendChild(el);
    });
    box.scrollTop = box.scrollHeight;
    syncAppPage();
  }

  // 应用页开关：初始显示聊天页；带 side 的消息打开应用页；
  // ai-hint / ai-recommend 不渲染气泡，但保留已打开的应用页（避免核对表单被误关）；
  // 其余无 side 消息（销售/客户/纯说明）回到聊天。
  function syncAppPage() {
    var appPage = $('app-page');
    if (!appPage || currentFormFactor() !== 'mobile') return;
    var show = false;
    if (idx > 0) {
      var last = cur.messages[idx - 1];
      if (sideSnapshot(last)) {
        show = true;
      } else if (last && (last.type === 'ai-hint' || last.type === 'ai-recommend')) {
        show = !!latestSide(idx);
      }
    }
    appPage.classList.toggle('open', show);
  }

  function buildMsg(m, i) {
    // Agent（订单助手 AI）是我方后台智能：不在企微聊天界面渲染气泡，客户/销售无感；
    // 其分析与动作通过右侧「AI Agent 执行记录」面板呈现（side/pip 仍生效）
    if (m.type === 'ai-hint' || m.type === 'ai-recommend') return null;
    // 手机 mockup 用 .wc-msg/.bubble，PC 用 .msg/.bub
    var msgCls = currentFormFactor() === 'mobile' ? 'wc-msg' : 'msg';
    var bubCls = currentFormFactor() === 'mobile' ? 'bubble' : 'bub';
    var avCls = currentFormFactor() === 'mobile' ? 'av' : 'mav';
    // 左右对齐类：PC 用 o/s，手机用 cust/sales
    var sideCls = function (right) {
      if (currentFormFactor() === 'mobile') return right ? 'sales' : 'cust';
      return right ? 's' : 'o';
    };
    if (m.type === 'sys') {
      var d = document.createElement('div');
      d.className = msgCls + ' sys';
      d.innerHTML =
        '<span class="' + avCls + ' bot-av">🤖</span>' +
        '<div class="mb" style="min-width:0;flex:1;">' +
        '  <div class="who"><span>订单助手</span><span class="bot-badge">BOT</span><span>· ' + (m.time || fmtClock(i * 2)) + '</span></div>' +
        '  <div class="' + bubCls + '">' + esc(m.content) + '</div>' +
        '</div>';
      return d;
    }
    // 图片/卡片消息：渲染为结构化卡片（模拟系统生成图片发到群聊）
    if (m.type === 'image') {
      var d2 = document.createElement('div');
      var right = m.side === 'right' || (m.side !== 'left' && m.sender === demo.roleName);
      d2.className = msgCls + ' ' + sideCls(right);
      // 卡片的方向与发送人分开表达：side 决定左右，sender 决定署名与头像。
      var av2 = (m.sender || (right ? demo.roleName : '客户')).charAt(0);
      var who2 = m.sender || (right ? demo.roleName : '客户');
      var rows = (m.imageRows || []).map(function (r) {
        return '<div class="img-row"><span class="k">' + esc(r.k) + '</span><span class="v">' + esc(r.v) + '</span></div>';
      }).join('');
      d2.innerHTML =
        '<span class="' + avCls + '">' + esc(av2) + '</span>' +
        '<div class="mb" style="min-width:0;flex:1;">' +
        '  <div class="who">' + esc(who2) + ' · ' + (m.time || fmtClock(i * 2)) + '</div>' +
        '  <div class="' + bubCls + ' bub-image">' +
        '    <div class="img-card">' +
        '      <div class="img-head"><span class="img-title">' + esc(m.imageTitle || '图片') + '</span>' +
        (m.imageTag ? '<span class="img-tag">' + esc(m.imageTag) + '</span>' : '') + '</div>' +
        (m.imageDesc ? '<div class="img-desc">' + esc(m.imageDesc) + '</div>' : '') +
        (rows ? '<div class="img-rows">' + rows + '</div>' : '') +
        (m.imageFooter ? '<div class="img-footer">' + esc(m.imageFooter) + '</div>' : '') +
        '    </div>' +
        '  </div>' +
        '</div>';
      return d2;
    }
    var d3 = document.createElement('div');
    var isCust = m.type === 'customer';
    d3.className = msgCls + ' ' + sideCls(!isCust);
    var av = isCust ? (m.sender || '客').charAt(0) : demo.avatarChar;
    var who = isCust ? (m.sender || '客户') : demo.roleName;
    var chip = '';
    if (m.type === 'ai-hint' || m.type === 'ai-recommend') {
      chip = '<div class="ai-chip">◈ AI 助手</div>';
      d3.className = msgCls + ' ' + sideCls(false);
      av = 'AI';
      who = '订单助手 AI';
    }
    d3.innerHTML =
      '<span class="' + avCls + '">' + esc(av) + '</span>' +
      '<div class="mb" style="min-width:0;flex:1;">' +
      '  <div class="who">' + esc(who) + ' · ' + (m.time || fmtClock(i * 2)) + '</div>' +
      chip +
      '  <div class="' + bubCls + '">' + esc(m.content) + '</div>' +
      '</div>';
    return d3;
  }

  // ===== 画中画：销售端操作面板（PiP） =====
  var PIP_STATUS = {
    idle:      { label: '查物流',        cls: 'pulse' },
    clicked:   { label: '已点击 · 生成中…', cls: 'clicked' },
    generating:{ label: '⏳ 正在生成物流图片…', cls: 'generating' },
    sent:      { label: '✅ 图片已生成并发送到群', cls: 'sent' }
  };

  function renderPip(pip) {
    var el = $('phone-pip');
    if (!el) return;
    if (!pip) { el.classList.remove('show'); return; }
    el.classList.add('show');
    var st = PIP_STATUS[pip.status] || PIP_STATUS.idle;
    el.innerHTML =
      '<div class="phone-pip-head">◈ 订单助手<span class="pip-badge">后台生成中</span></div>' +
      '<div class="phone-pip-body">' +
      '  <div class="pip-order">' +
      '    <div class="row"><span class="k">订单</span><span class="v">' + esc(pip.orderNo || 'ORD-2026-0118') + '</span></div>' +
      '    <div class="row"><span class="k">产品</span><span class="v">' + esc(pip.prod || '膨润土 AB-3 · 5 吨') + '</span></div>' +
      '  </div>' +
      '  <div class="pip-btn ' + st.cls + '">' + st.label + '</div>' +
      (pip.note ? '<div class="pip-note">' + esc(pip.note) + '</div>' : '') +
      '</div>';
  }

  function hidePip() {
    var el = $('phone-pip');
    if (el) el.classList.remove('show');
  }

  // 四节点订单状态组件：已开单 → 已备货 → 已发货 → 已开票（直线连接，全角色统一）
  function tl4Html(tl4) {
    var nodes = tl4.nodes || ['已开单', '已备货', '已发货', '已开票'];
    var cur = tl4.current || 0;
    var html = '<div class="tl4">';
    nodes.forEach(function (n, i) {
      if (i > 0) html += '<span class="tl4-line' + (i <= cur ? ' done' : '') + '"></span>';
      var cls = i < cur ? ' done' : (i === cur ? ' cur' : '');
      html += '<span class="tl4-node' + cls + '">' + esc(n) + '</span>';
    });
    return html + '</div>';
  }

  // 紧凑四节点（订单列表每行）：圆点直线 + 当前节点标签
  function tl4Mini(cur) {
    var nodes = ['已开单', '已备货', '已发货', '已开票'];
    var html = '<div class="tl4-mini">';
    nodes.forEach(function (nm, i) {
      if (i > 0) html += '<span class="m-line' + (i <= cur ? ' done' : '') + '"></span>';
      var cls = i < cur ? ' done' : (i === cur ? ' cur' : '');
      html += '<span class="m-node' + cls + '"></span>';
    });
    html += '<span class="m-cur">' + esc(nodes[cur] || '') + '</span></div>';
    return html;
  }

  // ===== 侧边栏快照渲染（手机端=应用页；PC=侧边栏） =====
  // PC：无 side 时整栏隐藏（不要空白占位）；有 side 再展开订单助手
  function syncPcAside(show) {
    var pc = $('stage-pc');
    if (!pc) return;
    pc.classList.toggle('no-aside', !show);
  }

  function sideContainer() {
    var t = currentFormFactor() === 'mobile' ? $('app-page') : $('stage-pc');
    return t ? t.querySelector('.aside-scroll, .aside-content') : null;
  }

  function sideTabsWrap() {
    var t = currentFormFactor() === 'mobile' ? $('app-page') : $('stage-pc');
    return t ? t.querySelector('.aside-tabs, .tabs') : null;
  }

  function renderSide(snap, isInit) {
    var box = sideContainer();
    if (!box) return;
    box.innerHTML = '';
    syncPcAside(!!snap);
    if (!snap) {
      box.innerHTML = '<div class="empty">等待演示开始…</div>';
      return;
    }
    // Tab 高亮
    if (snap.tab) {
      var tw = sideTabsWrap();
      if (tw) {
        Array.prototype.forEach.call(tw.querySelectorAll('.tab, .aside-tab'), function (t, i) {
          var tabs = demo.tabs || [];
          t.classList.toggle('on', tabs[i] === snap.tab);
        });
      }
    }
    // 群上下文头
    if (snap.ctx) {
      var c = document.createElement('div');
      c.className = 'wc-group-ctx';
      c.innerHTML =
        '<span class="ctx-avatar' + (snap.ctx.avatarBlue ? ' blue' : '') + '">' + esc(snap.ctx.avatar || '天') + '</span>' +
        '<div style="min-width:0;">' +
        '  <div class="ctx-name">' + esc(snap.ctx.name || '') + '</div>' +
        '  <div class="ctx-sub">' + esc(snap.ctx.sub || '') +
        (snap.ctx.grade ? '<span class="wc-grade ' + esc(snap.ctx.gradeCls || 'b') + '">' + esc(snap.ctx.grade) + '</span>' : '') +
        '  </div>' +
        '</div>';
      box.appendChild(c);
    }
    // 分组标题
    if (snap.secTitle) {
      var st = document.createElement('div');
      st.className = 'wc-sec-title';
      st.innerHTML = '<span class="dot"></span>' + esc(snap.secTitle) +
        (snap.secCount ? ' <span class="count">' + esc(snap.secCount) + '</span>' : '');
      box.appendChild(st);
    }
    // 块列表
    (snap.blocks || []).forEach(function (b) {
      var el = buildBlock(b);
      if (el) box.appendChild(el);
    });
  }

  function buildBlock(b) {
    switch (b.kind) {
      case 'order-list':
        var d = document.createElement('div');
        d.style.display = 'contents';
        b.items.forEach(function (o) {
          var c = document.createElement('div');
          c.className = 'wc-order' + (o.noAmount ? ' no-amount' : '');
          var meta = [o.date ? ('下单 ' + o.date) : '', o.shipDate ? ('发货 ' + o.shipDate) : '', o.wh ? ('发：' + o.wh) : '', o.eta ? ('预计 ' + o.eta) : '', o.addr ? ('收货 ' + o.addr) : ''].filter(function (x) { return x; });
          c.innerHTML =
            '<div class="r1"><span class="no">' + esc(o.no) + '</span><span class="state st-' + esc(o.stateCls || 'wait') + '">' + esc(o.state) + '</span></div>' +
            '<div class="r2"><span class="prod">' + esc(o.prod) + '</span><span>' + esc(o.qty || '') + '</span></div>' +
            '<div class="r3"><span class="amount">' + esc(o.amount || '—') + '</span><span class="progress">' + esc(o.progress || '') + '</span></div>' +
            (meta.length ? '<div class="r4">' + meta.join(' · ') + '</div>' : '') +
            (o.tl4c !== undefined && o.tl4c !== null ? tl4Mini(o.tl4c) : '');
          d.appendChild(c);
        });
        return d;
      case 'appr-list':
        var d2 = document.createElement('div');
        d2.style.display = 'contents';
        b.items.forEach(function (a) {
          var c = document.createElement('div');
          c.className = 'wc-approval';
          c.innerHTML =
            '<span class="ap-icon' + (a.warn ? ' warn' : '') + '">' + esc(a.icon || '•') + '</span>' +
            '<div class="ap-main"><div class="ap-title">' + esc(a.title) + '</div><div class="ap-sub">' + esc(a.sub || '') + '</div></div>' +
            (a.tag ? '<span class="ap-tag">' + esc(a.tag) + '</span>' : '') +
            '<span class="ap-arrow">›</span>';
          d2.appendChild(c);
        });
        return d2;
      case 'preorder':
        var p = document.createElement('div');
        p.className = 'wc-preorder' + (b.done ? ' done' : '');
        p.innerHTML =
          '<div class="wc-po-head"><span class="ai-chip">AI 识别</span><span class="po-title">' + esc(b.title || '自动下单待确认') + '</span><span class="po-status">' + esc(b.status || '') + '</span></div>' +
          '<div class="wc-po-body">' +
          '  <div class="wc-po-origin"><div class="label">群内原话</div><div class="quote">' + esc(b.quote || '') + '</div><span class="src">' + esc(b.src || '') + '</span></div>' +
          '  <div class="wc-po-fields">' +
          (b.fields || []).map(function (f) {
            return '<div class="wc-po-field"><span class="k">' + esc(f.k) + '</span><span class="v' + (f.amount ? ' amount' : '') + (f.warn ? ' warn' : '') + '">' + esc(f.v) + '</span></div>';
          }).join('') +
          '  </div>' +
          (b.hint ? '<div class="wc-po-hint">⚠ ' + esc(b.hint) + '</div>' : '') +
          '</div>' +
          (b.done ?
            '<div class="wc-po-result"><div class="ok-title">' + esc(b.doneTitle || '已生成正式订单') + '</div><div class="ok-no">' + esc(b.doneNo || '') + '</div><div class="ok-sub">' + esc(b.doneSub || '') + '</div>' +
            (b.timeline ? '<div class="wc-po-timeline">' + b.timeline.map(function (nd) {
              return '<div class="tnode' + (nd.done ? ' done' : (nd.cur ? ' cur' : '')) + '">' +
                '<span class="n-dot">' + (nd.done ? '✓' : (nd.cur ? '◈' : '·')) + '</span>' +
                '<div class="n-main"><div class="n-name">' + esc(nd.name) + '</div>' +
                '<div class="n-meta">' + esc(nd.role || '') + ' · ' + esc(nd.who || '待分配') + ' · ' + esc(nd.status || '') + '</div>' +
                (nd.note ? '<div class="n-note">' + esc(nd.note) + '</div>' : '') + '</div></div>';
            }).join('') + '</div>' : '') +
            '</div>' :
            '<div class="wc-po-actions">' +
            (b.actions || ['忽略', '修改', '确认']).map(function (a, i) {
              var cls = ['btn-ignore', 'btn-edit', 'btn-confirm'][i] || 'btn-confirm';
              return '<span class="btn ' + cls + '">' + esc(a) + '</span>';
            }).join('') +
            '</div>') +
          '</div>';
        return p;
      case 'success':
        var s2 = document.createElement('div');
        s2.className = 'wc-success';
        s2.innerHTML =
          '<div class="ok-ico">✓</div>' +
          '<h4>' + esc(b.title || '订单已提交') + '</h4>' +
          '<p>' + esc(b.sub || '') + '</p>' +
          '<div class="no">' + esc(b.no || '') + '</div>' +
          (b.foot ? '<p style="font-size:10px;color:#B5BAC1;margin-top:4px;">' + esc(b.foot) + '</p>' : '');
        return s2;
      case 'long-form':
        // 单页长表单（评审 XS2-02）：客户顶部锁定，字段分组向下滑动填写，底部提交
        // hint 始终渲染为字段下方来源文案（有值时 placeholder 不可见，不能靠 placeholder 承载来源）
        // confirm 字段带「本次已核对」；未齐勾选时提交保持 disabled
        var lf = document.createElement('div');
        lf.className = 'wc-long-form';
        lf.innerHTML =
          '<div class="lf-head">' +
          '  <span class="lf-title">' + esc(b.title || '人工下单') + '</span>' +
          (b.ctx ? '<div class="lf-ctx">🔒 ' + esc(b.ctx) + '</div>' : '') +
          '</div>' +
          (b.groups || []).map(function (g) {
            return '<div class="lf-group' + (g.mustConfirm ? ' must-confirm' : '') + '"><div class="lf-group-title">' + esc(g.title || '') + '</div>' +
              (g.fields || []).map(function (fd) {
                if (fd.type === 'check') {
                  return '<div class="wc-form-row"><label>' + esc(fd.label) + '</label><label class="check-row"><input type="checkbox" checked /><span>' + esc(fd.value || '') + '</span></label></div>';
                }
                var ph = (!fd.value && fd.hint && !fd.confirm) ? ' placeholder="' + esc(fd.hint) + '"' : '';
                var hintCls = 'hint' + (fd.confirm || fd.hintWarn ? ' warn' : '');
                var hintHtml = fd.hint ? '<div class="' + hintCls + '">' + esc(fd.hint) + '</div>' : '';
                var confirmHtml = fd.confirm
                  ? '<label class="check-row confirm-check"><input type="checkbox" class="lf-confirm"' + (fd.confirmed ? ' checked' : '') + ' /><span>本次已核对</span></label>'
                  : '';
                return '<div class="wc-form-row' + (fd.confirm ? ' confirm-row' : '') + '"><label>' + esc(fd.label) + '</label><input value="' + esc(fd.value || '') + '"' + (fd.ro ? ' readonly class="ro"' : '') + ph + ' />' + hintHtml + confirmHtml + '</div>';
              }).join('') + '</div>';
          }).join('') +
          (b.tip ? '<div class="wc-tip' + (b.tipDanger ? ' danger' : '') + '">⚠ ' + esc(b.tip) + '</div>' : '') +
          (b.quota ? '<div class="wc-form-row"><div class="quota' + (b.quotaWarn ? ' warn' : '') + '">' + esc(b.quota) + '</div></div>' : '') +
          '<div class="wc-po-actions">' + (b.actions || [{ t: '提交订单', c: 'btn-confirm' }]).map(function (a) {
            return '<span class="btn ' + a.c + (a.disabled ? ' disabled' : '') + '">' + esc(a.t) + '</span>';
          }).join('') + '</div>';
        return lf;
      case 'form-step':
        // 下单向导：每一步是独立页面（push 导航），顶部为「第 N 步 / 共 M 步」指示而非 tab 切换
        var fs2 = document.createElement('div');
        fs2.className = 'wc-form-step';
        var total = b.total || 4;
        fs2.innerHTML =
          '<div class="fs-head">' +
          '  <span class="fs-count">第 ' + esc(b.step || 1) + ' 步 / 共 ' + total + ' 步</span>' +
          '  <span class="fs-title">' + esc(b.title || '') + '</span>' +
          '</div>' +
          (b.desc ? '<div class="fs-desc">' + esc(b.desc) + '</div>' : '') +
          (b.locked ? '<div class="fs-locked">🔒 ' + esc(b.locked) + '</div>' : '') +
          '<div class="wc-detail">' +
          (b.fields || []).map(function (fd) {
            if (fd.type === 'check') {
              return '<div class="wc-form-row"><label>' + esc(fd.label) + '</label><label class="check-row' + (fd.ro ? ' ro' : '') + '"><input type="checkbox" checked /> <span>' + esc(fd.value || '') + '</span></label></div>';
            }
            return '<div class="wc-form-row"><label>' + esc(fd.label) + '</label><input value="' + esc(fd.value || '') + '"' + (fd.ro ? ' readonly class="ro"' : '') + (fd.err ? (fd.ro ? ' class="ro err"' : ' class="err"') : '') + ' /></div>';
          }).join('') +
          (b.tip ? '<div class="wc-tip' + (b.tipDanger ? ' danger' : '') + '">⚠ ' + esc(b.tip) + '</div>' : '') +
          (b.quota ? '<div class="wc-form-row"><div class="quota' + (b.quotaWarn ? ' warn' : '') + '">' + esc(b.quota) + '</div></div>' : '') +
          '</div>' +
          (b.risk ? '<div class="wc-risk' + (b.riskDanger ? ' danger' : ' warn') + '"><div class="rk-head"><span class="rk-badge">' + esc(b.risk.icon || '✓') + '</span><span class="rk-title">' + esc(b.risk.title || '') + '</span></div><div class="rk-items">' +
            (b.risk.items || []).map(function (r) {
              return '<div class="rk-item"><span class="mk">✓</span><span class="text">' + esc(r) + '</span></div>';
            }).join('') + '</div></div>' : '') +
          '<div class="wc-po-actions">' + (b.actions || [{ t: '提交', c: 'btn-confirm' }]).map(function (a) {
            return '<span class="btn ' + a.c + (a.disabled ? ' disabled' : '') + '">' + esc(a.t) + '</span>';
          }).join('') + '</div>';
        return fs2;
      case 'form':
        var f = document.createElement('div');
        var steps = (b.steps || ['客户', '风控', '合同', '明细', '提交']);
        var active = b.active || 0;
        f.innerHTML =
          '<div class="wc-steps">' + steps.map(function (s, i) {
            return '<span class="pill' + (i === active ? ' active' : '') + '">' + esc(s) + '</span>';
          }).join('') + '</div>' +
          '<div class="wc-detail">' +
          '  <div class="od-head"><span class="od-no">' + esc(b.title || '') + '</span></div>' +
          (b.fields || []).map(function (fd) {
            return '<div class="wc-form-row"><label>' + esc(fd.label) + '</label><input value="' + esc(fd.value || '') + '"' + (fd.err ? ' class="err"' : '') + ' /></div>';
          }).join('') +
          (b.tip ? '<div class="wc-tip' + (b.tipDanger ? ' danger' : '') + '">⚠ ' + esc(b.tip) + '</div>' : '') +
          (b.quota ? '<div class="wc-form-row"><div class="quota' + (b.quotaWarn ? ' warn' : '') + '">' + esc(b.quota) + '</div></div>' : '') +
          '</div>' +
          '<div class="wc-po-actions">' + (b.actions || [{ t: '提交', c: 'btn-confirm' }]).map(function (a) {
            return '<span class="btn ' + a.c + (a.disabled ? ' disabled' : '') + '">' + esc(a.t) + '</span>';
          }).join('') + '</div>';
        return f;
      case 'detail':
        var dt = document.createElement('div');
        var rjHtml = b.reject
          ? '<div class="od-reject"><div class="rj-head">↻ 驳回待补充</div>' +
            '<div class="rj-row"><span class="k">驳回人</span><span class="v">' + esc(b.reject.by || '') + '</span></div>' +
            '<div class="rj-row"><span class="k">驳回时间</span><span class="v">' + esc(b.reject.time || '') + '</span></div>' +
            '<div class="rj-row"><span class="k">驳回原因</span><span class="v">' + esc(b.reject.reason || '') + '</span></div>' +
            (b.reject.fix ? '<div class="rj-fix"><div class="rj-fix-title">建议修改字段（AI 解析驳回意见）</div>' +
              '<div class="rj-row"><span class="k">' + esc(b.reject.fix.label || '') + '</span><span class="v"><span class="orig">' + esc(b.reject.fix.orig || '') + '</span> → <span class="suggest">' + esc(b.reject.fix.suggest || '') + '</span></span></div>' +
              (b.reject.fix.basis ? '<div class="rj-basis">依据：' + esc(b.reject.fix.basis) + '</div>' : '') + '</div>' : '') +
            '</div>'
          : '';
        dt.innerHTML =
          '<div class="wc-detail">' +
          (b.back ? '<div class="od-back">‹ ' + esc(b.back) + '</div>' : '') +
          rjHtml +
          '  <div class="od-head"><span class="od-no">' + esc(b.no || '') + '</span><span class="od-state"><span class="state st-' + esc(b.stateCls || 'wait') + (b.stateEmphasis ? ' state-emphasis' : '') + '">' + esc(b.state || '') + '</span></span></div>' +
          '  <div class="od-kv">' + (b.kv || []).map(function (r) {
            return '<div class="row"><span class="k">' + esc(r.k) + '</span><span class="v">' + esc(r.v) + '</span></div>';
          }).join('') + '</div>' +
          (b.form ? (b.form.fields || []).map(function (fd) {
            var sub = (fd.orig ? '<div class="hint">原值 ' + esc(fd.orig) + (fd.suggest ? ' · 建议 ' + esc(fd.suggest) : '') + '</div>' : '') +
              (fd.basis ? '<div class="hint">依据：' + esc(fd.basis) + '</div>' : '');
            return '<div class="wc-form-row"><label>' + esc(fd.label) + '</label><input value="' + esc(fd.value || '') + '"' + (fd.err ? ' class="err"' : '') + ' />' + sub + '</div>';
          }).join('') : '') +
          (b.tl4 ? tl4Html(b.tl4) : '') +
          (b.tip ? '<div class="wc-tip' + (b.tipDanger ? ' danger' : '') + '">⚠ ' + esc(b.tip) + '</div>' : '') +
          '</div>' +
          (b.risk ? '<div class="wc-risk warn"><div class="rk-head"><span class="rk-badge">⚠</span><span class="rk-title">' + esc(b.risk.title || '') + '</span></div><div class="rk-items">' +
            (b.risk.items || []).map(function (r) {
              return '<div class="rk-item"><span class="mk warn">!</span><span class="text">' + esc(r) + '</span></div>';
            }).join('') + '</div></div>' : '') +
          (b.actions ? '<div class="wc-po-actions">' + b.actions.map(function (a) {
            return '<span class="btn ' + a.c + '">' + esc(a.t) + '</span>';
          }).join('') + '</div>' : '');
        return dt;
      case 'ok-line':
        var ol = document.createElement('div');
        ol.className = 'wc-approval';
        ol.innerHTML =
          '<span class="ap-icon">✓</span>' +
          '<div class="ap-main"><div class="ap-title">✅ ' + esc(b.title) + '</div><div class="ap-sub">' + esc(b.sub || '') + '</div></div>' +
          '<span class="ap-arrow">›</span>';
        return ol;
      case 'customer-order-home':
        // 客户订单首页：公司信息 + 汇总 + 状态筛选 + 订单卡列表
        var oh = document.createElement('div');
        oh.className = 'wc-oh';
        var ohRows = (b.items || []).map(function (o) {
          var meta = [o.wh ? ('备货仓 ' + o.wh) : '', o.eta ? ('预计 ' + o.eta) : ''].filter(function (x) { return x; });
          return '<div class="wc-order">' +
            '<div class="r1"><span class="no">' + esc(o.no) + '</span><span class="state st-' + esc(o.stateCls || 'wait') + '">' + esc(o.state) + '</span></div>' +
            '<div class="r2"><span class="prod">' + esc(o.prod) + '</span><span>' + esc(o.qty || '') + '</span></div>' +
            '<div class="r3"><span class="amount">' + esc(o.amount || '—') + '</span><span class="progress">' + esc(o.progress || '') + '</span></div>' +
            (meta.length ? '<div class="r4">' + meta.join(' · ') + '</div>' : '') +
            (o.tl4c !== undefined && o.tl4c !== null ? tl4Mini(o.tl4c) : '') +
            '</div>';
        }).join('');
        oh.innerHTML =
          '<div class="wc-oh-head"><span class="wc-oh-company">' + esc(b.company || '') + '</span><span class="wc-oh-back">订单首页</span></div>' +
          (b.companySub ? '<div class="wc-oh-sub">' + esc(b.companySub) + '</div>' : '') +
          '<div class="wc-oh-summary">' + (b.summary || []).map(function (s) {
            return '<div class="wc-oh-stat"><b>' + esc(s.n) + '</b><span>' + esc(s.label) + '</span></div>';
          }).join('') + '</div>' +
          '<div class="wc-oh-filter">' + (b.filters || []).map(function (f) {
            return '<span class="wc-oh-chip' + (f.active ? ' on' : '') + '">' + esc(f.label) + (f.n !== undefined ? ' <b>' + esc(f.n) + '</b>' : '') + '</span>';
          }).join('') + '</div>' +
          '<div class="wc-oh-list">' + ohRows + '</div>';
        return oh;
      default:
        return null;
    }
  }

  // ===== AI 时间线 =====
  function timelineBox() {
    return $('tl-scroll');
  }

  // 由注解生成 Agent 工具链（与 renheng 导出演示一致：collectContext → llmReason → updateFields…）
  function buildToolChain(ann, msg) {
    var tools = [];
    if (!ann) return tools;
    var log = ann.agentLog || {};
    var trigger = msg ? msg.content : '';
    tools.push({
      phase: 'observe', icon: '☁', tool: 'collectContext',
      act: '收集当前上下文', out: '新消息: ' + (trigger || '—')
    });
    if (log.updates && log.updates.length) {
      tools.push({ phase: 'observe', icon: '▤', tool: 'readSystemFields', act: '读取系统字段', out: log.updates.length + ' 项字段待比对' });
    }
    tools.push({
      phase: 'reason', icon: '◎', tool: 'llmReason',
      act: 'LLM 综合分析',
      out: log.analysis || '对话正常推进，无需变更'
    });
    if (log.updates && log.updates.length) {
      tools.push({ phase: 'execute', icon: '✎', tool: 'updateFields', act: '更新业务状态', out: log.updates.join('；') });
    }
    if (log.stageChange) {
      tools.push({ phase: 'execute', icon: '➤', tool: 'advanceGate', act: '推进流程闸门', out: log.stageChange });
    }
    if (log.recommendedAction) {
      tools.push({ phase: 'execute', icon: '→', tool: 'generateSuggestion', act: '输出下一步建议', out: log.recommendedAction });
    }
    return tools;
  }

  function renderTimeline(ann, backMode) {
    var box = timelineBox();
    if (!box) return;
    if (backMode) {
      // 回退模式：重放全部已显示消息的链（无注解步沿用上一条，与导出演示一致）
      box.innerHTML = '';
      var acc = null;
      for (var i = 0; i < idx; i++) {
        var a = cur.annotations && cur.annotations[i] ? cur.annotations[i] : null;
        acc = a || acc;
        appendChain(box, acc, cur.messages[i], i);
      }
      if (idx === 0) box.innerHTML = '<div class="tl-empty">等待对话开始…</div>';
      return;
    }
    // 步进模式：直接追加本次链
    if (ann) {
      appendChain(box, ann, cur.messages[idx - 1], idx - 1);
    }
    box.scrollTop = box.scrollHeight;
  }

  function appendChain(box, ann, msg, i) {
    var tools = buildToolChain(ann, msg);
    tools.forEach(function (t, j) {
      var el = document.createElement('div');
      el.className = 'tl-item';
      el.innerHTML =
        '<span class="dot ' + t.phase + '">' + esc(t.icon) + '</span>' +
        '<div class="head">' +
        '  <span class="phase ' + t.phase + '">' + t.phase.toUpperCase() + '</span>' +
        '  <span>' + esc(t.act) + '</span>' +
        '  <span class="tool">' + esc(t.tool || '') + '</span>' +
        '</div>' +
        '<div class="body"><span class="out">' + esc(t.out) + '</span></div>' +
        '<div class="time">' + fmtClock(i * 2 + j) + '</div>';
      box.appendChild(el);
    });
  }

  // ===== 注解面板 =====
  function annoBox() {
    return $('annos-scroll');
  }

  function renderAnnos(ann) {
    var box = annoBox();
    if (!box) return;
    box.innerHTML = '';
    if (!ann) {
      box.innerHTML = '<div class="anno-empty">等待对话开始…</div>';
      return;
    }
    var cards = [];
    if (ann.todo) cards.push({ title: '待确认事项', ic: 'orange', cls: 'orange', body: ann.todo });
    if (ann.profile) cards.push({ title: '业务状态更新', ic: 'blue', cls: 'blue', body: ann.profile });
    if (ann.fields && Object.keys(ann.fields).length) {
      cards.push({ title: '系统字段', ic: 'green', cls: 'green', fields: ann.fields });
    }
    if (ann.progress) {
      cards.push({ title: '项目推进状态', ic: 'purple', cls: 'purple', progress: ann.progress });
    }
    if (ann.flows && ann.flows.length) {
      cards.push({ title: '流程动作', ic: 'green', cls: 'green', flows: ann.flows });
    }
    if (ann.cards && ann.cards.length) {
      cards.push({ title: '知识卡推荐', ic: 'blue', cls: 'blue', chips: ann.cards });
    }
    if (ann.nextAction) {
      cards.push({ title: '下一步建议', ic: 'orange', cls: 'orange', next: ann.nextAction });
    }
    cards.forEach(function (c) {
      var el = document.createElement('div');
      el.className = 'anno-card';
      el.innerHTML = '<h3><span class="ic ' + c.cls + '">' + (c.ic === 'orange' ? '!' : c.ic === 'purple' ? '◈' : '✓') + '</span>' + esc(c.title) + '</h3>' + bodyOf(c);
      box.appendChild(el);
    });
  }

  function bodyOf(c) {
    if (c.body) return '<div class="body">' + esc(c.body) + '</div>';
    if (c.fields) {
      var rows = Object.keys(c.fields).map(function (k) {
        var f = c.fields[k];
        var v = f.value ? f.value : '—';
        var cls = f.status === 'updated' ? 'updated' : (f.status === 'empty' ? 'empty' : '');
        return '<div class="row"><span class="k">' + esc(k) + '</span><span class="v ' + cls + '">' + esc(v) + '</span></div>';
      }).join('');
      return '<div class="kv">' + rows + '</div>';
    }
    if (c.progress) {
      var p = c.progress;
      var html = '<div class="body">当前阶段：<span class="hl">' + esc(p.currentStage || '—') + '</span></div><div class="kv">';
      (p.completed || []).forEach(function (t) {
        html += '<div class="prog-item"><span class="mk done">✓</span><span class="t">' + esc(t) + '</span></div>';
      });
      (p.pending || []).forEach(function (t) {
        html += '<div class="prog-item"><span class="mk pend">·</span><span class="t pend">' + esc(t) + '</span></div>';
      });
      if (p.stageChange) {
        html += '<div class="prog-item"><span class="mk cur">➤</span><span class="t cur">' + esc(p.stageChange) + '</span></div>';
      }
      return html + '</div>';
    }
    if (c.flows) {
      return '<div class="body">' + c.flows.map(function (f) { return '➤ ' + esc(f); }).join('\n') + '</div>';
    }
    if (c.chips) {
      return '<div class="body">' + c.chips.map(function (k, i) {
        return '<span class="tag-chip' + (i % 2 ? ' warn' : '') + '">' + esc(k) + '</span>';
      }).join('') + '</div>';
    }
    if (c.next) {
      return '<div class="next"><b>' + esc(c.next.suggestion || '') + '</b>' +
        (c.next.reason ? '<br/>' + esc(c.next.reason) : '') + '</div>';
    }
    return '';
  }

  // ===== 导出 =====
  global.DemoEngine = {
    init: init,
    selectRole: selectRole,
    stepForward: stepForward,
    stepBack: stepBack,
    resetAll: resetAll,
    selectScenario: selectScenario
  };

})(typeof window !== 'undefined' ? window : globalThis);
