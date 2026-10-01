// Vinx 助手页面脚本：二次确认、三档模型下拉、看板筛选抽屉、条目菜单、扫码登录轮询；没有外部依赖。
(function () {
  // 子路径前缀（反向代理挂在 /todo 之类下时非空），来自 <html data-base>
  var base = document.documentElement.getAttribute('data-base') || '';

  // 1. 危险操作二次确认
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-confirm]');
    if (b && !confirm(b.getAttribute('data-confirm'))) e.preventDefault();
  });

  // 2. 三档模型下拉：服务商的模型列表存在 <option data-models>（换行分隔），换服务商或拉取后重建模型下拉
  function modelsOf(providerSel) {
    var o = providerSel.options[providerSel.selectedIndex];
    var raw = o ? (o.getAttribute('data-models') || '') : '';
    return raw.split('\n').filter(function (m) { return m !== ''; });
  }
  // keepCurrent：当前值不在新列表里时仍保留（拉取后刷新用）；换服务商时不保留
  function fillModels(providerSel, keepCurrent) {
    var lv = providerSel.getAttribute('data-level');
    var sel = document.querySelector('select[data-model-for="' + lv + '"]');
    if (!sel) return;
    var cur = sel.value, list = modelsOf(providerSel);
    if (cur && keepCurrent && list.indexOf(cur) < 0) list.unshift(cur);
    sel.textContent = '';
    var ph = document.createElement('option');
    ph.value = '';
    ph.textContent = list.length ? '请选择' : '没有可选模型';
    sel.appendChild(ph);
    list.forEach(function (m) {
      var o = document.createElement('option');
      o.value = m; o.textContent = m;
      if (m === cur) o.selected = true;
      sel.appendChild(o);
    });
    var box = document.querySelector('[data-manual-box="' + lv + '"]');
    if (box && !list.length) box.open = true;
  }
  document.addEventListener('change', function (e) {
    var t = e.target;
    if (t.matches('select[data-level]')) fillModels(t, false);
    if (t.matches('select[data-model-for]')) {
      var manual = document.querySelector('[data-manual-for="' + t.getAttribute('data-model-for') + '"]');
      if (manual) manual.value = ''; // 从列表选了就不再用手动值
    }
  });

  // 拉取模型列表：服务端会保存下来；这里同步更新各档的下拉
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-models]');
    if (!b || b.tagName !== 'BUTTON') return;
    var id = b.getAttribute('data-models');
    var out = document.querySelector('[data-models-out="' + id + '"]');
    if (!out) return;
    out.textContent = '正在拉取…';
    b.disabled = true;
    fetch(base + '/settings/providers/' + encodeURIComponent(id) + '/models', { method: 'POST' })
      .then(function (r) { return r.json(); })
      .then(function (j) {
        if (j.error) { out.textContent = '拉取失败：' + j.error; return; }
        out.textContent = '共 ' + j.models.length + ' 个模型，已保存，可在下面三档里选择。';
        document.querySelectorAll('select[data-level]').forEach(function (sel) {
          Array.prototype.forEach.call(sel.options, function (o) {
            if (o.value === id) o.setAttribute('data-models', j.models.join('\n'));
          });
          if (sel.value === id) fillModels(sel, true);
        });
      })
      .catch(function () { out.textContent = '拉取失败'; })
      .then(function () { b.disabled = false; });
  });

  // 4. 看板侧栏与筛选抽屉
  var root = document.documentElement, wide = window.matchMedia('(min-width: 861px)');
  root.classList.add('js');
  var fbox = document.getElementById('filterbox'), fbody = document.getElementById('fb-body'), sheet = document.getElementById('filter-sheet');
  // 电脑：标签筛选直接展开在侧栏里
  if (fbox && wide.matches) fbox.open = true;
  // 手机：有 <dialog> 时，筛选按钮打开底部抽屉，把筛选内容移进去；关闭时放回原处
  if (fbox && fbody && sheet && typeof sheet.showModal === 'function') {
    root.classList.add('js-sheet');
    document.addEventListener('click', function (e) {
      if (e.target.closest('[data-open-filter]')) {
        sheet.appendChild(fbody);
        sheet.showModal();
      } else if (e.target.closest('[data-close-filter]')) {
        if (sheet.open) sheet.close();
      } else if (e.target === sheet) { // 点在抽屉外的遮罩上
        var r = sheet.getBoundingClientRect();
        if (e.clientY < r.top || e.clientX < r.left || e.clientX > r.right) sheet.close();
      }
    });
    sheet.addEventListener('close', function () { fbox.appendChild(fbody); });
  }
  // 手机分类横滑：把当前分类滚到可见处（不动页面的纵向位置）
  if (!wide.matches) {
    var cur = document.querySelector('.cats a[aria-current]');
    if (cur) cur.parentNode.scrollLeft = cur.offsetLeft - cur.parentNode.offsetLeft - 16;
  }

  // 5. 条目「⋯」菜单：同时只开一个；点外面或按 Esc 关闭
  function closeMenus(except) {
    document.querySelectorAll('details.rowmenu[open]').forEach(function (d) { if (d !== except) d.open = false; });
  }
  document.addEventListener('click', function (e) { closeMenus(e.target.closest('details.rowmenu')); });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var open = document.querySelector('details.rowmenu[open]');
    if (open) { open.open = false; open.querySelector('summary').focus(); }
  });

  // 6. 网页登录的微信验证码：每 2 秒查询一次，确认后跳回原来的地址；过期时提示换一个
  var pollBox = document.querySelector('[data-signin-poll]');
  if (pollBox) {
    var msg = document.getElementById('code-msg');
    var url = base + '/signin/code/status?next=' + encodeURIComponent(pollBox.getAttribute('data-next') || '/');
    var codeTimer = setInterval(function () {
      fetch(url, { cache: 'no-store', credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (j) {
        if (j.state === 'ok') { clearInterval(codeTimer); location.href = j.next || (base + '/'); }
        else if (j.state === 'expired') {
          clearInterval(codeTimer);
          msg.textContent = '验证码已过期。';
          var a = document.createElement('a');
          a.href = base + '/signin?next=' + encodeURIComponent(pollBox.getAttribute('data-next') || '/');
          a.textContent = '换一个验证码';
          msg.appendChild(document.createTextNode(' '));
          msg.appendChild(a);
        }
      }).catch(function () { /* 网络抖动：下次再查 */ });
    }, 2000);
  }

  // 3. 扫码登录：轮询状态
  var box = document.querySelector('[data-login-state]');
  if (!box) return;
  var active = ['waiting', 'scanned', 'need_verify'];
  if (active.indexOf(box.getAttribute('data-login-state')) < 0) return;
  var timer = setInterval(function () {
    fetch(base + '/login/status', { cache: 'no-store' }).then(function (r) {
      if (r.status === 401) { clearInterval(timer); location.reload(); throw new Error('未登录'); } // 登录失效：刷新后会跳到登录页
      return r.json();
    }).then(function (s) {
      document.getElementById('login-msg').textContent = s.message;
      document.getElementById('login-qr-box').hidden = !s.has_qr;
      document.getElementById('login-verify').hidden = s.state !== 'need_verify';
      if (active.indexOf(s.state) < 0) { clearInterval(timer); location.reload(); }
    }).catch(function () { /* 忽略，定时器继续 */ });
  }, 2000);
})();
