// ===== 统计分析视图 =====
Views.Statistics = {
  charts: [],
  querySeq: 0, // 同页查询序号（v1.8.3，F8）

  async render(el) {
    const seq = App.renderSeq; // M9（v1.7.6）：路由切换守卫
    this.destroyCharts();
    el.innerHTML = `
      <div class="card">
        <div class="toolbar" style="margin-bottom:0">
          <label class="field w-sm"><span>时间范围</span>
            <select id="st-range">
              <option value="today">今日</option>
              <option value="7d" selected>近 7 天</option>
              <option value="30d">近 30 天</option>
              <option value="custom">自定义</option>
            </select>
          </label>
          <label class="field hidden" id="st-cs-wrap"><span>开始日期</span><input type="date" id="st-cs"></label>
          <label class="field hidden" id="st-ce-wrap"><span>结束日期</span><input type="date" id="st-ce"></label>
          <button class="btn btn-primary" id="st-load">查询</button>
        </div>
      </div>
      <div id="st-body"><div class="muted">加载中...</div></div>`;

    const rangeSel = document.getElementById('st-range');
    rangeSel.onchange = () => {
      const custom = rangeSel.value === 'custom';
      document.getElementById('st-cs-wrap').classList.toggle('hidden', !custom);
      document.getElementById('st-ce-wrap').classList.toggle('hidden', !custom);
    };
    document.getElementById('st-load').onclick = () => this.loadData();
    await this.loadData();
    if (seq !== App.renderSeq) this.destroyCharts(); // 页面已切换：清理新建图表避免残留
  },

  destroyCharts() {
    this.charts.forEach(c => { try { c.destroy(); } catch (e) { } });
    this.charts = [];
  },

  // destroy 视图卸载钩子（v1.8.3，F9）：离开统计页时销毁全部 Chart 实例，
  // 不再依赖“下次进入统计页时才回收”的滞后清理（避免 detached canvas 与 ResizeObserver 滞留）。
  destroy() {
    this.destroyCharts();
  },

  async loadData() {
    const seq = App.renderSeq; // M9（v1.7.6）：重复查询/页面切换守卫
    const reqSeq = ++this.querySeq; // v1.8.3（F8）：同页多次查询的请求序号，慢的旧响应不再覆盖新结果
    this.destroyCharts(); // L13 修复：每次查询前销毁旧实例，防止重复查询时 Chart 实例（含 ResizeObserver）泄漏累积
    const body = document.getElementById('st-body');
    if (!body) return;
    const rangeSel0 = document.getElementById('st-range');
    if (!rangeSel0) return;
    const range = rangeSel0.value;
    const q = new URLSearchParams({ range });
    if (range === 'custom') {
      const cs = document.getElementById('st-cs').value;
      const ce = document.getElementById('st-ce').value;
      if (!cs || !ce) { showToast('请选择自定义日期范围', 'err'); return; }
      // v1.8.3（F12）：反向区间直接提示，不再静默返回全零数据
      if (cs > ce) { showToast('开始日期不能晚于结束日期', 'err'); return; }
      q.set('custom_start', cs);
      q.set('custom_end', ce);
    }
    body.innerHTML = '<div class="muted">加载中...</div>';
    let d;
    try {
      d = await API.get('/api/statistics?' + q.toString());
    } catch (e) {
      if (seq === App.renderSeq && reqSeq === this.querySeq) body.innerHTML = `<div class="card"><h3>加载失败</h3><p class="muted">${U.esc(e.message)}</p></div>`;
      return;
    }
    // v1.8.3（F8）：页面已切换或已有更新的查询时，丢弃本次响应
    if (seq !== App.renderSeq || reqSeq !== this.querySeq) return;
    const s = d.summary || {};
    body.innerHTML = `
      <div class="stat-grid">
        <div class="stat-card"><div class="stat-label">总调用</div><div class="stat-value">${U.fmtNum(s.total_calls)}</div></div>
        <div class="stat-card accent-green"><div class="stat-label">成功</div><div class="stat-value">${U.fmtNum(s.success)}</div></div>
        <div class="stat-card"><div class="stat-label">失败</div><div class="stat-value" style="color:var(--danger)">${U.fmtNum(s.failure)}</div></div>
        <div class="stat-card accent-green"><div class="stat-label">成功率</div><div class="stat-value">${U.fmtPct(s.success_rate)}</div></div>
        <div class="stat-card"><div class="stat-label">总 Token</div><div class="stat-value" style="font-size:20px">${U.fmtNum(s.total_tokens)}</div></div>
        <div class="stat-card"><div class="stat-label">平均耗时</div><div class="stat-value">${Math.round(s.avg_duration_ms || 0)}<span style="font-size:13px"> ms</span></div></div>
      </div>
      <div class="card">
        <h3>时间序列</h3>
        <div class="chart-box"><canvas id="ch-timeline"></canvas></div>
      </div>
      <div class="chart-row">
        <div class="card">
          <h3>按供应商</h3>
          <div class="chart-box"><canvas id="ch-provider"></canvas></div>
        </div>
        <div class="card">
          <h3>按模型（实际模型）</h3>
          <div class="chart-box"><canvas id="ch-model"></canvas></div>
        </div>
      </div>
      <div class="card">
        <h3>按代理 API Key 明细</h3>
        <div class="table-wrap">
          <table class="data">
            <thead><tr><th>Key 名称</th><th>API Key</th><th>调用次数</th><th>成功</th><th>失败</th><th>成功率</th><th>Token</th><th>平均耗时(ms)</th></tr></thead>
            <tbody>
              ${(d.by_apikey_detail || []).map(r => `
                <tr>
                  <td>${U.esc(r.apikey_name || '-')}</td>
                  <td class="mono">${U.esc(r.apikey_id ? r.apikey_id.slice(0, 8) + '…' : '-')}</td>
                  <td>${U.fmtNum(r.calls)}</td>
                  <td style="color:var(--success)">${U.fmtNum(r.success)}</td>
                  <td style="color:var(--danger)">${U.fmtNum(r.failure)}</td>
                  <td>${U.fmtPct(r.calls ? r.success / r.calls : 0)}</td>
                  <td>${U.fmtNum(r.tokens)}</td>
                  <td>${Math.round(r.avg_duration_ms || 0)}</td>
                </tr>`).join('') || '<tr><td colspan="8" class="muted">暂无数据</td></tr>'}
            </tbody>
          </table>
        </div>
      </div>`;

    this.drawTimeline(d.timeline || []);
    this.drawBar('ch-provider', d.by_provider || [], r => r.provider);
    this.drawBar('ch-model', d.by_model || [], r => r.model);
  },

  drawTimeline(tl) {
    const ctx = document.getElementById('ch-timeline');
    if (!ctx) return;
    // 小时粒度（今日）标签格式化为 "HH:00"，天粒度保持 YYYY-MM-DD
    const fmtLabel = (d) => {
      if (d && d.length === 13 && d[10] === 'T') return d.slice(11) + ':00';
      return d;
    };
    this.charts.push(new Chart(ctx, {
      type: 'line',
      data: {
        labels: tl.map(p => fmtLabel(p.date)),
        datasets: [
          {
            label: '调用量', data: tl.map(p => p.calls), borderColor: '#2f5af5',
            backgroundColor: 'rgba(47,90,245,.1)', fill: true, tension: .3, yAxisID: 'y',
          },
          {
            label: '成功率', data: tl.map(p => (p.success_rate * 100).toFixed(1)), borderColor: '#16a34a',
            backgroundColor: 'transparent', tension: .3, yAxisID: 'y1', borderDash: [5, 4],
          },
          {
            label: '平均耗时(ms)', data: tl.map(p => Math.round(p.avg_duration_ms)), borderColor: '#d97706',
            backgroundColor: 'transparent', tension: .3, yAxisID: 'y',
          },
        ],
      },
      options: {
        responsive: true, maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        scales: {
          y: { beginAtZero: true, position: 'left', title: { display: true, text: '次数 / ms' } },
          y1: { beginAtZero: true, max: 100, position: 'right', title: { display: true, text: '成功率 %' }, grid: { drawOnChartArea: false } },
        },
      },
    }));
  },

  drawBar(canvasId, rows, keyFn) {
    const ctx = document.getElementById(canvasId);
    if (!ctx) return;
    const top = rows.slice(0, 10);
    this.charts.push(new Chart(ctx, {
      type: 'bar',
      data: {
        labels: top.map(keyFn),
        datasets: [
          { label: '调用次数', data: top.map(r => r.calls), backgroundColor: 'rgba(47,90,245,.75)' },
          { label: 'Token', data: top.map(r => r.tokens), backgroundColor: 'rgba(22,163,74,.6)', yAxisID: 'y1' },
        ],
      },
      options: {
        responsive: true, maintainAspectRatio: false,
        scales: {
          y: { beginAtZero: true, position: 'left', title: { display: true, text: '次数' } },
          y1: { beginAtZero: true, position: 'right', title: { display: true, text: 'Token' }, grid: { drawOnChartArea: false } },
          x: { ticks: { maxRotation: 40, minRotation: 0 } },
        },
      },
    }));
  },
};
