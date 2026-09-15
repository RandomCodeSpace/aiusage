// aiusage dashboard client. Plain DOM, no framework. Ledger strings are only
// ever inserted as text nodes.
(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const css = getComputedStyle(document.documentElement);
  const v = (name) => css.getPropertyValue(name).trim();
  const SLOTS = [1, 2, 3, 4, 5, 6, 7, 8].map((i) => v(`--s${i}`));
  const RANGES = [['today', 'Today'], ['7d', '7 days'], ['30d', '30 days'], ['month', 'Month']];
  const DIMS = [
    ['tool', 'Harness'], ['model', 'Model'], ['project', 'Project'], ['agent', 'Subagent'],
    ['skill', 'Skill'], ['mcp_tool', 'MCP tool'], ['mcp_server', 'MCP server'], ['plugin', 'Plugin'],
  ];

  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    if (props) for (const [k, val] of Object.entries(props)) {
      if (k === 'class') node.className = val;
      else if (k === 'style') node.style.cssText = val;
      else if (k.startsWith('aria-') || k.startsWith('data-')) node.setAttribute(k, val);
      else node[k] = val;
    }
    for (const c of children) if (c != null) node.append(c);
    return node;
  }
  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); return node; }

  const usd = (micro) => '$' + (micro / 1e6).toFixed(2);
  function money(cost, opts = {}) {
    if (!cost || (cost.events > 0 && cost.micro_usd === 0 && cost.unpriced === cost.events)) {
      return el('span', { class: 'none' }, '-');
    }
    let cls = '';
    if (cost.computed > 0) cls += ' est';
    if (cost.unpriced > 0) cls += ' floor';
    const s = el('span', { class: cls.trim() }, usd(cost.micro_usd));
    return opts.suffix ? el('span', null, s, el('span', { class: 'unit' }, opts.suffix)) : s;
  }
  function tokens(n) {
    if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(0) + 'k';
    return String(n);
  }
  function ago(iso, now = Date.now()) {
    if (!iso) return 'never';
    const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
    if (s < 60) return s + ' s ago';
    if (s < 3600) return Math.round(s / 60) + ' min ago';
    if (s < 86400) return Math.round(s / 3600) + ' h ago';
    const d = Math.round(s / 86400);
    return d === 1 ? 'yesterday' : d + ' days ago';
  }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }

  // ----- charts -----
  const charts = {};
  function chartDefaults() {
    if (!window.Chart) return false;
    Chart.defaults.font.family = '"IBM Plex Sans", system-ui, sans-serif';
    Chart.defaults.color = v('--ink-3');
    Chart.defaults.animation = false;
    Chart.defaults.responsive = true;
    Chart.defaults.maintainAspectRatio = false;
    return true;
  }
  const gridColor = () => v('--rule-2');

  function hoursChart(hours) {
    if (!chartDefaults()) return;
    const labels = hours.map((h) => new Date(h.start).getHours() + ':00');
    const data = hours.map((h) => h.micro_usd / 1e6);
    if (charts.hours) {
      charts.hours.data.labels = labels;
      charts.hours.data.datasets[0].data = data;
      charts.hours.update('none');
      return;
    }
    charts.hours = new Chart($('hours'), {
      type: 'bar',
      data: { labels, datasets: [{ data, backgroundColor: SLOTS[0], borderRadius: { topLeft: 3, topRight: 3 }, borderSkipped: false, barPercentage: 0.7 }] },
      options: {
        plugins: { legend: { display: false }, tooltip: { callbacks: { label: (i) => '$' + i.raw.toFixed(2) } } },
        scales: {
          x: { grid: { display: false }, border: { display: false }, ticks: { maxTicksLimit: 6 } },
          y: { grid: { color: gridColor() }, border: { display: false }, ticks: { callback: (x) => '$' + x, maxTicksLimit: 3 } },
        },
      },
    });
  }

  function mixChart(models) {
    if (!chartDefaults()) return;
    const top = models.slice(0, 5);
    const rest = models.slice(5).reduce((s, m) => s + m.cost.micro_usd, 0);
    const datasets = top.map((m, i) => ({ label: m.model || '(no model)', data: [m.cost.micro_usd / 1e6], backgroundColor: SLOTS[i] }));
    if (rest > 0) datasets.push({ label: 'other', data: [rest / 1e6], backgroundColor: v('--ink-3') });
    if (charts.mix) {
      charts.mix.data.datasets = datasets;
      charts.mix.update('none');
      return;
    }
    charts.mix = new Chart($('mix'), {
      type: 'bar',
      data: { labels: ['24 h'], datasets },
      options: {
        indexAxis: 'y',
        plugins: { legend: { position: 'bottom', labels: { boxWidth: 10, boxHeight: 10, padding: 12 } }, tooltip: { callbacks: { label: (i) => i.dataset.label + ': $' + i.raw.toFixed(2) } } },
        scales: {
          x: { stacked: true, grid: { color: gridColor() }, border: { display: false }, ticks: { callback: (x) => '$' + x } },
          y: { stacked: true, display: false },
        },
        datasets: { bar: { borderColor: v('--paper'), borderWidth: 1, barThickness: 30 } },
      },
    });
  }

  function daysChart(h) {
    if (!chartDefaults()) return;
    const labels = h.days.map((d) => d.slice(5));
    const datasets = h.series.map((s, i) => ({ label: s.value || '(none)', data: s.micro_usd.map((m) => m / 1e6), backgroundColor: SLOTS[i], stack: 'd' }));
    if (charts.days) {
      charts.days.data.labels = labels;
      charts.days.data.datasets = datasets;
      charts.days.update('none');
      return;
    }
    charts.days = new Chart($('days'), {
      type: 'bar',
      data: { labels, datasets },
      options: {
        plugins: { legend: { display: false }, tooltip: { callbacks: { label: (i) => i.dataset.label + ': $' + i.raw.toFixed(2) } } },
        scales: {
          x: { stacked: true, grid: { display: false }, border: { display: false }, ticks: { maxTicksLimit: 10 } },
          y: { stacked: true, grid: { color: gridColor() }, border: { display: false }, ticks: { callback: (x) => '$' + x, maxTicksLimit: 5 } },
        },
        datasets: { bar: { borderColor: v('--paper'), borderWidth: 1, maxBarThickness: 36 } },
      },
    });
  }

  function sparkline(values) {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('viewBox', '0 0 120 22');
    svg.setAttribute('class', 'spark');
    svg.setAttribute('aria-label', 'tokens per hour, last 24 hours');
    const max = Math.max(1, ...values);
    const pts = values.map((x, i) => `${i * (120 / (values.length - 1))},${21 - 19 * x / max}`).join(' ');
    const line = document.createElementNS(ns, 'polyline');
    line.setAttribute('fill', 'none');
    line.setAttribute('stroke', SLOTS[0]);
    line.setAttribute('stroke-width', '1.5');
    line.setAttribute('points', pts);
    svg.append(line);
    return svg;
  }

  // ----- now view -----
  function renderNow(n) {
    const burn = $('burn');
    clear(burn).append(money(n.last_hour, { suffix: '/h' }));
    $('burn-sub').textContent = n.last_hour.events === 0
      ? 'nothing recorded in the last hour'
      : `${plural(n.last_hour.sessions, 'session', 'sessions')} across ${plural(n.last_hour.harnesses, 'harness', 'harnesses')}, ${tokens(n.last_hour.tokens)} tokens`;
    const today = $('today');
    clear(today).append('Today so far ', money(n.today));
    hoursChart(n.hours);

    const tb = clear($('lanes'));
    if (n.harnesses.length === 0) {
      tb.append(el('tr', null, el('td', { class: 'empty', colSpan: 7 }, 'No harness has written in the last 7 days.')));
    }
    for (const h of n.harnesses) {
      tb.append(el('tr', null,
        el('td', { class: 'tool' }, h.tool),
        el('td', { class: 'dim' }, ago(h.last_event, Date.parse(n.generated_at))),
        el('td', { class: 'n' }, h.sessions_1h ? String(h.sessions_1h) : el('span', { class: 'none' }, '0')),
        el('td', { class: 'n' }, h.tokens_1h ? tokens(h.tokens_1h) : el('span', { class: 'none' }, '0')),
        el('td', { class: 'n' }, h.cost_1h.events ? money(h.cost_1h) : el('span', { class: 'none' }, '-')),
        el('td', null, sparkline(h.spark_24h)),
        el('td', { class: 'dim' }, [h.cost_from, h.tier].filter(Boolean).join(', ')),
      ));
    }
    const idle = $('idle');
    idle.textContent = n.idle.length === 0 ? '' :
      `${plural(n.idle.length, 'harness has', 'harnesses have')} history but nothing this week: ` +
      n.idle.map((h) => `${h.tool} (${ago(h.last_event, Date.parse(n.generated_at))})`).join(', ') + '.';

    mixChart(n.models_24h);

    const notes = clear($('notes'));
    const note = (cls, ...parts) => notes.append(el('li', { class: cls }, el('i'), el('span', null, ...parts)));
    if (n.unpriced_7d.events > 0) {
      note('warn', el('b', null, plural(n.unpriced_7d.events, 'row', 'rows') + ' unpriced'),
        ` in the last 7 days (${n.unpriced_7d.models.join(', ')}). Totals show as floors until priced.`);
    }
    if (n.rollup_stale) {
      note('serious', el('b', null, 'Summary table is behind the ledger.'), ' The collector rebuilds it on its next pass.');
    }
    if (!n.watermark) {
      note('serious', el('b', null, 'The ledger is empty.'), ' Nothing has been collected yet.');
    }
    if (notes.children.length === 0) {
      note('good', 'Nothing outstanding. Every row in the last 7 days carries a price.');
    }
  }

  // ----- history view -----
  const hist = { dim: 'tool', range: '7d' };
  function segment(container, items, current, onPick) {
    clear(container);
    for (const [id, label] of items) {
      container.append(el('button', { type: 'button', 'aria-pressed': String(id === current), onclick: () => onPick(id) }, label));
    }
  }
  function renderHistory(h) {
    const dimLabel = Object.fromEntries(DIMS)[h.dim] || h.dim;
    $('hist-title').textContent = 'By ' + dimLabel.toLowerCase();
    const sub = $('hist-sub');
    let text = `${plural(h.rows.length, 'value', 'values')}, `;
    text += h.total.cost.events ? '' : 'nothing in this window';
    if (h.total.cost.events) {
      clear(sub).append(text, money(h.total.cost), ` over ${tokens(h.total.tokens)} tokens`);
      if (h.coverage) sub.append(`; ${h.coverage.turns} of ${h.coverage.of} turns carry a ${dimLabel.toLowerCase()}`);
    } else {
      sub.textContent = text;
    }

    const rank = clear($('rank'));
    for (const t of ['Value', '', 'Tokens', 'Cost']) rank.append(el('div', { class: 'h' + (t === 'Tokens' || t === 'Cost' ? ' r' : '') }, t));
    const max = h.rows.length ? Math.max(1, h.rows[0].cost.micro_usd) : 1;
    h.rows.forEach((r, i) => {
      const color = i < 5 ? SLOTS[i] : v('--ink-3');
      rank.append(
        el('div', { class: 'k' }, el('i', { style: `background:${color}` }), r.value || '(none)'),
        el('div', { class: 'bar' }, el('b', { style: `width:${100 * r.cost.micro_usd / max}%;background:${color}` })),
        el('div', { class: 'r tok' }, tokens(r.tokens)),
        el('div', { class: 'r' }, money(r.cost)),
      );
    });
    if (h.rows.length === 0) rank.append(el('div', { class: 'empty', style: 'grid-column:1/-1' }, 'No rows in this window.'));

    const legend = clear($('legend'));
    h.series.forEach((s, i) => legend.append(el('span', null, el('i', { style: `background:${SLOTS[i]}` }), s.value || '(none)')));
    daysChart(h);
  }

  // ----- data + routing -----
  async function getJSON(url) {
    const r = await fetch(url, { cache: 'no-store' });
    if (!r.ok) throw new Error(`${url}: ${r.status} ${await r.text()}`);
    return r.json();
  }
  let view = 'now';
  async function refresh() {
    try {
      if (view === 'now') renderNow(await getJSON('/api/now'));
      else renderHistory(await getJSON(`/api/history?dim=${encodeURIComponent(hist.dim)}&range=${encodeURIComponent(hist.range)}`));
    } catch (err) {
      setStatus('down', String(err.message || err));
    }
  }
  function route() {
    const hash = location.hash.replace(/^#/, '') || 'now';
    const [name, qs] = hash.split('?');
    view = name === 'history' ? 'history' : 'now';
    if (view === 'history') {
      const q = new URLSearchParams(qs || '');
      if (DIMS.some(([d]) => d === q.get('dim'))) hist.dim = q.get('dim');
      if (RANGES.some(([r]) => r === q.get('range'))) hist.range = q.get('range');
      segment($('range'), RANGES, hist.range, (r) => { location.hash = `history?dim=${hist.dim}&range=${r}`; });
      segment($('dim'), DIMS, hist.dim, (d) => { location.hash = `history?dim=${d}&range=${hist.range}`; });
    }
    $('view-now').hidden = view !== 'now';
    $('view-history').hidden = view !== 'history';
    for (const a of document.querySelectorAll('nav a')) {
      if (a.dataset.view === view) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    }
    refresh();
  }

  // ----- live -----
  let watermark = null;
  const status = $('status');
  function setStatus(cls, text) { status.className = 'status ' + cls; $('status-text').textContent = text; }
  function tickStatus() {
    if (status.classList.contains('down')) return;
    setStatus(watermark ? 'live' : 'stale', watermark ? 'collector wrote ' + ago(watermark) : 'ledger empty');
  }
  function connect() {
    const es = new EventSource('/events');
    es.addEventListener('tick', (e) => {
      const { watermark: wm } = JSON.parse(e.data);
      const moved = wm !== watermark && watermark !== null;
      watermark = wm || null;
      tickStatus();
      if (moved) refresh();
    });
    es.onerror = () => setStatus('down', 'connection lost, retrying');
    es.onopen = () => tickStatus();
  }

  window.addEventListener('hashchange', route);
  window.addEventListener('DOMContentLoaded', () => {
    route();
    connect();
    setInterval(tickStatus, 1000);
  });
})();
