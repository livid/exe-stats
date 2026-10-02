// The whole flow in headless Chromium, against a scratch server: a mock
// Wallet Standard wallet that signs with a real ed25519 key, a fake
// Solana RPC, and a site of its own carrying the snippet.
//
//   go build -o /tmp/x/exe-stats ./cmd/exe-stats
//   E2E_BIN=/tmp/x/exe-stats E2E_DIR=/tmp/x/run SHOTS=/tmp/x/shots \
//     NODE_PATH=~/tools/playwright/node_modules node cmd/exe-stats/e2e/e2e.js
//
// E2E_DIR is the scratch server's data directory: it must not exist yet,
// so a run can never land on a real one. Ports: 7795 the server, 17795
// the RPC, 17796 the site (shop.example is mapped to it in the browser).
const { chromium } = require('playwright');
const http = require('http'), crypto = require('crypto'), fs = require('fs'), path = require('path');
const { spawn } = require('child_process');

const BIN = process.env.E2E_BIN, DIR = process.env.E2E_DIR, SHOTS = process.env.SHOTS;
if (!BIN || !DIR) { console.error('E2E_BIN and E2E_DIR are needed'); process.exit(2); }
if (fs.existsSync(DIR)) { console.error('E2E_DIR exists: give a fresh path'); process.exit(2); }
const APP = 'http://127.0.0.1:7795';
const UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36';

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';
function b58(buf) {
  let n = BigInt('0x' + Buffer.from(buf).toString('hex')), s = '';
  while (n > 0n) { s = B58[Number(n % 58n)] + s; n /= 58n; }
  for (const b of buf) { if (b === 0) s = '1' + s; else break; }
  return s;
}
function key() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
  const raw = Buffer.from(publicKey.export({ format: 'jwk' }).x, 'base64url');
  return { b64: raw.toString('base64'), addr: b58(raw), privateKey };
}
const keys = { Rich: key(), Poor: key() };
const held = { [keys.Rich.addr]: 12000n * 1000000n, [keys.Poor.addr]: 5n * 1000000n };

const rpc = http.createServer((req, res) => {
  let body = '';
  req.on('data', d => body += d).on('end', () => {
    const j = JSON.parse(body), owner = j.params[0];
    const value = held[owner] ? [{ account: { data: { parsed: { info: { tokenAmount: { amount: String(held[owner]) } } } } } }] : [];
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ jsonrpc: '2.0', id: j.id, result: { value } }));
  });
});

// the site: every path is a page that carries the snippet, with a link
// that changes the address the way a single-page site does
let siteId = '';
const site = http.createServer((req, res) => {
  res.setHeader('Content-Type', 'text/html; charset=utf-8');
  res.end(`<!doctype html><title>Shop</title><script defer data-site="${siteId}" src="${APP}/js/s.js"></script>
<h1>${req.url}</h1><a id="push" href="#" onclick="history.pushState({}, '', '/cart'); return false">cart</a> <a id="hash" href="#top">top</a>`);
});

const mock = ({ names, keys }) => {
  window.__mock = { signs: 0, decline: false };
  const make = name => {
    const k = keys[name];
    const account = { address: k.addr, publicKey: Uint8Array.from(atob(k.b64), c => c.charCodeAt(0)), chains: ['solana:mainnet'], features: ['solana:signMessage'] };
    return { version: '1.0.0', name: name + ' Wallet', chains: ['solana:mainnet'], accounts: [], icon: 'data:,',
      features: {
        'standard:connect': { version: '1.0.0', connect: async () => ({ accounts: [account] }) },
        'standard:events': { version: '1.0.0', on: () => () => {} },
        'solana:signMessage': { version: '1.0.0', signMessage: async ({ message }) => {
          if (window.__mock.decline) { const e = new Error('User rejected the request.'); e.code = 4001; throw e; }
          window.__mock.signs++;
          window.__mock.lastText = new TextDecoder().decode(message);
          let s = ''; for (const b of message) s += String.fromCharCode(b);
          const sig = await window.__sign(name, btoa(s));
          return [{ signedMessage: message, signature: Uint8Array.from(atob(sig), c => c.charCodeAt(0)) }];
        } },
      } };
  };
  const ws = names.map(make);
  window.addEventListener('wallet-standard:app-ready', e => e.detail.register(...ws));
};

let fails = 0, n = 0;
const ok = (cond, what, detail) => { n++; if (!cond) fails++; console.log((cond ? 'ok   ' : 'FAIL ') + what + (cond || detail === undefined ? '' : ' — ' + JSON.stringify(detail))); };

async function context(browser, names, opts = {}) {
  const ctx = await browser.newContext({ viewport: opts.viewport || { width: 640, height: 900 }, deviceScaleFactor: opts.dpr || 1, userAgent: UA,
    isMobile: !!opts.phone, hasTouch: !!opts.phone });
  await ctx.exposeBinding('__sign', (_, name, msgB64) => crypto.sign(null, Buffer.from(msgB64, 'base64'), keys[name].privateKey).toString('base64'));
  if (names.length) await ctx.addInitScript(mock, { names, keys: Object.fromEntries(names.map(k => [k, { b64: keys[k].b64, addr: keys[k].addr }])) });
  const page = await ctx.newPage();
  page.errors = [];
  page.on('pageerror', e => page.errors.push(String(e)));
  page.on('console', m => { if (m.type() === 'error') page.errors.push(m.text()); });
  return { ctx, page };
}
const shot = async (page, name) => { if (SHOTS) { fs.mkdirSync(SHOTS, { recursive: true }); await page.screenshot({ path: path.join(SHOTS, name + '.png') }); } };
const box = (page, sel) => page.locator(sel).first().boundingBox();
const noScroll = page => page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth);

(async () => {
  fs.mkdirSync(DIR, { recursive: true });
  fs.writeFileSync(path.join(DIR, 'config.json'), JSON.stringify({ listen: '127.0.0.1:7795', data: path.join(DIR, 'data'), rpc_url: 'http://127.0.0.1:17795' }));
  await new Promise(r => rpc.listen(17795, '127.0.0.1', r));
  await new Promise(r => site.listen(17796, '127.0.0.1', r));
  const srv = spawn(BIN, ['-config', path.join(DIR, 'config.json')], { stdio: ['ignore', fs.openSync(path.join(DIR, 'server.log'), 'a'), fs.openSync(path.join(DIR, 'server.log'), 'a')] });
  let browser;
  const done = async code => { if (browser) await browser.close(); srv.kill(); rpc.close(); site.close(); process.exit(code); };
  try {
    for (let i = 0; ; i++) {
      try { await fetch(APP + '/robots.txt'); break; } catch (e) { if (i > 50) throw new Error('the server did not start'); await new Promise(r => setTimeout(r, 100)); }
    }
    browser = await chromium.launch({ args: ['--host-resolver-rules=MAP shop.example 127.0.0.1:17796'] });

    // ---- signed out, at every scale and on a phone ----
    for (const v of [{ dpr: 1 }, { dpr: 1.5 }, { dpr: 2 }, { dpr: 3, phone: true, viewport: { width: 390, height: 844 } }]) {
      const { ctx, page } = await context(browser, [], v);
      await page.goto(APP + '/');
      const b = await box(page, '#signin'), tag = v.phone ? 'phone' : 'dpr' + v.dpr;
      ok(Math.round(b.height) === 20, `${tag}: Sign in with Solana is 20px high`, b);
      ok(await noScroll(page), `${tag}: the welcome page does not scroll sideways`);
      await shot(page, 'welcome-' + tag);
      if (v.dpr === 1) {
        await page.click('#signin');
        await page.waitForFunction(() => document.getElementById('say').textContent.startsWith('No Solana wallet'));
        const b2 = await box(page, '#signin');
        ok(b2.y === b.y && b2.x === b.x, 'no wallet: the line says so and the button has not moved', [b, b2]);
      }
      ok(page.errors.length === 0, `${tag}: no script errors`, page.errors);
      await ctx.close();
    }

    // ---- a declined signature, then a sign-in ----
    const rich = await context(browser, ['Rich']);
    let page = rich.page;
    await page.goto(APP + '/');
    await page.evaluate(() => { window.__mock.decline = true; });
    await page.click('#signin');
    await page.waitForFunction(() => /rejected/.test(document.getElementById('say').textContent));
    ok(await page.locator('#account').count() === 0, 'a declined signature signs nobody in');
    await page.evaluate(() => { window.__mock.decline = false; });
    const text = page.evaluate(() => new Promise(res => { const t = setInterval(() => { if (window.__mock.lastText) { clearInterval(t); res(window.__mock.lastText); } }, 20); }));
    await Promise.all([page.waitForNavigation(), page.click('#signin')]);
    const signed = await text.catch(() => '');
    await page.waitForSelector('#account');
    ok((await page.locator('#account dl').innerText()).includes('12,000 $V2EX'), 'signed in: the holding is read', await page.locator('#account dl').innerText());
    ok((await page.locator('#account dl').innerText()).includes('Base'), 'signed in: the tier is Base');
    ok(await page.locator('#tiers tr.on td').first().innerText() === 'Base', 'the tier in force is the marked row');
    ok(signed === '' || signed.startsWith('127.0.0.1:7795 wants you to sign in with your Solana account:\n' + keys.Rich.addr), 'the signed text names the host and the address', signed);

    // ---- add a site, get the snippet ----
    await page.fill('.add .field', 'https://www.shop.example/some/page');
    await Promise.all([page.waitForNavigation(), page.click('.add .btn')]);
    const snippet = await page.locator('.site .snip').inputValue();
    siteId = (snippet.match(/data-site="([0-9a-f]{12})"/) || [])[1] || '';
    ok(siteId && snippet === `<script defer data-site="${siteId}" src="${APP}/js/s.js"></script>`, 'the site has its snippet', snippet);
    ok((await page.locator('.site .name').innerText()) === 'shop.example', 'the domain is read out of a pasted address');
    ok((await page.locator('.site .st').innerText()) === 'Counting', 'the site counts');
    for (const sel of ['.site .snip', '.add .field', '.add .btn', '.site .buttons .btn']) {
      const b = await box(page, sel);
      ok(Math.round(b.height) === 20, sel + ' is 20px high', b);
    }
    ok((await page.locator('#sites .statusbar').innerText()).startsWith('1 of 2 sites'), 'the status line counts the sites');
    const tz = await page.evaluate(() => document.getElementById('tz').value);
    ok(tz && tz !== '', 'the form carries the time zone', tz);

    // ---- the snippet on the site: a load, a pushState, a hash, another load ----
    const visitor = await context(browser, []);
    const beacons = [];
    visitor.page.on('response', r => { if (r.url() === APP + '/e') beacons.push(r.status() + ' ' + (r.headers()['x-exe-stats'] || '')); });
    await visitor.page.goto('http://shop.example/tea?utm_source=newsletter', { referer: 'https://www.google.com/' });
    await visitor.page.waitForTimeout(300);
    await visitor.page.click('#push');
    await visitor.page.waitForTimeout(300);
    await visitor.page.click('#hash');
    await visitor.page.waitForTimeout(300);
    await visitor.page.goto('http://shop.example/about');
    await visitor.page.waitForTimeout(1500); // the site's writer lands its batch once a second
    ok(visitor.page.errors.length === 0, 'the snippet throws nothing on the site', visitor.page.errors);
    await visitor.ctx.close();

    // ---- the desk ----
    await Promise.all([page.waitForNavigation(), page.click('.site .name')]);
    ok(page.url() === `${APP}/sites/${siteId}`, 'the name opens the desk', page.url());
    const desk = await page.locator('body').innerText();
    const tile = await page.locator('.tile', { hasText: 'Page views' }).locator('.tv').innerText().catch(() => '');
    ok(tile.trim() === '3', 'three page views: two loads and a pushState, the hash is none', { tile, beacons });
    ok(desk.includes('/tea') && desk.includes('/cart') && desk.includes('/about'), 'the pages are listed');
    ok(desk.includes('Google'), 'the source is where the reader came from');
    const range = page.locator('.ranges a', { hasText: /^24h$|^Today$|^30d$/ }).first();
    const href = await range.getAttribute('href');
    ok(href && href.startsWith(`/sites/${siteId}?range=`), 'the desk writes its links at its own path', href);
    await range.click();
    await page.waitForFunction(h => location.pathname + location.search === h, href);
    ok(await page.locator('.sdesk').count() === 1, 'a range pressed is fetched in place');
    await shot(page, 'desk-dpr1');
    const json = await page.evaluate(async id => (await fetch(`/v1/sites/${id}/stats`)).json(), siteId);
    ok(json.summary && json.summary.pageviews === 3 && json.summary.visitors === 1, 'the JSON says the same', json.summary);

    // ---- the account at every scale and on a phone ----
    const cookies = await rich.ctx.cookies();
    ok(cookies.length === 1 && cookies[0].httpOnly && cookies[0].sameSite === 'Lax', 'the session is one HttpOnly, SameSite=Lax cookie');
    for (const v of [{ dpr: 1 }, { dpr: 1.5 }, { dpr: 2 }, { dpr: 3, phone: true, viewport: { width: 390, height: 844 } }]) {
      const c = await context(browser, ['Rich'], v);
      await c.ctx.addCookies(cookies);
      await c.page.goto(APP + '/');
      const tag = v.phone ? 'phone' : 'dpr' + v.dpr;
      ok(await c.page.locator('#account').count() === 1 && await noScroll(c.page), `${tag}: the account page does not scroll sideways`);
      const b = await box(c.page, '#account .buttons .btn');
      ok(Math.round(b.height) === 20, `${tag}: account buttons are 20px high`, b);
      await shot(c.page, 'account-' + tag);
      await c.ctx.close();
    }

    // ---- a stranger: no tier, no adding, and not this desk ----
    const poor = await context(browser, ['Poor']);
    await poor.page.goto(APP + '/');
    await Promise.all([poor.page.waitForNavigation(), poor.page.click('#signin')]);
    ok((await poor.page.locator('#account dl').innerText()).includes('None'), 'five tokens are no tier');
    ok(await poor.page.locator('.add .field').isDisabled() && await poor.page.locator('.add .btn').isDisabled(), 'no tier: Add Site is dimmed');
    const r404 = await poor.page.goto(`${APP}/sites/${siteId}`);
    ok(r404.status() === 404, "another account's desk is not found", r404.status());
    await poor.ctx.close();

    // ---- short: the balance falls, Check Now says so ----
    await page.goto(APP + '/');
    held[keys.Rich.addr] = 500n * 1000000n;
    await Promise.all([page.waitForNavigation(), page.click('form[action="/v1/check"] .btn')]);
    const notice = await page.locator('.notice').first().innerText();
    ok(notice.startsWith('The balance is 9,500 $V2EX under the Base tier. Everything counts until') && notice.includes('then counting stops'), 'short: the page says what is missing and until when', notice);
    ok(await page.locator('.add .btn').isDisabled(), 'short: no new sites');
    ok(await noScroll(page), 'short: the page does not scroll sideways');
    await shot(page, 'short-dpr1');

    // ---- remove: asked first, then gone ----
    await page.click('.site .buttons .btn:has-text("Remove…")');
    await page.waitForSelector('.site .say');
    ok((await page.locator('.site .say').innerText()).startsWith('Remove shop.example and its numbers?'), 'Remove… asks first');
    await page.click('.site .buttons a:has-text("Cancel")');
    ok(await page.locator('.site').count() === 1, 'Cancel keeps the site');
    await page.click('.site .buttons .btn:has-text("Remove…")');
    await Promise.all([page.waitForNavigation(), page.click('.site .buttons button:has-text("Remove")')]);
    ok(await page.locator('.site').count() === 0, 'Remove takes the site away');
    ok(!fs.existsSync(path.join(DIR, 'data', 'sites', siteId + '.db')), "and its file");

    // ---- sign out ----
    await Promise.all([page.waitForNavigation(), page.click('form[action="/v1/signout"] .btn')]);
    ok(await page.locator('#signin').count() === 1, 'Sign Out leads back to the welcome');
    ok(page.errors.length === 0, 'no script errors on the account pages', page.errors);
    await rich.ctx.close();

    // ---- two wallets: the reader chooses ----
    const two = await context(browser, ['Rich', 'Poor']);
    await two.page.goto(APP + '/');
    const before = await box(two.page, '#signin');
    await two.page.click('#signin');
    await two.page.waitForSelector('#say .btn');
    const after = await box(two.page, '#signin');
    ok(await two.page.locator('#say .btn').count() === 2 && after.y === before.y, 'two wallets: two buttons, and Sign in has not moved', [before, after]);
    ok(await two.page.evaluate(() => window.__mock.signs) === 0, 'and nothing is signed before a wallet is chosen');
    await shot(two.page, 'choose-dpr1');
    await Promise.all([two.page.waitForNavigation(), two.page.click('#say .btn:has-text("Poor Wallet")')]);
    ok((await two.page.locator('#account code').getAttribute('title')) === keys.Poor.addr, 'the chosen wallet is the one signed in');
    await two.ctx.close();

    console.log(`${n - fails}/${n} ok`);
    await done(fails ? 1 : 0);
  } catch (e) {
    console.error(e);
    await done(1);
  }
})();
