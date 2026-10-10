// ===== 调用日志视图 =====
Views.Logs = {
  state: { page: 1, pageSize: 100 },
  detailSeq: 0, // v1.8.4（F2）：详情弹窗并发序号，防止慢的旧详情响应覆盖后打开的详情

  async render(el) {
    const seq = App.renderSeq; // M9（v1.7.6）：路由切换守卫
    el.innerHTML = '<div class="card"><div class="muted">加载中...</div></div>';
    const st = this.state;
    st.page = 1;
    // 下拉数据源（供应商 / 代理 Key）；加载失败不阻塞日志列表
    const [provRes, keyRes] = await Promise.allSettled([
      API.get('/api/providers'),
      API.get('/api/apikeys'),
    ]);
    if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应，避免覆盖新页面
    const providers = provRes.status === 'fulfilled' ? (provRes.value.providers || []) : [];
    const apikeys = keyRes.status === 'fulfilled' ? (keyRes.value.apikeys || []) : [];
    const provOpts = providers.map(p =>
      `<option value="${U.esc(p.name)}">${U.esc(p.name)}</option>`).join('');
    const keyOpts = apikeys.map(k => {
      const label = k.name ? `${k.name}（${k.key}）` : k.key;
      return `<option value="${U.esc(k.id)}">${U.esc(label)}</option>`;
    }).join('');
    el.innerHTML = `
      <div class="card">
        <div class="flex-between" style="margin-bottom:14px">
          <h3 style="margin-bottom:0">筛选</h3>
          <button class="btn btn-sm btn-ghost" id="lf-clear-all" style="color:var(--danger)">删除全部日志</button>
        </div>
        <div class="log-filter">
          <div class="log-filter-row">
            <label class="field w-sm"><span>状态</span>
              <select id="lf-status"><option value="">全部</option><option value="success">成功</option><option value="failure">失败</option></select>
            </label>
            <label class="field w-md"><span>供应商</span>
              <select id="lf-provider"><option value="">全部</option>${provOpts}</select>
            </label>
            <label class="field w-lg"><span>API Key</span>
              <select id="lf-apikey"><option value="">全部</option>${keyOpts}</select>
            </label>
            <label class="field f-grow"><span>模型</span><input type="text" id="lf-model" placeholder="模型名或别名"></label>
            <label class="field w-lg"><span>关键词</span><input type="text" id="lf-keyword" placeholder="搜索 Key / 模型 / 内容等"></label>
          </div>
          <div class="log-filter-row">
            <label class="field w-dt"><span>开始时间</span><input type="datetime-local" id="lf-start"></label>
            <label class="field w-dt"><span>结束时间</span><input type="datetime-local" id="lf-end"></label>
            <div class="f-spacer"></div>
            <div class="filter-actions">
              <button class="btn btn-primary" id="lf-search">查询</button>
              <button class="btn btn-ghost" id="lf-reset">重置</button>
            </div>
          </div>
        </div>
        <p class="form-hint" style="margin-top:8px">供应商与 API Key 下拉选项来自当前配置；已删除的供应商 / Key 不在下拉中，可通过关键词搜索其历史日志。</p>
      </div>
      <div class="card">
        <div class="table-wrap" id="logs-table"><div class="muted">加载中...</div></div>
        <div class="pagination" id="logs-pager"></div>
      </div>`;
    document.getElementById('lf-search').onclick = () => { st.page = 1; this.loadTable(); };
    document.getElementById('lf-reset').onclick = () => {
      ['lf-status', 'lf-provider', 'lf-apikey', 'lf-model', 'lf-keyword', 'lf-start', 'lf-end'].forEach(id => document.getElementById(id).value = '');
      st.page = 1;
      this.loadTable();
    };
    ['lf-status', 'lf-provider', 'lf-apikey'].forEach(id => {
      document.getElementById(id).onchange = () => { st.page = 1; this.loadTable(); };
    });
    document.getElementById('lf-clear-all').onclick = () => this.deleteAll();
    await this.loadTable();
  },

  // 速率显示：0 或缺失时显示 -
  fmtRate(v) {
    if (!v || v <= 0) return '-';
    return (v >= 100 ? Math.round(v) : v.toFixed(1)) + '';
  },

  async loadTable() {
    const st = this.state;
    const seq = App.renderSeq; // C10 请求序号守卫（v1.8.2）：旧响应不再覆盖新页面（与需求文档 v1.8.2 小节编号统一）
    const box = document.getElementById('logs-table');
    if (!box) return;
    const q = new URLSearchParams();
    q.set('page', st.page);
    q.set('page_size', st.pageSize);
    const status = document.getElementById('lf-status').value;
    const provider = document.getElementById('lf-provider').value;
    const apikey = document.getElementById('lf-apikey').value;
    const model = document.getElementById('lf-model').value.trim();
    const keyword = document.getElementById('lf-keyword').value.trim();
    const start = document.getElementById('lf-start').value;
    const end = document.getElementById('lf-end').value;
    if (status) q.set('status', status);
    if (provider) q.set('provider', provider);
    if (apikey) q.set('apikey_id', apikey);
    if (model) q.set('model', model);
    if (keyword) q.set('keyword', keyword);
    // 直接传 datetime-local 的本地时间值（后端按本地时区解析并归一化）。
    // 注意不要转 toISOString()：日志 timestamp 按本地时区 RFC3339 存储，
    // SQL 为字符串比较，UTC 串与本地串字典序不等于时间序，会导致筛选错乱。
    if (start) q.set('start_time', start);
    if (end) q.set('end_time', end);
    box.innerHTML = '<div class="muted">加载中...</div>';
    try {
      const res = await API.get('/api/logs?' + q.toString());
      if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应
      // 当前页删空且不是第一页时，回退到第一页重查
      if ((res.logs || []).length === 0 && st.page > 1) {
        st.page = 1;
        return this.loadTable();
      }
      const rows = (res.logs || []).map(l => `
        <tr>
          <td>${U.esc(U.fmtTime(l.timestamp))}</td>
          <td class="mono">${U.esc(l.apikey)}</td>
          <td>${U.esc(l.apikey_name || '-')}</td>
          <td>${U.esc(l.provider || '-')}</td>
          <td class="mono">${U.esc(l.requested_model)}</td>
          <td class="mono">${U.esc(l.actual_model || '-')}</td>
          <td>${badgeStatus(l.status)}</td>
          <td>${U.fmtNum(l.duration_ms)}</td>
          <td>${this.fmtRate(l.tokens_per_second)}</td>
          <td>${U.fmtNum(l.prompt_tokens)}</td>
          <td>${U.fmtNum(l.completion_tokens)}</td>
          <td>
            <button class="btn-link" data-log-id="${U.esc(l.id)}">查看</button>
            <button class="btn-link danger" data-log-del="${U.esc(l.id)}">删除</button>
          </td>
        </tr>`).join('');
      box.innerHTML = `
        <table class="data">
          <thead><tr><th>调用时间</th><th>API Key</th><th>Key 名称</th><th>供应商</th><th>调用模型</th><th>实际模型</th><th>状态</th><th>耗时(ms)</th><th>输出速率(tok/s)</th><th>请求Token</th><th>响应Token</th><th>操作</th></tr></thead>
          <tbody>${rows || '<tr><td colspan="12" class="muted">暂无日志</td></tr>'}</tbody>
        </table>`;
      box.querySelectorAll('button[data-log-id]').forEach(b => {
        b.onclick = () => this.showDetail(b.dataset.logId);
      });
      box.querySelectorAll('button[data-log-del]').forEach(b => {
        b.onclick = () => this.deleteOne(b.dataset.logDel);
      });
      // 分页
      const total = res.total || 0;
      const pages = Math.max(1, Math.ceil(total / st.pageSize));
      document.getElementById('logs-pager').innerHTML = `
        <span>共 ${U.fmtNum(total)} 条 · 第 ${st.page}/${pages} 页</span>
        <button class="btn btn-sm btn-ghost" id="pg-prev" ${st.page <= 1 ? 'disabled' : ''}>上一页</button>
        <button class="btn btn-sm btn-ghost" id="pg-next" ${st.page >= pages ? 'disabled' : ''}>下一页</button>`;
      const prev = document.getElementById('pg-prev');
      const next = document.getElementById('pg-next');
      if (prev) prev.onclick = () => { if (st.page > 1) { st.page--; this.loadTable(); } };
      if (next) next.onclick = () => { if (st.page < pages) { st.page++; this.loadTable(); } };
    } catch (e) {
      box.innerHTML = `<div class="muted">加载失败：${U.esc(e.message)}</div>`;
    }
  },

  // 删除单条日志
  deleteOne(id) {
    if (!confirmDialog('确认删除该条日志？该操作不可恢复。')) return;
    API.del(`/api/logs/${id}`).then(() => {
      showToast('已删除', 'ok');
      this.loadTable();
    }).catch(e => showToast('删除失败：' + e.message, 'err'));
  },

  // 清空全部日志（v1.8.1：后端删除日志数据库文件并重建，即时释放磁盘空间）
  deleteAll() {
    if (!confirmDialog('确认删除全部调用日志？将删除日志数据库文件并重新生成（不可恢复），以释放磁盘空间。')) return;
    API.del('/api/logs').then(res => {
      showToast(`已删除 ${U.fmtNum((res && res.deleted) || 0)} 条日志，数据库已重建`, 'ok');
      this.state.page = 1;
      this.loadTable();
    }).catch(e => showToast('删除失败：' + e.message, 'err'));
  },

  // ===== 调用详情（v1.8.0：三标签页 + 请求/响应体结构化解析）=====

  // 详情渲染器工厂：管理「显示全部 / 复制」全文注册表与事件绑定。
  // 全文存闭包数组（不进 DOM attribute），每个详情弹窗独立实例。
  makeRenderer() {
    const texts = [];
    let seq = 0;
    const reg = t => { texts.push(t); return texts.length - 1; };
    const R = {
      // JSON 代码块：语法高亮 + 超长折叠预览（前 6 万字符）+ 始终可复制
      code(text, opts) {
        const o = Object.assign({ maxLen: 60000, hl: true }, opts);
        if (text == null || text === '') return '<pre class="code">(空)</pre>';
        const idx = reg(text);
        const over = text.length > o.maxLen;
        let shown = text;
        if (over) shown = text.slice(0, o.maxLen) + '\n\n...(预览已截断，全文共 ' + text.length.toLocaleString() + ' 字符)';
        const inner = o.hl ? LogParser.highlightJSON(shown) : U.esc(shown);
        // v1.8.4（F8）：移除无样式定义的 jcode 死类（style.css 从未定义该选择器）
        return `<div class="code-block">
          <pre class="code"${over ? ` id="eb-${idx}"` : ''}>${inner}</pre>
          <div class="expand-actions">
            ${over ? `<button class="btn btn-sm btn-ghost" data-eb-expand="${idx}" data-eb-target="eb-${idx}">显示全部</button>` : ''}
            <button class="btn btn-sm btn-ghost" data-eb-copy="${idx}">复制</button>
          </div>
        </div>`;
      },

      // 长文本段（消息文本 / 工具描述）：超限折叠预览 + 展开 + 复制
      text(text, opts) {
        const o = Object.assign({ limit: 2000, cls: 'msg-text' }, opts);
        if (text == null || text === '') return '';
        const idx = reg(text);
        if (text.length <= o.limit) return `<div class="${o.cls}">${U.esc(text)}</div>`;
        const tid = 'eb-' + idx;
        const shown = U.esc(text.slice(0, o.limit)) +
          '\n\n…(已折叠，共 ' + text.length.toLocaleString() + ' 字符)';
        return `<div class="expand-block">
          <div class="${o.cls}" id="${tid}">${shown}</div>
          <div class="expand-actions">
            <button class="btn btn-sm btn-ghost" data-eb-expand="${idx}" data-eb-target="${tid}">展开全文</button>
            <button class="btn btn-sm btn-ghost" data-eb-copy="${idx}">复制全文</button>
          </div>
        </div>`;
      },

      // 图片段：外链 URL 仅展示文本；base64 内嵌图片注册后按需预览（不主动解码）
      image(p) {
        if (p.base64 && p.dataUrl) {
          const idx = reg(p.dataUrl);
          const meta = `内嵌图片${p.mime ? '（' + p.mime + '）' : ''}${p.kb ? ' · 约 ' + p.kb + ' KB' : ''}`;
          return `<div class="part-media"><span class="media-tag">图片</span>${U.esc(meta)}
            <button class="btn-link" data-img-toggle="${idx}">预览</button>
            <img class="b64img hidden" alt="内嵌图片预览"></div>`;
        }
        return `<div class="part-media"><span class="media-tag">图片</span><span class="mono">${U.esc(p.url || '')}</span>${p.detail ? `<span class="muted">detail: ${U.esc(p.detail)}</span>` : ''}</div>`;
      },

      // 可折叠容器（工具定义 / 其他字段）
      collapsible(headHTML, bodyHTML) {
        const tid = 'fold-' + (++seq);
        return `<div class="fold-box">
          <button class="fold-head" data-toggle="${tid}">${headHTML}<span class="fold-arrow">▸</span></button>
          <div class="fold-body hidden" id="${tid}">${bodyHTML}</div>
        </div>`;
      },

      // 绑定区块内全部交互（展开 / 复制 / 折叠切换 / 内嵌图片预览）
      bind(scope) {
        scope.querySelectorAll('[data-eb-expand]').forEach(btn => {
          btn.onclick = () => {
            const el = document.getElementById(btn.dataset.ebTarget);
            const full = texts[+btn.dataset.ebExpand];
            if (el && full != null) {
              // 代码块重新高亮（20 万字符护栏内）；文本段直接写 textContent
              if (el.tagName === 'PRE' && full.length <= 200000) el.innerHTML = LogParser.highlightJSON(full);
              else el.textContent = full;
              el.classList.add('code-full');
            }
            btn.textContent = '已显示全部';
            btn.disabled = true;
          };
        });
        scope.querySelectorAll('[data-eb-copy]').forEach(btn => {
          btn.onclick = async () => {
            // v1.8.3（F3）：改用带降级的复制工具，非安全上下文（http://IP 访问）不再必然失败
            if (await U.copyText(texts[+btn.dataset.ebCopy])) showToast('已复制全文到剪贴板', 'ok');
            else showToast('复制失败：请手动选中复制', 'err');
          };
        });
        scope.querySelectorAll('[data-toggle]').forEach(btn => {
          btn.onclick = () => {
            const el = document.getElementById(btn.dataset.toggle);
            if (!el) return;
            const hidden = el.classList.toggle('hidden');
            btn.classList.toggle('open', !hidden);
          };
        });
        scope.querySelectorAll('[data-img-toggle]').forEach(btn => {
          btn.onclick = () => {
            const img = btn.parentElement.querySelector('img.b64img');
            if (!img) return;
            if (!img.getAttribute('src')) img.setAttribute('src', texts[+btn.dataset.imgToggle] || '');
            const hidden = img.classList.toggle('hidden');
            btn.textContent = hidden ? '预览' : '收起';
          };
        });
      },
    };
    return R;
  },

  // finish_reason / stop_reason → 中文徽章
  finishBadge(fr) {
    if (!fr) return '';
    const map = {
      stop: '正常结束', length: '达到长度限制', tool_calls: '调用工具', function_call: '调用工具',
      content_filter: '内容过滤', end_turn: '正常结束', max_tokens: '达到长度限制',
      tool_use: '调用工具', pause_turn: '暂停回合', refusal: '拒绝回答',
    };
    const label = map[fr] || fr;
    const ok = ['stop', 'end_turn'].includes(fr);
    return `<span class="finish-badge${ok ? '' : ' warn'}">${U.esc(label)} · ${U.esc(fr)}</span>`;
  },

  httpBadge(code) {
    if (code == null || code === 0) return '';
    const ok = code >= 200 && code < 400;
    return `<span class="http-badge${ok ? '' : ' err'}">HTTP ${U.esc(String(code))}</span>`;
  },

  // 内容段渲染（文本 / 推理 / 图片 / 音频 / 工具调用 / 工具结果 / 未知块）
  renderPart(R, p) {
    switch (p.t) {
      case 'text': return R.text(p.text, { cls: 'msg-text' });
      case 'thinking':
        return `<div class="think-box"><div class="think-label">${p.label ? U.esc(p.label) + ' · ' : ''}推理内容</div>${R.text(p.text, { cls: 'think-text' })}</div>`;
      case 'image': return R.image(p);
      case 'audio':
        return `<div class="part-media"><span class="media-tag">音频</span>${U.esc(p.format || '')}${p.kb ? ' · 约 ' + p.kb + ' KB' : ''}</div>`;
      case 'file':
        return `<div class="part-media"><span class="media-tag">文件</span>${U.esc(p.name || '')}${p.mime ? '（' + U.esc(p.mime) + '）' : ''}${p.kb ? ' · 约 ' + p.kb + ' KB' : ''}</div>`;
      case 'toolcall': return this.renderToolCall(R, p);
      case 'toolresult': return this.renderToolResult(R, p);
      case 'raw': {
        let s; try { s = JSON.stringify(p.value, null, 2); } catch (e) { s = String(p.value); }
        return `<div class="part-raw"><span class="media-tag">${U.esc(p.label)}</span>${R.code(s, { maxLen: 20000 })}</div>`;
      }
      default: return '';
    }
  },

  // 消息卡片：role 徽章 + 元信息 + 内容段列表
  // role=tool 且内容为单一工具结果时，直接渲染工具结果卡（避免卡片套卡片）
  renderMessageCard(R, m) {
    const role = m.role || 'unknown';
    if (role === 'tool' && m.parts && m.parts.length === 1 && m.parts[0].t === 'toolresult') {
      return this.renderToolResult(R, m.parts[0]);
    }
    const meta = [];
    if (m.extra && m.extra.toolCallId) meta.push('tool_call_id: ' + m.extra.toolCallId);
    if (m.extra && m.extra.fnName) meta.push('name: ' + m.extra.fnName);
    if (m.extra && m.extra.itemType && ['function_call', 'function_call_output', 'reasoning', 'system', 'instructions', 'item_reference'].includes(m.extra.itemType)) {
      meta.push('类型: ' + m.extra.itemType);
    }
    const bodyHTML = m.parts && m.parts.length
      ? m.parts.map(p => this.renderPart(R, p)).join('')
      : '<div class="muted" style="padding:4px 0">(无内容)</div>';
    return `<div class="msg-card">
      <div class="msg-head">
        <span class="role-badge role-${U.esc(role)}">${U.esc(role)}</span>
        ${meta.length ? `<span class="msg-meta mono">${U.esc(meta.join(' · '))}</span>` : ''}
      </div>
      <div class="msg-body">${bodyHTML}</div>
    </div>`;
  },

  // 工具调用：函数名徽章 + call id + arguments 二次解析为格式化 JSON（失败回退原文）
  renderToolCall(R, tc) {
    let argsHTML;
    if (tc.args) {
      let parsed = null;
      try { parsed = JSON.parse(tc.args); } catch (e) { parsed = null; }
      if (parsed !== null) {
        argsHTML = R.code(JSON.stringify(parsed, null, 2), { maxLen: 20000 });
      } else {
        argsHTML = '<div class="args-note">参数不是合法 JSON（可能被截断），按原文展示</div>' +
          R.code(tc.args, { maxLen: 20000, hl: false });
      }
    } else {
      argsHTML = '<pre class="code">(空参数)</pre>';
    }
    return `<div class="toolcall-box">
      <div class="tc-head">
        <span class="fn-badge">${U.esc(tc.name || '(未命名)')}</span>
        ${tc.id ? `<span class="tc-id">id: ${U.esc(tc.id)}</span>` : ''}
        ${tc.type && tc.type !== 'function' ? `<span class="tc-id">type: ${U.esc(tc.type)}</span>` : ''}
      </div>
      <div class="tc-args">${argsHTML}</div>
    </div>`;
  },

  // 工具结果：tool_call_id + 内容（文本或块列表）；内容为合法 JSON 时格式化展示
  renderToolResult(R, tr) {
    let inner = '';
    if (tr.parts && tr.parts.length) {
      inner = tr.parts.map(p => this.renderPart(R, p)).join('');
    } else if (tr.text != null && tr.text !== '') {
      let parsed = null;
      try { parsed = JSON.parse(tr.text); } catch (e) { parsed = null; }
      if (parsed !== null) inner = R.code(JSON.stringify(parsed, null, 2), { maxLen: 20000 });
      else inner = R.text(tr.text, { cls: 'msg-text' });
    } else {
      inner = '<div class="muted" style="padding:4px 0">(无内容)</div>';
    }
    return `<div class="toolresult-box${tr.isError ? ' is-error' : ''}">
      <div class="tc-head">
        <span class="media-tag">工具结果</span>
        ${tr.id ? `<span class="tc-id">tool_call_id: ${U.esc(tr.id)}</span>` : ''}
        ${tr.isError ? '<span class="badge off">执行出错</span>' : ''}
      </div>
      <div class="msg-body">${inner}</div>
    </div>`;
  },

  // 工具定义：可折叠卡片（默认收起）——名称徽章 + 类型 + 描述 + Schema
  renderToolDef(R, t) {
    const body = [];
    if (t.description) body.push(R.text(t.description, { limit: 800, cls: 'msg-text' }));
    if (t.schema != null) {
      let s; try { s = JSON.stringify(t.schema, null, 2); } catch (e) { s = String(t.schema); }
      body.push('<div class="schema-label">参数 Schema</div>' + R.code(s, { maxLen: 20000 }));
    }
    if (!t.description && t.schema == null) {
      let s; try { s = JSON.stringify(t.raw, null, 2); } catch (e) { s = String(t.raw); }
      body.push(R.code(s, { maxLen: 20000 }));
    }
    const head = `<span class="fn-badge">${U.esc(t.name)}</span><span class="tc-id">${U.esc(t.type || '')}</span>`;
    return R.collapsible(head, body.join(''));
  },

  // 顶层参数 kv（有值才显示；对象/数组短则内联 JSON、长则代码块）
  renderParamsKV(R, params) {
    if (!params || !params.length) return '';
    const rows = params.map(([k, v]) => {
      let val;
      if (v === null) val = '<span class="val-null">null</span>';
      else if (typeof v === 'boolean') val = v ? 'true' : 'false';
      else if (typeof v === 'number') val = String(v);
      else if (typeof v === 'string') val = `<span class="mono breakable">${U.esc(v)}</span>`;
      else {
        let s; try { s = JSON.stringify(v); } catch (e) { s = String(v); }
        val = s.length <= 160
          ? `<span class="mono breakable">${U.esc(s)}</span>`
          : R.code(JSON.stringify(v, null, 2), { maxLen: 20000 });
      }
      return `<div class="k">${U.esc(k)}</div><div>${val}</div>`;
    }).join('');
    return `<h5 class="sub-h">请求参数</h5><div class="kv kv2">${rows}</div>`;
  },

  // 其他字段折叠区（与已知字段合计对账原始 JSON 全部 key）
  renderOtherFields(R, other) {
    const keys = Object.keys(other || {});
    if (!keys.length) return '';
    let s; try { s = JSON.stringify(other, null, 2); } catch (e) { s = String(other); }
    return R.collapsible(
      `<span class="other-title">其他字段（${keys.length} 个）</span>`,
      R.code(s, { maxLen: 60000 }));
  },

  renderUsage(rows) {
    if (!rows || !rows.length) return '';
    const cells = rows.map(([k, v]) =>
      `<div class="k">${U.esc(k)}</div><div class="mono">${U.esc(v)}</div>`).join('');
    return `<div class="usage-box"><span class="usage-title">Token 用量</span><div class="kv kv-inline">${cells}</div></div>`;
  },

  // 解析后的请求体：模型 + 参数 kv + 消息卡片 + 工具定义 + 其他字段
  renderParsedRequest(R, req) {
    const out = [];
    if (req.model) out.push(`<div class="model-line">模型 <span class="mono">${U.esc(req.model)}</span></div>`);
    out.push(this.renderParamsKV(R, req.params));
    if (req.messages && req.messages.length) {
      out.push(`<h5 class="sub-h">消息（${req.messages.length} 条）</h5>`);
      out.push(req.messages.map(m => this.renderMessageCard(R, m)).join(''));
    }
    if (req.tools && req.tools.length) {
      out.push(`<h5 class="sub-h">工具定义（${req.tools.length} 个）</h5>`);
      out.push(req.tools.map(t => this.renderToolDef(R, t)).join(''));
    }
    out.push(this.renderOtherFields(R, req.other));
    return out.join('');
  },

  // 解析后的响应体：SSE 统计 + 错误卡 + 结果卡片（choices/blocks/output）+ usage + 其他字段
  renderParsedResponse(R, resp) {
    const out = [];
    if (resp.isSSE && resp.sse) {
      out.push(`<div class="sse-stats">SSE 流已重组为最终结果 · 共 ${resp.sse.frames} 帧 · ${resp.sse.chars.toLocaleString()} 字符${resp.sse.bad ? ' · <b>' + resp.sse.bad + ' 帧无法解析，结果可能不完整</b>' : ''}</div>`);
    }
    if (resp.error) {
      out.push(`<div class="err-card"><span class="err-title">错误响应${resp.error.type ? '（' + U.esc(resp.error.type) + '）' : ''}</span>${U.esc(resp.error.message)}${resp.error.param ? `<div class="muted">param: ${U.esc(resp.error.param)}</div>` : ''}</div>`);
    }
    if (resp.format === 'cc') {
      for (const ch of (resp.choices || [])) {
        const bodyHTML = [
          ...ch.parts.map(p => this.renderPart(R, p)),
          ...ch.toolCalls.map(tc => this.renderToolCall(R, tc)),
        ].join('');
        out.push(`<div class="msg-card">
          <div class="msg-head">
            <span class="role-badge role-assistant">assistant</span>
            ${ch.index ? `<span class="tc-id">choice #${U.esc(String(ch.index))}</span>` : ''}
            ${this.finishBadge(ch.finishReason || resp.finishReason)}
          </div>
          <div class="msg-body">${bodyHTML || '<div class="muted" style="padding:4px 0">(无内容)</div>'}</div>
        </div>`);
      }
      if (!(resp.choices || []).length && !resp.error) out.push('<div class="empty-note">(响应无 choices 内容)</div>');
    } else if (resp.format === 'anthropic') {
      const bodyHTML = (resp.blocks || []).map(p => this.renderPart(R, p)).join('');
      out.push(`<div class="msg-card">
        <div class="msg-head">
          <span class="role-badge role-assistant">assistant</span>
          ${this.finishBadge(resp.stopReason)}
        </div>
        <div class="msg-body">${bodyHTML || '<div class="muted" style="padding:4px 0">(无内容)</div>'}</div>
      </div>`);
    } else if (resp.format === 'responses') {
      for (const it of (resp.output || [])) {
        const bodyHTML = (it.parts || []).map(p => this.renderPart(R, p)).join('');
        out.push(`<div class="msg-card">
          <div class="msg-head">
            <span class="role-badge role-${U.esc(it.role)}">${U.esc(it.role)}</span>
            ${it.extra && it.extra.itemType && it.extra.itemType !== 'message' ? `<span class="tc-id">${U.esc(it.extra.itemType)}</span>` : ''}
          </div>
          <div class="msg-body">${bodyHTML || '<div class="muted" style="padding:4px 0">(无内容)</div>'}</div>
        </div>`);
      }
      if (resp.status) out.push(`<div class="finish-note">状态：${U.esc(resp.status)}</div>`);
      if (!(resp.output || []).length && !resp.error) out.push('<div class="empty-note">(响应无 output 内容)</div>');
    }
    out.push(this.renderUsage(resp.usage));
    out.push(this.renderOtherFields(R, resp.other));
    return out.join('');
  },

  // 方向区块：标题 + 元信息行（方法/URL/状态码/格式/内容类型）+ 解析视图或原文回退
  renderDirection(R, d) {
    // d: { title, kind: 'request'|'response', method, url, status, note, body, emptyHint }
    const body = d.body == null || d.body === undefined ? '' : String(d.body);
    const empty = body === '';
    const truncated = LogParser.isTruncated(body);
    const parsed = empty ? null
      : (d.kind === 'request' ? LogParser.parseRequest(body, d.url) : LogParser.parseResponse(body, d.url));

    const chips = [];
    if (d.method || d.url) {
      chips.push(`<span class="dmeta-item mono">${U.esc((d.method || '') + (d.method ? ' ' : '') + (d.url || ''))}</span>`);
    }
    if (d.status != null && d.status !== 0) chips.push(this.httpBadge(d.status));
    if (d.note) chips.push(`<span class="dmeta-item muted">${U.esc(d.note)}</span>`);
    if (empty) {
      chips.push('<span class="dmeta-badge warn">无数据</span>');
    } else if (parsed && parsed.ok) {
      chips.push(`<span class="dmeta-badge">${U.esc(LogParser.FORMAT_LABELS[parsed.format] || parsed.format)}</span>`);
      if (parsed.isSSE && parsed.sse) {
        chips.push(`<span class="dmeta-badge">SSE 流 · ${parsed.sse.frames} 帧（已重组）</span>`);
      } else {
        chips.push(`<span class="dmeta-badge">${d.kind === 'request' ? 'JSON 请求体' : 'JSON 响应体'}</span>`);
      }
      chips.push(`<span class="dmeta-item muted">${body.length.toLocaleString()} 字符</span>`);
    } else {
      const reason = parsed && parsed.reason;
      // v1.8.4（F6）：SSE 流但帧格式未识别时如实标注，不再误标为「未识别 JSON」
      const label = truncated ? '已截断'
        : (reason === 'non-json' ? '非 JSON 内容'
          : (parsed && parsed.isSSE ? 'SSE 流（未识别格式）' : '未识别 JSON'));
      chips.push(`<span class="dmeta-badge warn">${label}</span>`);
    }

    let contentHTML = '';
    if (empty) {
      contentHTML = `<div class="empty-note">${U.esc(d.emptyHint || '(无数据)')}</div>`;
    } else {
      const warns = truncated
        ? '<div class="trunc-warn">该数据超过 1MB 已截断，内容不完整（后端标记 ...(truncated)）</div>' : '';
      if (parsed && parsed.ok) {
        contentHTML = warns + (d.kind === 'request'
          ? this.renderParsedRequest(R, parsed)
          : this.renderParsedResponse(R, parsed));
      } else {
        const reason = parsed && parsed.reason;
        let fallback;
        if (reason === 'unknown-format' && parsed && parsed.isSSE) {
          // v1.8.4（F6）：SSE 原文不是合法 JSON，JSON.parse 美化必然失败退回原文；
          // 文案与行为对齐——如实声明按原文展示，且不做 JSON 高亮
          fallback = '<div class="plain-note">未识别的 SSE 流，按原文展示</div>' + R.code(body, { maxLen: 60000, hl: false });
        } else if (reason === 'unknown-format') {
          let pretty = body;
          try { pretty = JSON.stringify(JSON.parse(body), null, 2); } catch (e) { /* 保持原文 */ }
          fallback = '<div class="plain-note">未识别的格式，按 JSON 美化展示</div>' + R.code(pretty, { maxLen: 60000 });
        } else {
          fallback = R.code(body, { maxLen: 60000, hl: false });
        }
        contentHTML = warns + fallback;
      }
    }

    return `<div class="detail-section">
      <h4>${U.esc(d.title)}</h4>
      <div class="dmeta">${chips.join('')}</div>
      ${contentHTML}
    </div>`;
  },

  // 原始数据标签页的四组区块（含请求头/响应头 + 原文 + 高亮 + 截断警示 + 复制）
  renderRawSection(R, title, kvPairs, headersLabel, headers, bodyLabel, body) {
    const b = body == null || body === undefined ? '' : String(body);
    const truncated = LogParser.isTruncated(b);
    let bodyHTML;
    if (b === '') {
      bodyHTML = '<pre class="code">(空)</pre>';
    } else {
      let pretty = b;
      if (!truncated) { try { pretty = JSON.stringify(JSON.parse(b), null, 2); } catch (e) { /* SSE/非 JSON 保持原文 */ } }
      bodyHTML = (truncated ? '<div class="trunc-warn">该数据超过 1MB 已截断，内容不完整（后端标记 ...(truncated)）</div>' : '') +
        R.code(pretty, { maxLen: 60000 });
    }
    return `<div class="detail-section">
      <h4>${U.esc(title)}</h4>
      ${kvPairs && kvPairs.length ? kvHTML(kvPairs) : ''}
      <h5 class="sub-h">${U.esc(headersLabel)}</h5>
      <pre class="code">${U.esc(headersText(headers))}</pre>
      <h5 class="sub-h mt8">${U.esc(bodyLabel)}</h5>
      ${bodyHTML}
    </div>`;
  },

  async showDetail(id) {
    // v1.8.4（F2）：并发守卫——await 期间路由已切换、或用户已点击另一条日志时，
    // 迟到的响应不再打开 Modal 覆盖新页面 / 后开的详情
    const seq = App.renderSeq;
    const reqSeq = ++this.detailSeq;
    let l;
    try { l = await API.get(`/api/logs/${id}`); }
    catch (e) {
      if (seq === App.renderSeq && reqSeq === this.detailSeq) showToast(e.message, 'err');
      return;
    }
    if (seq !== App.renderSeq || reqSeq !== this.detailSeq) return;
    const attempts = Array.isArray(l.attempt_logs) ? l.attempt_logs : [];
    // 旧数据兼容：v1.7.5 之前 req_url 记录上游 URL、流式 resp_body 为发给客户端的字节
    const upstreamURL = l.upstream_url || l.req_url || '';
    // v1.8.2（B2）：client_resp_body 回退仅限 v1.7.5 之前的旧数据（该列不存在，且 resp_body
    // 本就是发给客户端的字节）。新数据以 upstream_url / client_resp_status 字段识别，
    // 不再回退——避免「客户端断开」等失败日志（client_resp_body 为空、resp_body 是
    // 未送达客户端的上游响应）被错误展示为客户端响应。判定逻辑抽取为 LogParser 纯函数（可单测）。
    const clientRespBody = LogParser.clientRespBody(l);
    const R = this.makeRenderer();

    // ---- 常驻区：基本信息（紧凑网格）+ 尝试记录（结构化列表） ----
    const ig = (k, v, full) =>
      `<div class="ig${full ? ' ig-full' : ''}"><span class="ig-k">${U.esc(k)}</span><span class="ig-v">${v}</span></div>`;
    const persistHTML = `
      <div class="detail-section">
        <h4>基本信息</h4>
        <div class="info-grid">
          ${ig('调用时间', U.esc(U.fmtTime(l.timestamp)))}
          ${ig('状态', badgeStatus(l.status))}
          ${ig('供应商', U.esc(l.provider || '-'))}
          ${ig('模型', `<span class="mono">${U.esc(l.requested_model || '-')}</span>${l.actual_model && l.actual_model !== l.requested_model ? ' → <span class="mono">' + U.esc(l.actual_model) + '</span>' : ''}`)}
          ${ig('代理 API Key', `<span class="mono">${U.esc(l.apikey || '-')}</span>${l.apikey_name ? '（' + U.esc(l.apikey_name) + '）' : ''}`)}
          ${ig('重试次数', String(l.retry_count || 0))}
          ${ig('总耗时', `${U.fmtNum(l.duration_ms)} ms`, false)}
          ${ig('上游首字节', l.first_byte_ms > 0 ? U.fmtNum(l.first_byte_ms) + ' ms' : '-')}
          ${ig('生成耗时', `${U.fmtNum(l.generation_ms)} ms`)}
          ${ig('Token', `输入 ${U.fmtNum(l.prompt_tokens)} / 输出 ${U.fmtNum(l.completion_tokens)} / 总计 ${U.fmtNum(l.total_tokens)}`)}
          ${ig('输出速率', l.tokens_per_second > 0 ? this.fmtRate(l.tokens_per_second) + ' tok/s' : '-')}
          ${l.error_msg ? ig('错误信息', `<span style="color:var(--danger)">${U.esc(l.error_msg)}</span>`, true) : ''}
        </div>
      </div>
      <div class="detail-section">
        <h4>尝试记录（共 ${attempts.length} 次）</h4>
        ${attempts.length === 0 ? '<div class="muted">无</div>' : '<div class="attempt-list">' + attempts.map((a, i) => {
          const ok = !a.error_msg;
          return `<div class="attempt-item${ok ? ' ok' : ' fail'}">
            <span class="att-seq">#${i + 1}</span>
            <span class="att-prov">${U.esc(a.provider || '-')}</span>
            <span>${ok ? '成功' : '失败'}</span>
            ${ok ? '' : `<span class="att-err">${U.esc(a.error_msg || '')}</span>`}
          </div>`;
        }).join('') + '</div>'}
      </div>`;

    Modal.open('调用详情', `
      <div class="logdetail">
        <div class="logdetail-persist">${persistHTML}</div>
        <div class="dtabs">
          <div class="dtabs-nav">
            <button class="dtab active" data-dtab="client">客户端记录</button>
            <button class="dtab" data-dtab="upstream">上游记录</button>
            <button class="dtab" data-dtab="raw">原始数据</button>
          </div>
          <div class="dtab-panel active" data-dpanel="client"></div>
          <div class="dtab-panel" data-dpanel="upstream"></div>
          <div class="dtab-panel" data-dpanel="raw"></div>
        </div>
      </div>
    `, true, { xl: true });

    // ---- 标签页内容构建器（懒渲染：首次切换才构建 DOM） ----
    const clientRespEmptyHint = l.client_resp_status
      ? '(未记录响应体)'
      : (l.status === 'failure' ? '(响应未送达客户端——失败发生在写出响应之前，如重试期间或全部上游失败)' : '(未记录响应体)');
    const upstreamReqEmptyHint = upstreamURL ? '(未记录上游请求体)' : '(模型未匹配或未发起上游调用)';
    const upstreamRespEmptyHint = upstreamURL ? '(未记录上游响应体)' : '(模型未匹配或未发起上游调用)';

    const builders = {
      client: () => [
        this.renderDirection(R, {
          title: '客户端请求（客户端 → 代理）', kind: 'request',
          method: l.req_method || 'POST', url: l.client_req_url || '',
          body: l.req_body, emptyHint: '(未记录请求体)',
        }),
        this.renderDirection(R, {
          title: '客户端响应（代理 → 客户端）', kind: 'response',
          status: l.client_resp_status, url: l.client_req_url || '',
          body: clientRespBody,
          note: l.sent_bytes > 0 ? '已发送 ' + U.fmtNum(l.sent_bytes) + ' 字节' : '',
          emptyHint: clientRespEmptyHint,
        }),
      ].join(''),
      upstream: () => [
        this.renderDirection(R, {
          title: '上游请求（代理 → 供应商）', kind: 'request',
          method: 'POST', url: upstreamURL || '-',
          body: l.upstream_req_body,
          note: '认证头已脱敏',
          emptyHint: upstreamReqEmptyHint,
        }),
        this.renderDirection(R, {
          title: '上游响应（供应商 → 代理）', kind: 'response',
          status: l.resp_status, url: upstreamURL,
          body: l.resp_body,
          emptyHint: upstreamRespEmptyHint,
        }),
      ].join(''),
      raw: () => [
        this.renderRawSection(R, '客户端请求（客户端 → 代理）', [
          ['URL', `<span class="mono">${U.esc(l.client_req_url || (l.req_method ? l.req_method + '（路径未记录，v1.7.5 之前日志）' : '-'))}</span>`],
          ['方法', U.esc(l.req_method || '-')],
        ], '请求头', l.req_headers, '请求体', l.req_body),
        this.renderRawSection(R, '上游请求（代理 → 供应商）', [
          ['URL', `<span class="mono">${U.esc(upstreamURL || '-')}</span>`],
          ['认证头', '已脱敏记录（保留头尾，便于区分不同 Key）'],
        ], '请求头', l.upstream_req_headers, '请求体', l.upstream_req_body),
        this.renderRawSection(R, '上游响应（供应商 → 代理）', [
          ['HTTP 状态码', l.resp_status ? String(l.resp_status) : '-'],
        ], '响应头', l.resp_headers, '响应体', l.resp_body),
        this.renderRawSection(R, '客户端响应（代理 → 客户端）', [
          ['HTTP 状态码', l.client_resp_status ? String(l.client_resp_status) : '-'],
          ['已发送字节', l.sent_bytes > 0 ? U.fmtNum(l.sent_bytes) : '-'],
        ], '响应头', l.client_resp_headers, '响应体', clientRespBody),
        `<div class="detail-section">
          <h4>尝试记录（attempt_logs 原始数据）</h4>
          ${R.code(JSON.stringify(attempts, null, 2), { maxLen: 60000 })}
        </div>`,
      ].join(''),
    };

    // ---- 标签切换：懒渲染 + 滚动位置独立记忆 ----
    // （display:none 会使浏览器重置 scrollTop，需显式保存/恢复）
    const mbox = document.getElementById('modal-body');
    const rendered = {};
    const scrollPos = {};
    const activate = name => {
      const cur = mbox.querySelector('.dtab-panel.active');
      if (cur && cur.dataset.dpanel !== name) scrollPos[cur.dataset.dpanel] = cur.scrollTop;
      mbox.querySelectorAll('.dtab').forEach(b => b.classList.toggle('active', b.dataset.dtab === name));
      mbox.querySelectorAll('.dtab-panel').forEach(p => {
        const on = p.dataset.dpanel === name;
        p.classList.toggle('active', on);
        if (on && !rendered[p.dataset.dpanel]) {
          p.innerHTML = builders[p.dataset.dpanel]();
          R.bind(p);
          rendered[p.dataset.dpanel] = true;
        }
        if (on) {
          // 刚从 display:none 切换回来时布局未就绪，下一帧再恢复滚动位置
          const pos = scrollPos[name] || 0;
          if (pos > 0) requestAnimationFrame(() => { p.scrollTop = pos; });
        }
      });
    };
    mbox.querySelectorAll('.dtab').forEach(b => { b.onclick = () => activate(b.dataset.dtab); });
    activate('client'); // 默认标签：客户端记录
  },
};
