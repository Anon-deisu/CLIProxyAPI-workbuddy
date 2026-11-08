(function () {
  'use strict';
  if (window.__cpaPerformanceLoaded) return;
  window.__cpaPerformanceLoaded = true;

  const interval = 1000;
  const historyDuration = 120000;
  const endpoint = '/v8/management/observability/performance';
  const nativeFetch = window.fetch;
  let connection = null;
  let panel = null;
  let timer = 0;
  let pending = null;
  let history = [];
  let latest = null;
  let chinese = true;
  let selectedInterface = '';
  const labels = {
    system: ['服务器', 'SERVER'], total: ['总占用', 'Total'], live: ['实时 · 1s', 'LIVE · 1s'],
    waiting: ['等待连接', 'Connecting'], warming: ['采样中', 'Sampling'], stale: ['更新失败', 'Update failed'],
    unsupported: ['系统指标暂不支持', 'Host metrics unsupported'], unavailable: ['系统采样不可用', 'Host metrics unavailable'],
    cores: ['逻辑核', 'cores'], memory: ['内存', 'Memory'], rpm: ['60s', '60s'],
    download: ['今日下载', 'Downloaded today'], upload: ['今日上传', 'Uploaded today'],
    now: ['现在', 'now'], network: ['网卡', 'Interface'], since: ['起点 ', 'Since '],
    unsaved: ['累计流量未持久化', 'Traffic not persisted']
  };
  const text = key => labels[key][chinese ? 0 : 1];
  const field = name => panel.querySelector('[data-perf="' + name + '"]');
  const percent = value => value == null ? '--' : value.toFixed(1) + '%';
  const mbps = value => value == null ? '-- Mbps' : (value * 8 / 1000000).toFixed(2) + ' Mbps';

  function bytes(value, binary) {
    const unit = binary ? 1024 : 1000;
    const units = binary ? ['B', 'KiB', 'MiB', 'GiB', 'TiB'] : ['B', 'KB', 'MB', 'GB', 'TB'];
    let index = 0;
    while (value >= unit && index < units.length - 1) { value /= unit; index++; }
    return value.toFixed(index > 0 ? 2 : 0) + ' ' + units[index];
  }

  function baseFor(value) {
    const url = new URL(value, window.location.href);
    const match = url.pathname.match(/\/v(?:0|8)\/management\//);
    return match ? url.origin + url.pathname.slice(0, match.index) : '';
  }

  function capture(base, authorization) {
    const bearer = String(authorization || '').match(/^Bearer\s+(.+)$/i);
    if (!base || !bearer) return;
    if (!connection || connection.base !== base || connection.key !== bearer[1]) {
      cancelPoll();
      connection = {base, key: bearer[1]};
      history = [];
      latest = null;
      selectedInterface = '';
      schedule(0);
    }
  }

  window.fetch = function (input, init) {
    const url = typeof input === 'string' || input instanceof URL ? String(input) : input.url;
    const base = baseFor(url);
    if (base) capture(base, new Headers((init && init.headers) || input.headers || {}).get('authorization'));
    return nativeFetch.apply(this, arguments);
  };
  const xhrOpen = XMLHttpRequest.prototype.open;
  const xhrHeader = XMLHttpRequest.prototype.setRequestHeader;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cpaPerformanceBase = baseFor(url);
    return xhrOpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    if (name.toLowerCase() === 'authorization') capture(this.__cpaPerformanceBase, value);
    return xhrHeader.apply(this, arguments);
  };

  function visible() { return panel && panel.isConnected && !document.hidden; }

  function cancelPoll() {
    clearTimeout(timer);
    timer = 0;
    if (pending) pending.abort();
    pending = null;
  }

  function schedule(delay) {
    clearTimeout(timer);
    if (visible() && connection && !connection.suspended && !pending) timer = window.setTimeout(poll, delay);
  }

  function status(state, label) {
    panel.dataset.state = state;
    field('status').textContent = label;
  }

  function mount() {
    const target = document.querySelector('[class*="dashboard-module__heroPanel___"]');
    if (panel && panel.isConnected && target) return;
    cancelPoll();
    panel = null;
    if (!target) return;
    const label = target.querySelector('[class*="heroPanelLabel"]');
    chinese = /[\u3400-\u9fff]/.test(label.textContent);
    const layout = target.parentElement;
    layout.classList.add('cpa-performance-layout');
    panel = document.createElement('section');
    panel.id = 'cpa-performance';
    panel.className = 'cpa-performance';
    panel.setAttribute('aria-label', text('system'));
    panel.innerHTML = `
      <span class="cpa-performance-status" data-perf="status" role="status"></span>
      <fieldset class="cpa-performance-cpu">
        <div class="cpa-performance-row"><span data-label="total"></span><span class="cpa-performance-meter"><i data-perf="cpu-bar"></i></span><b class="cpa-performance-value cpa-performance-total" data-perf="cpu">--</b></div>
        <div class="cpa-performance-cores" data-perf="cores" tabindex="0"></div>
      </fieldset>
      <fieldset class="cpa-performance-memory">
        <div class="cpa-performance-row"><span class="cpa-performance-memory-label">MEM</span><b class="cpa-performance-value" data-perf="memory">--</b></div>
        <div class="cpa-performance-meter"><i data-perf="memory-bar"></i></div><span class="cpa-performance-memory-detail" data-perf="memory-bytes">-- / --</span>
      </fieldset>
      <fieldset class="cpa-performance-rpm"><b class="cpa-performance-value" data-perf="rpm">--</b><span class="cpa-performance-muted">RPM</span></fieldset>
      <fieldset class="cpa-performance-net">
        <div class="cpa-performance-net-head"><label class="cpa-performance-net-selector"><span>NET</span><select data-perf="interface"></select></label><div class="cpa-performance-rates"><b class="cpa-performance-value cpa-performance-down" data-perf="download">↓ -- Mbps</b><b class="cpa-performance-value cpa-performance-up" data-perf="upload">↑ -- Mbps</b></div></div>
        <div class="cpa-performance-plot"><div class="cpa-performance-y-axis" aria-hidden="true"><span data-perf="scale-top">-- Mbps</span><span data-perf="scale-mid">-- Mbps</span><span>0 Mbps</span></div><canvas data-perf="chart" role="img"></canvas></div>
        <div class="cpa-performance-axis"><span>−2m</span><span data-perf="scale">-- Mbps</span><span data-label="now"></span></div>
        <div class="cpa-performance-daily"><div class="cpa-performance-row"><span data-label="download"></span><b class="cpa-performance-value cpa-performance-down" data-perf="daily-download">--</b></div><div class="cpa-performance-row"><span data-label="upload"></span><b class="cpa-performance-value cpa-performance-up" data-perf="daily-upload">--</b></div>
          <div class="cpa-performance-daily-note"><span data-perf="day">UTC+8</span><span data-perf="since"></span></div>
        </div>
      </fieldset>`;
    panel.querySelectorAll('[data-label]').forEach(node => { node.textContent = text(node.dataset.label); });
    field('interface').setAttribute('aria-label', text('network'));
    field('cores').setAttribute('aria-label', 'CPU ' + text('cores'));
    field('interface').addEventListener('change', event => {
      selectedInterface = event.target.value;
      renderNetwork();
    });
    layout.insertBefore(panel, target);
    resizeObserver.observe(panel);
    status('waiting', text('waiting'));
    if (latest) render(latest);
    schedule(0);
  }

  function render(data) {
    const rpm = data.rpm < 100000 ? data.rpm.toLocaleString() : data.rpm.toLocaleString('en-US', {notation: 'compact', maximumFractionDigits: 1});
    field('rpm').textContent = rpm;
    field('rpm').style.fontSize = rpm.length > 5 ? '16.5px' : '21px';
    field('rpm').title = data.rpm.toLocaleString() + ' / 60s';
    const system = data.system;
    if (!system.available) {
      status('unavailable', text(system.error === 'unsupported_platform' ? 'unsupported' : 'unavailable'));
      ['cpu', 'memory', 'memory-bytes', 'daily-download', 'daily-upload'].forEach(name => { field(name).textContent = '--'; });
      field('cores').replaceChildren();
      delete field('cores').dataset.names;
      field('cpu-bar').style.width = field('memory-bar').style.width = '0%';
      field('download').textContent = '↓ -- Mbps';
      field('upload').textContent = '↑ -- Mbps';
      drawChart();
      return;
    }
    status(system.cpu.total_percent == null ? 'warming' : 'live', text(system.cpu.total_percent == null ? 'warming' : 'live'));
    field('status').title = new Date(system.sampled_at).toLocaleTimeString();
    field('cpu').textContent = percent(system.cpu.total_percent);
    field('cpu-bar').style.width = (system.cpu.total_percent || 0) + '%';
    const cores = field('cores');
    if (cores.dataset.names !== system.cpu.cores.map(core => core.name).join(',')) {
      cores.dataset.names = system.cpu.cores.map(core => core.name).join(',');
      cores.replaceChildren();
      system.cpu.cores.forEach(core => {
        const row = document.createElement('div');
        row.className = 'cpa-performance-core';
        row.innerHTML = '<span></span><span class="cpa-performance-meter"><i></i></span><b class="cpa-performance-value"></b>';
        row.firstChild.textContent = core.name.replace('cpu', '#');
        cores.appendChild(row);
      });
    }
    system.cpu.cores.forEach((core, index) => {
      const row = cores.children[index];
      row.querySelector('b').textContent = core.percent == null ? '--' : Math.round(core.percent) + '%';
      row.querySelector('i').style.width = (core.percent || 0) + '%';
      row.title = core.name + ': ' + percent(core.percent);
    });
    field('memory').textContent = percent(system.memory.percent);
    field('memory-bar').style.width = system.memory.percent + '%';
    field('memory-bytes').textContent = bytes(system.memory.used_bytes, true) + ' / ' + bytes(system.memory.total_bytes, true);
    const select = field('interface');
    const interfaces = system.network.interfaces;
    if (select.dataset.names !== interfaces.map(item => item.name).join(',')) {
      select.replaceChildren();
      select.dataset.names = interfaces.map(item => item.name).join(',');
      interfaces.forEach(item => select.add(new Option(item.name, item.name)));
    }
    if (!interfaces.some(item => item.name === selectedInterface)) selectedInterface = system.network.default_interface;
    select.value = selectedInterface;
    select.disabled = interfaces.length === 0;
    renderNetwork();
  }

  function renderNetwork() {
    if (!latest || !latest.system.network) return;
    const network = latest.system.network;
    const iface = network.interfaces.find(item => item.name === selectedInterface);
    field('download').textContent = '↓ ' + mbps(iface ? iface.download_bps : null);
    field('upload').textContent = '↑ ' + mbps(iface ? iface.upload_bps : null);
    field('daily-download').textContent = iface ? bytes(iface.today_download_bytes, false) : '--';
    field('daily-upload').textContent = iface ? bytes(iface.today_upload_bytes, false) : '--';
    field('day').textContent = network.day + ' · UTC+8';
    field('since').textContent = network.persisted ? text('since') + new Date(network.since).toLocaleTimeString('en-GB', {timeZone: 'Asia/Shanghai', hour12: false}) : text('unsaved');
    field('since').title = new Date(network.since).toLocaleString(chinese ? 'zh-CN' : 'en-GB', {timeZone: 'Asia/Shanghai'}) + ' UTC+8';
    drawChart();
  }

  function drawChart() {
    if (!panel || !panel.isConnected) return;
    const canvas = field('chart');
    const width = canvas.clientWidth, height = canvas.clientHeight;
    const ratio = window.devicePixelRatio || 1;
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    const ctx = canvas.getContext('2d');
    ctx.scale(ratio, ratio);
    const colors = getComputedStyle(panel);
    const points = history.map(sample => ({at: sample.at, iface: sample.network && sample.network.interfaces.find(item => item.name === selectedInterface)}));
    const peakMbps = Math.max(.01, ...points.flatMap(point => point.iface ? [point.iface.download_bps || 0, point.iface.upload_bps || 0] : []).map(value => value * 8 / 1000000));
    const magnitude = Math.pow(10, Math.floor(Math.log10(peakMbps)));
    const topMbps = [1, 2, 5, 10].find(step => step * magnitude >= peakMbps) * magnitude;
    const ceiling = topMbps * 1000000 / 8;
    const end = latest ? Date.parse(latest.system.sampled_at) : Date.now();
    const baseline = height - 1;
    ctx.strokeStyle = colors.getPropertyValue('--perf-grid').trim();
    ctx.lineWidth = 1;
    ctx.globalAlpha = .65;
    for (const y of [1, height / 2, baseline]) { ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(width, y); ctx.stroke(); }
    for (const [metric, color] of [['download_bps', '--perf-down'], ['upload_bps', '--perf-up']]) {
      ctx.strokeStyle = ctx.fillStyle = colors.getPropertyValue(color).trim();
      let segment = [];
      function finish() {
        if (!segment.length) return;
        ctx.beginPath();
        ctx.moveTo(segment[0][0], baseline);
        segment.forEach(point => ctx.lineTo(point[0], point[1]));
        ctx.lineTo(segment[segment.length - 1][0], baseline);
        ctx.closePath(); ctx.globalAlpha = .1; ctx.fill();
        ctx.beginPath(); ctx.moveTo(...segment[0]);
        segment.slice(1).forEach(point => ctx.lineTo(...point));
        ctx.globalAlpha = 1; ctx.lineWidth = 1.5; ctx.stroke();
        segment = [];
      }
      let previousAt = 0;
      points.forEach(point => {
        const value = point.iface ? point.iface[metric] : null;
        if (value == null || (previousAt && point.at - previousAt > interval * 3)) finish();
        if (value != null) segment.push([Math.max(0, width * (1 - (end - point.at) / historyDuration)), baseline - Math.min(value / ceiling, 1) * (height - 3)]);
        previousAt = point.at;
      });
      finish();
    }
    const tick = value => Number(value.toPrecision(3)).toLocaleString('en-US', {maximumFractionDigits: 3});
    field('scale-top').textContent = tick(topMbps) + ' Mbps';
    field('scale-mid').textContent = tick(topMbps / 2) + ' Mbps';
    field('scale').textContent = 'Mbps';
    canvas.setAttribute('aria-label', (chinese ? '近两分钟带宽，上行和下行' : 'Upload and download bandwidth, last two minutes') + ' · Mbps');
  }

  async function poll() {
    if (!visible() || !connection || pending) return;
    const controller = new AbortController();
    const currentPanel = panel, currentConnection = connection;
    pending = controller;
    try {
      const response = await nativeFetch(currentConnection.base + endpoint, {signal: controller.signal, headers: {'Authorization': 'Bearer ' + currentConnection.key, 'Accept': 'application/json'}, cache: 'no-store'});
      if (response.status === 401 || response.status === 403) currentConnection.suspended = true;
      if (!response.ok) throw new Error('performance HTTP ' + response.status);
      const data = await response.json();
      if (controller.signal.aborted || panel !== currentPanel || connection !== currentConnection) return;
      latest = data;
      const at = Date.parse(data.system.sampled_at);
      if (!history.length || history[history.length - 1].at !== at) history.push({at, network: data.system.network});
      history = history.filter(sample => at - sample.at <= historyDuration).slice(-Math.ceil(historyDuration / interval) - 1);
      render(data);
    } catch (error) {
      if (!controller.signal.aborted && panel === currentPanel) status('stale', text('stale'));
    } finally {
      if (pending === controller) { pending = null; schedule(interval); }
    }
  }

  const resizeObserver = new ResizeObserver(drawChart);
  const observer = new MutationObserver(() => {
    if (panel && !panel.isConnected) { resizeObserver.unobserve(panel); cancelPoll(); panel = null; }
    if (!panel) mount();
  });
  observer.observe(document.documentElement, {childList: true, subtree: true});
  new MutationObserver(drawChart).observe(document.documentElement, {attributes: true, attributeFilter: ['data-theme']});
  document.addEventListener('visibilitychange', () => { if (document.hidden) cancelPoll(); else schedule(0); });
  window.addEventListener('pagehide', cancelPoll);
  window.addEventListener('hashchange', () => {
    if (/login/.test(window.location.hash)) { cancelPoll(); connection = null; latest = null; history = []; }
    mount();
  });
  mount();
})();
