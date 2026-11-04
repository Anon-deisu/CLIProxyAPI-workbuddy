'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

// The runner auto-executes unless this flag is set, and it reads stdin when it
// runs, so mark it as a library import before requiring it.
process.env.CPA_CODEX_AUTO_OAUTH_LIB = '1';
const { totp, personalWorkspace, parseProxy, isCloudflareChallenge, responseFailure, chooseWorkspace } = require('./auto_oauth_runner.cjs');

test('TOTP matches the RFC 6238 SHA-1 vector', () => {
  assert.equal(totp('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', 59000), '287082');
});

test('personal workspace is detected by label or kind', () => {
  assert.equal(personalWorkspace('Personal', ''), true);
  assert.equal(personalWorkspace('Example Team', 'team'), false);
  assert.equal(personalWorkspace('', 'personal'), true);
});

test('proxy URL is split into server and credentials', () => {
  const proxy = parseProxy('http://user%40name:pa%3Ass@proxy.example.test:3010');
  assert.equal(proxy.server, 'http://proxy.example.test:3010');
  assert.equal(proxy.username, 'user@name');
  assert.equal(proxy.password, 'pa:ss');
  assert.equal(parseProxy(''), null);
  assert.throws(() => parseProxy('not a url'), /proxy_invalid/);
});

test('cloudflare interstitial is recognised', () => {
  assert.equal(isCloudflareChallenge('Performing security verification'), true);
  assert.equal(isCloudflareChallenge('Enable JavaScript and cookies to continue'), true);
  assert.equal(isCloudflareChallenge('Sign in to your account'), false);
});

test('generic authorization failures do not imply an account ban', () => {
  assert.equal(responseFailure({ error: { code: 'unauthorized' } }), '');
  assert.equal(responseFailure({ error: { code: 'account_deactivated' } }), 'account_banned');
});

test('workspace selection prefers a team but retains an exact recovery target', () => {
  const personal = { id: 'personal', name: 'Personal', kind: 'personal' };
  const team = { id: 'team-a', name: 'Example Team', kind: 'team' };
  assert.equal(chooseWorkspace([personal, team], '', ''), team);
  assert.equal(chooseWorkspace([personal, team], 'personal', ''), personal);
  assert.equal(chooseWorkspace([personal], '', ''), personal);
  assert.throws(() => chooseWorkspace([personal, team], 'missing', 'Gone Team'), /workspace_mismatch/);
});

test('unsupported proxy protocols and authenticated SOCKS fail without leaking credentials', () => {
  for (const proxy of ['ftp://secret:password@example.test', 'socks5://secret:password@example.test:1080']) {
    assert.throws(() => parseProxy(proxy), error => error.code === 'proxy_invalid' && !error.message.includes('password'));
  }
});
