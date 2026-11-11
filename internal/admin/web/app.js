/* bps-server 管理控制台前端。所有数据经 /admin/api，凭证仅保存在内存中。 */
(function () {
  'use strict';

  var state = { csrf: null, view: 'overview', usageOffset: 0 };

  // ---------- 工具 ----------
  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        if (k === 'class') node.className = attrs[k];
        else if (k === 'text') node.textContent = attrs[k];
        else if (k.slice(0, 2) === 'on') node.addEventListener(k.slice(2), attrs[k]);
        else node.setAttribute(k, attrs[k]);
      });
    }
    (children || []).forEach(function (c) { if (c) node.appendChild(c); });
    return node;
  }

  function toast(message, isError) {
    var t = document.getElementById('toast');
    t.textContent = message;
    t.className = 'toast' + (isError ? ' error' : '');
    clearTimeout(t._timer);
    t._timer = setTimeout(function () { t.classList.add('hidden'); }, 3500);
  }

  function fmtTime(ms) {
    if (!ms) return '—';
    var d = new Date(ms);
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
      p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  function fmtTokens(v) { return v === null || v === undefined ? '未上报' : String(v); }

  function badge(text, kind) { return el('span', { class: 'badge ' + kind, text: text }); }

  function statusBadge(status) {
    var map = {
      ready: ['就绪', 'ok'], ready_disabled: ['已禁用', 'off'],
      authorizing: ['授权中', 'warn'], pending_authorization: ['待授权', 'warn'],
      authorization_failed: ['授权失败', 'err'], auth_required: ['需重新授权', 'err'],
      cooldown: ['冷却中', 'warn']
    };
    var m = map[status] || [status, 'off'];
    return badge(m[0], m[1]);
  }

  function outcomeBadge(status) {
    var map = {
      completed: ['成功', 'ok'], incomplete: ['不完整', 'warn'], failed: ['失败', 'err'],
      http_error: ['HTTP 错误', 'err'], upstream_eof: ['上游中断', 'err'],
      client_closed: ['客户端断开', 'off'], unknown: ['未知', 'off']
    };
    var m = map[status] || [status, 'off'];
    return badge(m[0], m[1]);
  }

  function api(path, opts) {
    opts = opts || {};
    var headers = { 'Content-Type': 'application/json' };
    if (state.csrf && path !== '/login') headers['X-CSRF-Token'] = state.csrf;
    return fetch('/admin/api' + path, {
      method: opts.method || 'GET',
      headers: headers,
      body: opts.body ? JSON.stringify(opts.body) : undefined
    }).then(function (res) {
      if (res.status === 401 && path !== '/login' && path !== '/session') {
        showLogin();
        throw new Error('登录已过期，请重新登录');
      }
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) {
          throw new Error(data && data.error ? data.error.message : ('HTTP ' + res.status));
        }
        return data;
      });
    });
  }

  function openModal(children) {
    var modal = document.getElementById('modal');
    while (modal.firstChild) modal.removeChild(modal.firstChild);
    children.forEach(function (c) { modal.appendChild(c); });
    document.getElementById('modal-overlay').classList.remove('hidden');
  }

  function closeModal() { document.getElementById('modal-overlay').classList.add('hidden'); }

  document.getElementById('modal-overlay').addEventListener('click', function (e) {
    if (e.target === this) closeModal();
  });

  // ---------- 登录 ----------
  function showLogin() {
    document.getElementById('view-login').classList.remove('hidden');
    document.getElementById('view-main').classList.add('hidden');
  }

  function showMain() {
    document.getElementById('view-login').classList.add('hidden');
    document.getElementById('view-main').classList.remove('hidden');
    switchView('overview');
  }

  document.getElementById('login-form').addEventListener('submit', function (e) {
    e.preventDefault();
    var errEl = document.getElementById('login-error');
    errEl.classList.add('hidden');
    api('/login', { method: 'POST', body: { password: document.getElementById('login-password').value } })
      .then(function (data) {
        state.csrf = data.csrf_token;
        document.getElementById('login-password').value = '';
        showMain();
      })
      .catch(function (err) {
        errEl.textContent = err.message;
        errEl.classList.remove('hidden');
      });
  });

  document.getElementById('logout-btn').addEventListener('click', function () {
    api('/logout', { method: 'POST' }).catch(function () {}).then(function () {
      state.csrf = null;
      showLogin();
    });
  });

  // ---------- 导航 ----------
  document.querySelectorAll('.nav-btn').forEach(function (btn) {
    btn.addEventListener('click', function () { switchView(btn.dataset.view); });
  });

  function switchView(view) {
    state.view = view;
    document.querySelectorAll('.nav-btn').forEach(function (b) {
      b.classList.toggle('active', b.dataset.view === view);
    });
    document.querySelectorAll('.page').forEach(function (p) { p.classList.add('hidden'); });
    document.getElementById('page-' + view).classList.remove('hidden');
    if (view === 'overview') loadOverview();
    if (view === 'accounts') loadAccounts();
    if (view === 'keys') loadKeys();
    if (view === 'usage') loadUsage();
    if (view === 'models') loadModels();
    if (view === 'settings') loadProxySettings();
  }

  // ---------- 总览 ----------
  // tone 为 'warn' 时高亮数值，用于接近上限的容量提示。
  function card(label, value, tone) {
    var valueEl = el('div', { class: 'value', text: String(value) });
    if (tone) valueEl.classList.add('value-' + tone);
    return el('div', { class: 'card' }, [
      el('div', { class: 'label', text: label }),
      valueEl
    ]);
  }

  // fmtBytes 把字节数渲染成便于阅读的单位。
  function fmtBytes(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + ' B';
    var units = ['KB', 'MB', 'GB', 'TB'];
    var v = n / 1024;
    var i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (v >= 10 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
  }

  function loadOverview() {
    var errEl = document.getElementById('overview-error');
    errEl.classList.add('hidden');
    api('/overview').then(function (data) {
      var box = document.getElementById('overview-cards');
      while (box.firstChild) box.removeChild(box.firstChild);
      var u = data.usage_today || {};
      box.appendChild(card('可用账号', (data.accounts || {}).enabled + ' / ' + (data.accounts || {}).total));
      box.appendChild(card('在途请求', (data.accounts || {}).active_requests || 0));
      box.appendChild(card('今日请求', u.requests || 0));
      box.appendChild(card('今日成功', u.completed || 0));
      box.appendChild(card('今日失败', u.failed || 0));
      box.appendChild(card('输入 Tokens', fmtTokens(u.input_tokens)));
      box.appendChild(card('输出 Tokens', fmtTokens(u.output_tokens)));
      box.appendChild(card('Key 数量', (data.api_keys || {}).enabled + ' / ' + (data.api_keys || {}).total));
      // 图片暂存用量：仅在中继启用时显示，避免展示无意义的 0。
      var ir = data.image_relay;
      if (ir) {
        var used = ir.bytes + (ir.reserved_bytes || 0);
        var pct = ir.max_bytes ? Math.round(used * 100 / ir.max_bytes) : 0;
        box.appendChild(card('图片暂存', fmtBytes(used) + ' / ' + fmtBytes(ir.max_bytes)));
        box.appendChild(card('暂存图片数', ir.images + ' / ' + ir.max_images));
        box.appendChild(card('暂存占用率', pct + '%', pct >= 80 ? 'warn' : ''));
        box.appendChild(card('累计淘汰', ir.evicted_total || 0));
      }
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    });
  }

  // ---------- 账号 ----------
  function loadAccounts() {
    var errEl = document.getElementById('accounts-error');
    errEl.classList.add('hidden');
    api('/accounts').then(function (data) {
      var tbody = document.querySelector('#accounts-table tbody');
      while (tbody.firstChild) tbody.removeChild(tbody.firstChild);
      (data.accounts || []).forEach(function (a) {
        var actions = el('td');
        var authorized = !!a.upstream_account_id;
        actions.appendChild(el('button', {
          class: 'btn btn-mini', text: authorized ? '重新授权' : '开始授权',
          onclick: function () { openAuthDialog(a.id, a.name, authorized); }
        }));
        actions.appendChild(el('button', {
          class: 'btn btn-mini', text: a.enabled ? '禁用' : '启用',
          onclick: function () { toggleAccount(a.id, !a.enabled); }
        }));
        if (a.authorization && a.authorization.state === 'running') {
          actions.appendChild(el('button', {
            class: 'btn btn-mini danger', text: '取消授权',
            onclick: function () { cancelAuth(a.id); }
          }));
        }
        actions.appendChild(el('button', {
          class: 'btn btn-mini', text: '权重',
          onclick: function () { openWeightDialog(a); }
        }));
        actions.appendChild(el('button', {
          class: 'btn btn-mini', text: '测试',
          onclick: function () { openTestDialog(a); }
        }));
        actions.appendChild(el('button', {
          class: 'btn btn-mini danger', text: '删除',
          onclick: function () { deleteAccount(a); }
        }));
        tbody.appendChild(el('tr', null, [
          el('td', { text: a.name }),
          el('td', { text: a.upstream_account_id || '—' }),
          el('td', { text: a.plan_type || '—' }),
          el('td', null, [statusBadge(a.status)]),
          el('td', { text: String(a.weight) }),
          el('td', { text: String(a.active_requests) }),
          el('td', { text: fmtTime(a.token_expires_at) }),
          el('td', { text: a.last_error_code || a.last_auth_error || '—' }),
          actions
        ]));
      });
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    });
  }

  function toggleAccount(id, enabled) {
    api('/accounts/' + id + '/enabled', { method: 'POST', body: { enabled: enabled } })
      .then(function () { toast(enabled ? '账号已启用' : '账号已禁用'); loadAccounts(); })
      .catch(function (err) { toast(err.message, true); });
  }

  function cancelAuth(id) {
    api('/accounts/' + id + '/authorization', { method: 'DELETE' })
      .then(function () { toast('授权任务已取消'); loadAccounts(); })
      .catch(function (err) { toast(err.message, true); });
  }

  function deleteAccount(a) {
    var msg = '确定删除账号「' + a.name + '」？\n\n' +
      '此操作不可恢复：将删除账号记录、磁盘上的授权凭据文件，并停止其调度。\n' +
      '历史用量记录会保留，但不再关联该账号。';
    if (!window.confirm(msg)) return;
    api('/accounts/' + a.id, { method: 'DELETE' })
      .then(function () { toast('账号已删除'); loadAccounts(); })
      .catch(function (err) { toast(err.message, true); });
  }

  function openWeightDialog(a) {
    var input = el('input', { type: 'number', min: '1', max: '100', value: String(a.weight) });
    var err = el('p', { class: 'error hidden' });
    openModal([
      el('h3', { text: '调整账号权重' }),
      el('div', { class: 'form-row' }, [
        el('span', { text: '账号：' + a.name }),
        input,
        el('span', { class: 'hint', text: '权重 1-100，数值越大分配到的请求越多' })
      ]),
      err,
      el('div', { class: 'modal-actions' }, [
        el('button', { class: 'btn subtle', text: '取消', onclick: closeModal }),
        el('button', {
          class: 'btn primary', text: '保存',
          onclick: function () {
            api('/accounts/' + a.id + '/weight', { method: 'POST', body: { weight: parseInt(input.value, 10) } })
              .then(function () { closeModal(); toast('权重已更新'); loadAccounts(); })
              .catch(function (e) { err.textContent = e.message; err.classList.remove('hidden'); });
          }
        })
      ])
    ]);
  }

  document.getElementById('add-account-btn').addEventListener('click', function () {
    var input = el('input', { type: 'text', placeholder: '例如：工作账号 A' });
    var err = el('p', { class: 'error hidden' });
    openModal([
      el('h3', { text: '新增账号' }),
      el('div', { class: 'form-row' }, [
        el('span', { text: '账号备注名称' }),
        input,
        el('span', { class: 'hint', text: '创建后需要在设备授权页面完成浏览器登录' })
      ]),
      err,
      el('div', { class: 'modal-actions' }, [
        el('button', { class: 'btn subtle', text: '取消', onclick: closeModal }),
        el('button', {
          class: 'btn primary', text: '创建并开始授权',
          onclick: function () {
            api('/accounts', { method: 'POST', body: { name: input.value } })
              .then(function (data) { closeModal(); openAuthDialog(data.id, data.name); })
              .catch(function (e) { err.textContent = e.message; err.classList.remove('hidden'); });
          }
        })
      ])
    ]);
  });

  function openAuthDialog(accountID, name, alreadyAuthorized, isReauth) {
    // 已授权账号默认不重复发起授权：先展示状态，用户确认后才走重新授权。
    if (alreadyAuthorized) {
      var err = el('p', { class: 'error hidden' });
      openModal([
        el('h3', { text: '账号已授权：' + name }),
        el('p', { class: 'muted', text: '该账号已完成设备授权，无需重复授权。若凭据过期或需要更换账号，可重新授权（会生成新的凭据文件）。' }),
        err,
        el('div', { class: 'modal-actions' }, [
          el('button', { class: 'btn subtle', text: '关闭', onclick: function () { closeModal(); loadAccounts(); } }),
          el('button', {
            class: 'btn primary', text: '重新授权',
            onclick: function () {
              if (!window.confirm('重新授权将生成新的凭据文件，完成后需重新启用账号。确认继续？')) return;
              closeModal();
              openAuthDialog(accountID, name, false, true);
            }
          })
        ])
      ]);
      return;
    }
    var panel = el('div', { class: 'auth-panel' });
    var urlEl = el('code', { text: '正在启动授权…' });
    var openBtn = el('button', { class: 'btn primary', text: '打开授权页' });
    openBtn.disabled = true;
    openBtn.addEventListener('click', function () {
      if (panel._url) window.open(panel._url, '_blank', 'noopener');
    });
    var codeEl = el('code', { text: '' });
    var statusEl = el('p', { class: 'muted', text: '' });
    panel.appendChild(el('h3', { text: '设备授权：' + name }));
    panel.appendChild(el('div', { class: 'form-row' }, [
      el('span', { text: '第 1 步：打开授权地址（已自动带入用户码）' }), urlEl, openBtn
    ]));
    panel.appendChild(el('div', { class: 'form-row' }, [
      el('span', { text: '第 2 步：登录后核对用户码' }), codeEl,
      el('span', { class: 'hint', text: '若授权页要求手动输入，请填写上方用户码' })
    ]));
    panel.appendChild(statusEl);
    panel.appendChild(el('div', { class: 'auth-hint' }, [
      el('div', { class: 'auth-hint-title', text: '授权页报错怎么办？' }),
      el('div', {
        class: 'auth-hint-body',
        text: '若浏览器显示「糟糕，出错了！」且 error_code: deviceauth_disabled，' +
          '说明该 ChatGPT 账号关闭了「设备代码登录」。请到 ChatGPT 设置 → 安全 中开启设备代码登录' +
          '（工作区账号需管理员在工作区权限中开启），然后回到本面板重新点「打开授权页」。'
      })
    ]));
    openModal([
      panel,
      el('div', { class: 'modal-actions' }, [
        el('button', {
          class: 'btn subtle', text: '关闭',
          onclick: function () { closeModal(); loadAccounts(); }
        })
      ])
    ]);

    function copyText(text) {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { toast('已复制'); }, function () {});
      }
    }
    codeEl.addEventListener('click', function () { if (panel._code) copyText(panel._code); });

    var stopped = false;
    function poll(isFirst) {
      if (stopped) return;
      // Only the first call of a reauthorization asks to replace credentials;
      // follow-up polls just read the running task's state.
      var body = isFirst ? { reauthorize: true } : undefined;
      api('/accounts/' + accountID + '/authorization', { method: 'POST', body: body })
        .then(function (task) {
          panel._url = task.url || '';
          panel._code = task.user_code || '';
          urlEl.textContent = task.url || '等待授权服务返回…';
          openBtn.disabled = !task.url;
          codeEl.textContent = task.user_code || '…';
          statusEl.textContent = '状态：' + task.state + (task.error ? '；' + task.error : '');
          if (task.state === 'running') {
            setTimeout(function () { poll(false); }, 2000);
          } else if (task.state === 'succeeded') {
            toast('授权成功，账号已就绪（默认禁用，可在列表中启用）');
            loadAccounts();
            setTimeout(function () { if (!document.getElementById('modal-overlay').classList.contains('hidden')) closeModal(); }, 1200);
          } else {
            statusEl.textContent = '授权未完成：' + (task.error || task.state);
          }
        })
        .catch(function (err) {
          statusEl.textContent = err.message;
          setTimeout(function () { poll(false); }, 4000);
        });
    }
    poll(!!isReauth);
    // 对话框关闭时停止轮询
    var observer = new MutationObserver(function () {
      if (document.getElementById('modal-overlay').classList.contains('hidden')) {
        stopped = true;
        observer.disconnect();
      }
    });
    observer.observe(document.getElementById('modal-overlay'), { attributes: true, attributeFilter: ['class'] });
  }

  // ---------- 账号测试 ----------
  function openTestDialog(account) {
    var modelSel = el('select', { id: 'test-model' });
    modelSel.appendChild(el('option', { value: '', text: '加载模型列表…' }));
    modelSel.disabled = true;

    var promptInput = el('textarea', { id: 'test-prompt', rows: '3' });
    promptInput.value = 'hi';

    var resultBox = el('div', { id: 'test-result', class: 'test-result hidden' });
    var errEl = el('p', { class: 'error hidden' });
    var metaEl = el('p', { class: 'muted', text: '' });

    var sendBtn = el('button', { class: 'btn primary', text: '发送' });
    sendBtn.disabled = true;

    openModal([
      el('h3', { text: '测试账号：' + account.name }),
      el('div', { class: 'form-row' }, [
        el('span', { text: '模型' }), modelSel,
        el('span', { class: 'hint', text: '列表来自该账号的上游模型目录' })
      ]),
      el('div', { class: 'form-row' }, [
        el('span', { text: '提示词' }), promptInput
      ]),
      errEl,
      metaEl,
      resultBox,
      el('div', { class: 'modal-actions' }, [
        el('button', { class: 'btn subtle', text: '关闭', onclick: closeModal }),
        sendBtn
      ])
    ]);

    // 载入该账号可用模型
    api('/accounts/' + account.id + '/models')
      .then(function (data) {
        var models = data.models || [];
        while (modelSel.firstChild) modelSel.removeChild(modelSel.firstChild);
        if (models.length === 0) {
          modelSel.appendChild(el('option', { value: '', text: '该账号没有可用模型' }));
          return;
        }
        models.forEach(function (m) {
          var label = m.id + (m.label ? '（' + m.label + '）' : '');
          modelSel.appendChild(el('option', { value: m.id, text: label }));
        });
        if (data.default_model) {
          for (var i = 0; i < modelSel.options.length; i++) {
            if (modelSel.options[i].value === data.default_model) {
              modelSel.selectedIndex = i;
              break;
            }
          }
        }
        modelSel.disabled = false;
        sendBtn.disabled = false;
        metaEl.textContent = '计划：' + (data.plan_type || '—') +
          '；共 ' + models.length + ' 个模型';
      })
      .catch(function (err) {
        while (modelSel.firstChild) modelSel.removeChild(modelSel.firstChild);
        modelSel.appendChild(el('option', { value: '', text: '加载失败' }));
        errEl.textContent = err.message;
        errEl.classList.remove('hidden');
      });

    sendBtn.addEventListener('click', function () {
      var model = modelSel.value;
      var prompt = promptInput.value;
      if (!model) return;
      // 每次发送先清除上一次结果
      while (resultBox.firstChild) resultBox.removeChild(resultBox.firstChild);
      resultBox.className = 'test-result hidden';
      errEl.classList.add('hidden');
      metaEl.textContent = '正在请求上游…';
      sendBtn.disabled = true;
      sendBtn.textContent = '发送中…';

      api('/accounts/' + account.id + '/test', {
        method: 'POST',
        body: { model: model, prompt: prompt, effort: '' }
      })
        .then(function (res) {
          resultBox.className = 'test-result ok';
          var head = el('div', { class: 'test-head' }, [
            el('span', { text: '返回模型：' + (res.reported_model || '—') }),
            el('span', { text: '耗时：' + res.latency_ms + 'ms' })
          ]);
          resultBox.appendChild(head);
          if (res.usage) {
            resultBox.appendChild(el('div', { class: 'test-meta', text:
              '输入 ' + fmtTokens(res.usage.input_tokens) +
              ' · 输出 ' + fmtTokens(res.usage.output_tokens) +
              ' · 合计 ' + fmtTokens(res.usage.total_tokens) }));
          }
          if (res.reasoning) {
            resultBox.appendChild(el('div', { class: 'test-label', text: '推理过程' }));
            resultBox.appendChild(el('pre', { class: 'test-pre', text: res.reasoning }));
          }
          resultBox.appendChild(el('div', { class: 'test-label', text: '输出' }));
          resultBox.appendChild(el('pre', { class: 'test-pre', text: res.text || '（空）' }));
          metaEl.textContent = res.requested_model !== res.reported_model
            ? '注意：请求 ' + res.requested_model + '，上游实际返回 ' + res.reported_model
            : '';
        })
        .catch(function (err) {
          resultBox.className = 'test-result err';
          resultBox.appendChild(el('div', { class: 'test-label', text: '请求失败' }));
          resultBox.appendChild(el('pre', { class: 'test-pre', text: err.message }));
          metaEl.textContent = '';
        })
        .then(function () {
          resultBox.classList.remove('hidden');
          sendBtn.disabled = false;
          sendBtn.textContent = '发送';
        });
    });
  }

  // ---------- API Key ----------
  function loadKeys() {
    var errEl = document.getElementById('keys-error');
    errEl.classList.add('hidden');
    api('/api-keys').then(function (data) {
      var tbody = document.querySelector('#keys-table tbody');
      while (tbody.firstChild) tbody.removeChild(tbody.firstChild);
      (data.keys || []).forEach(function (k) {
        var statusEl = el('td');
        if (k.revoked) statusEl.appendChild(badge('已撤销', 'err'));
        else if (k.expired) statusEl.appendChild(badge('已过期', 'err'));
        else if (k.enabled) statusEl.appendChild(badge('启用', 'ok'));
        else statusEl.appendChild(badge('禁用', 'off'));

        var actions = el('td');
        // 复制按钮：按需向服务端索取明文，列表本身只显示脱敏前缀。
        actions.appendChild(el('button', {
          class: 'btn btn-mini', text: '复制',
          title: k.revealable ? '复制完整 Key' : '该 Key 创建于加密存储启用前，无法复制',
          onclick: function () { copyKey(k); }
        }));
        if (!k.revoked) {
          actions.appendChild(el('button', {
            class: 'btn btn-mini', text: k.enabled ? '禁用' : '启用',
            onclick: function () {
              api('/api-keys/' + k.id + '/enabled', { method: 'POST', body: { enabled: !k.enabled } })
                .then(function () { loadKeys(); })
                .catch(function (err) { toast(err.message, true); });
            }
          }));
          actions.appendChild(el('button', {
            class: 'btn btn-mini danger', text: '撤销',
            onclick: function () {
              if (!window.confirm('撤销后该 Key 将永久失效，确认撤销？')) return;
              api('/api-keys/' + k.id + '/revoke', { method: 'POST' })
                .then(function () { toast('Key 已撤销'); loadKeys(); })
                .catch(function (err) { toast(err.message, true); });
            }
          }));
        }
        actions.appendChild(el('button', {
          class: 'btn btn-mini danger', text: '删除',
          onclick: function () { deleteKey(k); }
        }));
        tbody.appendChild(el('tr', null, [
          el('td', { text: k.name }),
          el('td', { text: k.prefix + '…' }),
          statusEl,
          el('td', { text: fmtTime(k.last_used_at) }),
          el('td', { text: k.expires_at ? fmtTime(k.expires_at) : '永不' }),
          el('td', { text: fmtTime(k.created_at) }),
          actions
        ]));
      });
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    });
  }

  document.getElementById('add-key-btn').addEventListener('click', function () {
    var name = el('input', { type: 'text', placeholder: '例如：本机 Codex' });
    var err = el('p', { class: 'error hidden' });
    openModal([
      el('h3', { text: '创建 API Key' }),
      el('div', { class: 'form-row' }, [el('span', { text: 'Key 名称' }), name]),
      err,
      el('div', { class: 'modal-actions' }, [
        el('button', { class: 'btn subtle', text: '取消', onclick: closeModal }),
        el('button', {
          class: 'btn primary', text: '创建',
          onclick: function () {
            api('/api-keys', { method: 'POST', body: { name: name.value } })
              .then(function (data) { showKeyModal(data.key); })
              .catch(function (e) { err.textContent = e.message; err.classList.remove('hidden'); });
          }
        })
      ])
    ]);
  });

  // copyKey 取回明文并写入剪贴板。明文只在这一次请求里出现，不进入页面状态。
  function copyKey(k) {
    api('/api-keys/' + k.id + '/reveal', { method: 'POST' })
      .then(function (data) {
        return writeClipboard(data.key).then(function () {
          toast('已复制完整 Key');
        });
      })
      .catch(function (err) {
        toast(err.message || '复制失败', true);
      });
  }

  // writeClipboard 优先用异步剪贴板 API，非安全上下文回退到 execCommand。
  function writeClipboard(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).catch(function () {
        return fallbackCopy(text);
      });
    }
    return Promise.resolve(fallbackCopy(text));
  }

  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
  }

  function deleteKey(k) {
    var msg = '确定删除 Key「' + k.name + '」？\n\n' +
      '此操作不可恢复：该 Key 立即失效，记录将被永久移除。\n' +
      '历史用量记录会保留，但不再关联该 Key。';
    if (!window.confirm(msg)) return;
    api('/api-keys/' + k.id, { method: 'DELETE' })
      .then(function () { toast('Key 已删除'); loadKeys(); })
      .catch(function (err) { toast(err.message, true); });
  }

  function showKeyModal(plaintext) {
    var reveal = el('div', { class: 'key-reveal', text: plaintext });
    var copyBtn = el('button', {
      class: 'btn primary', text: '复制 Key',
      onclick: function () {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(plaintext).then(function () { toast('已复制'); }, function () {});
        }
      }
    });
    openModal([
      el('h3', { text: 'Key 已创建' }),
      el('p', { class: 'muted', text: '完整 Key 仅显示这一次，请立即复制保存。' }),
      reveal,
      el('div', { class: 'modal-actions' }, [
        copyBtn,
        el('button', { class: 'btn subtle', text: '我已保存，关闭', onclick: function () { closeModal(); loadKeys(); } })
      ])
    ]);
  }

  // ---------- 用量 ----------
  function usageQuery() {
    var params = [];
    function add(k, v) { if (v) params.push(k + '=' + encodeURIComponent(v)); }
    var from = document.getElementById('f-from').value;
    var to = document.getElementById('f-to').value;
    if (from) add('from', new Date(from).getTime());
    if (to) add('to', new Date(to).getTime());
    add('account_id', document.getElementById('f-account').value);
    add('api_key_id', document.getElementById('f-key').value);
    add('model', document.getElementById('f-model').value.trim());
    add('group_by', document.getElementById('f-group').value);
    add('limit', '50');
    add('offset', String(state.usageOffset));
    return params.join('&');
  }

  function fillFilterOptions() {
    api('/accounts').then(function (data) {
      var sel = document.getElementById('f-account');
      while (sel.options.length > 1) sel.remove(1);
      (data.accounts || []).forEach(function (a) {
        var opt = el('option', { value: a.id, text: a.name });
        sel.appendChild(opt);
      });
    });
    api('/api-keys').then(function (data) {
      var sel = document.getElementById('f-key');
      while (sel.options.length > 1) sel.remove(1);
      (data.keys || []).forEach(function (k) {
        var opt = el('option', { value: k.id, text: k.name });
        sel.appendChild(opt);
      });
    });
  }

  function loadUsage() {
    var errEl = document.getElementById('usage-error');
    errEl.classList.add('hidden');
    var q = usageQuery();
    Promise.all([
      api('/usage/summary?' + q),
      api('/usage/requests?' + q)
    ]).then(function (results) {
      var summary = results[0].rows || [];
      var box = document.getElementById('usage-summary');
      while (box.firstChild) box.removeChild(box.firstChild);
      summary.forEach(function (row) {
        var c = el('div', { class: 'card' }, [
          el('div', { class: 'label', text: row.label }),
          el('div', { class: 'value', text: String(row.requests) })
        ]);
        c.appendChild(el('div', { class: 'muted', text:
          '成功 ' + row.completed + ' / 失败 ' + row.failed +
          ' · 输入 ' + fmtTokens(row.input_tokens) + ' · 输出 ' + fmtTokens(row.output_tokens) +
          (row.avg_duration_ms === null ? '' : ' · 平均 ' + Math.round(row.avg_duration_ms) + 'ms')
        }));
        box.appendChild(c);
      });

      var tbody = document.querySelector('#usage-table tbody');
      while (tbody.firstChild) tbody.removeChild(tbody.firstChild);
      var rows = results[1].rows || [];
      rows.forEach(function (u) {
        tbody.appendChild(el('tr', null, [
          el('td', { text: fmtTime(u.started_at) }),
          el('td', { text: u.requested_model || u.reported_model || '—' }),
          el('td', { text: u.account_id || '—' }),
          el('td', { text: u.api_key_id || '—' }),
          el('td', null, [outcomeBadge(u.status)]),
          el('td', { text: fmtTokens(u.input_tokens) }),
          el('td', { text: fmtTokens(u.output_tokens) }),
          el('td', { text: fmtTokens(u.cached_tokens) }),
          el('td', { text: fmtTokens(u.reasoning_tokens) }),
          el('td', { text: u.duration_ms + 'ms' }),
          el('td', { text: u.error_code || '—' })
        ]));
      });
      var total = results[1].total || 0;
      var page = Math.floor(state.usageOffset / 50) + 1;
      var pages = Math.max(1, Math.ceil(total / 50));
      document.getElementById('usage-page-info').textContent = '第 ' + page + ' / ' + pages + ' 页，共 ' + total + ' 条';
      document.getElementById('usage-prev').disabled = state.usageOffset <= 0;
      document.getElementById('usage-next').disabled = state.usageOffset + 50 >= total;
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    });
  }

  document.getElementById('f-apply').addEventListener('click', function () {
    state.usageOffset = 0;
    loadUsage();
  });
  document.getElementById('usage-prev').addEventListener('click', function () {
    state.usageOffset = Math.max(0, state.usageOffset - 50);
    loadUsage();
  });
  document.getElementById('usage-next').addEventListener('click', function () {
    state.usageOffset += 50;
    loadUsage();
  });

  // ---------- 模型 ----------
  // 已同步的模型 ID 列表，映射下拉与「不在目录」提示都用它。
  var modelIDs = [];
  var mappingDraft = [];

  function loadModels() {
    var errEl = document.getElementById('models-error');
    var metaEl = document.getElementById('models-meta');
    var emptyEl = document.getElementById('models-empty');
    errEl.classList.add('hidden');
    api('/models').then(function (data) {
      var tbody = document.querySelector('#models-table tbody');
      while (tbody.firstChild) tbody.removeChild(tbody.firstChild);
      modelIDs = [];

      if (!data.catalog) {
        metaEl.textContent = '';
        emptyEl.classList.remove('hidden');
        refreshModelOptions();
        return;
      }
      emptyEl.classList.add('hidden');
      var cat = data.catalog;
      (cat.data || []).forEach(function (m) {
        modelIDs.push(m.id);
        var efforts = el('td', { class: 'efforts' });
        (m.efforts || []).forEach(function (e) {
          efforts.appendChild(el('span', { class: 'effort-pill', text: e.value }));
        });
        if (!m.efforts || m.efforts.length === 0) {
          efforts.appendChild(el('span', { class: 'muted', text: '—' }));
        }
        tbody.appendChild(el('tr', null, [
          el('td', { class: 'mono', text: m.id }),
          el('td', { text: m.label || '—' }),
          el('td', { text: m.default_effort || '—' }),
          efforts
        ]));
      });
      metaEl.textContent = '上次同步：' + fmtTime(data.synced_at) +
        (data.source_account ? '（来自 ' + data.source_account + '）' : '') +
        '；共 ' + modelIDs.length + ' 个模型';
      refreshModelOptions();
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    }).then(loadMappings).then(loadCapabilities);
  }

  function loadMappings() {
    var errEl = document.getElementById('mappings-error');
    errEl.classList.add('hidden');
    api('/models/mappings').then(function (data) {
      mappingDraft = (data.mappings || []).map(function (m) {
        return { source: m.source, target: m.target };
      });
      renderMappings();
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    });
  }

  // 源模型是否命中某个模型 ID。只认 * 和 ?，与后端保持同一套语义。
  function globMatch(pattern, model) {
    var p = 0, m = 0, starP = -1, starM = 0;
    while (m < model.length) {
      if (p < pattern.length && (pattern[p] === '?' || pattern[p] === model[m])) {
        p++; m++;
      } else if (p < pattern.length && pattern[p] === '*') {
        starP = p; starM = m; p++;
      } else if (starP >= 0) {
        p = starP + 1; starM++; m = starM;
      } else {
        return false;
      }
    }
    while (p < pattern.length && pattern[p] === '*') p++;
    return p === pattern.length;
  }

  function hasWildcard(s) {
    return s.indexOf('*') >= 0 || s.indexOf('?') >= 0;
  }

  // matchSummary 说明一条源模式当前会影响多少模型，帮助用户确认通配符写法。
  function matchSummary(pattern) {
    if (!pattern) return { text: '', cls: 'mapping-hint' };
    if (modelIDs.length === 0) return { text: '', cls: 'mapping-hint' };
    var hits = 0;
    for (var i = 0; i < modelIDs.length; i++) {
      if (globMatch(pattern, modelIDs[i])) hits++;
    }
    if (hits > 0) {
      return {
        text: hasWildcard(pattern) ? '匹配 ' + hits + ' 个模型' : '精确匹配',
        cls: 'mapping-hint ok'
      };
    }
    return {
      text: hasWildcard(pattern) ? '当前目录无匹配' : '不在当前目录',
      cls: 'mapping-hint warn'
    };
  }

  // 源模型输入：datalist 同时支持下拉选择与手动填写通配符。
  function modelInput(value, placeholder) {
    var input = el('input', {
      type: 'text', class: 'mapping-input', list: 'model-options',
      placeholder: placeholder || '选择或输入，如 gpt-6*',
      autocomplete: 'off', spellcheck: 'false'
    });
    input.value = value || '';
    return input;
  }

  // 目标模型必须是具体模型名，因此只用下拉，避免填出无法解析的通配符。
  function targetSelect(value) {
    var sel = el('select', { class: 'mapping-input' });
    sel.appendChild(el('option', { value: '', text: '选择模型…' }));
    var known = modelIDs.slice();
    if (value && known.indexOf(value) === -1) known.push(value);
    known.forEach(function (id) {
      sel.appendChild(el('option', { value: id, text: id }));
    });
    sel.value = value || '';
    return sel;
  }

  function refreshModelOptions() {
    var dl = document.getElementById('model-options');
    while (dl.firstChild) dl.removeChild(dl.firstChild);
    modelIDs.forEach(function (id) {
      dl.appendChild(el('option', { value: id }));
    });
  }

  function renderMappings() {
    var box = document.getElementById('mappings-rows');
    var emptyEl = document.getElementById('mappings-empty');
    while (box.firstChild) box.removeChild(box.firstChild);

    if (mappingDraft.length === 0) {
      emptyEl.classList.remove('hidden');
      return;
    }
    emptyEl.classList.add('hidden');

    mappingDraft.forEach(function (row, idx) {
      var src = modelInput(row.source);
      var dst = targetSelect(row.target);
      var hint = el('span', { class: 'mapping-hint' });

      function syncHint() {
        var s = matchSummary(src.value.trim());
        hint.textContent = s.text;
        hint.className = s.cls;
      }
      src.addEventListener('input', function () {
        mappingDraft[idx].source = src.value;
        syncHint();
      });
      dst.addEventListener('change', function () { mappingDraft[idx].target = dst.value; });
      syncHint();

      box.appendChild(el('div', { class: 'mapping-row' }, [
        src,
        el('span', { class: 'mapping-arrow', text: '→' }),
        dst,
        hint,
        el('button', {
          class: 'btn btn-mini danger', text: '删除',
          onclick: function () { mappingDraft.splice(idx, 1); renderMappings(); }
        })
      ]));
    });
  }

  document.getElementById('add-mapping-btn').addEventListener('click', function () {
    if (modelIDs.length === 0) {
      toast('请先同步上游模型列表', true);
      return;
    }
    mappingDraft.push({ source: '', target: '' });
    renderMappings();
  });

  document.getElementById('save-mappings-btn').addEventListener('click', function () {
    var errEl = document.getElementById('mappings-error');
    errEl.classList.add('hidden');
    var seen = {};
    var payload = [];
    for (var i = 0; i < mappingDraft.length; i++) {
      var s = (mappingDraft[i].source || '').trim();
      var t = (mappingDraft[i].target || '').trim();
      if (!s && !t) continue;
      if (!s || !t) {
        errEl.textContent = '第 ' + (i + 1) + ' 行的源模型与目标模型都必须填写';
        errEl.classList.remove('hidden');
        return;
      }
      // 目标是发给上游的具体模型名，通配符无法解析。
      if (hasWildcard(t)) {
        errEl.textContent = '第 ' + (i + 1) + ' 行的目标模型不能包含通配符';
        errEl.classList.remove('hidden');
        return;
      }
      if (s === t) continue; // 等价于不映射
      if (seen[s]) {
        errEl.textContent = '源模型重复：' + s;
        errEl.classList.remove('hidden');
        return;
      }
      seen[s] = true;
      payload.push({ source: s, target: t });
    }
    api('/models/mappings', { method: 'PUT', body: { mappings: payload } })
      .then(function (data) {
        mappingDraft = (data.mappings || []).map(function (m) {
          return { source: m.source, target: m.target };
        });
        renderMappings();
        toast('映射已保存');
      })
      .catch(function (err) {
        errEl.textContent = err.message;
        errEl.classList.remove('hidden');
      });
  });

  document.getElementById('sync-models-btn').addEventListener('click', function () {
    var btn = this;
    var errEl = document.getElementById('models-error');
    errEl.classList.add('hidden');
    btn.disabled = true;
    btn.textContent = '同步中…';
    api('/models/sync', { method: 'POST' })
      .then(function () { toast('模型列表已同步'); loadModels(); })
      .catch(function (err) {
        errEl.textContent = err.message;
        errEl.classList.remove('hidden');
      })
      .then(function () {
        btn.disabled = false;
        btn.textContent = '同步上游最新';
      });
  });

  // ---------- 账号模型能力 ----------
  var capPollTimer = null;

  function loadCapabilities() {
    var errEl = document.getElementById('cap-error');
    var statusEl = document.getElementById('cap-status');
    errEl.classList.add('hidden');
    api('/capabilities').then(function (data) {
      var tbody = document.querySelector('#cap-table tbody');
      while (tbody.firstChild) tbody.removeChild(tbody.firstChild);

      if (!data.available) {
        var reason = data.unavailable_reason || '归因服务未配置';
        errEl.textContent = '归因检测不可用：' + reason +
          '。请设置 BPS_MODELTRACE_BANK 指向指纹库文件后重启服务。';
        errEl.classList.remove('hidden');
      }

      (data.cells || []).forEach(function (c) {
        var verdict;
        if (c.error) {
          verdict = el('span', { class: 'cap-err', text: '检测失败' });
        } else if (c.verified) {
          verdict = el('span', { class: 'cap-ok', text: '真实提供' });
        } else {
          verdict = el('span', { class: 'cap-bad', text: '已降级' });
        }
        tbody.appendChild(el('tr', null, [
          el('td', { text: c.account }),
          el('td', { text: c.model }),
          el('td', { text: c.error ? '—' : (c.top || '—') }),
          el('td', { text: c.error ? '—' : (c.self_prob * 100).toFixed(1) + '%' }),
          el('td', null, [verdict]),
          el('td', { text: c.checked_at ? fmtTime(c.checked_at) : '—' })
        ]));
      });

      // 定时配置
      if (document.activeElement !== document.getElementById('cap-interval')) {
        document.getElementById('cap-interval').value = data.interval_minutes || 0;
      }
      var chSel = document.getElementById('cap-challenges');
      if (chSel && data.challenges) {
        chSel.value = String(data.challenges);
      }

      // 进度
      var st = data.status || {};
      if (st.running) {
        statusEl.textContent = '检测中 ' + st.done + ' / ' + st.total +
          (st.failed ? '（失败 ' + st.failed + '）' : '');
        scheduleCapPoll();
      } else if (st.finished_at) {
        statusEl.textContent = '上次完成：' + fmtTime(st.finished_at) +
          (st.failed ? '，失败 ' + st.failed + ' 项' : '') +
          (st.last_error ? '；' + st.last_error : '');
      } else if (data.last_run) {
        statusEl.textContent = '上次运行：' + fmtTime(data.last_run);
      }
    }).catch(function (err) {
      errEl.textContent = err.message;
      errEl.classList.remove('hidden');
    });
  }

  function scheduleCapPoll() {
    if (capPollTimer) return;
    capPollTimer = setTimeout(function () {
      capPollTimer = null;
      loadCapabilities();
    }, 3000);
  }

  document.getElementById('detect-cap-btn').addEventListener('click', function () {
    var btn = this;
    var errEl = document.getElementById('cap-error');
    errEl.classList.add('hidden');
    btn.disabled = true;
    btn.textContent = '检测中…';
    api('/capabilities/detect', { method: 'POST' })
      .then(function () { toast('检测已启动'); loadCapabilities(); })
      .catch(function (err) {
        errEl.textContent = err.message;
        errEl.classList.remove('hidden');
      })
      .then(function () {
        btn.disabled = false;
        btn.textContent = '立即检测';
      });
  });

  document.getElementById('save-interval-btn').addEventListener('click', function () {
    var errEl = document.getElementById('cap-error');
    errEl.classList.add('hidden');
    var minutes = parseInt(document.getElementById('cap-interval').value || '0', 10);
    var challenges = parseInt(document.getElementById('cap-challenges').value || '3', 10);
    if (isNaN(minutes) || minutes < 0 || minutes > 10080) {
      errEl.textContent = '间隔必须是 0 到 10080 之间的整数（分钟）';
      errEl.classList.remove('hidden');
      return;
    }
    if (isNaN(challenges) || challenges < 1 || challenges > 3) {
      errEl.textContent = '挑战次数必须在 1 到 3 之间';
      errEl.classList.remove('hidden');
      return;
    }
    api('/capabilities/schedule', {
      method: 'PUT',
      body: { interval_minutes: minutes, challenges: challenges }
    })
      .then(function () {
        toast(minutes > 0
          ? '已保存：每 ' + minutes + ' 分钟检测，每次 ' + challenges + ' 轮挑战'
          : '已保存：定时检测关闭，挑战 ' + challenges + ' 轮');
        loadCapabilities();
      })
      .catch(function (err) {
        errEl.textContent = err.message;
        errEl.classList.remove('hidden');
      });
  });

  // ---------- 代理设置 ----------
  function renderProxyResult(result, saved) {
    var box = document.getElementById('proxy-result');
    while (box.firstChild) box.removeChild(box.firstChild);
    box.className = 'proxy-result ' + (result.ok ? 'ok' : 'err');
    var title = saved ? (result.ok ? '已保存并立即生效' : '保存失败')
                      : (result.ok ? '代理可用' : '代理不可用');
    box.appendChild(el('div', { class: 'title', text: title }));
    if (result.proxy_url) {
      box.appendChild(el('div', { class: 'meta', text: '代理：' + (result.proxy_url || '直连') }));
    }
    if (result.target) {
      box.appendChild(el('div', { class: 'meta', text: '检测目标：' + result.target }));
    }
    if (result.ok) {
      box.appendChild(el('div', { class: 'meta', text:
        'HTTP ' + result.status_code + ' · 耗时 ' + result.latency_ms + 'ms' +
        (result.status_code === 401 ? '（401 属预期：检测请求不带凭据）' : '') }));
    } else if (result.error) {
      box.appendChild(el('div', { class: 'meta', text: '失败阶段：' + (result.stage || 'unknown') }));
      box.appendChild(el('div', { class: 'meta', text: '错误：' + result.error }));
    }
  }

  function loadProxySettings() {
    var input = document.getElementById('proxy-input');
    var current = document.getElementById('proxy-current');
    var box = document.getElementById('proxy-result');
    box.classList.add('hidden');
    current.textContent = '';
    api('/settings/proxy').then(function (data) {
      input.value = data.proxy_url || '';
      current.textContent = data.proxy_url
        ? '当前生效：' + (data.proxy_display || data.proxy_url)
        : '当前生效：直连（未使用代理）';
    }).catch(function (err) {
      current.textContent = err.message;
    });
  }

  function checkProxy() {
    var btn = document.getElementById('proxy-check-btn');
    var input = document.getElementById('proxy-input');
    btn.disabled = true;
    btn.textContent = '检测中…';
    api('/settings/proxy/check', { method: 'POST', body: { proxy_url: input.value.trim() } })
      .then(function (result) { renderProxyResult(result, false); })
      .catch(function (err) {
        renderProxyResult({ ok: false, error: err.message, stage: 'request' }, false);
      })
      .then(function () {
        btn.disabled = false;
        btn.textContent = '检测代理';
        document.getElementById('proxy-result').classList.remove('hidden');
      });
  }

  function saveProxy() {
    var btn = document.getElementById('proxy-save-btn');
    var input = document.getElementById('proxy-input');
    var value = input.value.trim();
    if (value && !window.confirm('保存后所有上游请求将立即改用该代理，确认？')) return;
    btn.disabled = true;
    btn.textContent = '保存中…';
    api('/settings/proxy', { method: 'PUT', body: { proxy_url: value } })
      .then(function (data) {
        var display = data.proxy_url ? (data.proxy_display || data.proxy_url) : '直连';
        document.getElementById('proxy-current').textContent = '当前生效：' + display;
        renderProxyResult({ ok: true, proxy_url: display, target: '已应用', status_code: '—', latency_ms: '—' }, true);
        document.getElementById('proxy-result').classList.remove('hidden');
        toast('代理已保存并立即生效');
      })
      .catch(function (err) {
        renderProxyResult({ ok: false, error: err.message, stage: 'save' }, true);
        document.getElementById('proxy-result').classList.remove('hidden');
        toast(err.message, true);
      })
      .then(function () {
        btn.disabled = false;
        btn.textContent = '保存并立即生效';
      });
  }

  document.getElementById('proxy-check-btn').addEventListener('click', checkProxy);
  document.getElementById('proxy-save-btn').addEventListener('click', saveProxy);

  // ---------- 启动 ----------
  api('/session').then(function (data) {
    if (data && data.authenticated) {
      state.csrf = data.csrf_token;
      fillFilterOptions();
      showMain();
    } else {
      showLogin();
    }
  }).catch(function () { showLogin(); });
})();

