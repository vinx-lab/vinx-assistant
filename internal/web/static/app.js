// Vinx 助手页面脚本：只有三件事，没有外部依赖。
(function () {
  // 1. 危险操作二次确认
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-confirm]');
    if (b && !confirm(b.getAttribute('data-confirm'))) e.preventDefault();
  });

  // 2. 拉取模型列表，填进三档的候选
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-models]');
    if (!b) return;
    var id = b.getAttribute('data-models');
    var out = document.querySelector('[data-models-out="' + id + '"]');
    if (!out) return;
    out.textContent = '正在拉取…';
    fetch('/settings/providers/' + encodeURIComponent(id) + '/models', { method: 'POST' })
      .then(function (r) { return r.json(); })
      .then(function (j) {
        if (j.error) { out.textContent = '拉取失败：' + j.error; return; }
        out.textContent = '共 ' + j.models.length + ' 个模型，已填入下面三档的候选。';
        document.querySelectorAll('select[data-level]').forEach(function (sel) {
          if (sel.value !== id) return;
          var dl = document.getElementById('models-' + sel.getAttribute('data-level'));
          if (!dl) return;
          dl.textContent = '';
          j.models.forEach(function (m) { var o = document.createElement('option'); o.value = m; dl.appendChild(o); });
        });
      })
      .catch(function () { out.textContent = '拉取失败'; });
  });

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
