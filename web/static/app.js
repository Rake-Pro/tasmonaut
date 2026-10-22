/* tasmonaut page script. Vanilla, no build step. One file: the frame (theme,
   drawer, account menu), an API wrapper that carries the CSRF token, toasts
   and dialogs, then one controller per page picked by what the page's data
   block contains. */
(function () {
  'use strict';

  // ---------- frame ----------
  var themeOrder = ['auto', 'light', 'dark'];
  function themeCur() { try { return localStorage.getItem('tasmonaut.theme') || 'auto'; } catch (e) { return 'auto'; } }
  function themeApply(t) {
    if (t === 'auto') document.documentElement.removeAttribute('data-theme');
    else document.documentElement.setAttribute('data-theme', t);
    var lbl = document.getElementById('themeLabel');
    if (lbl) lbl.textContent = t.charAt(0).toUpperCase() + t.slice(1);
  }
  themeApply(themeCur());
  var themeBtn = document.getElementById('themeBtn');
  if (themeBtn) themeBtn.addEventListener('click', function () {
    var n = themeOrder[(themeOrder.indexOf(themeCur()) + 1) % themeOrder.length];
    try { localStorage.setItem('tasmonaut.theme', n); } catch (e) {}
    themeApply(n);
  });

  var menuBtn = document.getElementById('menuBtn'), scrim = document.getElementById('scrim');
  function navOpen(open) {
    document.body.classList.toggle('nav-open', open);
    if (menuBtn) menuBtn.setAttribute('aria-expanded', String(open));
  }
  if (menuBtn) menuBtn.addEventListener('click', function () { navOpen(!document.body.classList.contains('nav-open')); });
  if (scrim) scrim.addEventListener('click', function () { navOpen(false); });

  var userBtn = document.getElementById('userBtn'), userMenu = document.getElementById('userMenu');
  if (userBtn && userMenu) {
    userBtn.addEventListener('click', function (e) {
      e.stopPropagation();
      var open = userMenu.hidden;
      userMenu.hidden = !open;
      userBtn.setAttribute('aria-expanded', String(open));
    });
    document.addEventListener('click', function () { userMenu.hidden = true; userBtn.setAttribute('aria-expanded', 'false'); });
    userMenu.addEventListener('click', function (e) { e.stopPropagation(); });
  }

  // ---------- helpers ----------
  function h(tag, attrs, children) {
    var el = document.createElement(tag);
    if (attrs) Object.keys(attrs).forEach(function (k) {
      var v = attrs[k];
      if (v === null || v === undefined || v === false) return;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'html') el.innerHTML = v; // only for trusted SVG below
      else if (k.indexOf('on') === 0 && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? '' : v);
    });
    (children || []).forEach(function (c) {
      if (c === null || c === undefined || c === false) return;
      el.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
    });
    return el;
  }
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }
  function pageData() {
    var el = document.getElementById('page-data');
    if (!el) return {};
    try { return JSON.parse(el.textContent); } catch (e) { return {}; }
  }
  function fmtAgo(iso) {
    if (!iso || iso.indexOf('0001-') === 0) return 'never';
    var d = (Date.now() - new Date(iso).getTime()) / 1000;
    if (d < 5) return 'just now';
    if (d < 60) return Math.round(d) + 's ago';
    if (d < 3600) return Math.round(d / 60) + 'm ago';
    if (d < 86400) return Math.round(d / 3600) + 'h ago';
    return new Date(iso).toLocaleString();
  }
  function fmtUptime(sec, raw) {
    if (!sec) return raw || '';
    var d = Math.floor(sec / 86400), hh = Math.floor((sec % 86400) / 3600), mm = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + 'd ' + hh + 'h';
    if (hh > 0) return hh + 'h ' + mm + 'm';
    return mm + 'm';
  }
  function chip(cls, text, dot) {
    var c = h('span', { class: 'chip ' + cls });
    if (dot) c.appendChild(h('span', { class: 'dot ' + dot }));
    c.appendChild(document.createTextNode(text));
    return c;
  }
  function stateChip(dev) {
    return dev.online ? chip('flat ok', 'Live', 'ok') : chip('flat muted', 'Offline', 'muted');
  }
  function fwChip(dev) {
    var st = dev.status;
    if (!st) return chip('', 'unknown');
    var v = st.Version || {};
    var label = (v.Major || 0) + '.' + (v.Minor || 0) + '.' + (v.Patch || 0);
    if (v.Variant === 'safeboot') return chip('warn', label + ' safeboot');
    if (dev.updateAvailable) return chip('warn', label + ' (update available)');
    return chip('ok', label);
  }

  // ---------- api ----------
  function api(method, url, body, opts) {
    opts = opts || {};
    var init = { method: method, headers: { 'Accept': 'application/json' }, credentials: 'same-origin' };
    if (body instanceof FormData) init.body = body;
    else if (body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
    return fetch(url, init).then(function (res) {
      if (res.status === 401 || res.status === 403) { throw new Error('the SSO gate refused the request (' + res.status + '); reload the page to sign in again'); }
      if (opts.blob) {
        if (!res.ok) return res.json().then(function (j) { throw new Error(j.error || ('HTTP ' + res.status)); });
        return res.blob().then(function (b) { return { blob: b, filename: filenameFrom(res.headers.get('Content-Disposition')) }; });
      }
      return res.text().then(function (t) {
        var j = {};
        try { j = t ? JSON.parse(t) : {}; } catch (e) { j = { error: t }; }
        if (!res.ok) throw new Error(j.error || ('HTTP ' + res.status));
        return j;
      });
    });
  }
  function filenameFrom(cd) {
    var m = /filename="?([^";]+)"?/.exec(cd || '');
    return m ? m[1] : 'download';
  }
  function saveBlob(blob, name) {
    var a = document.createElement('a');
    a.href = URL.createObjectURL(blob); a.download = name;
    document.body.appendChild(a); a.click();
    setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
  }

  // ---------- toasts ----------
  function toast(kind, title, msg) {
    var wrap = document.getElementById('toasts');
    if (!wrap) return;
    var t = h('div', { class: 'toast ' + kind, role: 'status' }, [h('div', { class: 't-title', text: title }), msg ? h('div', { class: 't-msg', text: msg }) : null]);
    wrap.appendChild(t);
    setTimeout(function () { t.remove(); }, kind === 'err' ? 8000 : 4000);
  }
  function fail(err) { toast('err', 'That did not work', err && err.message ? err.message : String(err)); }

  // ---------- dialogs ----------
  // dialog({title, body: [nodes|string], confirm, danger, cancel, typed, build}) -> Promise<result|null>
  // build(form) may add inputs to the form; result is the FormData-ish object the caller reads.
  function dialog(o) {
    return new Promise(function (resolve) {
      var titleId = 'dlg-' + Math.random().toString(36).slice(2);
      var body = h('div', { class: 'modal-body' });
      (Array.isArray(o.body) ? o.body : [o.body]).forEach(function (b) {
        if (!b) return;
        body.appendChild(typeof b === 'string' ? h('p', { text: b }) : b);
      });
      var form = h('form', { class: 'modal-form', novalidate: true });
      var ok = h('button', { class: 'btn ' + (o.danger ? 'danger' : 'primary'), type: 'submit', text: o.confirm || 'OK' });
      var cancel = h('button', { class: 'btn ghost', type: 'button', text: o.cancel || 'Cancel' });
      var typedInput = null;
      if (o.typed) {
        var fg = h('div', { class: 'field-group modal-typed' }, [h('label', { text: 'Type ' + o.typed + ' to confirm' })]);
        typedInput = h('input', { class: 'field mono', type: 'text', autocomplete: 'off' });
        fg.appendChild(typedInput);
        body.appendChild(fg);
        ok.disabled = true;
        typedInput.addEventListener('input', function () { ok.disabled = typedInput.value.trim() !== o.typed; });
      }
      var fields = {};
      if (o.build) o.build(body, fields);
      form.appendChild(body);
      form.appendChild(h('div', { class: 'modal-actions' }, [ok, cancel]));
      var modal = h('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': titleId }, [h('h2', { class: 'modal-title', id: titleId, text: o.title }), form]);
      var scrimEl = h('div', { class: 'modal-scrim' }, [modal]);
      var prev = document.activeElement;
      function close(result) {
        document.removeEventListener('keydown', onKey);
        scrimEl.remove();
        if (prev && prev.focus) prev.focus();
        resolve(result);
      }
      function onKey(e) { if (e.key === 'Escape') close(null); }
      cancel.addEventListener('click', function () { close(null); });
      scrimEl.addEventListener('click', function (e) { if (e.target === scrimEl) close(null); });
      form.addEventListener('submit', function (e) {
        e.preventDefault();
        if (ok.disabled) return;
        var out = {};
        Object.keys(fields).forEach(function (k) { var el = fields[k]; out[k] = el.type === 'file' ? el.files : el.value; });
        close(out);
      });
      document.addEventListener('keydown', onKey);
      document.body.appendChild(scrimEl);
      var first = modal.querySelector('input, select, textarea, button');
      if (first) first.focus();
    });
  }
  function field(label, input, hint) {
    var fg = h('div', { class: 'field-group' }, [h('label', { text: label }), input]);
    if (hint) fg.appendChild(h('div', { class: 'hint', text: hint }));
    return fg;
  }
  function targetList(devs) {
    return h('ul', { class: 'target-list' }, devs.map(function (d) { return h('li', { text: d.name + ' (' + d.host + ')' }); }));
  }

  // ---------- shared actions ----------
  function relaySwitch(dev, relay, onDone) {
    var sw = h('button', { class: 'switch', type: 'button', role: 'switch', 'aria-checked': String(!!relay.On), 'aria-label': 'Power ' + relay.Index + (relay.Name ? ' ' + relay.Name : '') });
    if (!dev.online) sw.classList.add('disabled');
    sw.addEventListener('click', function (e) {
      e.stopPropagation();
      if (sw.classList.contains('busy') || !dev.online) return;
      sw.classList.add('busy');
      api('POST', '/api/devices/' + encodeURIComponent(dev.id) + '/power', { relay: relay.Index, state: 'TOGGLE' })
        .then(function (r) { sw.setAttribute('aria-checked', String(!!r.on)); if (onDone) onDone(r.device); })
        .catch(fail)
        .then(function () { sw.classList.remove('busy'); });
    });
    return sw;
  }
  function startUpgradeDialog(devs, firmware) {
    var ota = h('input', { class: 'field mono', type: 'text', placeholder: 'leave empty to use each device\'s own OtaUrl', autocomplete: 'off' });
    var latest = firmware && firmware.version && firmware.version.Major ? (firmware.version.Major + '.' + firmware.version.Minor + '.' + firmware.version.Patch) : null;
    var intro = devs.length === 1
      ? 'The device downloads the image from its OtaUrl, restarts and comes back on the new version. It is unavailable for about a minute; a relay that is on stays on through the restart only if PowerOnState says so.'
      : devs.length + ' devices upgrade four at a time. Each downloads from its own OtaUrl unless you give one below.';
    return dialog({
      title: devs.length === 1 ? 'Upgrade firmware on ' + devs[0].name + '?' : 'Upgrade firmware on ' + devs.length + ' devices?',
      body: [intro, latest ? 'Latest release: ' + latest + '.' : null,
        'ESP32 boards pass through safeboot and may stay silent for a long time while flashing; the job re-issues the second stage when needed and reports a silent board as such rather than failed.',
        devs.length > 1 ? targetList(devs) : null],
      confirm: 'Upgrade firmware',
      build: function (body, fields) { body.appendChild(field('OtaUrl override', ota, 'Optional. Written to every selected device before the upgrade.')); fields.otaUrl = ota; }
    }).then(function (r) {
      if (!r) return null;
      return api('POST', '/api/bulk/upgrade', { ids: devs.map(function (d) { return d.id; }), otaUrl: r.otaUrl.trim() })
        .then(function (res) { location.href = '/jobs/' + res.job.id; })
        .catch(fail);
    });
  }

  // ---------- devices page ----------
  function devicesPage(data) {
    var rows = document.getElementById('rows'), empty = document.getElementById('empty');
    var filter = document.getElementById('filter'), bulkBar = document.getElementById('bulkBar'), bulkCount = document.getElementById('bulkCount');
    var selAll = document.getElementById('selAll'), headLabels = document.getElementById('headLabels');
    var state = { devices: data.devices || [], firmware: data.firmware || {}, selected: {}, stateFilter: 'all', lastScan: null };

    function visible() {
      var q = (filter.value || '').trim().toLowerCase();
      return state.devices.filter(function (d) {
        if (state.stateFilter === 'online' && !d.online) return false;
        if (state.stateFilter === 'offline' && d.online) return false;
        if (state.stateFilter === 'update' && !d.updateAvailable) return false;
        if (!q) return true;
        var st = d.status || {};
        var hay = [d.name, d.host, st.Hostname, st.Hardware, st.DeviceName, st.Version && st.Version.Raw, st.MAC].join(' ').toLowerCase();
        return hay.indexOf(q) >= 0;
      });
    }
    function selectedDevices() { return state.devices.filter(function (d) { return state.selected[d.id]; }); }
    function updateBulk() {
      var n = selectedDevices().length;
      bulkBar.hidden = n === 0;
      bulkCount.textContent = n + ' selected';
      var vis = visible();
      selAll.checked = vis.length > 0 && vis.every(function (d) { return state.selected[d.id]; });
    }
    function render() {
      var list = visible();
      clear(rows);
      empty.hidden = state.devices.length > 0;
      list.forEach(function (d) {
        var st = d.status || {};
        var cb = h('input', { type: 'checkbox', 'aria-label': 'Select ' + d.name });
        cb.checked = !!state.selected[d.id];
        cb.addEventListener('change', function () { state.selected[d.id] = cb.checked; if (!cb.checked) delete state.selected[d.id]; updateBulk(); });
        var sub = h('div', { class: 'cell-sub' }, [h('span', { class: 'mono', text: d.host }), stateChip(d)]);
        if (st.Hostname && st.Hostname !== d.host) sub.appendChild(h('span', { class: 'trunc', text: st.Hostname }));
        if (d.lastError && !d.online) sub.appendChild(h('span', { class: 'trunc err-text', text: d.lastError, title: d.lastError }));
        var nameCell = h('td', {}, [h('div', { class: 'cell-main' }, [h('a', { class: 'trunc', href: '/devices/' + encodeURIComponent(d.id), text: d.name })]), sub]);
        var hw = h('td', { class: 'hide-sm' }, [h('div', { class: 'cell-main' }, [h('span', { class: 'trunc', text: st.Hardware || '' })]), h('div', { class: 'cell-sub' }, [h('span', { class: 'trunc', text: st.Module !== undefined ? 'module ' + st.Module : '' })])]);
        var fw = h('td', {}, [fwChip(d)]);
        var sw = h('div', { class: 'relay-switches' });
        (st.Relays || []).forEach(function (r) {
          var lbl = (st.Relays.length > 1) ? (r.Name || ('Relay ' + r.Index)) : '';
          sw.appendChild(h('label', { class: 'relay-switch' }, [relaySwitch(d, r, function (fresh) { if (fresh) mergeDevice(fresh); }), lbl ? h('span', { text: lbl }) : null]));
        });
        if (st.Energy) sw.appendChild(h('span', { class: 'energy-note', text: Math.round(st.Energy.Power) + ' W' }));
        var power = h('td', {}, [sw]);
        var sig = h('td', { class: 'hide-sm' }, [st.Wifi ? h('span', { class: 'mono', text: st.Wifi.RSSI + '% ' + st.Wifi.Signal + ' dBm', title: 'channel ' + st.Wifi.Channel }) : null]);
        var up = h('td', { class: 'hide-md' }, [h('span', { class: 'mono', text: d.online ? fmtUptime(st.UptimeSec, st.Uptime) : ('seen ' + fmtAgo(d.lastSeen)) })]);
        rows.appendChild(h('tr', {}, [h('td', { class: 'sel-cell' }, [cb]), nameCell, hw, fw, power, sig, up]));
      });
      clear(headLabels);
      var online = state.devices.filter(function (d) { return d.online; }).length;
      var upd = state.devices.filter(function (d) { return d.updateAvailable; }).length;
      headLabels.appendChild(chip('flat ok', online + ' of ' + state.devices.length + ' live', 'ok'));
      if (upd) headLabels.appendChild(chip('warn', upd + ' with an update available'));
      var fwv = state.firmware && state.firmware.version;
      if (fwv && fwv.Major) headLabels.appendChild(h('span', { class: 'head-meta', text: 'latest Tasmota ' + fwv.Major + '.' + fwv.Minor + '.' + fwv.Patch + (state.firmware.source === 'config' ? ' (pinned)' : '') }));
      if (state.lastScan && state.lastScan.indexOf('0001-') !== 0) headLabels.appendChild(h('span', { class: 'head-meta', text: 'last scan ' + fmtAgo(state.lastScan) }));
      updateBulk();
    }
    function mergeDevice(fresh) {
      var i = state.devices.findIndex(function (d) { return d.id === fresh.id; });
      if (i >= 0) state.devices[i] = fresh; else state.devices.push(fresh);
      render();
    }
    function load() {
      return api('GET', '/api/devices').then(function (r) {
        state.devices = r.devices || []; state.firmware = r.firmware || {}; state.lastScan = r.lastScan;
        Object.keys(state.selected).forEach(function (id) { if (!state.devices.some(function (d) { return d.id === id; })) delete state.selected[id]; });
        render();
      }).catch(function (e) { if (e.message !== 'signed out') fail(e); });
    }

    filter.addEventListener('input', render);
    document.getElementById('stateFilter').addEventListener('click', function (e) {
      var b = e.target.closest('[data-state]');
      if (!b) return;
      state.stateFilter = b.getAttribute('data-state');
      Array.prototype.forEach.call(e.currentTarget.querySelectorAll('[data-state]'), function (x) { x.classList.toggle('on', x === b); x.classList.toggle('off', x !== b && x.getAttribute('data-state') !== 'all'); });
      render();
    });
    selAll.addEventListener('change', function () {
      visible().forEach(function (d) { if (selAll.checked) state.selected[d.id] = true; else delete state.selected[d.id]; });
      render();
    });

    document.addEventListener('click', function (e) {
      var btn = e.target.closest('[data-action], [data-bulk]');
      if (!btn) return;
      var action = btn.getAttribute('data-action'), bulk = btn.getAttribute('data-bulk');
      if (action === 'refresh-all') {
        btn.disabled = true;
        api('POST', '/api/refresh').then(function (r) { state.devices = r.devices || []; state.firmware = r.firmware || {}; render(); toast('ok', 'Refreshed', state.devices.length + ' devices polled'); }).catch(fail).then(function () { btn.disabled = false; });
      } else if (action === 'scan') {
        api('POST', '/api/scan').then(function () { toast('ok', 'Scanning', 'Sweeping the configured ranges; new devices appear as they answer.'); setTimeout(load, 5000); setTimeout(load, 15000); setTimeout(load, 40000); }).catch(fail);
      } else if (action === 'add-device') {
        var inp = h('input', { class: 'field mono', type: 'text', placeholder: '10.0.4.23 or plug-kitchen.lan', autocomplete: 'off', required: true });
        dialog({ title: 'Add device', body: ['The address is probed now; the device joins the inventory for the life of the process. Put it in the configuration to keep it across restarts.'], confirm: 'Add device',
          build: function (body, fields) { body.appendChild(field('Address', inp)); fields.host = inp; } })
          .then(function (r) {
            if (!r) return;
            return api('POST', '/api/devices', { host: r.host.trim() }).then(function (res) { mergeDevice(res.device); toast('ok', 'Added', res.device.name); }).catch(fail);
          });
      } else if (bulk) {
        var devs = selectedDevices();
        if (bulk === 'clear') { state.selected = {}; render(); return; }
        if (!devs.length) return;
        var ids = devs.map(function (d) { return d.id; });
        if (bulk === 'power-on' || bulk === 'power-off') {
          var st = bulk === 'power-on' ? 'ON' : 'OFF';
          dialog({ title: 'Power ' + st.toLowerCase() + ' ' + devs.length + ' devices?', body: ['Relay 1 on each selected device switches ' + st.toLowerCase() + '. Multi-relay boards switch only their first relay; use Send command with Power0 for all of them.', targetList(devs)], confirm: 'Power ' + st.toLowerCase() })
            .then(function (r) { if (!r) return; return api('POST', '/api/bulk/power', { ids: ids, relay: 1, state: st }).then(function (res) { location.href = '/jobs/' + res.job.id; }).catch(fail); });
        } else if (bulk === 'command') {
          var cmd = h('input', { class: 'field mono', type: 'text', placeholder: 'SetOption65 1', autocomplete: 'off', required: true });
          dialog({ title: 'Send a command to ' + devs.length + ' devices?', body: ['The same console command goes to every selected device, four at a time. Replies are collected on the job page.', targetList(devs)], confirm: 'Send command',
            build: function (body, fields) { body.appendChild(field('Command', cmd)); fields.command = cmd; } })
            .then(function (r) { if (!r || !r.command.trim()) return; return api('POST', '/api/bulk/command', { ids: ids, command: r.command.trim() }).then(function (res) { location.href = '/jobs/' + res.job.id; }).catch(fail); });
        } else if (bulk === 'backup') {
          btn.disabled = true;
          api('POST', '/api/bulk/backup', { ids: ids }, { blob: true }).then(function (r) { saveBlob(r.blob, r.filename); toast('ok', 'Downloaded', devs.length + ' configuration dumps in one zip'); }).catch(fail).then(function () { btn.disabled = false; });
        } else if (bulk === 'upgrade') {
          startUpgradeDialog(devs, state.firmware);
        }
      }
    });

    render();
    setInterval(function () { if (!document.querySelector('.modal-scrim')) load(); }, 30000);
  }

  // ---------- device page ----------
  function devicePage(data) {
    var dev = data.device, raw = data.raw, firmware = data.firmware || {};
    var relays = document.getElementById('relays'), banner = document.getElementById('offlineBanner'), headLabels = document.getElementById('headLabels');
    var out = document.getElementById('consoleOut');
    function kv(id, pairs) {
      var el = clear(document.getElementById(id));
      pairs.forEach(function (p) {
        if (p[1] === undefined || p[1] === null || p[1] === '') return;
        el.appendChild(h('span', { class: 'k', text: p[0] }));
        el.appendChild(typeof p[1] === 'string' || typeof p[1] === 'number' ? h('span', { class: 'v', text: String(p[1]) }) : h('span', { class: 'v' }, [p[1]]));
      });
    }
    function render() {
      var st = dev.status || {};
      document.getElementById('devName').textContent = dev.name;
      document.getElementById('crumbName').textContent = dev.name;
      document.getElementById('devHost').textContent = dev.host;
      document.getElementById('openUi').href = 'http://' + dev.host + '/';
      document.title = dev.name + ' - Tasmonaut';
      clear(headLabels);
      headLabels.appendChild(stateChip(dev));
      headLabels.appendChild(fwChip(dev));
      headLabels.appendChild(h('span', { class: 'head-meta', text: 'polled ' + fmtAgo(dev.lastPolled) + (dev.source ? ', ' + dev.source : '') }));
      banner.hidden = dev.online;
      if (!dev.online) { clear(banner); banner.appendChild(h('b', { text: 'Offline. ' })); banner.appendChild(document.createTextNode((dev.lastError || 'The device did not answer.') + ' Last seen ' + fmtAgo(dev.lastSeen) + '.')); }
      var upBtn = document.getElementById('upgradeBtn');
      upBtn.classList.toggle('primary', !!dev.updateAvailable);
      upBtn.classList.toggle('ghost', !dev.updateAvailable);

      clear(relays);
      (st.Relays || []).forEach(function (r) {
        var sw = relaySwitch(dev, r, function (fresh) { if (fresh) { dev = fresh; render(); } });
        relays.appendChild(h('div', { class: 'relay-row' }, [
          h('div', {}, [h('span', { class: 'relay-name', text: r.Name || ('Relay ' + r.Index) }), h('span', { class: 'relay-idx', text: 'Power' + r.Index })]),
          h('div', { class: 'relay-state' }, [h('span', { class: 'mono', text: r.On ? 'ON' : 'OFF' }), sw])]));
      });
      if (!(st.Relays || []).length) relays.appendChild(h('div', { class: 'hint', text: 'No power outputs reported.' }));
      if (st.Dimmer >= 0) relays.appendChild(h('div', { class: 'hint', text: 'Dimmer ' + st.Dimmer + '%' }));

      kv('kvIdentity', [['Device name', st.DeviceName], ['Friendly names', (st.FriendlyNames || []).join(', ')], ['Topic', st.Topic], ['Module', st.Module], ['Hardware', st.Hardware], ['Source', dev.source]]);
      kv('kvNetwork', [['Address', dev.host], ['Hostname', st.Hostname], ['MAC', st.MAC], ['Gateway', st.Gateway],
        ['Wi-Fi', st.Wifi ? st.Wifi.SSID + ' ch ' + st.Wifi.Channel + ' (' + st.Wifi.Mode + ')' : ''], ['Signal', st.Wifi ? st.Wifi.RSSI + '% / ' + st.Wifi.Signal + ' dBm' : ''], ['AP', st.Wifi ? st.Wifi.BSSID : ''], ['Wi-Fi downtime', st.Wifi ? st.Wifi.Downtime : '']]);
      var fwv = firmware.version && firmware.version.Major ? firmware.version.Major + '.' + firmware.version.Minor + '.' + firmware.version.Patch : 'unknown';
      kv('kvFirmware', [['Version', st.Version ? st.Version.Raw : ''], ['Built', st.BuildDateTime], ['Core / SDK', st.Core ? st.Core + ' / ' + st.SDK : ''], ['OtaUrl', st.OtaURL], ['Latest release', fwv + (firmware.tag ? ' (' + firmware.tag + ')' : '')],
        ['Uptime', fmtUptime(st.UptimeSec, st.Uptime)], ['Restart reason', st.RestartReason], ['Boot count', st.BootCount], ['Started (UTC)', st.StartupUTC], ['Heap', st.Heap ? st.Heap + ' KB' : ''], ['Load', st.LoadAvg]]);
      kv('kvMqtt', [['Broker', st.MQTT && st.MQTT.Host ? st.MQTT.Host + ':' + st.MQTT.Port : ''], ['Client', st.MQTT ? st.MQTT.Client : ''], ['Connections', st.MQTT ? st.MQTT.Count : '']]);
      var ec = document.getElementById('energyCard');
      if (st.Energy) {
        ec.hidden = false;
        var e = st.Energy;
        kv('kvEnergy', [['Power', e.Power + ' W'], ['Apparent / reactive', e.ApparentPower + ' VA / ' + e.ReactivePower + ' var'], ['Power factor', e.Factor], ['Voltage', e.Voltage + ' V'], ['Current', e.Current + ' A'], ['Today', e.Today + ' kWh'], ['Yesterday', e.Yesterday + ' kWh'], ['Total', e.Total + ' kWh']]);
      } else ec.hidden = true;
      document.getElementById('rawJson').textContent = raw ? JSON.stringify(raw, null, 2) : '';
    }
    function refresh() {
      return api('POST', '/api/devices/' + encodeURIComponent(dev.id) + '/refresh').then(function (r) { dev = r.device; raw = r.raw; render(); }).catch(function (e) { if (e.message !== 'signed out') fail(e); });
    }
    function logLine(cls, text) {
      var line = h('div', {}, [h('span', { class: 'c-time', text: new Date().toLocaleTimeString() + ' ' }), h('span', { class: cls, text: text })]);
      out.insertBefore(line, out.firstChild);
    }
    document.getElementById('consoleForm').addEventListener('submit', function (e) {
      e.preventDefault();
      var inp = document.getElementById('consoleInput'), cmd = inp.value.trim();
      if (!cmd) return;
      logLine('c-cmd', '> ' + cmd);
      api('POST', '/api/devices/' + encodeURIComponent(dev.id) + '/command', { command: cmd }).then(function (r) {
        logLine('', JSON.stringify(r.reply));
        inp.value = '';
        if (!/^status/i.test(cmd)) setTimeout(refresh, 1500);
      }).catch(function (err) { logLine('c-err', err.message); });
    });
    document.addEventListener('click', function (e) {
      var btn = e.target.closest('[data-action]');
      if (!btn) return;
      var a = btn.getAttribute('data-action');
      if (a === 'refresh') { btn.disabled = true; refresh().then(function () { btn.disabled = false; }); }
      else if (a === 'restart') {
        dialog({ title: 'Restart ' + dev.name + '?', body: ['The device reboots now and is away for a few seconds. Relays follow PowerOnState.'], confirm: 'Restart device' })
          .then(function (r) { if (!r) return; return api('POST', '/api/devices/' + encodeURIComponent(dev.id) + '/restart').then(function () { toast('ok', 'Restarting', dev.name); setTimeout(refresh, 8000); }).catch(fail); });
      } else if (a === 'upgrade') {
        startUpgradeDialog([dev], firmware);
      } else if (a === 'remove') {
        dialog({ title: 'Remove ' + dev.name + ' from the inventory?', body: ['The device itself is untouched. A configured device returns on restart; a discovered one returns on the next scan.'], confirm: 'Remove device', danger: true })
          .then(function (r) { if (!r) return; return api('DELETE', '/api/devices/' + encodeURIComponent(dev.id)).then(function () { location.href = '/'; }).catch(fail); });
      } else if (a === 'restore') {
        var file = h('input', { class: 'field', type: 'file', accept: '.dmp,application/octet-stream', required: true });
        dialog({ title: 'Restore configuration on ' + dev.name + '?', body: ['The device replaces its whole configuration with the uploaded dump (Wi-Fi, MQTT, names, options, everything) and restarts. A dump from a different device moves that device\'s identity here.'], confirm: 'Restore configuration', danger: true, typed: dev.name,
          build: function (body, fields) { body.appendChild(field('Configuration dump (.dmp)', file)); fields.file = file; } })
          .then(function (r) {
            if (!r || !r.file || !r.file.length) return;
            var fd = new FormData(); fd.append('file', r.file[0]);
            return api('POST', '/api/devices/' + encodeURIComponent(dev.id) + '/restore', fd).then(function () { toast('ok', 'Configuration restored', 'The device is restarting.'); setTimeout(refresh, 10000); }).catch(fail);
          });
      }
    });
    render();
    setInterval(function () { if (!document.querySelector('.modal-scrim')) refresh(); }, 15000);
  }

  // ---------- jobs pages ----------
  function jobRow(j) {
    var ok = 0, failed = 0, running = 0;
    j.items.forEach(function (it) { if (it.state === 'ok') ok++; else if (it.state === 'failed') failed++; else running++; });
    var status = j.finishedAt && j.finishedAt.indexOf('0001-') !== 0 ? (failed ? chip('err', failed + ' failed') : chip('ok', 'done')) : chip('brand', 'running');
    return h('tr', { class: 'clickable', onclick: function () { location.href = '/jobs/' + j.id; } }, [
      h('td', {}, [h('span', { class: 'mono', text: new Date(j.startedAt).toLocaleString() })]),
      h('td', {}, [h('a', { href: '/jobs/' + j.id, text: j.summary })]),
      h('td', {}, [h('span', { class: 'job-progress', text: ok + ' ok / ' + failed + ' failed / ' + j.items.length + ' total' })]),
      h('td', {}, [status])]);
  }
  function jobsPage(data) {
    var rows = document.getElementById('rows'), empty = document.getElementById('empty');
    function render(jobs) { clear(rows); empty.hidden = jobs.length > 0; document.getElementById('jobsWrap').hidden = jobs.length === 0; jobs.forEach(function (j) { rows.appendChild(jobRow(j)); }); }
    render(data.jobs || []);
    setInterval(function () { api('GET', '/api/jobs').then(function (r) { render(r.jobs || []); }).catch(function () {}); }, 5000);
  }
  function jobPage(data) {
    var rows = document.getElementById('rows'), stateEl = document.getElementById('jobState'), labels = document.getElementById('headLabels');
    function render(j) {
      clear(rows);
      var ok = 0, failed = 0;
      j.items.forEach(function (it) {
        if (it.state === 'ok') ok++; else if (it.state === 'failed') failed++;
        var c = it.state === 'ok' ? chip('ok', 'ok') : it.state === 'failed' ? chip('err', 'failed') : it.state === 'running' ? chip('brand', 'running') : chip('', 'pending');
        rows.appendChild(h('tr', {}, [h('td', {}, [h('a', { href: '/devices/' + encodeURIComponent(it.deviceId), text: it.name }), h('div', { class: 'cell-sub' }, [h('span', { class: 'mono', text: it.host })])]), h('td', {}, [c]), h('td', { class: 'result', text: it.message || '' }), h('td', { class: 'hide-sm' }, [h('span', { class: 'mono', text: fmtAgo(it.updated) })])]));
      });
      var done = j.finishedAt && j.finishedAt.indexOf('0001-') !== 0;
      stateEl.textContent = done ? 'Finished ' + fmtAgo(j.finishedAt) + '.' : 'Running.';
      clear(labels);
      labels.appendChild(done ? (failed ? chip('err', failed + ' of ' + j.items.length + ' failed') : chip('ok', 'all ' + j.items.length + ' ok')) : chip('brand', ok + failed + ' of ' + j.items.length + ' done'));
      return done;
    }
    var done = render(data.job);
    if (!done) {
      var timer = setInterval(function () {
        api('GET', '/api/jobs/' + encodeURIComponent(data.job.id)).then(function (r) { if (render(r.job)) clearInterval(timer); }).catch(function () {});
      }, 3000);
    }
  }

  // ---------- dispatch ----------
  var data = pageData();
  if (data.device) devicePage(data);
  else if (data.devices) devicesPage(data);
  else if (data.job) jobPage(data);
  else if (data.jobs) jobsPage(data);
})();
