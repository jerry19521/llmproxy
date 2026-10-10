// ===== 通用 UI 组件 =====

// 全局视图命名空间（各 views-*.js 挂载于此）
const Views = {};

function showToast(msg, type) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast ' + (type || '');
  clearTimeout(t._timer);
  t._timer = setTimeout(() => t.classList.add('hidden'), 2600);
}

const Modal = {
  // 当前弹窗是否允许点击遮罩关闭（默认允许，可通过 open 的 opts.maskClose = false 禁用）
  maskClose: true,
  open(title, bodyHTML, large, opts) {
    this.maskClose = !(opts && opts.maskClose === false);
    document.getElementById('modal-title').textContent = title;
    document.getElementById('modal-body').innerHTML = bodyHTML;
    const box = document.getElementById('modal-box');
    box.className = large ? 'modal modal-lg' : 'modal';
    if (opts && opts.xl) box.classList.add('modal-xl'); // v1.0.0：调用详情等宽内容弹窗（约 1100px）
    document.getElementById('modal-mask').classList.remove('hidden');
    document.body.classList.add('modal-open'); // v1.0.0：弹窗期间锁定背景页面滚动，避免双滚动条干扰
  },
  close() {
    document.getElementById('modal-mask').classList.add('hidden');
    document.getElementById('modal-body').innerHTML = '';
    document.body.classList.remove('modal-open');
  },
};

function badgeEnabled(on) {
  return on ? '<span class="badge on">启用</span>' : '<span class="badge off">停用</span>';
}
function badgeStatus(s) {
  if (s === 'success') return '<span class="badge on">成功</span>';
  return '<span class="badge off">失败</span>';
}
function badgeKeyTest(ok) {
  return ok ? '<span class="badge on">成功</span>' : '<span class="badge off">失败</span>';
}

function confirmDialog(msg) {
  return window.confirm(msg);
}

// 渲染键值对详情
function kvHTML(pairs) {
  return '<div class="kv">' + pairs.map(([k, v]) =>
    `<div class="k">${U.esc(k)}</div><div>${v}</div>`).join('') + '</div>';
}

// 格式化 JSON 或原文
function prettyJSON(s, maxLen) {
  if (!s) return '(空)';
  try {
    const obj = JSON.parse(s);
    let out = JSON.stringify(obj, null, 2);
    if (maxLen && out.length > maxLen) out = out.slice(0, maxLen) + '\n...(截断)';
    return out;
  } catch (e) {
    let out = s;
    if (maxLen && out.length > maxLen) out = out.slice(0, maxLen) + '\n...(截断)';
    return out;
  }
}

// headers JSON 字符串 → 排序文本
function headersText(hj) {
  if (!hj) return '(无)';
  try {
    const obj = JSON.parse(hj);
    return Object.keys(obj).sort().map(k => `${k}: ${obj[k]}`).join('\n');
  } catch (e) { return hj; }
}
