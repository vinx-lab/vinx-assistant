// Vinx 助手页面脚本：二次确认、三档模型下拉、侧栏展开与横滑定位、扫码登录轮询；没有外部依赖。
(function () {
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
    fetch('/settings/providers/' + encodeURIComponent(id) + '/models', { method: 'POST' })
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

  // 侧栏：桌面上标签筛选默认展开（手机上折叠）；分类条、设置小节条在手机上横向滚动，把当前项滚到可见处（不动页面的纵向位置）
  if (window.matchMedia('(min-width: 900px)').matches) {
    document.querySelectorAll('details[data-desktop-open]').forEach(function (d) { d.open = true; });
  } else {
    document.querySelectorAll('.cats a[aria-current], .subnav a[aria-current]').forEach(function (cur) {
      var strip = cur.parentNode;
      strip.scrollLeft = cur.offsetLeft - strip.offsetLeft - 16;
    });
  }

  // 3. 扫码登录：轮询状态
  var box = document.querySelector('[data-login-state]');
  if (!box) return;
  var active = ['waiting', 'scanned', 'need_verify'];
  if (active.indexOf(box.getAttribute('data-login-state')) < 0) return;
  var timer = setInterval(function () {
    fetch('/login/status', { cache: 'no-store' }).then(function (r) { return r.json(); }).then(function (s) {
      document.getElementById('login-msg').textContent = s.message;
      document.getElementById('login-qr-box').hidden = !s.has_qr;
      document.getElementById('login-verify').hidden = s.state !== 'need_verify';
      if (active.indexOf(s.state) < 0) { clearInterval(timer); location.reload(); }
    }).catch(function () { /* 忽略，定时器继续 */ });
  }, 2000);
})();
