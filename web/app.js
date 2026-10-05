// hs-dashboard — page logic. Plain JavaScript, no build step, no libraries.
//
// Data comes from: /api/info, /api/history, /api/stream (live, every 2s) and /api/services.
// The big idea: the page always tries to fit on ONE screen. fit() picks the largest
// row size at which every service and the system monitor are visible without scrolling.

const $ = (sel) => document.querySelector(sel);
const root = document.documentElement;
const HIST_CAP = 150;                    // must match historyLen in stats.go
const hist = [];                         // [{cpu, mem, rx, tx}, ...]
let lastSnap = null;
let settings = {};
let onlyDown = false;

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
const css = (name) => getComputedStyle(root).getPropertyValue(name).trim();

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

// ---------- clock ----------
let clockFmt, dateFmt;
function setupClock(c) {
  const loc = c.locale || undefined, tz = c.timezone || undefined;
  const t = { hour: '2-digit', minute: '2-digit', second: '2-digit', timeZone: tz };
  if (typeof c.hour12 === 'boolean') t.hourCycle = c.hour12 ? 'h12' : 'h23';
  const d = { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric', timeZone: tz };
  try {
    clockFmt = new Intl.DateTimeFormat(loc, t);
    dateFmt = new Intl.DateTimeFormat(loc, d);
  } catch (e) {                                   // bad locale or timezone name in the config
    clockFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
    dateFmt = new Intl.DateTimeFormat(undefined, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' });
  }
  tick();
}
function tick() {
  const now = new Date();
  const p = Object.fromEntries(clockFmt.formatToParts(now).map((x) => [x.type, x.value]));
  $('#hm').textContent = p.hour + ':' + p.minute;
  $('#sec').textContent = ':' + p.second;
  $('#ap').textContent = p.dayPeriod || '';
  $('#date').textContent = dateFmt.format(now);
}
setupClock({});
setInterval(tick, 1000);

// ---------- drawing (graphs behind the monitor cells) ----------
// layers: [{data, color, fill, width, max}]  top = how much empty room to leave above the lines
function draw(canvas, layers, top = 0.3) {
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
  for (const L of layers) {
    const n = L.data.length;
    if (n < 2) continue;
    const max = L.max || 1;
    const step = w / (HIST_CAP - 1);
    const x0 = w - (n - 1) * step;                 // newest point sits at the right edge
    const y0 = hgt * top;
    const pts = L.data.map((v, i) => [x0 + i * step, hgt - 3 - Math.min(v / max, 1) * (hgt - 6 - y0)]);
    c.beginPath();
    c.moveTo(pts[0][0], pts[0][1]);
    for (let i = 1; i < n; i++) {
      const mx = (pts[i - 1][0] + pts[i][0]) / 2, my = (pts[i - 1][1] + pts[i][1]) / 2;
      c.quadraticCurveTo(pts[i - 1][0], pts[i - 1][1], mx, my);
    }
    c.lineTo(pts[n - 1][0], pts[n - 1][1]);
    c.strokeStyle = L.color; c.lineWidth = L.width || 1.75; c.lineJoin = 'round'; c.lineCap = 'round';
    c.stroke();
    if (L.fill) {
      c.lineTo(pts[n - 1][0], hgt); c.lineTo(pts[0][0], hgt); c.closePath();
      const g = c.createLinearGradient(0, y0, 0, hgt);
      g.addColorStop(0, hexA(L.color, 0.28));
      g.addColorStop(1, hexA(L.color, 0));
      c.fillStyle = g; c.fill();
    }
  }
}
function drawAll() {
  draw($('#cpuBg'), [{ data: hist.map((p) => p.cpu), color: css('--accent'), fill: true, max: 100 }]);
  draw($('#memBg'), [{ data: hist.map((p) => p.mem), color: css('--accent-2'), fill: true, max: 100 }]);
  const peak = Math.max(1, ...hist.map((p) => Math.max(p.rx, p.tx))) * 1.1;
  draw($('#netBg'), [
    { data: hist.map((p) => p.tx), color: css('--green'), width: 1.4, max: peak },
    { data: hist.map((p) => p.rx), color: css('--blue'), fill: true, max: peak },
  ], 0.15);
}

// ---------- settings from config.jsonc ----------
const COLORS = ['mauve', 'blue', 'sapphire', 'teal', 'green', 'yellow', 'peach', 'red', 'pink'];
function applySettings(d) {
  settings = d;
  const a = COLORS.includes(d.accent) ? d.accent : 'mauve';
  const b = COLORS.includes(d.accent2) ? d.accent2 : 'sapphire';
  root.style.setProperty('--accent', `var(--${a})`);
  root.style.setProperty('--accent-2', `var(--${b})`);
  setupClock(d);
  renderTitle();
}

// ---------- header ----------
let info = null;
function renderTitle() {
  if (!info) return;
  const name = settings.title || info.host || 'home server';
  $('#host').textContent = name;
  const meta = $('#meta');
  meta.replaceChildren();
  for (const t of [info.model, info.os, info.arch, info.kernel && 'Linux ' + info.kernel]) {
    if (t) meta.append(h('span', {}, t));
  }
  meta.append(h('span', { id: 'upt' }, ''));
  updateTitle();
}
function updateTitle() {
  const name = settings.title || (info && info.host) || 'Home server';
  const down = serviceList.filter((s) => s.state === 'down').length;
  document.title = (down ? `(${down} down) ` : '') + name;
}

// ---------- live numbers ----------
function update(s) {
  lastSnap = s;
  const memPct = s.mem.total ? (100 * s.mem.used) / s.mem.total : 0;
  hist.push({ cpu: s.cpu.total, mem: memPct, rx: s.net.rx, tx: s.net.tx });
  if (hist.length > HIST_CAP) hist.shift();

  const upt = $('#upt'); if (upt) upt.textContent = 'up ' + uptime(s.uptime);

  $('#cpuVal').textContent = s.cpu.total.toFixed(0) + '%';
  const n = s.cpu.cores.length;
  $('#cpuSub').textContent = `${n} ${n === 1 ? 'core' : 'cores'}`;
  const cores = $('#cores');
  if (cores.children.length !== n) cores.replaceChildren(...s.cpu.cores.map(() => h('i')));
  s.cpu.cores.forEach((v, i) => {
    const el = cores.children[i];
    el.style.setProperty('--v', v + '%');
    el.className = v > 90 ? 'max' : v > 70 ? 'hot' : '';
    el.title = `Core ${i}: ${v.toFixed(0)}%`;
  });

  $('#memVal').textContent = bytes(s.mem.used);
  $('#memOf').textContent = 'of ' + bytes(s.mem.total);
  $('#swap').textContent = s.mem.swapTotal ? 'swap ' + bytes(s.mem.swapUsed) : '';
  const cache = Math.min(s.mem.cached, s.mem.total - s.mem.used);
  $('#segUsed').style.width = memPct + '%';
  $('#segCache').style.width = (s.mem.total ? (100 * cache) / s.mem.total : 0) + '%';

  $('#load1').textContent = s.load[0].toFixed(2);
  $('#loadSub').textContent = `5m ${s.load[1].toFixed(2)} · 15m ${s.load[2].toFixed(2)}`;

  const t = $('#temp');
  if (s.temp == null) {
    t.textContent = '–'; t.className = 'value num'; $('#tempSub').textContent = 'No sensor found';
  } else {
    t.replaceChildren(s.temp.toFixed(0) + ' °C');
    t.className = 'value num ' + (s.temp >= 75 ? 'temp-hot' : s.temp >= 60 ? 'temp-warm' : '');
    $('#tempSub').textContent = 'Hottest sensor';
  }

  $('#rx').textContent = '↓ ' + bytes(s.net.rx) + '/s';
  $('#tx').textContent = '↑ ' + bytes(s.net.tx) + '/s';

  renderDisks(s.disks);
  drawAll();
}

let diskCount = -1;
function renderDisks(disks) {
  const box = $('#disks');
  if (!disks.length) {
    box.replaceChildren(h('span', { class: 'none' }, 'No disks found. Mount the host root as /:/host/root:ro,rslave'));
  } else {
    box.replaceChildren(...disks.map((d) => {
      const pct = (100 * d.used) / d.total;
      return h('div', { class: 'd', title: `${d.mount}  (${d.device}, ${d.fs})\n${bytes(d.used)} of ${bytes(d.total)} used` },
        h('span', { class: 'n' }, d.mount),
        h('span', { class: 'bar-s' }, h('i', { class: pct >= 90 ? 'full' : pct >= 75 ? 'warn' : '', style: `--v:${pct}%` })),
        h('span', { class: 'p num' }, pct.toFixed(0) + '%'));
    }));
  }
  if (disks.length !== diskCount) { diskCount = disks.length; fit(); }   // monitor height may have changed
}

// ---------- services ----------
let serviceList = [];
let groups = [];                         // [{el, n}]
let placedCols = 0;

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

// The little square: logo image if there is one, otherwise emoji/text, otherwise initials.
function badge(s) {
  const text = s.icon || initials(s.name);
  const b = h('span', { class: 'badge', style: `--tile: var(${tileColor(s.name)})` });
  if (s.logo) {
    const img = h('img', { src: s.logo, alt: '', loading: 'lazy', decoding: 'async' });
    img.addEventListener('error', () => { b.classList.remove('has-logo'); img.replaceWith(document.createTextNode(text)); });
    b.classList.add('has-logo');
    b.append(img);
  } else {
    if (/\p{Extended_Pictographic}/u.test(text)) b.classList.add('emoji');
    b.append(text);
  }
  return b;
}

function row(s) {
  const status = s.state === 'up' ? s.ms + ' ms' : s.state === 'down' ? 'down' : '';
  const tip = `${s.name}${s.desc ? ' — ' + s.desc : ''}\n${s.url}${s.state !== 'unknown' ? '\n' + s.state + (s.state === 'up' ? ', ' + s.ms + ' ms' : '') : ''}`;
  return h('a', {
      class: 'row', href: safeUrl(s.url), title: tip, 'data-state': s.state,
      'data-q': `${s.name} ${s.desc || ''} ${s.group || ''}`.toLowerCase(),
      target: settings.newTab ? '_blank' : false, rel: 'noopener',
    },
    badge(s),
    h('span', { class: 'txt' }, h('b', {}, s.name), h('small', {}, s.desc || hostOf(s.url))),
    h('span', { class: 'st' }, h('span', { class: 'ms' }, status), h('i', { class: 'dot' })));
}

function renderServices(data) {
  const bannerEl = $('#banner');
  const hadBanner = !bannerEl.hidden;
  bannerEl.hidden = !data.error;
  if (data.error) bannerEl.textContent = `config.jsonc has a mistake (${data.error}). Showing the last working version.`;

  serviceList = data.services;
  applySettings(data);

  groups = [];
  const byGroup = new Map();
  for (const s of serviceList) {
    const g = s.group || 'Other';
    if (!byGroup.has(g)) byGroup.set(g, []);
    byGroup.get(g).push(s);
  }
  for (const [name, list] of byGroup) {
    const down = list.filter((s) => s.state === 'down').length;
    const el = h('section', { class: 'group' },
      h('h3', {}, h('span', {}, name), h('em', { class: down ? 'bad' : '' }, down ? `${down} down` : String(list.length))),
      list.map(row));
    groups.push({ el, n: list.length });
  }

  // summary chip: "All 60 up" / "2 down" (click to show only the ones that are down)
  const checked = serviceList.filter((s) => s.state !== 'unknown');
  const down = serviceList.filter((s) => s.state === 'down').length;
  const chip = $('#summary');
  chip.hidden = !checked.length;
  chip.className = 'chip' + (down ? ' bad' : '');
  chip.replaceChildren(h('i'), down ? `${down} down` : `All ${checked.length} up`);
  if (!down) onlyDown = false;
  chip.setAttribute('aria-pressed', String(onlyDown));
  updateTitle();

  applyFilter(false);
  placedCols = 0;
  if (!serviceList.length) {
    $('#services').replaceChildren(h('p', { class: 'empty' }, 'No services yet. Add some to ', h('code', {}, 'config.jsonc'), ' and they appear here within seconds.'));
    return;
  }
  fit();
  if (hadBanner !== !bannerEl.hidden) fit();
}

// ---------- fit everything on one screen ----------
// Largest first. fit() uses the first one where the whole page fits the window.
const STEPS = [
  { row: 60, f: 18,   col: 400, logo: 40, gap: 16 },
  { row: 54, f: 17,   col: 360, logo: 36, gap: 14 },
  { row: 48, f: 16,   col: 330, logo: 34, gap: 14 },
  { row: 44, f: 15,   col: 310, logo: 30, gap: 12 },
  { row: 40, f: 14.5, col: 290, logo: 28, gap: 12 },
  { row: 36, f: 14,   col: 270, logo: 26, gap: 12 },
  { row: 32, f: 13,   col: 250, logo: 23, gap: 10 },
  { row: 29, f: 12,   col: 235, logo: 21, gap: 10 },
  { row: 26, f: 11.5, col: 220, logo: 19, gap: 8 },
  { row: 23, f: 11,   col: 205, logo: 17, gap: 8 },
];
const PHONE = { row: 46, f: 15, col: 300, logo: 30, gap: 10 };   // phones: one column, scroll instead of shrinking

function applyStep(st) {
  root.style.setProperty('--row', st.row + 'px');
  root.style.setProperty('--f', st.f + 'px');
  root.style.setProperty('--logo', st.logo + 'px');
  root.style.setProperty('--gap', st.gap + 'px');
  root.dataset.dense = st.row < 40 ? '1' : '';       // small rows hide the description text
}

// Spreads the groups over `cols` columns, always adding to the shortest column.
function place(cols) {
  const colEls = Array.from({ length: cols }, () => h('div', { class: 'col' }));
  const heights = Array(cols).fill(0);
  for (const g of groups) {
    if (g.el.hidden) continue;
    const i = heights.indexOf(Math.min(...heights));
    colEls[i].append(g.el);
    heights[i] += g.n + 1.6;
  }
  for (const g of groups) if (g.el.hidden) colEls[0].append(g.el);   // keep hidden ones attached
  $('#services').replaceChildren(h('div', { class: 'cols', style: `--cols:${cols}` }, colEls));
  placedCols = cols;
}

function fit() {
  if (!serviceList.length) return;
  const box = $('#services');
  const width = box.clientWidth || window.innerWidth;
  const visibleGroups = Math.max(1, groups.filter((g) => !g.el.hidden).length);
  const steps = window.innerWidth < 720 ? [PHONE] : STEPS;
  for (const st of steps) {
    applyStep(st);
    const cols = Math.max(1, Math.min(visibleGroups, Math.floor((width + st.gap) / (st.col + st.gap))));
    if (cols !== placedCols) place(cols);
    if (root.scrollHeight <= window.innerHeight + 1) return;   // it fits: stop here
  }
}

// ---------- filter ("/" to focus, Enter opens the first match, "f" = full screen) ----------
function applyFilter(refit = true) {
  const q = $('#filter').value.trim().toLowerCase();
  document.querySelectorAll('.row').forEach((r) => {
    r.hidden = (q && !r.dataset.q.includes(q)) || (onlyDown && r.dataset.state !== 'down');
  });
  groups.forEach((g) => { g.el.hidden = !g.el.querySelector('.row:not([hidden])'); });
  if (refit) { placedCols = 0; fit(); }
}
$('#filter').addEventListener('input', () => applyFilter());
$('#filter').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { const first = document.querySelector('.row:not([hidden])'); if (first) first.click(); }
  if (e.key === 'Escape') { e.target.value = ''; applyFilter(); e.target.blur(); }
});
$('#summary').addEventListener('click', () => {
  onlyDown = !onlyDown;
  $('#summary').setAttribute('aria-pressed', String(onlyDown));
  applyFilter();
});
function toggleFull() {
  if (document.fullscreenElement) document.exitFullscreen();
  else root.requestFullscreen().catch(() => {});
}
$('#full').addEventListener('click', toggleFull);
if (!document.fullscreenEnabled) $('#full').hidden = true;
addEventListener('keydown', (e) => {
  if (e.ctrlKey || e.metaKey || e.altKey || /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) return;
  if (e.key === '/') { e.preventDefault(); $('#filter').focus(); }
  if (e.key === 'f') toggleFull();
});

// ---------- theme ----------
$('#theme').addEventListener('click', () => {
  const next = root.dataset.theme === 'light' ? 'dark' : 'light';
  root.dataset.theme = next;
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

let resizeTimer;
addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => { fit(); drawAll(); }, 120);
});

async function boot() {
  const [i, history, svc] = await Promise.all([getJSON('api/info'), getJSON('api/history'), getJSON('api/services')]);
  info = i;
  history.forEach((p) => hist.push(p));
  renderServices(svc);
  renderTitle();
  drawAll();
  connect();
  setInterval(() => getJSON('api/services').then(renderServices).catch(() => {}), 20000);
}
boot().catch((err) => { $('#host').textContent = 'Cannot reach the server'; console.error(err); });
