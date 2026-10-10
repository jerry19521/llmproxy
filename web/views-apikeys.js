// ===== 代理 Key 管理视图 =====
Views.APIKeys = {
  fullKeys: {}, // id → 完整 key（按需拉取后缓存于当前页面会话）

  async render(el) {
    const seq = App.renderSeq; // M9（v1.7.6）：路由切换守卫
    el.innerHTML = '<div class="muted">加载中...</div>';
    try {
      const res = await API.get('/api/apikeys');
      if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应
      const list = res.apikeys || [];
      const rows = list.map(k => `
        <tr data-id="${k.id}">
          <td><strong>${U.esc(k.name)}</strong></td>
          <td class="mono key-cell" data-masked="${U.esc(k.key)}">${U.esc(k.key)}</td>
          <td>${(k.models || []).length === 0 ? '<span class="badge gray">不限制</span>' : U.esc((k.models || []).join(', '))}</td>
          <td>${k.expires_at ? U.esc(U.fmtTime(k.expires_at)) : '<span class="badge gray">永久有效</span>'}</td>
          <td class="mono">${U.esc((k.ip_whitelist || []).join(', '))}</td>
          <td>${badgeEnabled(k.enabled)}</td>
          <td>
            <div class="actions">
              <button class="btn btn-sm btn-ghost" data-act="show">显示</button>
              <button class="btn btn-sm btn-ghost" data-act="copy">复制</button>
              <button class="btn btn-sm btn-ghost" data-act="toggle" data-on="${k.enabled}">${k.enabled ? '停用' : '启用'}</button>
              <button class="btn btn-sm btn-ghost" data-act="edit">修改</button>
              <button class="btn btn-sm btn-ghost" style="color:var(--danger)" data-act="del">删除</button>
            </div>
          </td>
        </tr>`).join('');
      el.innerHTML = `
        <div class="card">
          <div class="flex-between" style="margin-bottom:14px">
            <h3 style="margin-bottom:0">代理 API Key<span class="card-sub">终端用户调用本代理的凭证</span></h3>
            <button class="btn btn-primary" id="ak-add">＋ 新增代理 Key</button>
          </div>
          <div class="table-wrap">
            <table class="data">
              <thead><tr><th>名称</th><th>Key</th><th>可用模型</th><th>有效期至</th><th>IP 白名单</th><th>状态</th><th>操作</th></tr></thead>
              <tbody>${rows || '<tr><td colspan="7" class="muted">暂无代理 Key</td></tr>'}</tbody>
            </table>
          </div>
        </div>`;
      document.getElementById('ak-add').onclick = () => this.editDialog(null);
      el.querySelectorAll('tr[data-id]').forEach(tr => {
        const id = tr.dataset.id;
        const item = list.find(x => x.id === id);
        tr.querySelectorAll('button[data-act]').forEach(b => {
          b.onclick = () => this.rowAction(b.dataset.act, item, tr);
        });
      });
    } catch (e) {
      if (seq !== App.renderSeq) return; // v1.8.3（F2）：页面已切换时迟到的失败响应不覆盖新页面
      el.innerHTML = `<div class="card"><h3>加载失败</h3><p class="muted">${U.esc(e.message)}</p></div>`;
    }
  },

  async rowAction(act, item, tr) {
    const keyCell = tr.querySelector('.key-cell');
    try {
      if (act === 'show') {
        if (keyCell.dataset.shown === '1') {
          keyCell.textContent = keyCell.dataset.masked;
          keyCell.dataset.shown = '';
          tr.querySelector('[data-act=show]').textContent = '显示';
        } else {
          const d = await API.get(`/api/apikeys/${item.id}`);
          this.fullKeys[item.id] = d.key;
          keyCell.textContent = d.key;
          keyCell.dataset.shown = '1';
          tr.querySelector('[data-act=show]').textContent = '隐藏';
        }
      } else if (act === 'copy') {
        let full = this.fullKeys[item.id];
        if (!full) {
          const d = await API.get(`/api/apikeys/${item.id}`);
          full = d.key;
          this.fullKeys[item.id] = full;
        }
        // v1.8.3（F3）：改用带降级的复制工具，非安全上下文（http://IP 访问）不再必然失败
        if (await U.copyText(full)) showToast('已复制完整 Key 到剪贴板', 'ok');
        else showToast('复制失败：请手动选中复制', 'err');
      } else if (act === 'toggle') {
        await API.post(`/api/apikeys/${item.id}/${item.enabled ? 'disable' : 'enable'}`);
        showToast('操作成功', 'ok');
        App.render();
      } else if (act === 'edit') {
        this.editDialog(item);
      } else if (act === 'del') {
        if (confirmDialog(`确认删除代理 Key「${item.name}」？删除后使用该 Key 的调用将立即失效（真删除）。`)) {
          await API.del(`/api/apikeys/${item.id}`);
          showToast('已删除', 'ok');
          App.render();
        }
      }
    } catch (e) {
      showToast(e.message, 'err');
    }
  },

  async editDialog(item) {
    // v1.8.4（F1）：await 期间路由可能已切换——捕获序号，迟到的响应不再弹出表单覆盖新页面
    const seq = App.renderSeq;
    // 可用模型数据源
    let allModels = [];
    try {
      const res = await API.get('/api/models');
      if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应
      allModels = res.models || [];
    } catch (e) {
      if (seq !== App.renderSeq) return; // 页面已切换：迟到的失败不再弹 toast
      // v1.8.3（F10）：不再静默吞错——提示加载失败，已勾选模型仍可保存（转入额外模型输入框）
      showToast('模型列表加载失败：' + e.message + '（已有勾选仍可保存）', 'err');
    }
    const modelOptions = allModels.map(m =>
      `<label class="check-row">
        <input type="checkbox" data-am="${U.esc(m.id)}" ${item && (item.models || []).some(x => x.toLowerCase() === m.id.toLowerCase()) ? 'checked' : ''}>
        <span class="check-label mono">${U.esc(m.id)}${m.alias_for ? ` <span class="muted">（别名 → ${U.esc(m.alias_for)}）</span>` : ''}</span>
      </label>`).join('');
    const modelsText = item && (item.models || []).length
      ? item.models.filter(m => !allModels.some(a => a.id.toLowerCase() === m.toLowerCase())).join(', ')
      : '';

    Modal.open(item ? '修改代理 Key' : '新增代理 Key', `
      <label class="field"><span>名称 *</span><input type="text" id="akf-name" value="${U.esc(item ? item.name : '')}" placeholder="如 客户端A"></label>
      <label class="field"><span>API Key *</span>
        <div class="row-item">
          <input type="text" id="akf-key" value="${U.esc(item ? item.key : '')}" placeholder="sk-...（32位随机字符串）" autocomplete="off">
          <button class="btn btn-sm btn-ghost" id="akf-gen">随机生成</button>
        </div>
      </label>
      <label class="field"><span>有效期至（留空表示永久有效）</span><input type="date" id="akf-exp" value="${item && item.expires_at ? U.esc(U.fmtDateLocal(item.expires_at)) : ''}"></label>
      <label class="field"><span>IP 白名单（多条规则用英文逗号分隔；* 允许所有；192.168.* 前缀匹配；支持精确 IP）</span>
        <input type="text" id="akf-ip" value="${U.esc(item ? (item.ip_whitelist || ['*']).join(', ') : '*')}" placeholder="* 或 192.168.*,10.0.*"></label>
      <div class="field"><span>是否启用</span>
        <label class="switch"><input type="checkbox" id="akf-enabled" ${!item || item.enabled ? 'checked' : ''}><span class="slider"></span></label>
      </div>
      <div class="field"><span>可用模型（不勾选表示不限制）</span>
        <div style="max-height:200px;overflow:auto;border:1px solid var(--border);border-radius:8px;padding:8px 12px">
          ${modelOptions || '<div class="muted">当前没有可用模型（请先在供应商中配置）</div>'}
        </div>
        ${modelsText ? `<div class="form-hint mt8">以下模型来自已停用/已删除的供应商，仍将保留：${U.esc(modelsText)}。如需修改请清空该字段说明。<input type="text" id="akf-extra-models" value="${U.esc(modelsText)}" placeholder="额外模型（逗号分隔）"></div>` : ''}
      </div>
      <div style="display:flex;gap:10px;justify-content:center">
        <button class="btn btn-primary" id="akf-save" style="min-width:120px;text-align:center">保 存</button>
        <button class="btn btn-ghost" id="akf-cancel" style="min-width:120px;text-align:center">取 消</button>
      </div>
    `, true, { maskClose: false });

    document.getElementById('akf-gen').onclick = () => {
      document.getElementById('akf-key').value = U.randomKey();
    };
    document.getElementById('akf-cancel').onclick = () => Modal.close();
    document.getElementById('akf-save').onclick = async (e) => {
      // v1.8.3（F6）：保存期间禁用按钮，防双击重复提交
      const btn = e.target;
      if (btn.disabled) return;
      btn.disabled = true;
      try {
        await this.doSave(item);
      } finally {
        btn.disabled = false;
      }
    };
  },

  async doSave(item) {
      const name = document.getElementById('akf-name').value.trim();
      const key = document.getElementById('akf-key').value.trim();
      if (!name) { showToast('名称不能为空', 'err'); return; }
      if (!key) { showToast('API Key 不能为空', 'err'); return; }
      const exp = document.getElementById('akf-exp').value;
      const ipRaw = document.getElementById('akf-ip').value.trim();
      const ipList = ipRaw ? ipRaw.split(',').map(s => s.trim()).filter(Boolean) : ['*'];
      const models = Array.from(document.querySelectorAll('input[data-am]')).filter(b => b.checked).map(b => b.dataset.am);
      const extra = document.getElementById('akf-extra-models');
      if (extra && extra.value.trim()) {
        extra.value.split(',').map(s => s.trim()).filter(Boolean).forEach(m => {
          if (!models.some(x => x.toLowerCase() === m.toLowerCase())) models.push(m);
        });
      }
      const body = {
        name, key,
        expires_at: exp ? new Date(exp + 'T23:59:59').toISOString() : '',
        ip_whitelist: ipList,
        enabled: document.getElementById('akf-enabled').checked,
        models,
      };
      try {
        if (item) await API.put(`/api/apikeys/${item.id}`, body);
        else await API.post('/api/apikeys', body);
        // M10 修复：保存成功后使完整 Key 缓存失效，避免「复制」拿到修改前的旧 Key
        if (item) delete this.fullKeys[item.id];
        Modal.close();
        showToast('保存成功', 'ok');
        App.render();
      } catch (e) {
        showToast('保存失败：' + e.message, 'err');
      }
  },
};
