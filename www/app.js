/* hs-dashboard — app.js
   Edit config.json and services.json to configure. Never edit this file.
   github.com/mmBesar/hs-dashboard */
'use strict';

// ── Icons ─────────────────────────────────────────────────────────────────────

// ── Theme ─────────────────────────────────────────────────────────────────────
function applyTheme(t) {
  const resolved = t === 'auto'
    ? (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light') : t;
  document.documentElement.setAttribute('data-theme', resolved);
  document.querySelectorAll('.theme-btn').forEach(b =>
    b.classList.toggle('active', b.dataset.theme === t));
  localStorage.setItem('hs-theme', t);
}
function setTheme(t) { applyTheme(t); }
function initTheme(def) { applyTheme(localStorage.getItem('hs-theme') || def || 'auto'); }
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if ((localStorage.getItem('hs-theme') || 'auto') === 'auto') applyTheme('auto');
});

// ── Clock ─────────────────────────────────────────────────────────────────────
function startClock(tz) {
  const tick = () => {
    const now = new Date();
    document.getElementById('clock').textContent =
      now.toLocaleTimeString('en-GB', {timeZone:tz,hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false});
    document.getElementById('date').textContent =
      now.toLocaleDateString('en-GB', {timeZone:tz,weekday:'short',day:'numeric',month:'short',year:'numeric'});
  };
  tick(); setInterval(tick, 1000);
}

// ── Fetch ─────────────────────────────────────────────────────────────────────
async function fetchJSON(url) {
  try {
    const r = await fetch(url + '?_=' + Date.now());
    if (!r.ok) throw new Error(r.status);
    return r.json();
  } catch { return null; }
}

// ── Helpers ───────────────────────────────────────────────────────────────────
const fmtUptime = s => {
  const d = Math.floor(s/86400), h = Math.floor((s%86400)/3600), m = Math.floor((s%3600)/60);
  return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m`;
};
const barClass = p => p >= 90 ? 'danger' : p >= 70 ? 'warn' : '';
const tempColor = c => c >= 80 ? 'var(--ctp-red)' : c >= 65 ? 'var(--ctp-peach)' : 'var(--ctp-green)';

// ── Stats update ──────────────────────────────────────────────────────────────
function updateStats(data) {
  if (!data || !data.timestamp) return;

  // Core stats
  const cpu = Math.round(data.cpu_percent);
  el('stat-cpu').textContent = cpu + '%';
  setBar('bar-cpu', cpu);

  const ram = Math.round(data.ram_percent);
  el('stat-ram').textContent = ram + '%';
  el('stat-ram-sub').textContent = `${Math.round(data.ram_used_mb)} / ${Math.round(data.ram_total_mb)} MB`;
  setBar('bar-ram', ram);

  el('stat-load').textContent = data.load_1m.toFixed(2);
  el('stat-load-sub').textContent = `${data.load_1m.toFixed(2)} · ${data.load_5m.toFixed(2)} · ${data.load_15m.toFixed(2)}`;
  el('stat-uptime').textContent = fmtUptime(data.uptime_seconds);

  // Dynamic cards — temps + GPU + disks — all in ONE grid
  let html = '';

  (data.temps || []).forEach(t => {
    html += card(t.label, `<span style="color:${tempColor(t.value)}">${t.value}°C</span>`,
      `<div class="stat-sub">${t.description || ''}</div>`);
  });

  (data.gpus || []).forEach(g => {
    const vram = g.vram_total_mb > 0
      ? `<div class="stat-sub">${g.vram_used_mb} / ${g.vram_total_mb} MB VRAM</div>` : '';
    const usage = g.usage_percent > 0
      ? `<div class="stat-sub">${g.usage_percent.toFixed(1)}% load</div>
         <div class="stat-bar"><div class="stat-bar-fill ${barClass(g.usage_percent)}" style="width:${g.usage_percent}%"></div></div>` : '';
    html += card(`${g.vendor} GPU`,
      `<span style="color:${tempColor(g.temp)}">${g.temp > 0 ? g.temp + '°C' : '—'}</span>`,
      vram + usage);
  });

  (data.disks || []).forEach(d => {
    html += card(`Disk ${d.mount}`,
      `${d.percent}%`,
      `<div class="stat-sub">${d.used_gb} / ${d.total_gb} GB</div>
       <div class="stat-bar"><div class="stat-bar-fill ${barClass(d.percent)}" style="width:${d.percent}%"></div></div>`);
  });

  el('dynamic-cards').innerHTML = html;

  // Arch badge
  if (data.arch) {
    const badge = el('arch-tag');
    badge.textContent = data.arch;
    badge.style.display = 'inline-block';
    const av = document.getElementById('info-arch-value');
    if (av) av.innerHTML = archTag(data.arch);
  }

  el('footer-updated').textContent = 'updated ' + new Date(data.timestamp * 1000).toLocaleTimeString('en-GB');
}

function el(id) { return document.getElementById(id); }

function setBar(id, pct) {
  const b = el(id);
  b.style.width = pct + '%';
  b.className = 'stat-bar-fill ' + barClass(pct);
}

function card(label, valueHTML, extraHTML = '') {
  return `<div class="stat-card">
    <div class="stat-label">${label}</div>
    <div class="stat-value">${valueHTML}</div>
    ${extraHTML}
  </div>`;
}

function archTag(arch) {
  return `<span id="info-arch-value" class="tag"
    style="background:rgba(198,160,246,.15);color:var(--accent);border:1px solid rgba(198,160,246,.3)"
    >${arch}</span>`;
}

// ── Status ────────────────────────────────────────────────────────────────────
let statusData = {};
async function refreshStatus() {
  const data = await fetchJSON('/api/status');
  if (data?.services) {
    statusData = data.services;
    Object.entries(statusData).forEach(([url, state]) => {
      document.querySelectorAll(`[data-status-url="${CSS.escape(url)}"]`).forEach(e => {
        e.className = 'status-badge ' + state;
        e.textContent = state;
      });
    });
  }
}

// ── Services ──────────────────────────────────────────────────────────────────
function renderServices(servers) {
  const c = el('services-container');
  c.innerHTML = '';
  servers.forEach(srv => {
    const lbl = document.createElement('div');
    lbl.className = 'section-label';
    lbl.textContent = srv.label
      ? `services · ${srv.name} · ${srv.label}`
      : `services · ${srv.name}`;
    c.appendChild(lbl);

    const grid = document.createElement('div');
    grid.className = 'services-grid';
    (srv.services || []).forEach(svc => {
      const disabled = !!svc.disabled;
      const accent = svc.accent || 'var(--accent)';
      const iconBg = accent.startsWith('#') ? accent + '20' : 'rgba(198,160,246,.15)';
      const state = disabled ? 'soon' : (statusData[svc.url] || 'checking');
      const a = document.createElement('a');
      a.href = disabled ? '#' : svc.url;
      if (!disabled) { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
      a.className = 'service-card' + (disabled ? ' disabled' : '');
      a.style.setProperty('--card-accent', accent);
      a.style.setProperty('--icon-bg', iconBg);
      a.innerHTML = `
        <div class="card-top">
          <div class="card-icon" style="color:${accent}">${getIcon(svc.icon)}</div>
          <span class="status-badge ${state}" data-status-url="${svc.url}">${state}</span>
        </div>
        <div class="card-name">${svc.name}</div>
        <div class="card-desc">${svc.desc}</div>
        <div class="card-url">${svc.display || svc.url}</div>`;
      grid.appendChild(a);
    });
    c.appendChild(grid);
  });
}

// ── Info tables ───────────────────────────────────────────────────────────────
function renderInfo(cfg, arch) {
  const s = cfg.server, h = cfg.hardware;
  const row = (k, v) => `<tr><td>${k}</td><td>${v}</td></tr>`;

  el('info-network').innerHTML = [
    row('hostname', s.fqdn || '—'),
    row('ip', `<span style="color:var(--ctp-sky)">${s.ip || '—'}</span>`),
    s.dns ? row('dns', `<span style="color:var(--ctp-green)">${s.dns}</span>`) : '',
    row('os', h.os || '—'),
    row('kernel', h.kernel || '—'),
  ].join('');

  el('info-hardware').innerHTML = [
    row('board', h.board || '—'),
    row('cpu', h.cpu || '—'),
    row('arch', archTag(arch || h.arch || 'detecting...')),
    row('ram', h.ram || '—'),
    row('storage', h.storage || '—'),
  ].join('');
}

// ── Logo ──────────────────────────────────────────────────────────────────────
function renderLogo(server, logo) {
  const area = el('logo-area');
  const name = server.name || 'hs';
  if (logo?.image) {
    const img = document.createElement('img');
    img.src = logo.image; img.alt = name; img.className = 'logo-img';
    img.style.height = (logo.height || 48) + 'px';
    img.onerror = () => { area.innerHTML = textLogo(name); };
    area.appendChild(img);
  } else {
    area.innerHTML = textLogo(name);
  }
}
const textLogo = name =>
  `<div class="logo">${name.slice(0,-1)}<span class="logo-accent">${name.slice(-1)}</span></div>`;

// ── Boot ──────────────────────────────────────────────────────────────────────
async function boot() {
  const [cfg, svcData] = await Promise.all([
    fetchJSON('/config.json'),
    fetchJSON('/services.json'),
  ]);

  if (!cfg || !svcData) {
    document.body.innerHTML =
      `<div style="padding:2rem;font-family:monospace;color:#ed8796">
        Error: could not load config.json or services.json<br>
        Check that files are mounted at /config/
      </div>`;
    return;
  }

  // Apply scale
  document.documentElement.style.setProperty('--scale', cfg.display?.scale || 1.0);
  document.documentElement.style.setProperty('--tile-width', (cfg.display?.tile_width || 200) + 'px');
  if (cfg.display?.show_urls === false) document.documentElement.classList.add('hide-urls');

  // Theme + clock
  initTheme(cfg.display?.theme);
  startClock(cfg.display?.timezone || 'UTC');

  // Page title + logo
  document.title = (cfg.server?.fqdn || 'Dashboard') + ' — Dashboard';
  renderLogo(cfg.server || {}, cfg.logo);
  el('logo-sub').textContent = cfg.server?.description || cfg.server?.fqdn || '';
  el('footer-fqdn').textContent = cfg.server?.fqdn || '';

  // Info tables (arch comes from stats later)
  renderInfo(cfg, null);

  // Services
  const initStatus = await fetchJSON('/api/status');
  statusData = initStatus?.services || {};
  renderServices(svcData.servers || []);

  // Initial stats
  const initStats = await fetchJSON('/api/stats');
  updateStats(initStats);

  // Refresh loops
  const sm = cfg.display?.refresh_stats_ms || 5000;
  const stm = cfg.display?.refresh_status_ms || 30000;
  setInterval(async () => updateStats(await fetchJSON('/api/stats')), sm);
  setInterval(refreshStatus, stm);
}

boot();
