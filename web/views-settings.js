// ===== 设置视图（基础设置 / 路由策略 / 冷却池）=====
Views.Settings = {
  cooldownTimer: null,

  async render(el) {
    const seq = App.renderSeq; // M9（v1.7.6）：路由切换守卫
    this.stopPoll();
    el.innerHTML = '<div class="muted">加载中...</div>';
    let s;
    try {
      s = await API.get('/api/settings');
    } catch (e) {
      if (seq === App.renderSeq) el.innerHTML = `<div class="card"><h3>加载失败</h3><p class="muted">${U.esc(e.message)}</p></div>`;
      return;
    }
    if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应，避免旧设置页重绘回来并复活冷却池轮询
    el.innerHTML = `
      <div class="card">
        <h3>基础设置</h3>
        <div class="form-row">
          <label class="field"><span>主机监听地址</span><input type="text" id="s-host" value="${U.esc(s.listen.host)}"></label>
          <label class="field" style="flex:0 0 160px"><span>监听端口</span><input type="number" id="s-port" value="${s.listen.port}"></label>
          <label class="field" style="flex:0 0 160px"><span>IP 层级</span><input type="number" id="s-iptl" value="${s.ip_trust_level}" min="0"></label>
        </div>
        <div class="form-row">
          <label class="field" style="flex:0 0 200px"><span>上游请求超时（秒）</span><input type="number" id="s-timeout" value="${s.upstream_timeout_seconds}" min="1"></label>
          <label class="field" style="flex:0 0 260px"><span>日志数据库路径</span><input type="text" id="s-dbpath" value="${U.esc(s.log.db_path)}"></label>
          <label class="field" style="flex:0 0 220px"><span>记录调用日志</span>
            <div class="switch-line">
              <label class="switch"><input type="checkbox" id="s-log-enabled" ${s.log.enabled ? 'checked' : ''}><span class="slider"></span></label>
              <span class="switch-text" id="s-log-enabled-text">${s.log.enabled ? '已开启' : '已关闭'}</span>
            </div>
          </label>
        </div>
        <label class="field" style="max-width:480px"><span>默认 User-Agent（发往上游的请求）</span>
          <input type="text" id="s-ua" value="${U.esc(s.default_user_agent)}" placeholder="留空使用 Go 默认（Go-http-client/1.1）"></label>
        <p class="form-hint" style="margin-top:4px">部分上游网关会拦截 Go 默认 User-Agent（返回 403），可在此设置全局默认值（如 curl/8.10.1 或浏览器 UA）；供应商自定义请求头中的 User-Agent 优先于此处。留空则不设置，使用 Go 默认。</p>
        <label class="field"><span>测试发送内容（供应商 API Key 测试时发送的消息）</span>
          <textarea id="s-testmsg" rows="2">${U.esc(s.test_message)}</textarea></label>
        <p class="form-hint">当前运行监听：<span class="mono">${U.esc(s.running_listen)}</span>。监听地址/端口修改后需重启服务生效，其余修改立即生效（热加载）。日志记录关闭后新调用不再写入日志库（已有日志仍可查询/删除），需审计排查时再开启。</p>
      </div>

      <div class="card">
        <h3>路由策略<span class="card-sub">多个供应商拥有同一模型时的选择方式</span></h3>
        <label class="field" style="max-width:280px"><span>策略</span>
          <select id="s-strategy">
            <option value="random" ${s.routing.strategy === 'random' ? 'selected' : ''}>随机（random）</option>
            <option value="round_robin" ${s.routing.strategy === 'round_robin' ? 'selected' : ''}>轮询（round_robin）</option>
            <option value="weighted" ${s.routing.strategy === 'weighted' ? 'selected' : ''}>加权随机（weighted，按供应商权重比例随机分配）</option>
          </select>
        </label>
      </div>

      <div class="card">
        <h3>冷却池<span class="card-sub">供应商失败后的自动熔断与恢复</span></h3>
        <div class="form-row">
          <label class="field" style="flex:0 0 220px"><span>是否启用冷却池</span>
            <div class="switch-line">
              <label class="switch"><input type="checkbox" id="s-cd-enabled" ${s.cooldown.enabled ? 'checked' : ''}><span class="slider"></span></label>
              <span class="switch-text" id="s-cd-enabled-text">${s.cooldown.enabled ? '已启用' : '已停用'}</span>
            </div>
          </label>
          <label class="field" style="flex:0 0 200px"><span>冷却池时长（秒）</span><input type="number" id="s-cooldown" value="${s.cooldown.duration_seconds}" min="1"></label>
          <label class="field" style="flex:0 0 220px"><span>失败重试次数</span><input type="number" id="s-cd-retry" value="${s.cooldown.retry_count}" min="1"></label>
        </div>
        <p class="form-hint">同一供应商同一模型的所有 API Key 连续失败达到「失败重试次数」轮后进入冷却池（期间任一次调用成功即清零计数，设为 1 表示一轮失败即冷却）；冷却期满自动恢复，禁用后失败不再进入冷却池。修改立即生效（热加载）。</p>
      </div>

      <button class="btn btn-primary" id="s-save" style="min-width:120px;margin-bottom:24px">保存设置</button>

      <div class="card">
        <div class="flex-between" style="margin-bottom:12px">
          <h3 style="margin-bottom:0">冷却池条目<span class="card-sub">连续失败达到重试次数后进入冷却；每 10 秒自动刷新</span></h3>
          <button class="btn btn-sm btn-ghost" id="cd-clear" style="color:var(--danger)">清空全部</button>
        </div>
        <div class="cooldown-list" id="cooldown-list"><div class="muted">加载中...</div></div>
      </div>`;

    document.getElementById('s-cd-enabled').onchange = (e) => {
      document.getElementById('s-cd-enabled-text').textContent = e.target.checked ? '已启用' : '已停用';
    };
    document.getElementById('s-log-enabled').onchange = (e) => {
      document.getElementById('s-log-enabled-text').textContent = e.target.checked ? '已开启' : '已关闭';
    };
    document.getElementById('s-save').onclick = async () => {
      // v1.8.3（F4）：数值字段做正数校验——负值/0 会被后端静默忽略但前端提示“已生效”，状态不一致
      const numVal = (id, name) => {
        const v = parseInt(document.getElementById(id).value, 10);
        if (!Number.isFinite(v) || v <= 0) { showToast(name + '需为正整数', 'err'); return null; }
        return v;
      };
      const iptl = parseInt(document.getElementById('s-iptl').value, 10);
      const cd = numVal('s-cooldown', '冷却池时长');
      if (cd === null) return;
      const retry = numVal('s-cd-retry', '失败重试次数');
      if (retry === null) return;
      const timeout = numVal('s-timeout', '上游请求超时');
      if (timeout === null) return;
      // v1.8.4（F7）：端口做 1-65535 校验——此前 parseInt || 9080 会把任意非法值
      // 静默替换为 9080，用户以为改了端口、实际生效的是默认值
      const port = parseInt(document.getElementById('s-port').value, 10);
      if (!Number.isFinite(port) || port < 1 || port > 65535) { showToast('监听端口需为 1-65535 的整数', 'err'); return; }
      const body = {
        listen: { host: document.getElementById('s-host').value.trim(), port },
        ip_trust_level: Number.isFinite(iptl) && iptl >= 0 ? iptl : 0,
        routing: { strategy: document.getElementById('s-strategy').value },
        cooldown: {
          enabled: document.getElementById('s-cd-enabled').checked,
          duration_seconds: cd,
          retry_count: retry,
        },
        upstream_timeout_seconds: timeout,
        test_message: document.getElementById('s-testmsg').value,
        default_user_agent: document.getElementById('s-ua').value.trim(),
        log: { enabled: document.getElementById('s-log-enabled').checked, db_path: document.getElementById('s-dbpath').value.trim() },
      };
      try {
        const res = await API.put('/api/settings', body);
        this.cdEnabled = body.cooldown.enabled; // v1.8.2（C9）：同步冷却池启用状态，保存后空态提示立即正确
        if (res && res.warning) showToast(res.warning, 'err');
        else showToast('设置已保存并生效', 'ok');
      } catch (e) {
        showToast('保存失败：' + e.message, 'err');
      }
    };
    document.getElementById('cd-clear').onclick = async () => {
      if (confirmDialog('确认清空全部冷却条目？')) {
        try { await API.post('/api/cooldowns/clear'); showToast('已清空', 'ok'); this.pollCooldown(); } catch (e) { showToast(e.message, 'err'); }
      }
    };
    this.cdEnabled = !!s.cooldown.enabled;
    this.pollCooldown();
    this.cooldownTimer = setInterval(() => this.pollCooldown(), 10000);
  },

  stopPoll() {
    if (this.cooldownTimer) { clearInterval(this.cooldownTimer); this.cooldownTimer = null; }
  },

  // destroy 视图卸载钩子（v1.8.3，F9）：离开设置页时立即停止冷却池轮询
  // （不再依赖“下一次 tick 发现元素不存在后自杀”的 10 秒延迟窗口）。
  destroy() {
    this.stopPoll();
  },

  async pollCooldown() {
    const seq = App.renderSeq; // M9（v1.7.6）：页面切换后停止旧页面的轮询写回
    const box = document.getElementById('cooldown-list');
    if (!box || seq !== App.renderSeq) { this.stopPoll(); return; }
    try {
      const res = await API.get('/api/cooldowns');
      const items = res.items || [];
      if (items.length === 0) {
        box.innerHTML = this.cdEnabled
          ? '<div class="muted">冷却池为空</div>'
          : '<div class="muted">冷却池为空（当前已停用：失败不会进入冷却池）</div>';
        return;
      }
      const now = Date.now();
      box.innerHTML = items.map(it => {
        const remainSec = Math.max(0, Math.round((new Date(it.until).getTime() - now) / 1000));
        const remain = remainSec >= 60 ? `${Math.floor(remainSec / 60)} 分 ${remainSec % 60} 秒` : `${remainSec} 秒`;
        return `<div class="cooldown-item">
          <span><strong>${U.esc(it.provider_name || it.provider_id)}</strong> · 模型 <span class="mono">${U.esc(it.model)}</span></span>
          <span>
            <span class="muted">剩余 ${remain}</span>
            <button class="btn btn-sm btn-ghost" data-cd-remove data-pid="${U.esc(it.provider_id)}" data-model="${U.esc(it.model)}" style="margin-left:8px">移除</button>
          </span>
        </div>`;
      }).join('');
      box.querySelectorAll('[data-cd-remove]').forEach(b => {
        b.onclick = async () => {
          try {
            await API.post('/api/cooldowns/remove', { provider_id: b.dataset.pid, model: b.dataset.model });
            showToast('已移除', 'ok');
            this.pollCooldown();
          } catch (e) { showToast(e.message, 'err'); }
        };
      });
    } catch (e) {
      box.innerHTML = `<div class="muted">加载失败：${U.esc(e.message)}</div>`;
    }
  },
};
