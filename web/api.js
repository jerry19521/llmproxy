// ===== API 请求封装 =====
const API = {
  token: localStorage.getItem('llmproxy_token') || '',

  setToken(t) {
    this.token = t || '';
    if (t) localStorage.setItem('llmproxy_token', t);
    else localStorage.removeItem('llmproxy_token');
  },

  async request(method, path, body, raw) {
    const opts = { method, headers: {} };
    if (this.token) opts.headers['Authorization'] = 'Bearer ' + this.token;
    if (body !== undefined && body !== null) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = typeof body === 'string' ? body : JSON.stringify(body);
    }
    const resp = await fetch(path, opts);
    if (raw) return resp;
    let data = null;
    const text = await resp.text();
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = { raw: text }; }
    if (resp.status === 401 && !path.includes('/api/login')) {
      API.setToken('');
      location.href = '/login';
      throw new Error('未登录或登录已过期');
    }
    if (!resp.ok) {
      const msg = data && data.error ? (data.error.message || JSON.stringify(data.error)) : ('HTTP ' + resp.status);
      const err = new Error(msg);
      err.status = resp.status;
      err.code = data && data.error ? data.error.code : '';
      throw err;
    }
    return data;
  },

  get(path) { return this.request('GET', path); },
  post(path, body) { return this.request('POST', path, body); },
  put(path, body) { return this.request('PUT', path, body); },
  del(path) { return this.request('DELETE', path); },
};

// ===== 通用工具 =====
const U = {
  esc(s) {
    if (s === null || s === undefined) return '';
    return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  },
  fmtTime(iso) {
    if (!iso) return '-';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    const p = n => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  },
  fmtNum(n) {
    if (n === null || n === undefined) return '0';
    return Number(n).toLocaleString('zh-CN');
  },
  fmtPct(v) {
    if (v === null || v === undefined) return '-';
    return (v * 100).toFixed(1) + '%';
  },
  randomKey() {
    // 拒绝采样（rejection sampling）消除取模偏差：256 % 62 = 8，
    // 朴素 b % 62 会使前 8 个字符出现概率偏高约 1.6%（L14 修复，与后端 randomString 同口径）
    const cs = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
    const limit = 248; // 62 的最大整数倍且 ≤ 256，保证整除均匀
    let s = '';
    while (s.length < 32) {
      const arr = new Uint8Array(32);
      crypto.getRandomValues(arr);
      for (const b of arr) {
        if (b >= limit) continue; // 拒绝高偏差区间
        s += cs[b % cs.length];
        if (s.length === 32) break;
      }
    }
    return 'sk-' + s;
  },
  // copyText 复制文本到剪贴板（v1.8.3，F3 修复）：navigator.clipboard 仅在安全上下文
  // （https / localhost）可用，局域网 http://IP:9080 访问时不存在——降级为临时 textarea +
  // execCommand 兼容方案，复制功能不再完全失效。
  async copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      try { await navigator.clipboard.writeText(text); return true; } catch (e) { /* 降级 */ }
    }
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.focus(); ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      return ok;
    } catch (e) { return false; }
  },
  // fmtDateLocal ISO 时间 → 本地日期 YYYY-MM-DD（v1.8.3，F11 修复）：
  // 有效期回填应按本地时区取日期，直接 slice(0,10) 取 UTC 日期会在西时区漂移一天。
  fmtDateLocal(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso).slice(0, 10);
    const p = n => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  },
};
