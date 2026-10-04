// hs-dashboard — page logic. Plain JavaScript, no build step, no libraries.
// Data comes from four endpoints: /api/info, /api/history, /api/stream (live) and /api/services.

const $ = (sel) => document.querySelector(sel);
const HIST_CAP = 150;                    // must match historyLen in stats.go
const hist = [];                         // [{cpu, mem, rx, tx}, ...]
let services = [];
let lastSnap = null;

// ---------- small helpers ----------
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === 'class') el.className = v;
    else if (k === 'style') el.style.cssText = v;
    else if (v !== false && v != null) el.setAttribute(k, v);
  }
  for (const kid of kids.flat()) if (kid != null) el.append(kid);
  return el;
}
const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

function bytes(n) {
  const u = ['B', 'kB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
  return (n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)) + ' ' + u[i];
}
function uptime(s) {
  const d = Math.floor(s / 86400), hr = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d}d ${hr}h` : hr ? `${hr}h ${m}m` : `${m}m`;
}
function hexA(hex, a) {
  const n = parseInt(hex.replace('#', ''), 16);
  return `rgba(${n >> 16}, ${(n >> 8) & 255}, ${n & 255}, ${a})`;
}

// ---------- drawing ----------
// Draws one or more smooth lines on a canvas. layers: [{data, color, fill, width, max}]
function draw(canvas, layers, grid) {
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth, hgt = canvas.clientHeight;
  if (!w || !hgt) return;
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(hgt * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(hgt * dpr);
  }
  const c = canvas.getContext('2d');
  c.setTransform(dpr, 0, 0, dpr, 0, 0);
  c.clearRect(0, 0, w, hgt);

  if (grid) {
    c.strokeStyle = css('--surface0');
    c.lineWidth = 1;
    for (const f of [0.25, 0.5, 0.75]) {
      const y = Math.round(hgt * f) + 0.5;
      c.beginPath(); c.moveTo(0, y); c.lineTo(w, y); c.stroke();
    }
  }
  for (const L of layers) {
    const n = L.data.length;
    if (n < 2) continue;
    const max = L.max || 1;
    const step = w / (HIST_CAP - 1);
    const x0 = w - (n - 1) * step;                 // newest point sits at the right edge
    const pts = L.data.map((v, i) => [x0 + i * step, hgt - 3 - Math.min(v / max, 1) * (hgt - 8)]);
    c.beginPath();
    c.moveTo(pts[0][0], pts[0][1]);
    for (let i = 1; i < n; i++) {
      const mx = (pts[i - 1][0] + pts[i][0]) / 2, my = (pts[i - 1][1] + pts[i][1]) / 2;
      c.quadraticCurveTo(pts[i - 1][0], pts[i - 1][1], mx, my);
    }
    c.lineTo(pts[n - 1][0], pts[n - 1][1]);
    c.strokeStyle = L.color; c.lineWidth = L.width || 2; c.lineJoin = 'round'; c.lineCap = 'round';
    c.stroke();
    if (L.fill) {
      c.lineTo(pts[n - 1][0], hgt); c.lineTo(pts[0][0], hgt); c.closePath();
      const g = c.createLinearGradient(0, 0, 0, hgt);
      g.addColorStop(0, hexA(L.color, 0.32));
      g.addColorStop(1, hexA(L.color, 0));
      c.fillStyle = g; c.fill();
    }
  }
}
function drawAll() {
  draw($('#graph'), [
    { data: hist.map((p) => p.mem), color: css('--accent-2'), width: 1.5, max: 100 },
    { data: hist.map((p) => p.cpu), color: css('--accent'), fill: true, width: 2.25, max: 100 },
  ], true);
  const peak = Math.max(1, ...hist.map((p) => Math.max(p.rx, p.tx))) * 1.1;
  draw($('#netGraph'), [
    { data: hist.map((p) => p.tx), color: css('--green'), width: 1.5, max: peak },
    { data: hist.map((p) => p.rx), color: css('--blue'), fill: true, width: 1.75, max: peak },
  ]);
}

// ---------- header ----------
function renderInfo(info, title) {
  $('#host').textContent = title || info.host || 'home server';
  document.title = (title || info.host || 'Home server') + ' · dashboard';
  const meta = $('#meta');
  meta.replaceChildren();
  for (const t of [info.model, info.os, info.arch, info.kernel && 'Linux ' + info.kernel]) {
    if (t) meta.append(h('span', {}, t));
  }
  meta.append(h('span', { id: 'upt' }, ''));
}

// ---------- live numbers ----------
function update(s) {
  lastSnap = s;
  const memPct = s.mem.total ? (100 * s.mem.used) / s.mem.total : 0;
  hist.push({ cpu: s.cpu.total, mem: memPct, rx: s.net.rx, tx: s.net.tx });
  if (hist.length > HIST_CAP) hist.shift();

  const upt = $('#upt'); if (upt) upt.textContent = 'up ' + uptime(s.uptime);
  $('#cpuNow').textContent = s.cpu.total.toFixed(0) + '%';
  $('#memNow').textContent = memPct.toFixed(0) + '%';

  // one little bar per core
  const cores = $('#cores');
  if (cores.children.length !== s.cpu.cores.length) {
    cores.replaceChildren(...s.cpu.cores.map((_, i) => h('i', { title: 'Core ' + i })));
  }
  s.cpu.cores.forEach((v, i) => {
    const el = cores.children[i];
    el.style.setProperty('--v', v + '%');
    el.className = v > 90 ? 'max' : v > 70 ? 'hot' : '';
    el.title = `Core ${i}: ${v.toFixed(0)}%`;
  });

  // memory
  $('#memUsed').textContent = bytes(s.mem.used);
  $('#memSub').textContent = `of ${bytes(s.mem.total)}` + (s.mem.swapTotal ? `, swap ${bytes(s.mem.swapUsed)}` : '');
  const cache = Math.min(s.mem.cached, s.mem.total - s.mem.used);
  $('#segUsed').style.width = memPct + '%';
  $('#segCache').style.width = (s.mem.total ? (100 * cache) / s.mem.total : 0) + '%';

  // load
  $('#load1').textContent = s.load[0].toFixed(2);
  const n = s.cpu.cores.length;
  $('#loadSub').textContent = `5m ${s.load[1].toFixed(2)}, 15m ${s.load[2].toFixed(2)}, ${n} ${n === 1 ? 'core' : 'cores'}`;

  // temperature
  const t = $('#temp');
  if (s.temp == null) {
    t.textContent = '–'; $('#tempSub').textContent = 'No sensor found';
  } else {
    t.replaceChildren(s.temp.toFixed(0), h('small', {}, '°C'));
    t.className = 'value num ' + (s.temp >= 75 ? 'temp-hot' : s.temp >= 60 ? 'temp-warm' : '');
    $('#tempSub').textContent = 'Hottest sensor';
  }

  // network
  $('#rx').textContent = '↓ ' + bytes(s.net.rx) + '/s';
  $('#tx').textContent = '↑ ' + bytes(s.net.tx) + '/s';

  renderDisks(s.disks);
  drawAll();
}

function renderDisks(disks) {
  const root = $('#disks');
  if (!disks.length) {
    root.replaceChildren(h('p', { class: 'empty' }, 'No disks found. Mount the host root into the container with ', h('code', {}, '/:/host/root:ro,rslave'), '.'));
    return;
  }
  root.replaceChildren(...disks.map((d) => {
    const pct = (100 * d.used) / d.total;
    return h('div', { class: 'disk' },
      h('div', { class: 'name' }, d.mount, h('small', {}, `${d.device}, ${d.fs}`)),
      h('div', { class: 'bar', role: 'img', 'aria-label': pct.toFixed(0) + '% used' },
        h('i', { class: pct >= 90 ? 'full' : pct >= 75 ? 'warn' : '', style: `--v:${pct}%` })),
      h('div', { class: 'use num' }, h('b', {}, bytes(d.used)), ` of ${bytes(d.total)}  ·  ${pct.toFixed(0)}%`));
  }));
}

// ---------- services ----------
const TILE_COLORS = ['--mauve', '--blue', '--teal', '--green', '--peach', '--pink', '--sapphire', '--yellow'];
function tileColor(name) {
  let n = 0;
  for (const ch of name) n = (n * 31 + ch.charCodeAt(0)) >>> 0;
  return TILE_COLORS[n % TILE_COLORS.length];
}
function initials(name) {
  const w = name.trim().split(/[\s\-_.]+/).filter(Boolean);
  return (w.length > 1 ? w[0][0] + w[1][0] : name.trim().slice(0, 2)).toUpperCase();
}
function safeUrl(u) {
  try { const p = new URL(u, location.href); return /^https?:$/.test(p.protocol) ? p.href : '#'; } catch { return '#'; }
}
function hostOf(u) { try { return new URL(u).host; } catch { return ''; } }

function tile(s) {
  const status = s.state === 'up' ? s.ms + ' ms' : s.state === 'down' ? 'down' : '';
  return h('a', {
      class: 'tile', href: safeUrl(s.url), rel: 'noopener', 'data-state': s.state,
      'data-q': `${s.name} ${s.desc || ''} ${s.group || ''}`.toLowerCase(),
    },
    h('span', { class: 'badge', style: `--tile: var(${tileColor(s.name)})` }, s.icon || initials(s.name)),
    h('span', { class: 'txt' }, h('b', {}, s.name), h('small', {}, s.desc || hostOf(s.url))),
    h('span', { class: 'st', title: s.state }, status, h('i', { class: 'dot' })));
}

function renderServices(data) {
  services = data.services;
  renderInfoTitle(data.title);
  const root = $('#services');
  root.replaceChildren();
  if (!services.length) {
    root.append(h('p', { class: 'empty' }, 'No services yet. Add some to ', h('code', {}, 'config.jsonc'), ' and they appear here within seconds.'));
    $('#summary').textContent = '';
    return;
  }
  const groups = new Map();
  for (const s of services) {
    const g = s.group || 'Other';
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(s);
  }
  for (const [name, list] of groups) {
    root.append(h('div', { class: 'group' }, h('h3', {}, name), h('div', { class: 'tiles' }, list.map(tile))));
  }
  const checked = services.filter((s) => s.state !== 'unknown');
  const down = services.filter((s) => s.state === 'down').length;
  const sum = $('#summary');
  if (!checked.length) { sum.textContent = ''; sum.className = 'summary'; }
  else if (down) { sum.textContent = `${down} of ${checked.length} down`; sum.className = 'summary bad'; }
  else { sum.textContent = `All ${checked.length} running`; sum.className = 'summary ok'; }
  applyFilter();
}
let titleOverride = '';
let infoCache = null;
function renderInfoTitle(title) {
  if (title !== titleOverride && infoCache) { titleOverride = title; renderInfo(infoCache, title); if (lastSnap) update(lastSnap); }
}

// ---------- filter (press "/" to focus, Enter opens the first match) ----------
function applyFilter() {
  const q = $('#filter').value.trim().toLowerCase();
  document.querySelectorAll('.tile').forEach((t) => { t.hidden = q && !t.dataset.q.includes(q); });
  document.querySelectorAll('.group').forEach((g) => { g.hidden = !g.querySelector('.tile:not([hidden])'); });
}
$('#filter').addEventListener('input', applyFilter);
$('#filter').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { const first = document.querySelector('.tile:not([hidden])'); if (first) first.click(); }
  if (e.key === 'Escape') { e.target.value = ''; applyFilter(); e.target.blur(); }
});
addEventListener('keydown', (e) => {
  const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
  if (e.key === '/' && !typing && !e.ctrlKey && !e.metaKey) { e.preventDefault(); $('#filter').focus(); }
});

// ---------- theme ----------
$('#theme').addEventListener('click', () => {
  const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  document.documentElement.dataset.theme = next;
  try { localStorage.setItem('theme', next); } catch (e) {}
  drawAll();
});

// ---------- start ----------
async function getJSON(url) { const r = await fetch(url); if (!r.ok) throw new Error(url); return r.json(); }

function connect() {
  const live = $('#live'), text = $('#liveText');
  const es = new EventSource('api/stream');
  es.onopen = () => { live.classList.add('on'); text.textContent = 'live'; };
  es.onmessage = (e) => update(JSON.parse(e.data));
  es.onerror = () => { live.classList.remove('on'); text.textContent = 'reconnecting'; };   // the browser retries by itself
}

async function boot() {
  const [info, history, svc] = await Promise.all([getJSON('api/info'), getJSON('api/history'), getJSON('api/services')]);
  infoCache = info;
  titleOverride = svc.title || '';
  renderInfo(info, titleOverride);
  history.forEach((p) => hist.push(p));
  renderServices(svc);
  drawAll();
  connect();
  setInterval(() => getJSON('api/services').then(renderServices).catch(() => {}), 20000);
  addEventListener('resize', drawAll);
}
boot().catch((err) => { $('#host').textContent = 'Cannot reach the server'; console.error(err); });
