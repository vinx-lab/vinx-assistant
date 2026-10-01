// Vinx 助手页面脚本：确认弹窗、三档模型下拉、看板筛选抽屉、条目菜单、详情页原地编辑、扫码登录轮询；没有外部依赖。
(function () {
  // 子路径前缀（反向代理挂在 /todo 之类下时非空），来自 <html data-base>
  var base = document.documentElement.getAttribute('data-base') || '';

  var root = document.documentElement;
  root.classList.add('js');

  // 1. 危险操作二次确认：页面内的确认弹窗（<dialog>）；不支持 <dialog> 的浏览器退回原生 confirm。
  // 确认后把原按钮再点一次，按原表单提交（formaction、name/value 都保留）。
  var confirmDlg = null, confirmFrom = null;
  function pressAgain(b) {
    b.setAttribute('data-confirmed', '1');
    b.click();
    b.removeAttribute('data-confirmed');
  }
  function buildConfirm() {
    var d = document.createElement('dialog');
    d.className = 'confirm';
    d.setAttribute('aria-labelledby', 'confirm-h');
    d.setAttribute('aria-describedby', 'confirm-msg');
    d.innerHTML = '<h2 id="confirm-h">请确认</h2><p id="confirm-msg"></p>' +
      '<div class="confirm-foot"><button type="button" class="btn" data-confirm-cancel>取消</button><button type="button" class="btn primary" data-confirm-go>确定</button></div>';
    document.body.appendChild(d);
    d.addEventListener('click', function (e) {
      if (e.target.closest('[data-confirm-cancel]')) { d.close(); return; }
      if (e.target.closest('[data-confirm-go]')) { d.close('ok'); return; }
      if (e.target === d) { // 点在遮罩上
        var r = d.getBoundingClientRect();
        if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) d.close();
      }
    });
    d.addEventListener('close', function () {
      var b = confirmFrom;
      confirmFrom = null;
      if (!b) return;
      b.focus();
      if (d.returnValue === 'ok') pressAgain(b);
    });
    return d;
  }
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-confirm]');
    if (!b || b.hasAttribute('data-confirmed')) return;
    e.preventDefault();
    if (typeof HTMLDialogElement !== 'function') {
      if (confirm(b.getAttribute('data-confirm'))) pressAgain(b);
      return;
    }
    confirmDlg = confirmDlg || buildConfirm();
    confirmFrom = b;
    confirmDlg.returnValue = '';
    confirmDlg.querySelector('#confirm-msg').textContent = b.getAttribute('data-confirm');
    var go = confirmDlg.querySelector('[data-confirm-go]');
    go.textContent = b.getAttribute('data-confirm-ok') || '确定';
    go.className = 'btn ' + (b.classList.contains('danger') ? 'danger-solid' : 'primary');
    confirmDlg.showModal();
    confirmDlg.querySelector('[data-confirm-cancel]').focus();
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
  var wide = window.matchMedia('(min-width: 861px)');
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

  // 7. 条目详情原地编辑：标题、分类、优先级、截止、两种标签逐个字段保存（POST {base}/items/{id}/field，返回条目当前状态）
  var card = document.querySelector('[data-field-url]');
  if (card) initItemEdit(card);

  // 6. 网页登录的微信验证码：每 2 秒查询一次，确认后跳回原来的地址；过期时提示换一个
  // 复制要发给 Bot 的文字（没有剪贴板接口时按钮保持隐藏）
  document.querySelectorAll('[data-copy]').forEach(function (b) {
    if (!navigator.clipboard) return;
    b.hidden = false;
    b.addEventListener('click', function () {
      navigator.clipboard.writeText(b.getAttribute('data-copy')).then(function () {
        b.textContent = '已复制';
        setTimeout(function () { b.textContent = '复制'; }, 2000);
      }, function () { b.textContent = '复制失败，请手动输入'; });
    });
  });
  var pollBox = document.querySelector('[data-signin-poll]');
  if (pollBox) {
    var msg = document.getElementById('code-msg');
    var help = document.getElementById('code-help');
    var started = Date.now();
    var url = base + '/signin/code/status?next=' + encodeURIComponent(pollBox.getAttribute('data-next') || '/');
    var codeTimer = setInterval(function () {
      fetch(url, { cache: 'no-store', credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (j) {
        if (j.state === 'ok') { clearInterval(codeTimer); location.href = j.next || (base + '/'); return; }
        if (j.state === 'pending' && help && Date.now() - started > 30000) help.hidden = false; // 30 秒还没确认：给出排查办法
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

  function initItemEdit(card) {
    var url = card.getAttribute('data-field-url');
    var errBox = card.querySelector('[data-field-err]'), h1 = card.querySelector('.detail-title');
    var toast = document.createElement('p');
    toast.className = 'toast';
    toast.setAttribute('role', 'status');
    toast.hidden = true;
    document.body.appendChild(toast);
    var toastTimer;

    function save(params) {
      return fetch(url, { method: 'POST', body: new URLSearchParams(params), credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
        .then(function (r) {
          if (r.status === 401) { // 登录过期：去登录页，登录后回到这里
            location.href = base + '/signin?next=' + encodeURIComponent(location.pathname.slice(base.length) + location.search);
            throw new Error('需要重新登录');
          }
          return r.json().catch(function () { return { error: '保存失败（' + r.status + '）' }; }).then(function (j) {
            if (!r.ok || !j.item) throw new Error(j.error || '保存失败');
            return j.item;
          });
        }, function () { throw new Error('网络不通，没有保存'); });
    }
    function ok(v) {
      render(v);
      errBox.hidden = true;
      toast.textContent = '已保存';
      toast.hidden = false;
      clearTimeout(toastTimer);
      toastTimer = setTimeout(function () { toast.hidden = true; }, 1600);
    }
    function fail(e) {
      errBox.textContent = e.message;
      errBox.hidden = false;
    }
    function boardHref(cat, tag) { return base + '/?cat=' + encodeURIComponent(cat) + '&tag=' + encodeURIComponent(tag); }
    function render(v) {
      h1.textContent = '#' + v.id + ' ' + v.display;
      h1.setAttribute('data-title', v.title);
      document.title = v.display + ' · Vinx 助手';
      card.querySelectorAll('select[data-field]').forEach(function (sel) {
        sel.value = v[sel.getAttribute('data-field')];
        sel.setAttribute('data-prev', sel.value);
      });
      card.querySelector('[data-status-text]').textContent = v.status_name;
      card.querySelector('[data-due-text]').textContent = v.due ? '截止 ' + v.due + (v.overdue ? ' 逾期' : '') : '加截止时间';
      card.querySelector('[data-due-pill]').classList.toggle('is-over', v.overdue);
      card.querySelector('[data-due-date]').value = v.due_date;
      card.querySelector('[data-due-time]').value = v.due_time;
      renderTags('labels', v.labels, v.category);
      renderTags('topics', v.topics, v.category);
    }
    function tagsOf(kind) {
      return Array.prototype.map.call(card.querySelectorAll('[data-tags="' + kind + '"] [data-tag]'), function (c) { return c.getAttribute('data-tag'); });
    }
    function renderTags(kind, list, cat) {
      var row = card.querySelector('[data-tags="' + kind + '"]'), add = row.querySelector('.add-pop');
      row.querySelectorAll('[data-tag]').forEach(function (c) { c.remove(); });
      list.forEach(function (t) {
        var chip = document.createElement('span');
        chip.className = 'chip ' + (kind === 'labels' ? 'label' : 'topic') + ' ed';
        chip.setAttribute('data-tag', t);
        var a = document.createElement('a');
        a.href = boardHref(cat, t);
        a.textContent = (kind === 'topics' ? '#' : '') + t;
        var x = document.createElement('button');
        x.type = 'button';
        x.className = 'chip-x';
        x.setAttribute('data-del-tag', '');
        x.setAttribute('aria-label', '删除' + (kind === 'labels' ? '类别标签' : '内容标签') + ' ' + t);
        x.innerHTML = '<svg class="i" aria-hidden="true" focusable="false"><use href="#i-x"></use></svg>';
        chip.appendChild(a);
        chip.appendChild(x);
        row.insertBefore(chip, add);
      });
    }
    function saveTags(kind, list, after) {
      var p = { field: kind };
      p[kind] = list.join(',');
      return save(p).then(function (v) { ok(v); if (after) after(); }, fail);
    }

    // 标题：双击标题或点铅笔进入编辑；Enter 或失焦保存，Esc 取消；清空保存即交回 AI 生成
    var editing = false;
    function editTitle() {
      if (editing) return;
      editing = true;
      var display = h1.textContent.replace(/^#\d+ /, '');
      var start = h1.getAttribute('data-title') || display;
      var input = document.createElement('input');
      input.className = 'title-input';
      input.maxLength = 60;
      input.value = start;
      input.setAttribute('aria-label', '标题（清空后保存即恢复 AI 生成的标题）');
      input.placeholder = '清空后保存即恢复 AI 生成的标题';
      h1.hidden = true;
      h1.parentNode.insertBefore(input, h1.nextSibling);
      input.focus();
      input.select();
      var done = false;
      function finish(commit) {
        if (done) return;
        done = true;
        var v = input.value.trim();
        input.remove();
        h1.hidden = false;
        editing = false;
        if (commit && v !== start) save({ field: 'title', title: v }).then(ok, fail);
      }
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') { e.preventDefault(); finish(true); card.querySelector('[data-edit-title]').focus(); }
        else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(false); card.querySelector('[data-edit-title]').focus(); }
      });
      input.addEventListener('blur', function () { finish(true); });
    }
    h1.addEventListener('dblclick', editTitle);

    // 分类、优先级：选中即保存，失败时恢复原值
    card.querySelectorAll('select[data-field]').forEach(function (sel) {
      sel.setAttribute('data-prev', sel.value);
      sel.addEventListener('change', function () {
        var p = { field: sel.getAttribute('data-field') };
        p[p.field] = sel.value;
        save(p).then(ok, function (e) { sel.value = sel.getAttribute('data-prev'); fail(e); });
      });
    });

    card.addEventListener('click', function (e) {
      var t = e.target;
      if (t.closest('[data-edit-title]')) { editTitle(); return; }
      var pop = t.closest('details.pop');
      if (t.closest('[data-due-save]') || t.closest('[data-due-clear]')) {
        var clear = !!t.closest('[data-due-clear]');
        save({ field: 'due', due_date: clear ? '' : card.querySelector('[data-due-date]').value, due_time: clear ? '' : card.querySelector('[data-due-time]').value })
          .then(function (v) { ok(v); pop.open = false; pop.querySelector('summary').focus(); }, fail);
        return;
      }
      var x = t.closest('[data-del-tag]');
      if (x) {
        var row = x.closest('[data-tags]'), kind = row.getAttribute('data-tags'), name = x.closest('[data-tag]').getAttribute('data-tag');
        saveTags(kind, tagsOf(kind).filter(function (n) { return n !== name; }), function () { row.querySelector('.add-pop summary').focus(); });
      }
    });
    card.addEventListener('submit', function (e) {
      var f = e.target.closest('[data-add-tag]');
      if (!f) return;
      e.preventDefault();
      var input = f.querySelector('input'), v = input.value.trim(), kind = f.closest('[data-tags]').getAttribute('data-tags');
      if (!v) return;
      var list = tagsOf(kind);
      if (list.indexOf(v) < 0) list.push(v);
      saveTags(kind, list, function () { input.value = ''; input.focus(); });
    });
    // 弹层同时只开一个；打开时焦点进第一个输入框；右边放不下时改为靠右对齐，避免出屏
    card.addEventListener('toggle', function (e) {
      var d = e.target;
      if (!d.matches || !d.matches('details.pop') || !d.open) return;
      card.querySelectorAll('details.pop[open]').forEach(function (o) { if (o !== d) o.open = false; });
      var p = d.querySelector('.popover');
      p.classList.remove('flip');
      if (p.getBoundingClientRect().right > window.innerWidth - 8) p.classList.add('flip');
      var first = p.querySelector('input');
      if (first) first.focus();
    }, true);
    // 点弹层外面或按 Esc 关闭
    document.addEventListener('click', function (e) {
      card.querySelectorAll('details.pop[open]').forEach(function (d) { if (!d.contains(e.target)) d.open = false; });
    });
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape') return;
      var d = card.querySelector('details.pop[open]');
      if (d) { d.open = false; d.querySelector('summary').focus(); }
    });
  }
})();
