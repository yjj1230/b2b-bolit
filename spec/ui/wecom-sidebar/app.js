// AI-Sale Demo 系统 - 公共脚本

// ===== 工具函数 =====
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => document.querySelectorAll(sel);

// 防抖
function debounce(fn, wait = 300) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), wait);
  };
}

// 模拟打字机效果
async function typeWriter(el, text, speed = 20) {
  el.textContent = '';
  for (let i = 0; i < text.length; i++) {
    el.textContent += text[i];
    await new Promise(r => setTimeout(r, speed));
  }
}

// 模拟AI思考延迟
function mockDelay(ms = 800) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

// 加载JSON数据
async function loadData(name) {
  try {
    const res = await fetch(`/demo/data/${name}.json`);
    return await res.json();
  } catch (e) {
    console.warn(`加载数据失败: ${name}`, e);
    return {};
  }
}

// 创建消息气泡
function createMessageBubble(content, sender = 'ai') {
  const div = document.createElement('div');
  div.className = `chat-message ${sender}`;
  const avatarText = sender === 'ai' ? 'AI' : '我';
  div.innerHTML = `
    <div class="chat-avatar ${sender}">${avatarText}</div>
    <div class="chat-bubble">${content}</div>
  `;
  return div;
}

// 创建加载中消息
function createLoadingBubble() {
  const div = document.createElement('div');
  div.className = 'chat-message ai';
  div.id = 'loading-msg';
  div.innerHTML = `
    <div class="chat-avatar ai">AI</div>
    <div class="chat-bubble">
      <span class="loading-dot"></span>
      <span class="loading-dot" style="animation-delay:0.2s"></span>
      <span class="loading-dot" style="animation-delay:0.4s"></span>
    </div>
  `;
  return div;
}

// 移除加载中消息
function removeLoadingBubble() {
  const el = document.getElementById('loading-msg');
  if (el) el.remove();
}

// 滚动到底部
function scrollToBottom(el) {
  el.scrollTop = el.scrollHeight;
}

// 复制到剪贴板
async function copyToClipboard(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = document.createElement('textarea');
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
    return true;
  }
}

// 导出CSV
function exportCSV(data, filename = 'export.csv') {
  const headers = Object.keys(data[0]);
  const csv = [
    headers.join(','),
    ...data.map(row => headers.map(h => {
      const v = row[h] ?? '';
      return `"${String(v).replace(/"/g, '""')}"`;
    }).join(','))
  ].join('\n');

  const blob = new Blob(['\uFEFF' + csv], { type: 'text/csv;charset=utf-8;' });
  const link = document.createElement('a');
  link.href = URL.createObjectURL(blob);
  link.download = filename;
  link.click();
}

// 场景信息
const SCENES = [
  { id: 1, name: '销售随身智能知识库', category: 'sales', desc: '产品参数、场景适配、竞品对比，一问即答', badge: '销售赋能', badgeClass: 'tag-blue' },
  { id: 2, name: '企微实时话术参谋', category: 'sales', desc: '侧边栏实时推荐话术，半自动辅助不替代人工', badge: '销售赋能', badgeClass: 'tag-blue' },
  { id: 3, name: '客户分层+智能SOP跟进', category: 'customer', desc: '自动标签分层，按阶段定时提醒触达', badge: '客户运营', badgeClass: 'tag-green' },
  { id: 4, name: '聊天记录AI复盘沉淀', category: 'asset', desc: '成交案例拆解，流失原因分析，话术自动沉淀', badge: '资产沉淀', badgeClass: 'tag-purple' },
  { id: 5, name: '客户个性化数据应答', category: 'customer', desc: '对接内部系统，实时查客户专属订单/售后/进度', badge: '客户运营', badgeClass: 'tag-green' },
  { id: 6, name: 'AI新人模拟培训对练', category: 'sales', desc: 'AI扮演各类客户，实战对练+三维评分', badge: '销售赋能', badgeClass: 'tag-blue' },
  { id: 7, name: '线索智能初筛评级', category: 'customer', desc: '自动判定意向/预算/类型，优先推送高价值', badge: '客户运营', badgeClass: 'tag-green' },
  { id: 8, name: '行业方案智能生成', category: 'asset', desc: '按客户类型自动生成配套方案/案例/报价', badge: '资产沉淀', badgeClass: 'tag-purple' },
];

// 初始化导航
function initNav(title, badge, badgeClass) {
  const nav = document.getElementById('scene-nav');
  if (!nav) return;
  nav.innerHTML = `
    <div class="scene-nav-inner">
      <a href="../../index.html" class="scene-nav-back">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 12H5M12 19l-7-7 7-7"/></svg>
        返回首页
      </a>
      <div style="display:flex;align-items:center;gap:12px">
        <span class="scene-nav-title">${title}</span>
        <span class="scene-nav-badge ${badgeClass}">${badge}</span>
      </div>
      <div style="width:60px"></div>
    </div>
  `;
}

// Toast 提示
function toast(message, type = 'info') {
  const div = document.createElement('div');
  const colors = {
    info: 'background:#2563EB',
    success: 'background:#10B981',
    warning: 'background:#F59E0B',
    error: 'background:#EF4444'
  };
  div.style.cssText = `
    position:fixed; top:20px; right:20px; padding:12px 20px;
    ${colors[type]}; color:white; border-radius:8px; font-size:14px;
    z-index:9999; animation:fadeIn 0.3s ease; box-shadow:0 4px 12px rgba(0,0,0,0.15);
  `;
  div.textContent = message;
  document.body.appendChild(div);
  setTimeout(() => div.remove(), 3000);
}

// ===== Mock AI 响应（各场景共用）=====
const AI_RESPONSES = {
  // 场景1: 知识库
  kb: {
    default: `您好！我是您的智能厨具顾问。您可以问我：\n• 某型号的产品参数\n• 不同场景的配置推荐\n• 竞品对比\n• 报价咨询\n• 常见售后问题`,
    '型号': '我们目前主推四款机型：\n• YC-1000：适合小型创业店，2.98万\n• YC-2000：标准款快餐/食堂，4.28万\n• YC-3000：大型餐饮/连锁，6.98万\n• YC-C：定制款，根据需求报价',
    '食堂': '食堂场景推荐 YC-2000 × 2-3 台（视用餐人数），搭配自动洗锅模块。已服务 200+ 企业食堂，平均出餐效率提升 60%。',
    '竞品': '相比传统品牌，我们的核心优势：\n1. 温控精度 ±1℃，行业领先\n2. 菜谱云端更新，每月新增 50+ 道\n3. 质保 3 年，上门维护响应 < 4 小时\n4. 支持定制开发，对接食堂管理系统',
    '报价': '报价根据配置浮动：\n• 标准配置：YC-2000 + 操作台 + 基础排烟 = 4.98万\n• 高配方案：+自动洗锅 +智能调料 = 6.28万\n• 本月活动：标准配置立减 3000，再送 3 年延保',
  },
  // 场景6: 培训
  training: {
    intro: '👤 客户画像\n━━━━━━━━━━━━━━━━━━\n王老板 | 挑剔型连锁餐饮老板\n已有 3 家火锅店，想开第 4 家\n特点：懂行、比价、压价狠、决策慢',
    start: '王老板：你们这机器多少钱？\n\n【AI点评】⚠️ 直接报价风险高\n→ 过早报价容易进入比价模式\n→ 建议先了解场景和需求\n\n【标准答案】\n"王老板，价格根据配置不同 3-8 万都有。您这是新开店还是老店升级？面积多大？日均大概多少单？我先帮您匹配个最适合的方案。"',
    score: '📊 综合评分：72 / 100\n• 话术专业度：75（产品介绍准确）\n• 引导力：65（需求挖掘不够深）\n• 成交概率：70（过早报价扣分了）\n\n💡 改进建议：\n1. 前 3 轮不要报价，先挖需求\n2. 多问开放式问题\n3. 客户说"贵"时，用"人力成本"换算',
  }
};

// 简单的关键词匹配响应
function getAIResponse(scene, input) {
  const responses = AI_RESPONSES[scene] || {};
  for (const [key, value] of Object.entries(responses)) {
    if (input.includes(key)) return value;
  }
  return responses.default || '收到您的问题，让我为您查询一下...';
}
