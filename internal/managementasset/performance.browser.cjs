// Run against a local CPA instance with CPA_PERFORMANCE_URL and CPA_PERFORMANCE_KEY.
const assert = require('node:assert/strict');
const path = require('node:path');
const {chromium} = require(process.env.CPA_PERFORMANCE_PLAYWRIGHT || path.resolve(__dirname, '../../tools/codex-auto-oauth/node_modules/playwright'));
const base = process.env.CPA_PERFORMANCE_URL;
const key = process.env.CPA_PERFORMANCE_KEY;
const artifacts = process.env.CPA_PERFORMANCE_ARTIFACTS;

(async () => {
  const browser = await chromium.launch({headless: true});
  const context = await browser.newContext({viewport: {width: 1440, height: 1000}});
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    await page.goto(base + '/management.html');
    await page.locator('input[type="password"]').fill(key);
    await page.getByRole('button', {name: '登录', exact: true}).click();
    await page.locator('#cpa-performance[data-state="live"]').waitFor({timeout: 15000});
    const panel = page.locator('#cpa-performance');
    const initial = await page.locator('[data-perf="status"]').getAttribute('title');
    await page.waitForFunction(value => document.querySelector('[data-perf="status"]').title !== value, initial);
    assert.match(await page.locator('[data-perf="download"]').textContent(), /Mbps$/);
    assert.match(await page.locator('[data-perf="day"]').textContent(), /UTC\+8/);
    assert(await page.locator('.cpa-performance-core').count() > 0);
    const samples = [];
    page.on('response', async response => {
      if (response.url().endsWith('/v8/management/observability/performance') && response.ok()) {
        const data = await response.json();
        samples.push({at: Date.now(), refresh: data.refresh_seconds});
      }
    });
    await page.waitForTimeout(3300);
    assert(samples.length >= 3, 'dashboard must update at least three times in 3.3 seconds');
    assert(samples.every(sample => sample.refresh === 1), 'backend must advertise one-second refresh');
    console.log('PASS live Linux sampling and one-second refresh');

    let calls = 0, mode = 'healthy';
    let sampleTime = Date.now();
    let cores = 8;
    await page.route('**/v8/management/observability/performance', async route => {
      calls++;
      if (mode === 'error') return route.fulfill({status: 503, json: {error: 'unavailable'}});
      if (mode === 'unauthorized') return route.fulfill({status: 401, json: {error: 'unauthorized'}});
      sampleTime += 1000;
      const system = mode === 'unsupported' ? {available: false, error: 'unsupported_platform', sampled_at: new Date(sampleTime).toISOString(), cpu: null, memory: null, network: null} : {
        available: true, sampled_at: new Date(sampleTime).toISOString(), interval_seconds: 1,
        cpu: {total_percent: 27.4, cores: Array.from({length: cores}, (_, index) => ({name: 'cpu' + index, percent: (index * 13 + calls) % 100}))},
        memory: {percent: 62.5, used_bytes: 5368709120, total_bytes: 8589934592},
        network: {default_interface: 'eth0', day: '2026-11-07', timezone: 'UTC+08:00', since: '2026-11-06T16:00:02Z', persisted: true, interfaces: [
          {name: 'eth0', download_bps: 12500000 + (calls % 5) * 1000000, upload_bps: 4500000 + (calls % 3) * 1000000, today_download_bytes: 23500000000 + calls, today_upload_bytes: 8100000000 + calls},
          {name: 'eth1', download_bps: 1250000, upload_bps: 250000, today_download_bytes: 1000000000, today_upload_bytes: 200000000}
        ]}
      };
      return route.fulfill({json: {rpm: 1234, window_seconds: 60, refresh_seconds: 1, system}});
    });
    await page.waitForFunction(() => document.querySelector('[data-perf="rpm"]').textContent === '1,234');
    for (const width of [1440, 1024, 768, 375, 320]) {
      await page.setViewportSize({width, height: 1000});
      await panel.scrollIntoViewIfNeeded();
      for (const theme of ['light', 'dark']) {
        await page.evaluate(theme => document.documentElement.setAttribute('data-theme', theme), theme);
        await page.waitForTimeout(60);
        const geometry = await panel.evaluate(node => {
          const bounds = node.getBoundingClientRect();
          const clipped = [...node.querySelectorAll('b, select, .cpa-performance-core, .cpa-performance-daily-note')].filter(item => item.scrollWidth > item.clientWidth + 2).map(item => item.className);
          const native = document.querySelector('[class*="dashboard-module__heroPanel___"]').getBoundingClientRect();
          return {left: bounds.left, right: bounds.right, height: bounds.height, width: innerWidth, clipped, before: bounds.right <= native.left, above: bounds.bottom <= native.top, horizontalGap: native.left - bounds.right, verticalGap: native.top - bounds.bottom};
        });
        assert(geometry.left >= 0 && geometry.right <= width + 1, JSON.stringify(geometry));
        assert.deepEqual(geometry.clipped, [], 'monitor text must not clip');
        assert(geometry.before || geometry.above, 'monitor must sit left of or above request totals');
        assert(Math.abs((geometry.before ? geometry.horizontalGap : geometry.verticalGap) - 8) < 1, 'monitor must sit eight pixels from the request card');
        if (width === 1440) assert.equal(geometry.right - geometry.left, 570, 'monitor must be 1.5 times its previous desktop width');
        assert.equal(await panel.evaluate(node => getComputedStyle(node).gap), '2px');
        const nativeStyles = await panel.evaluate(node => {
          const card = document.querySelector('[class*="dashboard-module__heroPanel___"]');
          const figure = card.querySelector('[class*="heroFigure"]');
          const title = document.querySelector('[class*="dashboard-module__heroTitle___"]');
          const period = title.querySelector('[class*="heroPeriod___"]');
          function snapshot() {
            const style = getComputedStyle(card), bounds = card.getBoundingClientRect();
            const titleStyle = getComputedStyle(title), periodStyle = getComputedStyle(period), periodBounds = period.getBoundingClientRect();
            return {width: bounds.width, height: bounds.height, padding: style.padding, borderRadius: style.borderRadius, fontSize: getComputedStyle(figure).fontSize,
              titleFontSize: titleStyle.fontSize, titleLetterSpacing: titleStyle.letterSpacing, titleLineHeight: titleStyle.lineHeight,
              periodFontSize: periodStyle.fontSize, periodWidth: periodBounds.width, periodHeight: periodBounds.height,
              periodAnimation: periodStyle.animationName};
          }
          const actual = snapshot();
          const stylesheet = document.querySelector('link[href$="performance.css"]').sheet;
          stylesheet.disabled = true;
          node.style.display = 'none';
          const original = snapshot();
          stylesheet.disabled = false;
          node.style.removeProperty('display');
          return {actual, original};
        });
        assert.deepEqual(nativeStyles.actual, nativeStyles.original, 'CPA title, status dot and request card must retain their native dimensions and styles');
        const cpuPattern = await page.locator('[data-perf="cpu-bar"]').evaluate(node => getComputedStyle(node).backgroundImage);
        assert(cpuPattern.includes('1.5px') && cpuPattern.includes('2px'), 'CPU bar must use 1.5px blocks and 0.5px gaps');
        const memoryPattern = await page.locator('[data-perf="memory-bar"]').evaluate(node => getComputedStyle(node).backgroundImage);
        assert.deepEqual(memoryPattern.match(/[\d.]+px/g), cpuPattern.match(/[\d.]+px/g), 'memory blocks must use the same dense pattern as CPU');
        const labels = await panel.evaluate(node => ({
          cpuHeading: node.querySelector('.cpa-performance-cpu legend'),
          memory: node.querySelector('.cpa-performance-memory .cpa-performance-row').textContent.trim(),
          rpm: node.querySelector('.cpa-performance-rpm').textContent.trim(),
          rpmLegend: node.querySelector('.cpa-performance-rpm legend'),
          netLegend: node.querySelector('.cpa-performance-net legend'),
          netLabel: node.querySelector('[data-perf="interface"]').previousElementSibling.textContent,
          statusClipped: getComputedStyle(node.querySelector('[data-perf="status"]')).clipPath === 'inset(50%)'
        }));
        assert.equal(labels.cpuHeading, null);
        assert.equal(labels.rpmLegend, null);
        assert.equal(labels.netLegend, null);
        assert.equal(labels.netLabel, 'NET');
        assert.equal(labels.rpm, '1,234RPM');
        assert(labels.memory.startsWith('MEM62.5%') && labels.statusClipped);
        if (theme === 'light') console.log(`SIZE ${width}px: ${(geometry.right - geometry.left).toFixed(1)} x ${geometry.height.toFixed(1)}`);
        await page.screenshot({path: path.join(artifacts, `cpa-performance-${theme}-${width}.png`)});
      }
    }
    assert.equal(await page.locator('[data-perf="scale"]').textContent(), 'Mbps');
    const ticks = await page.locator('.cpa-performance-y-axis span').allTextContents();
    assert(ticks.every(tick => tick.endsWith(' Mbps')), 'every bandwidth tick must carry its unit');
    const top = Number.parseFloat(ticks[0].replace(/,/g, ''));
    const middle = Number.parseFloat(ticks[1].replace(/,/g, ''));
    assert(top >= 100 && middle === top / 2, 'bandwidth axis must show maximum, midpoint and zero');
    const pixels = await page.locator('[data-perf="chart"]').evaluate(canvas => {
      const data = canvas.getContext('2d').getImageData(0, 0, canvas.width, canvas.height).data;
      let colored = 0;
      for (let i = 0; i < data.length; i += 4) if (data[i + 3] && Math.max(data[i], data[i + 1], data[i + 2]) - Math.min(data[i], data[i + 1], data[i + 2]) > 30) colored++;
      return colored;
    });
    assert(pixels > 10, 'bandwidth graph must contain rendered series');
    await page.locator('[data-perf="interface"]').selectOption('eth1');
    assert.equal(await page.locator('[data-perf="download"]').textContent(), '↓ 10.00 Mbps');
    assert.equal(await page.locator('[data-perf="daily-download"]').textContent(), '1.00 GB');
    mode = 'error';
    await page.locator('#cpa-performance[data-state="stale"]').waitFor();
    mode = 'unsupported';
    await page.locator('#cpa-performance[data-state="unavailable"]').waitFor();
    assert.equal(await page.locator('[data-perf="cpu"]').textContent(), '--');
    mode = 'healthy';
    cores = 128;
    await page.waitForFunction(() => document.querySelectorAll('.cpa-performance-core').length === 128);
    assert(await page.locator('[data-perf="cores"]').evaluate(node => node.scrollHeight > node.clientHeight));
    await page.setViewportSize({width: 1440, height: 1000});
    await page.getByRole('link', {name: '配额管理', exact: true}).click();
    await panel.waitFor({state: 'detached'});
    const before = calls;
    await page.waitForTimeout(2300);
    assert.equal(calls, before, 'polling must stop away from dashboard');
    await page.getByRole('link', {name: '仪表盘', exact: true}).click();
    await panel.waitFor();
    await page.waitForFunction(() => document.querySelector('#cpa-performance').dataset.state === 'live');
    assert.equal(await panel.count(), 1, 'route re-entry duplicated monitor');
    await page.evaluate(() => {
      Object.defineProperty(document, 'hidden', {configurable: true, get: () => true});
      document.dispatchEvent(new Event('visibilitychange'));
    });
    const hiddenCalls = calls;
    await page.waitForTimeout(2300);
    assert.equal(calls, hiddenCalls, 'hidden dashboard continued polling');
    await page.evaluate(() => {
      delete document.hidden;
      document.dispatchEvent(new Event('visibilitychange'));
    });
    mode = 'unauthorized';
    await page.locator('#cpa-performance[data-state="stale"]').waitFor();
    const unauthorizedCalls = calls;
    await page.waitForTimeout(2300);
    assert.equal(calls, unauthorizedCalls, 'rejected credentials were repeatedly submitted');
    assert.deepEqual(errors, []);
    console.log('PASS 1.5x monitor, 8px adjacency, 2px box spacing, native request card preserved, dense CPU/MEM bars, inline MEM/RPM, Mbps units on every tick, responsive themes and polling lifecycle');
  } catch (error) {
    console.error({url: page.url(), errors});
    await page.screenshot({path: path.join(artifacts, 'cpa-performance-failure.png')});
    throw error;
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
