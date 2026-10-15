// ==========================================================================
// WeCom Chat Helpers
// 企微聊天组件运行时工具：消息构造、时间分隔、打字气泡、SVG 图标库
// ==========================================================================

// ---------- SVG Icon Library（替代 emoji）----------
const WC_ICONS = {
  user: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="8" r="4"/><path d="M4 21c0-4.418 3.582-8 8-8s8 3.582 8 8"/></svg>',
  phone: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72c.13.96.37 1.9.72 2.81a2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45c.91.35 1.85.59 2.81.72a2 2 0 0 1 1.72 2z"/></svg>',
  video: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="m22 8-6 4 6 4V8Z"/><rect width="14" height="12" x="2" y="6" rx="2"/></svg>',
  mic: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="2" width="6" height="11" rx="3"/><path d="M19 10a7 7 0 0 1-14 0"/><path d="M12 19v3"/></svg>',
  emoji: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M8 14s1.5 2 4 2 4-2 4-2"/><circle cx="9" cy="10" r="0.5" fill="currentColor"/><circle cx="15" cy="10" r="0.5" fill="currentColor"/></svg>',
  file: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>',
  clip: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 17.93 8.8l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>',
  image: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="18" x="3" y="3" rx="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21"/></svg>',
  scissors: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="6" r="3"/><path d="M8.12 8.12 12 12"/><path d="M20 4 8.12 15.88"/><circle cx="6" cy="18" r="3"/><path d="M14.8 14.8 20 20"/></svg>',
  history: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/><path d="M12 7v5l4 2"/></svg>',
  search: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/></svg>',
  more: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/></svg>',
  sidebar: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/></svg>',
  send: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="m22 2-7 20-4-9-9-4Z"/><path d="M22 2 11 13"/></svg>',
  takeover: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 9-9"/><path d="M21 3v6h-6"/><path d="M12 7v5l3 2"/></svg>',
  read: '<svg viewBox="0 0 16 12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><polyline points="2 6 5 9 10 3"/><polyline points="6 9 9 12 14 4"/></svg>',
  unread: '<svg viewBox="0 0 16 12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 7 7 11 13 4"/></svg>',
  store: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="m2 7 1.5-4.5A2 2 0 0 1 5.4 1h13.2a2 2 0 0 1 1.9 1.5L22 7"/><path d="M2 7v2a3 3 0 0 0 6 0V7"/><path d="M8 7v2a3 3 0 0 0 6 0V7"/><path d="M14 7v2a3 3 0 0 0 6 0V7"/><path d="M4 11v9h16v-9"/><path d="M10 20v-5h4v5"/></svg>',
  alertCircle: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/></svg>',
  sparkles: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z"/></svg>',
  lightbulb: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M15 14c.2-1 .7-1.7 1.5-2.5 1-.9 1.5-2.2 1.5-3.5A6 6 0 0 0 6 8c0 1 .2 2.2 1.5 3.5.7.7 1.2 1.5 1.4 2.5"/><path d="M9 18h6"/><path d="M10 22h4"/></svg>',
  calendar: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="18" rx="2"/><path d="M16 2v4"/><path d="M8 2v4"/><path d="M3 10h18"/></svg>',
  tag: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M12.586 2.586A2 2 0 0 0 11.172 2H4a2 2 0 0 0-2 2v7.172a2 2 0 0 0 .586 1.414l8.704 8.704a2.426 2.426 0 0 0 3.42 0l6.58-6.58a2.426 2.426 0 0 0 0-3.42z"/><circle cx="7.5" cy="7.5" r="1.5"/></svg>',
};

// ---------- 时间分隔 ----------
function wcTimeDivider(label) {
  const el = document.createElement('div');
  el.className = 'wc-time-divider';
  el.textContent = label;
  return el;
}

function wcSystemTip(html) {
  const el = document.createElement('div');
  el.className = 'wc-system-tip';
  el.innerHTML = html;
  return el;
}

// ---------- 单条消息 ----------
/**
 * @param {Object} cfg
 * @param {'self'|'other'} cfg.side
 * @param {string} cfg.name
 * @param {string} cfg.role - 角色徽标（如 "客户" / "AI" / "新人"）
 * @param {string} cfg.avatar - 头像文字（取首字）
 * @param {string} cfg.colorClass - 头像配色类（c1~c8）
 * @param {string|HTMLElement} cfg.content - 文本或 dom
 * @param {Object} [cfg.quote] - { from, text }
 * @param {string} [cfg.time] - 时间戳
 * @param {'sent'|'read'|'sending'} [cfg.status]
 * @param {'gold'|'loss'|'turn'} [cfg.mark]
 * @param {string} [cfg.markLabel]
 * @param {'text'|'file'|'image'|'voice'|'typing'} [cfg.type]
 * @param {Object} [cfg.file] - { name, size, ext }
 * @param {string} [cfg.imageUrl]
 * @param {number} [cfg.voiceSec]
 */
function wcMessage(cfg) {
  const wrap = document.createElement('div');
  wrap.className = `wc-msg ${cfg.side === 'self' ? 'right' : ''} ${cfg.mark ? 'has-mark mark-' + cfg.mark : ''}`;

  // 头像
  const avatar = document.createElement('div');
  avatar.className = `wc-avatar wc-msg-avatar ${cfg.colorClass || 'c1'}`;
  if (cfg.online) avatar.dataset.online = '1';
  avatar.textContent = cfg.avatar || (cfg.name || '').slice(0, 1);

  // 主体
  const body = document.createElement('div');
  body.className = 'wc-msg-body';

  if (cfg.name) {
    const nameLine = document.createElement('div');
    nameLine.className = 'wc-msg-name';
    nameLine.innerHTML = `<span>${cfg.name}</span>${cfg.role ? `<span class="wc-role">${cfg.role}</span>` : ''}`;
    body.appendChild(nameLine);
  }

  // 气泡
  const bubble = document.createElement('div');
  bubble.className = 'wc-bubble';

  if (cfg.type === 'typing') {
    bubble.classList.add('is-typing');
    bubble.innerHTML = '<span></span><span></span><span></span>';
  } else if (cfg.type === 'file') {
    bubble.classList.add('is-file');
    const ext = (cfg.file?.ext || 'PDF').toUpperCase();
    bubble.innerHTML = `
      <div class="wc-file-ico">${ext}</div>
      <div class="wc-file-meta">
        <div class="wc-file-name">${cfg.file?.name || '附件'}</div>
        <div class="wc-file-sub">${cfg.file?.size || ''}</div>
      </div>
    `;
  } else if (cfg.type === 'image') {
    bubble.classList.add('is-img');
    bubble.innerHTML = `<img src="${cfg.imageUrl}" alt="" loading="lazy" />`;
  } else if (cfg.type === 'voice') {
    bubble.classList.add('is-voice');
    const bars = Array.from({ length: 9 }).map((_, i) => `<i style="height:${4 + (i % 5) * 2}px"></i>`).join('');
    bubble.innerHTML = `<div class="wc-voice-bar">${bars}</div><span>${cfg.voiceSec || 12}″</span>`;
  } else {
    if (cfg.quote) {
      const q = document.createElement('div');
      q.className = 'wc-quote';
      q.innerHTML = `<span class="qfrom">${cfg.quote.from}：</span>${cfg.quote.text}`;
      bubble.appendChild(q);
    }
    const text = document.createElement('div');
    if (typeof cfg.content === 'string') {
      text.innerHTML = cfg.content;
    } else if (cfg.content instanceof HTMLElement) {
      text.appendChild(cfg.content);
    }
    bubble.appendChild(text);
  }

  body.appendChild(bubble);

  // 标签（金句/转折/流失）
  if (cfg.mark && cfg.markLabel) {
    const tag = document.createElement('div');
    tag.className = `wc-msg-tag ${cfg.mark}`;
    tag.textContent = cfg.markLabel;
    body.appendChild(tag);
  }

  // 底部信息
  if (cfg.time || cfg.status) {
    const foot = document.createElement('div');
    foot.className = 'wc-msg-foot';
    if (cfg.side === 'self' && cfg.status) {
      const st = document.createElement('span');
      st.className = `wc-read ${cfg.status === 'read' ? 'read' : ''}`;
      st.innerHTML = (cfg.status === 'read' ? WC_ICONS.read : WC_ICONS.unread) + (cfg.status === 'read' ? '已读' : '已送达');
      foot.appendChild(st);
    }
    if (cfg.time) {
      const t = document.createElement('span');
      t.textContent = cfg.time;
      foot.appendChild(t);
    }
    body.appendChild(foot);
  }

  wrap.appendChild(avatar);
  wrap.appendChild(body);
  return wrap;
}

// ---------- 撤回提示 ----------
function wcRecalled(name) {
  const el = document.createElement('div');
  el.className = 'wc-recalled';
  el.textContent = `${name} 撤回了一条消息`;
  return el;
}

// ---------- 滚动到底 ----------
function wcScrollBottom(container) {
  if (!container) return;
  requestAnimationFrame(() => {
    container.scrollTop = container.scrollHeight;
  });
}

// ---------- 异步追加（带打字气泡） ----------
async function wcSendWithTyping(container, typingCfg, finalCfg, delay = 900) {
  const typing = wcMessage({ ...typingCfg, type: 'typing' });
  container.appendChild(typing);
  wcScrollBottom(container);
  await new Promise(r => setTimeout(r, delay));
  typing.remove();
  const real = wcMessage(finalCfg);
  container.appendChild(real);
  wcScrollBottom(container);
  return real;
}

// ---------- 工具：快速生成头像首字 ----------
function wcAvatarChar(name) {
  if (!name) return '?';
  // 取最后一字（中文姓名常见做法），兼容英文
  return name.length > 2 ? name.slice(-1) : name.slice(0, 1);
}

// 暴露到全局
window.WC = {
  ICONS: WC_ICONS,
  message: wcMessage,
  timeDivider: wcTimeDivider,
  systemTip: wcSystemTip,
  recalled: wcRecalled,
  scrollBottom: wcScrollBottom,
  sendWithTyping: wcSendWithTyping,
  avatarChar: wcAvatarChar,
};
