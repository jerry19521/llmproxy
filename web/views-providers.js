// 供应商接口类型显示名
function apiTypeLabel(t) {
  switch ((t || 'openai').toLowerCase()) {
    case 'responses': return 'Responses API';
    case 'anthropic': return 'Anthropic API';
    default: return 'Chat Completions';
  }
}

// ===== 供应商管理视图 =====
Views.Providers = {
  async renderList(el) {
    const seq = App.renderSeq; // M9（v1.0.0）：路由切换守卫
    el.innerHTML = '<div class="muted">加载中...</div>';
    try {
      const res = await API.get('/api/providers');
      if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应
      const rows = (res.providers || []).map(p => `
        <tr>
          <td><strong>${U.esc(p.name)}</strong></td>
          <td class="mono">${U.esc(p.base_url)}</td>
          <td>${apiTypeLabel(p.api_type)}</td>
          <td>${(p.models || []).length}</td>
          <td>${(p.api_keys || []).length}</td>
          <td>${badgeEnabled(p.enabled)}</td>
          <td>
            <div class="actions">
              <button class="btn btn-sm btn-ghost" data-act="edit" data-id="${p.id}">修改</button>
              <button class="btn btn-sm btn-ghost" data-act="toggle" data-id="${p.id}" data-on="${p.enabled}">${p.enabled ? '停用' : '启用'}</button>
              <button class="btn btn-sm btn-ghost danger" style="color:var(--danger)" data-act="del" data-id="${p.id}" data-name="${U.esc(p.name)}">删除</button>
            </div>
          </td>
        </tr>`).join('');
      el.innerHTML = `
        <div class="card">
          <div class="flex-between" style="margin-bottom:14px">
            <h3 style="margin-bottom:0">供应商列表</h3>
            <button class="btn btn-primary" id="p-add">＋ 新增供应商</button>
          </div>
          <div class="table-wrap">
            <table class="data">
              <thead><tr><th>名称</th><th>基础地址</th><th>接口类型</th><th>模型数量</th><th>API Key 数量</th><th>状态</th><th>操作</th></tr></thead>
              <tbody>${rows || '<tr><td colspan="7" class="muted">暂无供应商，点击右上角新增</td></tr>'}</tbody>
            </table>
          </div>
        </div>`;
      document.getElementById('p-add').onclick = () => App.navigate('/providers/new');
      el.querySelectorAll('button[data-act]').forEach(b => {
        b.onclick = async () => {
          const id = b.dataset.id;
          if (b.dataset.act === 'edit') App.navigate(`/providers/${id}/edit`);
          else if (b.dataset.act === 'toggle') {
            try {
              await API.post(`/api/providers/${id}/${b.dataset.on === 'true' ? 'disable' : 'enable'}`);
              showToast('操作成功', 'ok');
              this.renderList(el);
            } catch (e) { showToast(e.message, 'err'); }
          } else if (b.dataset.act === 'del') {
            if (confirmDialog(`确认删除供应商「${b.dataset.name}」？该操作不可恢复。`)) {
              try {
                await API.del(`/api/providers/${id}`);
                showToast('已删除', 'ok');
                this.renderList(el);
              } catch (e) { showToast(e.message, 'err'); }
            }
          }
        };
      });
    } catch (e) {
      if (seq !== App.renderSeq) return; // v1.0.0（F2）：页面已切换时迟到的失败响应不覆盖新页面
      el.innerHTML = `<div class="card"><h3>加载失败</h3><p class="muted">${U.esc(e.message)}</p></div>`;
    }
  },

  // ===== 新增 / 修改页 =====
  async renderEdit(el, id) {
    const seq = App.renderSeq; // M9（v1.0.0）：路由切换守卫
    const F = {
      id: id, name: '', base_url: '', api_type: 'openai', weight: 1, enabled: true,
      api_keys: [],   // {id, key}（key 可能为脱敏值）
      headers: [],    // {key, value}
      models: [],     // {name, aliases: "a,b"}
      testModel: '',
      testResults: {}, // keyId → result（即时动作，不持久化）
    };
    if (id) {
      try {
        const p = await API.get(`/api/providers/${id}`);
        if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应
        F.name = p.name; F.base_url = p.base_url; F.api_type = p.api_type || 'openai'; F.weight = p.weight || 1; F.enabled = p.enabled;
        F.api_keys = (p.api_keys || []).map(k => ({ id: k.id, key: k.key }));
        F.headers = (p.headers || []).map(h => ({ key: h.key, value: h.value }));
        F.models = (p.models || []).map(m => ({ name: m.name, aliases: (m.aliases || []).join(',') }));
      } catch (e) {
        if (seq === App.renderSeq) el.innerHTML = `<div class="card"><h3>加载失败</h3><p class="muted">${U.esc(e.message)}</p></div>`;
        return;
      }
    }
    const ctx = { F, tmpSeq: 0, seq }; // seq：本页渲染序号（v1.0.0 F7：异步动作完成后校验，页面已切换不再弹窗/写回）
    this.drawForm(el, ctx);
  },

  drawForm(el, ctx) {
    const F = ctx.F;
    el.innerHTML = `
      <div class="card">
        <h3>基本信息</h3>
        <div class="form-row">
          <label class="field"><span>名称 *</span><input type="text" id="f-name" value="${U.esc(F.name)}" placeholder="如 OpenAI"></label>
          <label class="field"><span>基础地址 *</span><input type="text" id="f-base" value="${U.esc(F.base_url)}" placeholder="如 https://api.openai.com/v1（末尾不带斜杠）"></label>
          <label class="field" style="flex:0 0 240px"><span>接口类型 *</span>
            <select id="f-apitype" style="padding:8px 12px;border:1px solid #d1d5db;border-radius:8px">
              <option value="openai">OpenAI Chat Completions</option>
              <option value="responses">OpenAI Responses API</option>
              <option value="anthropic">Anthropic Messages API</option>
            </select>
          </label>
          <label class="field" style="flex:0 0 130px"><span>权重</span><input type="number" id="f-weight" value="${F.weight}" min="1"></label>
        </div>
        <div class="field"><span>是否启用</span>
          <label class="switch"><input type="checkbox" id="f-enabled" ${F.enabled ? 'checked' : ''}><span class="slider"></span></label>
        </div>
        <p class="form-hint" style="margin-top:8px">接口类型决定请求上游的格式：Chat Completions 发往 {基础地址}/chat/completions；Responses API 发往 {基础地址}/responses；Anthropic 发往 {基础地址}/v1/messages（携带 x-api-key 头）。用户调用格式与供应商格式不同时，代理自动双向转换。</p>
      </div>

      <div class="card">
        <h3>模型测试<span class="card-sub">用选中的测试模型对供应商的所有 API Key 依次发起测试</span></h3>
        <div style="display:flex;gap:10px;align-items:center">
          <select id="f-test-model" style="padding:8px 12px;border:1px solid #d1d5db;border-radius:8px;min-width:220px">
            <option value="">-- 选择测试模型 --</option>
          </select>
          <button class="btn btn-primary" id="btn-test-all">▶ 测试</button>
          <span class="form-hint">测试发送内容可在「设置」页配置；单条 Key 也可在其行内测试</span>
        </div>
      </div>

      <div class="card">
        <h3>API Key<span class="card-sub">每条 Key 可单独测试；测试直接使用当前填写的内容（无需先保存）</span></h3>
        <div class="rows" id="keys-rows"></div>
        <div class="mt8"><button class="btn btn-sm btn-ghost" id="add-key">＋ 添加 API Key</button></div>
      </div>

      <div class="card">
        <h3>请求头<span class="card-sub">调用供应商接口时附加的 HTTP 头</span></h3>
        <div class="rows" id="headers-rows"></div>
        <div class="mt8"><button class="btn btn-sm btn-ghost" id="add-header">＋ 添加请求头</button></div>
      </div>

      <div class="card">
        <h3>模型列表<span class="card-sub">模型别名为可选，多个别名用英文逗号分隔；同供应商内模型名称不可重复</span></h3>
        <div class="mb8" style="display:flex;gap:8px;flex-wrap:wrap">
          <button class="btn btn-sm btn-ghost" id="add-model">＋ 添加模型</button>
          <button class="btn btn-sm btn-ghost" id="fetch-models">⤓ 自动获取</button>
          <span class="form-hint" style="align-self:center">自动获取：需已填写基础地址且至少一条 API Key</span>
        </div>
        <div class="rows" id="models-rows"></div>
      </div>

      <div style="display:flex;gap:10px;margin-bottom:30px">
        <button class="btn btn-primary" id="btn-save" style="min-width:120px">保 存</button>
        <button class="btn btn-ghost" id="btn-back">返 回</button>
      </div>`;

    const $ = s => el.querySelector(s);
    // 绘制动态行
    this.drawKeyRows(el, ctx);
    this.drawHeaderRows(el, ctx);
    this.drawModelRows(el, ctx);
    this.drawTestModelSelect(el, ctx);

    $('#f-apitype').value = F.api_type;
    $('#f-name').oninput = e => F.name = e.target.value;
    $('#f-base').oninput = e => F.base_url = e.target.value;
    $('#f-apitype').onchange = e => F.api_type = e.target.value;
    $('#f-weight').oninput = e => F.weight = parseInt(e.target.value) || 1;
    $('#f-enabled').onchange = e => F.enabled = e.target.checked;

    $('#add-key').onclick = () => { ctx.tmpSeq++; F.api_keys.push({ id: 'tmp-' + ctx.tmpSeq, key: '' }); this.drawKeyRows(el, ctx); };
    $('#add-header').onclick = () => { F.headers.push({ key: '', value: '' }); this.drawHeaderRows(el, ctx); };
    $('#add-model').onclick = () => { F.models.push({ name: '', aliases: '' }); this.drawModelRows(el, ctx); };
    $('#fetch-models').onclick = () => this.fetchModels(el, ctx);
    $('#btn-test-all').onclick = () => this.testKeys(el, ctx, null);
    $('#btn-back').onclick = () => App.navigate('/providers');
    $('#btn-save').onclick = () => this.save(el, ctx);
  },

  drawTestModelSelect(el, ctx) {
    const sel = el.querySelector('#f-test-model');
    if (!sel) return;
    const models = ctx.F.models.filter(m => m.name.trim()).map(m => m.name);
    let cur = ctx.F.testModel;
    if (!cur && models.length) cur = models[0]; // 默认选中第一个模型
    ctx.F.testModel = cur;
    sel.innerHTML = '<option value="">-- 选择测试模型 --</option>' +
      models.map(m => `<option value="${U.esc(m)}">${U.esc(m)}</option>`).join('');
    sel.value = cur || '';
    sel.onchange = () => ctx.F.testModel = sel.value;
  },

  drawKeyRows(el, ctx) {
    const box = el.querySelector('#keys-rows');
    const F = ctx.F;
    box.innerHTML = F.api_keys.length === 0 ? '<div class="muted">尚未添加 API Key</div>' : '';
    F.api_keys.forEach((k, i) => {
      const r = F.testResults[k.id];
      const resultHTML = r
        ? `${badgeKeyTest(r.success)} ${r.success ? '<button class="btn-link" data-view-key="' + U.esc(k.id) + '">查看详情</button>' : `<span class="muted" title="${U.esc(r.error)}">${U.esc((r.error || '').slice(0, 30))}</span> <button class="btn-link" data-view-key="${U.esc(k.id)}">详情</button>`}`
        : '';
      const div = document.createElement('div');
      div.className = 'row-item';
      div.innerHTML = `
        <input type="text" data-ki="${i}" value="${U.esc(k.key)}" placeholder="sk-..." autocomplete="off">
        <button class="btn btn-sm btn-ghost" data-test-key="${i}" title="测试该 Key">测试</button>
        <span class="key-test-result">${resultHTML}</span>
        <button class="btn btn-sm btn-ghost danger" style="color:var(--danger)" data-del-key="${i}">删除</button>`;
      box.appendChild(div);
    });
    box.querySelectorAll('input[data-ki]').forEach(inp => {
      inp.oninput = () => { F.api_keys[+inp.dataset.ki].key = inp.value; };
    });
    box.querySelectorAll('button[data-del-key]').forEach(b => {
      b.onclick = () => { F.api_keys.splice(+b.dataset.delKey, 1); this.drawKeyRows(el, ctx); };
    });
    box.querySelectorAll('button[data-test-key]').forEach(b => {
      b.onclick = () => this.testKeys(el, ctx, F.api_keys[+b.dataset.testKey].id);
    });
    box.querySelectorAll('button[data-view-key]').forEach(b => {
      b.onclick = () => {
        const r = F.testResults[b.dataset.viewKey];
        if (r) this.showTestDetail(r);
      };
    });
  },

  drawHeaderRows(el, ctx) {
    const box = el.querySelector('#headers-rows');
    const F = ctx.F;
    box.innerHTML = F.headers.length === 0 ? '<div class="muted">无自定义请求头</div>' : '';
    F.headers.forEach((h, i) => {
      const div = document.createElement('div');
      div.className = 'row-item';
      div.innerHTML = `
        <input type="text" data-hk="${i}" value="${U.esc(h.key)}" placeholder="Header 名称，如 X-Custom-Header">
        <input type="text" data-hv="${i}" value="${U.esc(h.value)}" placeholder="值">
        <button class="btn btn-sm btn-ghost danger" style="color:var(--danger)" data-del-header="${i}">删除</button>`;
      box.appendChild(div);
    });
    box.querySelectorAll('input[data-hk]').forEach(inp => inp.oninput = () => F.headers[+inp.dataset.hk].key = inp.value);
    box.querySelectorAll('input[data-hv]').forEach(inp => inp.oninput = () => F.headers[+inp.dataset.hv].value = inp.value);
    box.querySelectorAll('button[data-del-header]').forEach(b => b.onclick = () => { F.headers.splice(+b.dataset.delHeader, 1); this.drawHeaderRows(el, ctx); });
  },

  drawModelRows(el, ctx) {
    const box = el.querySelector('#models-rows');
    const F = ctx.F;
    box.innerHTML = F.models.length === 0 ? '<div class="muted">尚未添加模型</div>' : '';
    F.models.forEach((m, i) => {
      const div = document.createElement('div');
      div.className = 'row-item';
      div.innerHTML = `
        <input type="text" data-mn="${i}" value="${U.esc(m.name)}" placeholder="模型名称，如 gpt-4o">
        <input type="text" data-ma="${i}" value="${U.esc(m.aliases)}" placeholder="别名（可选，逗号分隔：gpt4,four-o）">
        <span class="dup-tip" data-dup="${i}"></span>
        <button class="btn btn-sm btn-ghost danger" style="color:var(--danger)" data-del-model="${i}">删除</button>`;
      box.appendChild(div);
    });
    const checkDup = () => {
      const seen = {};
      F.models.forEach((m, i) => {
        const lk = m.name.trim().toLowerCase();
        const dupEl = box.querySelector(`[data-dup="${i}"]`);
        const inp = box.querySelector(`input[data-mn="${i}"]`);
        if (lk && seen[lk] !== undefined) {
          dupEl.textContent = '该模型名称已存在';
          inp.classList.add('duplicate');
        } else {
          dupEl.textContent = '';
          inp.classList.remove('duplicate');
          if (lk) seen[lk] = i;
        }
      });
    };
    box.querySelectorAll('input[data-mn]').forEach(inp => {
      inp.oninput = () => { F.models[+inp.dataset.mn].name = inp.value; checkDup(); this.drawTestModelSelect(el, ctx); };
    });
    box.querySelectorAll('input[data-ma]').forEach(inp => inp.oninput = () => F.models[+inp.dataset.ma].aliases = inp.value);
    box.querySelectorAll('button[data-del-model]').forEach(b => b.onclick = () => {
      F.models.splice(+b.dataset.delModel, 1);
      this.drawModelRows(el, ctx); this.drawTestModelSelect(el, ctx);
    });
    checkDup();
  },

  hasDup(F) {
    const seen = new Set();
    for (const m of F.models) {
      const lk = m.name.trim().toLowerCase();
      if (!lk) continue;
      if (seen.has(lk)) return true;
      seen.add(lk);
    }
    return false;
  },

  async save(el, ctx) {
    const F = ctx.F;
    if (this.hasDup(F)) { showToast('同一供应商内模型名称不能重复', 'err'); return; }
    // v1.0.0（F6）：保存期间禁用按钮，防双击重复提交（新增页双击会创建两条同名记录）
    const btn = el.querySelector('#btn-save');
    if (btn.disabled) return;
    btn.disabled = true;
    try {
      await this.doSave(F);
    } finally {
      btn.disabled = false;
    }
  },

  async doSave(F) {
    const body = {
      name: F.name, base_url: F.base_url, api_type: F.api_type, weight: F.weight, enabled: F.enabled,
      api_keys: F.api_keys.map(k => ({ id: k.id, key: k.key })),
      headers: F.headers.filter(h => h.key.trim()),
      models: F.models.filter(m => m.name.trim()).map(m => ({ name: m.name.trim(), aliases: m.aliases })),
    };
    try {
      if (F.id) {
        await API.put(`/api/providers/${F.id}`, body);
      } else {
        await API.post('/api/providers', body);
      }
      showToast('保存成功', 'ok');
      // 保存成功后返回供应商列表页
      App.navigate('/providers');
    } catch (e) {
      showToast('保存失败：' + e.message, 'err');
    }
  },

  // ===== 自动获取模型 =====
  async fetchModels(el, ctx) {
    const F = ctx.F;
    if (!F.base_url.trim()) { showToast('请先填写基础地址', 'err'); return; }
    if (!F.api_keys.some(k => k.key.trim())) { showToast('请先添加至少一条 API Key', 'err'); return; }
    try {
      // v1.0.0（F1）：编辑页与新增页统一走 POST 表单模式——使用表单当前填写的
      // 基础地址 / Key / 请求头（与同页测试按钮“无需先保存”语义一致）；
      // 编辑页表单中的脱敏 Key（含 ***）由后端按 provider_id + key id 恢复完整值。
      const res = await API.post('/api/providers/fetch-models', {
        provider_id: F.id || '',
        base_url: F.base_url.trim(),
        api_type: F.api_type,
        api_keys: F.api_keys.map(k => ({ id: k.id, key: k.key })),
        headers: F.headers.filter(h => h.key.trim()),
      });
      const models = res.models || [];
      if (ctx.seq !== App.renderSeq) return; // 页面已切换：丢弃迟到响应（F7）
      this.showFetchModal(el, ctx, models);
    } catch (e) {
      showToast('获取模型失败：' + e.message, 'err');
    }
  },

  showFetchModal(el, ctx, models) {
    const F = ctx.F;
    const existing = new Set(F.models.map(m => m.name.trim().toLowerCase()));
    const options = models.map(name => {
      const exists = existing.has(name.toLowerCase());
      return `<label class="check-row${exists ? ' is-disabled' : ''}">
        <input type="checkbox" data-fm="${U.esc(name)}" ${exists ? 'disabled' : 'checked'}>
        <span class="check-label mono">${U.esc(name)}${exists ? '（已存在，不会重复添加）' : ''}</span>
      </label>`;
    }).join('');
    Modal.open(`自动获取模型（共 ${models.length} 个）`, `
      <div class="flex-between mb8">
        <button class="btn btn-sm btn-ghost" id="fm-all">全选</button>
        <button class="btn btn-sm btn-ghost" id="fm-none">取消全选</button>
      </div>
      <div style="max-height:340px;overflow:auto;border:1px solid var(--border);border-radius:8px;padding:8px 14px">${options || '<div class="muted">上游未返回任何模型</div>'}</div>
      <div class="mt16"><button class="btn btn-primary btn-block" id="fm-ok" ${models.length ? '' : 'disabled'}>追加所选模型</button></div>
    `, true);
    const boxes = () => Array.from(document.querySelectorAll('input[data-fm]')).filter(b => !b.disabled);
    document.getElementById('fm-all').onclick = () => boxes().forEach(b => b.checked = true);
    document.getElementById('fm-none').onclick = () => boxes().forEach(b => b.checked = false);
    document.getElementById('fm-ok').onclick = () => {
      const sel = Array.from(document.querySelectorAll('input[data-fm]')).filter(b => b.checked).map(b => b.dataset.fm);
      sel.forEach(name => F.models.push({ name, aliases: '' }));
      Modal.close();
      this.drawModelRows(el, ctx);
      this.drawTestModelSelect(el, ctx);
      showToast(`已追加 ${sel.length} 个模型`, 'ok');
    };
  },

  // ===== Key 测试（表单模式：直接使用当前填写的内容，无需先保存） =====
  async testKeys(el, ctx, singleKeyId) {
    const F = ctx.F;
    if (!F.base_url.trim()) { showToast('请先填写基础地址', 'err'); return; }
    const model = F.testModel;
    if (!model) { showToast('请先在模型列表中添加模型，并选择测试模型', 'err'); return; }
    if (F.api_keys.length === 0) { showToast('请先添加 API Key', 'err'); return; }
    const keyIds = singleKeyId ? [singleKeyId] : F.api_keys.map(k => k.id);
    try {
      showToast('测试中，请稍候...');
      const res = await API.post('/api/providers/test', {
        provider_id: F.id || '',
        base_url: F.base_url.trim(),
        api_type: F.api_type,
        // id 原样提交（tmp-N 仅存在于未保存表单，用于 key_ids 过滤与测试结果关联；
        // 后端仅在 id 为空时生成 key-N 兜底；正式 UUID 由保存时的后端生成）
        api_keys: F.api_keys.map(k => ({ id: k.id, key: k.key })),
        headers: F.headers.filter(h => h.key.trim()),
        model,
        key_ids: keyIds,
      });
      if (ctx.seq !== App.renderSeq) return; // 页面已切换：丢弃迟到响应（v1.0.0 F7）
      (res.results || []).forEach(r => { F.testResults[r.key_id] = r; });
      this.drawKeyRows(el, ctx);
      const okCount = (res.results || []).filter(r => r.success).length;
      showToast(`测试完成：${okCount}/${res.results.length} 成功`, okCount === res.results.length ? 'ok' : 'err');
    } catch (e) {
      showToast('测试失败：' + e.message, 'err');
    }
  },

  // ===== 测试详情弹窗 =====
  showTestDetail(r) {
    const req = r.request || {}, resp = r.response || {};
    Modal.open(`测试详情 — ${U.esc(r.masked_key)} ${r.success ? '✓ 成功' : '✗ 失败'}`, `
      <div class="detail-section">
        <h4>结果</h4>
        ${kvHTML([
          ['测试结果', r.success ? '<span class="badge on">成功</span>' : `<span class="badge off">失败</span> <span class="muted">${U.esc(r.error || '')}</span>`],
        ])}
      </div>
      <div class="detail-section">
        <h4>请求</h4>
        ${kvHTML([
          ['URL', `<span class="mono">${U.esc(req.url || '-')}</span>`],
          ['方法', U.esc(req.method || '-')],
        ])}
        <h4 class="mt8">请求头</h4>
        <pre class="code">${U.esc(headersText(JSON.stringify(req.headers || {})))}</pre>
        <h4 class="mt8">请求体</h4>
        <pre class="code">${U.esc(prettyJSON(req.body))}</pre>
      </div>
      <div class="detail-section">
        <h4>响应</h4>
        ${kvHTML([['HTTP 状态码', resp.status !== undefined ? U.esc(String(resp.status)) : '-']])}
        <h4 class="mt8">响应头</h4>
        <pre class="code">${U.esc(headersText(JSON.stringify(resp.headers || {})))}</pre>
        <h4 class="mt8">响应体</h4>
        <pre class="code">${U.esc(prettyJSON(resp.body))}</pre>
      </div>
    `, true);
  },
};
