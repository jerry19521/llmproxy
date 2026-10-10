// ===== 应用入口：路由、登录、布局 =====

const App = {
  routes: [],
  renderSeq: 0, // 渲染序号（M9 修复）：每次路由切换自增；视图内 await 返回后校验序号，
                // 防止旧页面的慢响应覆盖新页面内容（如仪表盘旧响应迟到重绘覆盖设置页）

  init() {
    document.getElementById('modal-close').onclick = () => Modal.close();
    document.getElementById('modal-mask').onclick = (e) => {
      if (e.target.id === 'modal-mask' && Modal.maskClose) Modal.close();
    };
    document.getElementById('btn-logout').onclick = () => {
      API.post('/api/logout').catch(() => {});
      API.setToken('');
      location.href = '/login';
    };
    document.getElementById('btn-change-pwd').onclick = () => this.changePwdDialog();
    document.getElementById('login-form').onsubmit = (e) => {
      e.preventDefault();
      this.doLogin();
    };
    window.addEventListener('popstate', () => this.render());
    this.render();
  },

  navigate(path) {
    history.pushState(null, '', path);
    this.render();
  },

  render() {
    const path = location.pathname;
    // 登录页
    if (path === '/login') {
      document.getElementById('login-view').classList.remove('hidden');
      document.getElementById('app-view').classList.add('hidden');
      return;
    }
    // 未登录 → 登录页
    if (!API.token) {
      location.replace('/login');
      return;
    }
    document.getElementById('login-view').classList.add('hidden');
    document.getElementById('app-view').classList.remove('hidden');

    // v1.8.3（F9）：视图卸载钩子——离开旧视图时调用其 destroy（销毁 Chart 实例、
    // 停止轮询等），不再依赖视图内部的滞后自杀机制
    if (this.activeView && this.activeView !== this.viewFor(path) && typeof this.activeView.destroy === 'function') {
      this.activeView.destroy();
    }

    this.renderSeq++; // 本次渲染生效：此前在途的旧视图响应全部作废（M9，v1.7.6）

    const main = document.getElementById('main-content');
    // 高亮菜单
    let active = '/dashboard';
    if (path.startsWith('/providers')) active = '/providers';
    else if (path.startsWith('/apikeys')) active = '/apikeys';
    else if (path.startsWith('/logs')) active = '/logs';
    else if (path.startsWith('/statistics')) active = '/statistics';
    else if (path.startsWith('/settings')) active = '/settings';
    document.querySelectorAll('#side-nav a').forEach(a => {
      a.classList.toggle('active', a.getAttribute('data-route') === active);
      a.onclick = (e) => { e.preventDefault(); this.navigate(a.getAttribute('data-route')); };
    });

    // 视图分发
    let m;
    if (path === '/' || path === '/dashboard' || path === '') {
      this.setTitle('仪表盘');
      this.activeView = Views.Dashboard;
      Views.Dashboard.render(main);
    } else if (path === '/providers') {
      this.setTitle('供应商管理');
      this.activeView = Views.Providers;
      Views.Providers.renderList(main);
    } else if (path === '/providers/new') {
      this.setTitle('新增供应商');
      this.activeView = Views.Providers;
      Views.Providers.renderEdit(main, null);
    } else if ((m = path.match(/^\/providers\/([^/]+)\/edit$/))) {
      this.setTitle('修改供应商');
      this.activeView = Views.Providers;
      Views.Providers.renderEdit(main, m[1]);
    } else if (path === '/apikeys') {
      this.setTitle('代理 Key 管理');
      this.activeView = Views.APIKeys;
      Views.APIKeys.render(main);
    } else if (path === '/logs') {
      this.setTitle('调用日志');
      this.activeView = Views.Logs;
      Views.Logs.render(main);
    } else if (path === '/statistics') {
      this.setTitle('统计分析');
      this.activeView = Views.Statistics;
      Views.Statistics.render(main);
    } else if (path === '/settings') {
      this.setTitle('设置');
      this.activeView = Views.Settings;
      Views.Settings.render(main);
    } else {
      this.setTitle('页面不存在');
      this.activeView = null;
      main.innerHTML = '<div class="card"><h3>404</h3><p class="muted">页面不存在</p></div>';
    }
  },

  // viewFor 路径 → 视图对象（v1.8.3 F9）：用于判断是否需要触发旧视图卸载
  viewFor(path) {
    if (path === '/' || path === '/dashboard' || path === '') return Views.Dashboard;
    if (path.startsWith('/providers')) return Views.Providers;
    if (path === '/apikeys') return Views.APIKeys;
    if (path === '/logs') return Views.Logs;
    if (path === '/statistics') return Views.Statistics;
    if (path === '/settings') return Views.Settings;
    return null;
  },

  setTitle(t) { document.getElementById('page-title').textContent = t; },

  async doLogin() {
    const errEl = document.getElementById('login-error');
    errEl.classList.add('hidden');
    const btn = document.getElementById('login-btn');
    btn.disabled = true;
    try {
      const res = await API.post('/api/login', {
        username: document.getElementById('login-username').value.trim(),
        password: document.getElementById('login-password').value,
      });
      API.setToken(res.token);
      location.href = '/dashboard';
    } catch (e) {
      errEl.textContent = '登录失败：' + e.message;
      errEl.classList.remove('hidden');
    } finally {
      btn.disabled = false;
    }
  },

  changePwdDialog() {
    Modal.open('修改密码', `
      <label class="field"><span>旧密码</span><input type="password" id="pwd-old"></label>
      <label class="field"><span>新密码</span><input type="password" id="pwd-new"></label>
      <label class="field"><span>确认新密码</span><input type="password" id="pwd-new2"></label>
      <div id="pwd-err" class="form-error hidden"></div>
      <button class="btn btn-primary btn-block" id="pwd-submit">保存</button>
    `);
    document.getElementById('pwd-submit').onclick = async () => {
      const oldP = document.getElementById('pwd-old').value;
      const newP = document.getElementById('pwd-new').value;
      const newP2 = document.getElementById('pwd-new2').value;
      const errEl = document.getElementById('pwd-err');
      errEl.classList.add('hidden');
      if (!newP) { errEl.textContent = '新密码不能为空'; errEl.classList.remove('hidden'); return; }
      if (newP !== newP2) { errEl.textContent = '两次输入的新密码不一致'; errEl.classList.remove('hidden'); return; }
      try {
        await API.put('/api/password', { old_password: oldP, new_password: newP });
        Modal.close();
        showToast('密码修改成功', 'ok');
      } catch (e) {
        errEl.textContent = e.message;
        errEl.classList.remove('hidden');
      }
    };
  },
};

document.addEventListener('DOMContentLoaded', () => App.init());
