'use strict';

// Login page: surface a failed attempt without inline scripts (CSP forbids them).
if (document.body.classList.contains('login')) {
  if (new URLSearchParams(location.search).get('error')) {
    const p = document.querySelector('.login-card p');
    p.textContent = 'Wrong password.';
    p.style.color = '#ff8a8a';
  }
} else {
  startDashboard();
}

function startDashboard() {
  const SVG = 'http://www.w3.org/2000/svg';
  const $ = (id) => document.getElementById(id);
  let state = null;
  let settingsPopulated = false;

  // ---- helpers (text is always set via textContent: names come from clients) ----
  const mbps = (bps) => (bps / 1e6).toFixed(bps >= 1e7 ? 1 : 2) + ' Mb/s';
  const bytes = (n) => {
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(i > 1 ? 2 : 0) + ' ' + u[i];
  };
  const color = (hue, l = 58) => `hsl(${hue} 70% ${l}%)`;
  const trunc = (s, n) => (s.length > n ? s.slice(0, n - 1) + '…' : s);

  function h(tag, attrs = {}, text) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function s(tag, attrs = {}, text) {
    const e = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function table(head, rows) {
    const t = h('table');
    const tr = h('tr');
    head.forEach((c) => tr.append(h('th', {}, c)));
    const thead = h('thead');
    thead.append(tr);
    t.append(thead);
    const body = h('tbody');
    rows.forEach((r) => {
      const row = h('tr');
      r.forEach((c) => row.append(c instanceof Node ? h('td').appendChild(c).parentNode : h('td', {}, String(c))));
      body.append(row);
    });
    t.append(body);
    return t;
  }
  function replace(el, ...kids) { el.replaceChildren(...kids); }
  function kpi(label, value) {
    const d = h('div', { class: 'kpi' });
    d.append(h('div', { class: 'v' }, value), h('div', { class: 'l' }, label));
    return d;
  }
  const badge = (text, cls) => h('span', { class: 'badge ' + cls }, text);

  async function apiPost(path, payload) {
    const headers = { 'Content-Type': 'application/json' };
    if (state && state.csrfToken) headers['X-CSRF-Token'] = state.csrfToken;
    const r = await fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers,
      body: JSON.stringify(payload),
    });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    return r.json();
  }

  // ---- tabs ----
  document.querySelectorAll('.tab').forEach((b) => b.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach((x) => x.classList.toggle('active', x === b));
    $('tab-live').hidden = b.dataset.tab !== 'live';
    $('tab-stats').hidden = b.dataset.tab !== 'stats';
    $('tab-settings').hidden = b.dataset.tab !== 'settings';
    if (b.dataset.tab === 'settings' && state) populateSettingsForm();
    if (state) render();
  }));

  // ---- settings & actions ----
  function populateSettingsForm() {
    if (!state || !state.abr) return;
    const c = state.abr.config;
    $('cfg-enabled').checked = Boolean(c.enabled);
    $('cfg-degrade').checked = Boolean(c.allowDegradingExistingStreams);
    $('cfg-upload').value = ((c.uploadSpeedBps || 16000000) / 1e6).toString();
    $('cfg-headroom').value = String(c.headroomPercent ?? 10);
    $('cfg-policy').value = c.policy || 'FairShare';
    $('cfg-min').value = ((c.minBitrateBps || 1500000) / 1e6).toString();
    $('cfg-ceiling').value = ((c.singleClientCeilingBps || 0) / 1e6).toString();
    $('cfg-fast-sec').value = String(c.fastStartSeconds || 10);
    $('cfg-fast-pct').value = String(c.fastStartSharePercent || 90);
    $('cfg-seek-sec').value = String(c.seekBorrowSeconds || 4);
    $('cfg-bg-pct').value = String(c.backgroundFloorPercent || 5);
    $('cfg-encoders').value = (state.encoders || []).join(',');
    if (state.storage) {
      $('cfg-storage-gb').value = String(state.storage.max_storage_gb || 50);
    }
    settingsPopulated = true;
  }

  $('settings-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const msg = $('settings-msg');
    msg.textContent = 'Saving…';
    try {
      const encoders = $('cfg-encoders').value
        .split(',')
        .map((x) => x.trim())
        .filter(Boolean);
      await apiPost('api/settings', {
        abr: {
          enabled: $('cfg-enabled').checked,
          uploadSpeedBps: Math.round((parseFloat($('cfg-upload').value) || 16) * 1e6),
          headroomPercent: parseInt($('cfg-headroom').value, 10) || 0,
          policy: $('cfg-policy').value || 'FairShare',
          minBitrateBps: Math.round((parseFloat($('cfg-min').value) || 1.5) * 1e6),
          singleClientCeilingBps: Math.round((parseFloat($('cfg-ceiling').value) || 0) * 1e6),
          allowDegradingExistingStreams: $('cfg-degrade').checked,
          fastStartSeconds: parseInt($('cfg-fast-sec').value, 10) || 10,
          fastStartSharePercent: parseInt($('cfg-fast-pct').value, 10) || 90,
          seekBorrowSeconds: parseInt($('cfg-seek-sec').value, 10) || 4,
          backgroundFloorPercent: parseInt($('cfg-bg-pct').value, 10) || 5,
        },
        encoders,
        maxStorageGb: parseFloat($('cfg-storage-gb').value) || 50,
      });
      msg.textContent = 'Settings saved and persisted.';
      await poll();
      populateSettingsForm();
    } catch (_) {
      msg.textContent = 'Failed to save settings.';
    }
  });

  document.querySelectorAll('[data-purge]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const cat = btn.getAttribute('data-purge');
      const msg = $('purge-msg');
      msg.textContent = `Purging ${cat}…`;
      try {
        const res = await apiPost('api/cache/purge', { category: cat });
        msg.textContent = `Purged ${res.removed || 0} entries (${cat}).`;
        await poll();
      } catch (_) {
        msg.textContent = 'Purge failed.';
      }
    });
  });

  $('preseed-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const raw = $('preseed-url').value.trim();
    if (!raw) return;
    const isUrl = raw.startsWith('/') || raw.startsWith('http') || raw.includes('.m3u8');
    const url = isUrl ? raw : `/Videos/${raw}/main.m3u8`;
    const msg = $('preseed-msg');
    msg.textContent = 'Starting job…';
    try {
      await apiPost('api/preseed/start', {
        itemName: $('preseed-name').value.trim() || raw,
        urls: [url],
      });
      msg.textContent = 'Pre-seeding started.';
      $('preseed-url').value = '';
      await poll();
    } catch (_) {
      msg.textContent = 'Could not start pre-seeding job.';
    }
  });

  // ---- polling ----
  async function poll() {
    try {
      const r = await fetch('api/state', { credentials: 'same-origin', cache: 'no-store' });
      if (r.status === 401) { location.reload(); return; }
      state = await r.json();
      if (state.csrfToken && $('csrf-logout')) {
        $('csrf-logout').value = state.csrfToken;
      }
      if (!settingsPopulated) {
        populateSettingsForm();
      }
      $('conn').textContent = 'live';
      $('conn').className = 'pill ok';
      render();
    } catch (_) {
      $('conn').textContent = 'offline';
      $('conn').className = 'pill bad';
    }
  }
  poll();
  setInterval(poll, 1000);

  function render() {
    renderKpis();
    if (!$('tab-live').hidden) { renderSchema(); renderLegend(); }
    if (!$('tab-stats').hidden) renderStats();
  }

  function renderKpis() {
    const l = state.live, a = state.abr;
    replace($('kpis-live'),
      kpi('Clients streaming', String(l.clients.length)),
      kpi('Home upload now', mbps(l.pullBps) + (a.totalBps ? ' / ' + mbps(a.totalBps) : '')),
      kpi('Sent to clients', mbps(l.outBps)),
      kpi('Served from cache', mbps(l.cacheBps)));
    const saved = Math.max(0, l.totalOutBytes - l.totalPulledBytes);
    const ratio = l.totalOutBytes > 0 ? Math.round((saved / l.totalOutBytes) * 100) : 0;
    const st = state.storage;
    replace($('kpis-stats'),
      kpi('Home upload saved', bytes(saved) + ' (' + ratio + '%)'),
      kpi('Shared pipes', String(a.sharedPipes)),
      kpi('Cache used', st ? st.total_gb.toFixed(2) + ' / ' + st.max_storage_gb + ' GB' : '-'),
      kpi('Uptime', Math.floor(l.uptimeSec / 60) + ' min'),
      kpi('Plugin paired', state.server && state.server.paired ? 'yes' : 'no'));
  }

  // ---- the live drawing ----
  function renderSchema() {
    const svg = $('schema');
    const { clients, items } = state.live;
    const rows = Math.max(clients.length, items.length, 2);
    const H = Math.max(360, rows * 96 + 40);
    svg.setAttribute('viewBox', `0 0 1000 ${H}`);
    svg.replaceChildren();

    const phase = -((performance.now() % 1000) / 1000) * 20;
    const maxBps = Math.max(1, state.abr.totalBps || 0, ...items.map((i) => i.pullBps), ...clients.map((c) => c.outBps));
    const width = (bps) => 2 + 10 * Math.min(1, bps / maxBps);
    const cy = H / 2;

    const node = (x, y, w, hgt, title, sub) => {
      svg.append(s('rect', { class: 'node', x, y, width: w, height: hgt, rx: 12 }));
      svg.append(s('text', { x: x + w / 2, y: y + 24, 'text-anchor': 'middle', 'font-weight': 600 }, title));
      svg.append(s('text', { class: 'sub', x: x + w / 2, y: y + 42, 'text-anchor': 'middle' }, sub));
    };

    const home = { x: 20, w: 170, y: cy - 60, h: 120 };
    const vps = { x: 415, w: 170, y: cy - 60, h: 120 };
    const cli = { x: 810, w: 170, h: 70 };

    const edge = (x1, y1, x2, y2, hue, bps, cached, dashPhase) => {
      const d = `M${x1} ${y1} C${x1 + 110} ${y1}, ${x2 - 110} ${y2}, ${x2} ${y2}`;
      svg.append(s('path', { class: 'idle', d }));
      if (bps <= 0) return d;
      const flow = s('path', {
        class: 'flow' + (cached ? ' cached' : ''), d, stroke: color(hue), 'stroke-width': width(bps),
      });
      flow.style.animationDelay = `${(dashPhase / 20).toFixed(3)}s`;
      svg.append(flow);
      return d;
    };

    items.forEach((it, k) => {
      const off = (k - (items.length - 1) / 2) * 22;
      const y = cy + off;
      edge(home.x + home.w, y, vps.x, y, it.hue, it.pullBps, false, phase);
      const label = trunc(it.name, 26) + ' · ' + mbps(it.pullBps);
      svg.append(s('text', { x: (home.x + home.w + vps.x) / 2, y: y - 8, 'text-anchor': 'middle', fill: color(it.hue, 70) }, label));
      if (it.shared) {
        svg.append(s('text', { class: 'sub', x: (home.x + home.w + vps.x) / 2, y: y + 14, 'text-anchor': 'middle' }, `1 stream shared by ${it.clients} clients`));
      }
    });

    const byItem = new Map(items.map((i) => [i.itemId, i]));
    clients.forEach((c, i) => {
      const top = (i + 0.5) * (H / clients.length) - cli.h / 2;
      const it = byItem.get(c.itemId);
      const hue = it ? it.hue : 210;
      const off = (i - (clients.length - 1) / 2) * 14;
      const mostlyCache = c.outBps > 0 && c.cacheBps / c.outBps > 0.5;
      edge(vps.x + vps.w, cy + off, cli.x, top + cli.h / 2, hue, c.outBps, mostlyCache, phase);
      node(cli.x, top, cli.w, cli.h, trunc(c.name, 20), trunc(c.itemName, 22));
      svg.append(s('text', { class: 'sub', x: cli.x + cli.w / 2, y: top + 58, 'text-anchor': 'middle' },
        mbps(c.outBps) + (mostlyCache ? ' · cache' : '')));
      svg.append(s('circle', { cx: cli.x + 14, cy: top + 14, r: 5, fill: color(hue) }));
    });
    if (clients.length === 0) {
      svg.append(s('text', { class: 'sub', x: 810 + 85, y: cy, 'text-anchor': 'middle' }, 'No active clients'));
    }

    const used = state.abr.totalBps ? Math.round((state.live.pullBps / state.abr.totalBps) * 100) : null;
    node(home.x, home.y, home.w, home.h, 'Home Jellyfin', used === null ? 'upload limit not set' : `upload ${used}% used`);
    node(vps.x, vps.y, vps.w, vps.h, 'JellyProxy (VPS)', `cache ${mbps(state.live.cacheBps)}`);
  }

  function renderLegend() {
    const { items } = state.live;
    if (items.length === 0) { replace($('legend'), h('div', { class: 'muted' }, 'Nothing is playing.')); return; }
    const pipes = new Map();
    state.abr.pipes.forEach((p) => pipes.set(p.itemId, p));
    replace($('legend'), ...items.map((it) => {
      const row = h('div', { class: 'row' });
      const dot = h('span', { class: 'dot' });
      dot.style.background = color(it.hue);
      row.append(dot, h('strong', {}, it.name), h('span', { class: 'muted' }, `${it.clients} client${it.clients === 1 ? '' : 's'} · home ${mbps(it.pullBps)} · out ${mbps(it.outBps)}`));
      if (it.shared) row.append(badge('shared stream', 'shared'));
      const p = pipes.get(it.itemId);
      if (p && p.burst) row.append(badge(p.burst, 'burst'));
      if (p && p.signature.includes('burn:')) row.append(badge('subtitle burn-in', 'burst'));
      return row;
    }));
  }

  // ---- stats tab ----
  function renderStats() {
    const names = new Map(state.live.items.map((i) => [i.itemId, i.name]));
    const pipes = state.abr.pipes;
    replace($('pipes'), pipes.length === 0 ? h('div', { class: 'muted' }, 'No active pipes.') :
      table(['Title', 'Clients', 'Codec', 'Requested', 'Allocated', 'Pull', 'State'], pipes.map((p) => {
        const st = h('span');
        if (p.shared) st.append(badge('shared', 'shared'), ' ');
        if (p.burst) st.append(badge(p.burst, 'burst'));
        return [trunc(names.get(p.itemId) || p.itemId, 28), p.clients, p.codec || '-', mbps(p.requestedBps), mbps(p.allocatedBps), mbps(p.pullBps), st];
      })));

    replace($('sessions'), state.sessions.length === 0 ? h('div', { class: 'muted' }, 'No reported sessions (needs the Jellyfin plugin).') :
      table(['Device', 'User', 'Title', 'Position', 'State', 'Prefetched'], state.sessions.map((x) => [
        x.device_name || x.client_name, x.user_name, trunc(x.item_name || x.item_id, 30),
        x.total_ticks ? Math.round((x.position_ticks / x.total_ticks) * 100) + '%' : '-',
        x.is_paused ? (x.prefetch_active ? 'paused · prefetching' : 'paused') : 'playing', x.prefetched_count,
      ])));

    const st = state.storage;
    if (st) {
      const bar = h('div', { class: 'bar' });
      const fill = h('div');
      fill.style.width = Math.min(100, st.usage_percent) + '%';
      bar.append(fill);
      const cats = Object.entries(st.categories || {}).map(([k, v]) => [k, v.file_count, bytes(v.used_bytes)]);
      replace($('cache'), bar, h('p', { class: 'muted' }, `${bytes(st.total_bytes)} of ${bytes(st.max_bytes)} · ${st.file_count} files`),
        cats.length ? table(['Category', 'Files', 'Size'], cats) : h('span'));
    }

    replace($('jobs'), state.jobs.length === 0 ? h('div', { class: 'muted' }, 'No pre-seed jobs.') :
      table(['Job', 'Title', 'Status', 'Progress', ''], state.jobs.map((j) => {
        const act = h('span');
        if (j.status === 'running') {
          const btn = h('button', { type: 'button', class: 'danger' }, 'Cancel');
          btn.addEventListener('click', () => apiPost('api/preseed/cancel', { id: j.id }).then(poll).catch(() => {}));
          act.append(btn);
        }
        return [j.id, j.item_name || '-', j.error ? j.status + ': ' + j.error : j.status, `${j.completed}/${j.total_segments}`, act];
      })));
  }
}
