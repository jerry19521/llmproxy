// ===== 仪表盘视图 =====
Views.Dashboard = {
  async render(el) {
    const seq = App.renderSeq; // M9（v1.0.0）：路由切换守卫
    el.innerHTML = '<div class="muted">加载中...</div>';
    try {
      const [d, recent] = await Promise.all([
        API.get('/api/dashboard'),
        API.get('/api/logs?page=1&page_size=10'),
      ]);
      if (seq !== App.renderSeq) return; // 页面已切换：丢弃过期响应，避免覆盖新页面
      const rows = (recent.logs || []).map(l => `
        <tr>
          <td>${U.esc(U.fmtTime(l.timestamp))}</td>
          <td class="mono">${U.esc(l.apikey_name || l.apikey)}</td>
          <td>${U.esc(l.provider || '-')}</td>
          <td class="mono">${U.esc(l.requested_model)}</td>
          <td>${badgeStatus(l.status)}</td>
          <td>${U.fmtNum(l.duration_ms)}</td>
          <td>${U.fmtNum(l.total_tokens)}</td>
        </tr>`).join('');

      el.innerHTML = `
        <div class="stat-grid">
          <div class="stat-card accent-blue">
            <div class="stat-label">供应商</div>
            <div class="stat-value">${d.providers.enabled} / ${d.providers.total}</div>
            <div class="stat-extra">启用 / 总数</div>
          </div>
          <div class="stat-card accent-blue">
            <div class="stat-label">代理 API Key</div>
            <div class="stat-value">${d.apikeys.enabled} / ${d.apikeys.total}</div>
            <div class="stat-extra">启用 / 总数</div>
          </div>
          <div class="stat-card accent-green">
            <div class="stat-label">今日调用</div>
            <div class="stat-value">${U.fmtNum(d.today_calls)}</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">总调用次数</div>
            <div class="stat-value">${U.fmtNum(d.total_calls)}</div>
          </div>
          <div class="stat-card accent-green">
            <div class="stat-label">成功率</div>
            <div class="stat-value">${U.fmtPct(d.success_rate)}</div>
          </div>
          <div class="stat-card">
            <div class="stat-label">Token 总量</div>
            <div class="stat-value" style="font-size:18px">${U.fmtNum(d.tokens.total)}</div>
            <div class="stat-extra">输入 ${U.fmtNum(d.tokens.prompt)} · 输出 ${U.fmtNum(d.tokens.completion)}</div>
          </div>
        </div>

        <div class="card">
          <h3>运行状态</h3>
          ${kvHTML([
            ['监听地址', `<span class="mono">${U.esc(d.listen)}</span>`],
            ['路由策略', U.esc(this.strategyName(d.routing_strategy))],
            ['冷却池条目', `${d.cooldown_pool_size} 条`],
            ['上游超时', `${d.upstream_timeout_seconds} 秒`],
          ])}
        </div>

        <div class="card">
          <div class="flex-between">
            <h3 style="margin-bottom:0">最近调用</h3>
            <button class="btn btn-link" id="db-more-logs">查看全部 →</button>
          </div>
          <div class="table-wrap mt16">
            <table class="data">
              <thead><tr><th>时间</th><th>API Key</th><th>供应商</th><th>模型</th><th>状态</th><th>耗时(ms)</th><th>Token</th></tr></thead>
              <tbody>${rows || '<tr><td colspan="7" class="muted">暂无调用记录</td></tr>'}</tbody>
            </table>
          </div>
        </div>`;
      document.getElementById('db-more-logs').onclick = () => App.navigate('/logs');
    } catch (e) {
      if (seq !== App.renderSeq) return; // v1.0.0（F2）：页面已切换时迟到的失败响应不覆盖新页面
      el.innerHTML = `<div class="card"><h3>加载失败</h3><p class="muted">${U.esc(e.message)}</p></div>`;
    }
  },

  strategyName(s) {
    return { random: '随机', round_robin: '轮询', weighted: '加权随机' }[s] || s;
  },
};
