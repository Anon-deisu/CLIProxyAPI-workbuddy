'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const readline = require('node:readline');

class LoginFailure extends Error {
  constructor(code) {
    super(code);
    this.code = code;
  }
}

function loadPlaywright() {
  const candidates = [];
  if (process.env.CPA_CODEX_AUTO_OAUTH_PLAYWRIGHT) candidates.push(process.env.CPA_CODEX_AUTO_OAUTH_PLAYWRIGHT);
  candidates.push(path.join(process.cwd(), 'tools', 'codex-auto-oauth', 'node_modules', 'playwright'));
  candidates.push('playwright');
  for (const candidate of candidates) {
    try { return require(candidate); } catch (_) {}
  }
  throw new LoginFailure('browser_unavailable');
}

function totp(secret, now = Date.now()) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = 0;
  let value = 0;
  const bytes = [];
  for (const char of String(secret).toUpperCase().replace(/=+$/, '')) {
    const digit = alphabet.indexOf(char);
    if (digit < 0) throw new LoginFailure('invalid_credentials');
    value = (value << 5) | digit;
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      bytes.push((value >>> bits) & 255);
    }
  }
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(now / 30000)));
  const digest = crypto.createHmac('sha1', Buffer.from(bytes)).update(counter).digest();
  const offset = digest[digest.length - 1] & 15;
  return String((digest.readUInt32BE(offset) & 0x7fffffff) % 1000000).padStart(6, '0');
}

function responseFailure(payload) {
  const error = payload && payload.error;
  const code = typeof error === 'object' && error ? error.code : error;
  if (['account_deactivated', 'account_disabled', 'account_suspended', 'account_banned', 'user_deactivated'].includes(code)) return 'account_banned';
  if (['invalid_password', 'invalid_credentials', 'invalid_otp', 'invalid_totp'].includes(code)) return 'invalid_credentials';
  return '';
}

function pageFailure(text) {
  if (/your account (?:has been|is) (?:deactivated|disabled|suspended|banned)|账号已被(?:封禁|停用)|帳號已被(?:停用|封鎖)/i.test(text)) return 'account_banned';
  if (/incorrect (?:email|password|code)|wrong password|invalid (?:verification|authentication) code|密码错误|验证码错误/i.test(text)) return 'invalid_credentials';
  if (/verify (?:that )?you are human|check your inbox|code (?:we|was) sent to your email|验证您是人类|检查.{0,5}邮箱/i.test(text)) return 'manual_required';
  return '';
}

// Recognize interstitials separately from account and workspace failures.
function isCloudflareChallenge(text) {
  return /performing security verification|enable javascript and cookies to continue|just a moment|verify you are human|checking your browser|needs to review the security of your connection/i.test(text);
}

// parseProxy converts a proxy URL (optionally with credentials) into the shape
// Playwright expects. Chromium cannot authenticate over SOCKS, so callers must
// supply an HTTP proxy for credentialed endpoints.
function parseProxy(raw) {
  const value = String(raw || '').trim();
  if (!value) return null;
  let parsed;
  try {
    parsed = new URL(value);
    if (!['http:', 'https:', 'socks5:'].includes(parsed.protocol) || !parsed.hostname || parsed.pathname !== '/' || parsed.search || parsed.hash) {
      throw new Error('unsupported proxy');
    }
    if (parsed.protocol === 'socks5:' && (parsed.username || parsed.password)) throw new Error('SOCKS authentication unsupported');
    const proxy = { server: `${parsed.protocol}//${parsed.host}` };
    if (parsed.username) proxy.username = decodeURIComponent(parsed.username);
    if (parsed.password) proxy.password = decodeURIComponent(parsed.password);
    return proxy;
  } catch (_) {
    throw new LoginFailure('proxy_invalid');
  }
}

function report(payload, step, percent) {
  process.stdout.write(JSON.stringify({ type: 'progress', step, percent }) + '\n');
}

// Default desktop Chrome identity used for the login page. Kept in sync with the
// bundled Chromium major version so the UA does not contradict the engine.
const DESKTOP_USER_AGENT = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36';

function personalWorkspace(name, kind) {
  return /personal|个人|個人/i.test(`${name} ${kind}`) || kind === 'personal';
}

function chooseWorkspace(options, expectedID, expectedName) {
  if (expectedID || expectedName) {
    const exact = options.find(option => expectedID && option.id === expectedID);
    if (exact) return exact;
    const named = options.filter(option => expectedName && option.name === expectedName);
    if (named.length === 1) return named[0];
    throw new LoginFailure('workspace_mismatch');
  }
  return options.find(option => !personalWorkspace(option.name, option.kind)) || options[0];
}

async function workspaceCandidates(page) {
  return page.evaluate(() => {
    const visible = node => !!(node.getBoundingClientRect().width && node.getBoundingClientRect().height);
    const heading = [...document.querySelectorAll('h1,h2,legend')].map(node => node.textContent).join(' ');
    const chooser = /(?:choose|select|switch).{0,40}(?:workspace|organization)|选择.{0,12}(?:工作空间|团队|组织)/i.test(heading);
    const selectors = 'select[name*="workspace"],select[name*="account"],select[name*="organization"],[data-workspace-id],[data-account-id],input[type="radio"],[role="radio"],[role="option"]' + (chooser ? ',button' : '');
    const options = [];
    for (const [index, node] of [...document.querySelectorAll(selectors)].entries()) {
      if (!visible(node) || node.disabled) continue;
      if (node.tagName === 'INPUT' && !chooser && !/workspace|account|organization/i.test(node.name)) continue;
      node.setAttribute('data-cpa-oauth-choice', String(index));
      if (node.tagName === 'SELECT') {
        for (const option of node.options) {
          if (!option.value || option.disabled) continue;
          options.push({ index, tag: 'SELECT', id: option.value, name: option.text.trim(), kind: option.dataset.workspaceType || '' });
        }
        continue;
      }
      const name = ((node.labels && node.labels[0] && node.labels[0].textContent) || node.innerText || node.getAttribute('aria-label') || '').replace(/\s+/g, ' ').trim();
      if (!name || /^(continue|next|back|cancel|log out|sign out|allow|authorize|继续|返回|取消)$/i.test(name)) continue;
      const id = node.getAttribute('data-workspace-id') || node.getAttribute('data-account-id') || node.value || '';
      options.push({ index, tag: node.tagName, id, name, kind: node.getAttribute('data-workspace-type') || node.getAttribute('data-kind') || '' });
    }
    return options;
  });
}

async function submitForm(page, field) {
  const form = field.locator('xpath=ancestor::form[1]');
  const container = await form.count() ? form : page;
  // Real sign-in forms submit through their own button; clicking it (rather
  // than pressing Enter) matches what the page expects.
  const submit = container.locator('button[type="submit"]:visible, input[type="submit"]:visible').first();
  if (await submit.count()) {
    await submit.click({ noWaitAfter: true });
    return;
  }
  const named = container.locator('button:visible').filter({ hasText: /^(continue|verify|log in|sign in|next|继续|验证|登录|下一步)$/i }).first();
  if (await named.count()) {
    await named.click({ noWaitAfter: true });
    return;
  }
  throw new LoginFailure('manual_required');
}

async function driveLogin(page, payload, advance, callback, responseError) {
  const origin = new URL(payload.auth_url).origin;
  const deadline = Date.now() + Math.min(payload.timeout_ms || 300000, 300000);
  const acted = new Set();
  let lastStage = 'waiting';
  let challengeSince = 0;
  let workspace = { id: '', name: '', team: false };
  while (Date.now() < deadline) {
    if (responseError()) throw new LoginFailure(responseError());
    if (callback()) return { callback: callback(), workspace_id: workspace.id, workspace_name: workspace.name, team_selected: workspace.team };
    const current = new URL(page.url());
    if (current.origin !== origin) throw new LoginFailure('manual_required');
    const text = await page.locator('body').innerText();
    if (isCloudflareChallenge(text)) {
      lastStage = 'challenge';
      if (!challengeSince) challengeSince = Date.now();
      advance('等待登录页安全验证', 20);
      if (Date.now() - challengeSince >= 60000) throw new LoginFailure('cloudflare_blocked');
      await page.waitForTimeout(250);
      continue;
    }
    challengeSince = 0;
    const failure = pageFailure(text);
    if (failure) throw new LoginFailure(failure);
    const fields = [
      ['email', '填写邮箱', 28, 'input[type="email"]:visible,input[name="email"]:visible,input[autocomplete="username"]:visible', payload.account.email],
      ['password', '填写密码', 42, 'input[type="password"]:visible,input[name="password"]:visible', payload.account.password],
      ['totp', '填写 2FA 验证码', 58, 'input[autocomplete="one-time-code"]:visible,input[name="code"]:visible,input[name="otp"]:visible,input[name="totp"]:visible', null],
    ];
    let hasField = false;
    for (const [stage, label, percent, selector, value] of fields) {
      const field = page.locator(selector).first();
      if (!await field.count() || !await field.isEditable()) continue;
      hasField = true;
      const key = current.pathname + ':' + stage;
      if (acted.has(key)) break;
      if (stage === 'totp' && !/authenticator|authentication app|two.factor|2fa|mfa|totp|身份验证|身份驗證|两步|兩步/i.test(current.pathname + ' ' + text)) throw new LoginFailure('manual_required');
      acted.add(key);
      lastStage = stage;
      advance(label, percent);
      await field.fill(stage === 'totp' ? totp(payload.account.totp_secret) : value);
      await submitForm(page, field);
      break;
    }
    if (hasField) { await page.waitForTimeout(200); continue; }

    const options = await workspaceCandidates(page);
    const workspaceKey = current.pathname + ':workspace';
    if (options.length && !acted.has(workspaceKey)) {
      const selected = chooseWorkspace(options, payload.workspace_id, payload.workspace_name);
      lastStage = 'workspace';
      advance('选择工作空间', 68);
      acted.add(workspaceKey);
      const target = page.locator('[data-cpa-oauth-choice="'+selected.index+'"]');
      workspace = { id: selected.id, name: selected.name, team: !personalWorkspace(selected.name, selected.kind) };
      if (selected.tag === 'SELECT') {
        await target.selectOption(selected.id);
        await submitForm(page, target);
      } else if (selected.tag === 'INPUT') {
        await target.check();
        await submitForm(page, target);
      } else {
        await target.click({ noWaitAfter: true });
      }
      advance('工作空间已选择，等待授权', 72);
      await page.waitForTimeout(200);
      continue;
    }
    const consentKey = current.pathname + ':consent';
    if (!options.length && /codex/i.test(text) && !acted.has(consentKey)) {
      const next = page.getByRole('button', { name: /^(continue|allow|authorize|confirm|继续|允许|授权|确认)$/i }).first();
      if (await next.isVisible()) {
        acted.add(consentKey);
        lastStage = 'consent';
        advance('确认 Codex 授权', 75);
        await next.click({ noWaitAfter: true });
        advance('等待 OAuth 回调', 76);
      }
    }
    if (lastStage === 'waiting') advance('等待登录表单', 20);
    await page.waitForTimeout(250);
  }
  throw new LoginFailure(lastStage === 'challenge' ? 'cloudflare_blocked' : 'login_timeout');
}

async function run(payload, progress = (step, percent) => report(payload, step, percent), implementation) {
  const { chromium } = implementation || loadPlaywright();
  let percent = 0;
  let previous = '';
  const advance = (step, value) => {
    percent = Math.max(percent, value);
    if (step !== previous) { progress(step, percent); previous = step; }
  };
  const runtime = await fs.mkdtemp(path.join(os.tmpdir(), 'cpa-oauth-'));
  const browserEnv = { ...process.env, HOME: runtime, XDG_CONFIG_HOME: path.join(runtime, 'config'), XDG_CACHE_HOME: path.join(runtime, 'cache'), XDG_RUNTIME_DIR: runtime };
  delete browserEnv.DBUS_SESSION_BUS_ADDRESS;
  const executablePath = process.env.CPA_CODEX_AUTO_OAUTH_CHROMIUM || undefined;
  const browserOptions = {
    headless: false,
    timeout: 60000,
    args: [
      '--headless=new',
      '--no-sandbox',
      '--disable-dev-shm-usage',
      '--disable-gpu',
      '--no-crashpad',
      '--disable-breakpad',
      '--disable-crash-reporter',
      '--disable-blink-features=AutomationControlled',
    ],
    env: browserEnv,
  };
  if (executablePath) browserOptions.executablePath = executablePath;
  let browser;
  try {
    const proxy = parseProxy(payload.proxy_url || process.env.CPA_CODEX_AUTO_OAUTH_PROXY || '');
    if (proxy) browserOptions.proxy = proxy;
    try { browser = await chromium.launch(browserOptions); }
    catch (_) { throw new LoginFailure('browser_launch_failed'); }
    advance('浏览器已启动', 12);
    // The default Linux headless User-Agent advertises "HeadlessChrome", which
    // OpenAI's bot protection rejects before rendering the login form. Present a
    // normal desktop Chrome profile so the real sign-in page loads.
    const context = await browser.newContext({
      locale: 'en-US',
      serviceWorkers: 'block',
      userAgent: process.env.CPA_CODEX_AUTO_OAUTH_USER_AGENT || DESKTOP_USER_AGENT,
      timezoneId: process.env.CPA_CODEX_AUTO_OAUTH_TIMEZONE || 'America/New_York',
      viewport: { width: 1280, height: 800 },
    });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    let callback = '';
    let responseFailureCode = '';
    let finishCallback;
    const callbackReady = new Promise(resolve => { finishCallback = resolve; });
    const expected = new URL(payload.redirect_uri);
    const state = new URL(payload.auth_url).searchParams.get('state');
    const matchesCallback = url => url.origin === expected.origin && url.pathname === expected.pathname;
    // Redirected requests emit events even when they bypass route interception.
    // Capture the authorization result before an unserved loopback navigation fails.
    context.on('request', request => {
      if (!request.isNavigationRequest()) return;
      const received = new URL(request.url());
      if (!matchesCallback(received)) return;
      if (received.searchParams.get('state') === state && received.searchParams.getAll('state').length === 1 && received.searchParams.getAll('code').length === 1 && received.searchParams.get('code') && !received.searchParams.has('error')) {
        callback = received.href;
      } else {
        responseFailureCode = 'invalid_callback';
      }
      finishCallback();
    });
    await context.route(matchesCallback, async route => {
      await route.fulfill({ contentType: 'text/html', body: '<p>OAuth callback received.</p>' });
    });
    const authOrigin = new URL(payload.auth_url).origin;
    page.on('response', async response => {
      if (responseFailureCode || new URL(response.url()).origin !== authOrigin || response.status() < 400) return;
      const headers = response.headers();
      let failure = '';
      if (/json/i.test(headers['content-type'] || '')) {
        failure = responseFailure(await response.json().catch(() => null));
      } else if (['fetch', 'xhr'].includes(response.request().resourceType())) {
        // API challenge HTML is consumed by the app, not rendered as a page.
        if (headers['cf-mitigated'] === 'challenge' || (/html/i.test(headers['content-type'] || '') && isCloudflareChallenge(await response.text().catch(() => '')))) {
          failure = 'cloudflare_blocked';
        }
      }
      if (failure) {
        responseFailureCode = failure;
        if (failure === 'cloudflare_blocked') advance('登录接口安全验证未通过', percent);
      }
    });
    advance('打开登录页面', 18);
    try {
      await Promise.race([page.goto(payload.auth_url, { waitUntil: 'domcontentloaded', timeout: 60000 }), callbackReady]);
    }
    catch (_) {
      if (!callback && !responseFailureCode) throw new LoginFailure('navigation_failed');
    }
    const result = await driveLogin(page, payload, advance, () => callback, () => responseFailureCode);
    advance('收到 OAuth 回调', 78);
    return result;
  } finally {
    try { if (browser) await browser.close(); }
    finally { await fs.rm(runtime, { recursive: true, force: true }); }
  }
}

function readPayload() {
  const rl = readline.createInterface({ input: process.stdin });
  return new Promise((resolve, reject) => {
    let settled = false;
    rl.once('line', line => {
      settled = true;
      rl.close();
      process.stdin.pause();
      // The Go caller keeps its write end open. Unref the read side without
      // destroying the pipe, so the writer cannot observe a broken pipe.
      if (typeof process.stdin.unref === 'function') process.stdin.unref();
      try {
        resolve(JSON.parse(line));
      } catch (error) {
        reject(new LoginFailure('browser_unavailable'));
      }
    });
    rl.once('close', () => {
      // rl.close() above emits 'close' synchronously; only treat an
      // unexpected close as a failure so the parsed payload is not lost.
      if (!settled) reject(new LoginFailure('browser_unavailable'));
    });
  });
}

async function main() {
  try {
    const result = await run(await readPayload());
    process.stdout.write(JSON.stringify({ type: 'result', ...result }) + '\n');
  } catch (error) {
    const code = error instanceof LoginFailure ? error.code : (error.name === 'TimeoutError' ? 'login_timeout' : 'browser_failed');
    process.stdout.write(JSON.stringify({ type: 'result', error: code }) + '\n');
    process.exitCode = 1;
  }
}

// Run by default. The Go caller invokes this file through `node -e`, where
// require.main !== module, so that check cannot be used to detect entry.
// Unit tests set CPA_CODEX_AUTO_OAUTH_LIB=1 before requiring the helpers.
if (process.env.CPA_CODEX_AUTO_OAUTH_LIB !== '1') {
  main();
}

module.exports = { parseProxy, isCloudflareChallenge, pageFailure, responseFailure, totp, personalWorkspace, chooseWorkspace, run, LoginFailure };
