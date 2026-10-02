// The whole flow in headless Chromium, against a scratch server: a mock
// Wallet Standard wallet that signs with a real ed25519 key, a fake
// Solana RPC, and a site of its own carrying the snippet.
//
//   go build -o /tmp/x/exe-stats ./cmd/exe-stats
//   E2E_BIN=/tmp/x/exe-stats E2E_DIR=/tmp/x/run SHOTS=/tmp/x/shots \
//     NODE_PATH=~/tools/playwright/node_modules node cmd/exe-stats/e2e/e2e.js
//
// E2E_DIR is the scratch server's data directory: it must not exist yet,
// so a run can never land on a real one. Ports: 17797 the server (7795 is
// the live one's: a port that already answers is refused), 17795 the
// RPC, 17796 the site (shop.example and the names under it are mapped to
// it in the browser).
const { chromium } = require('playwright');
const http = require('http'), crypto = require('crypto'), fs = require('fs'), path = require('path');
const { spawn } = require('child_process');

const BIN = process.env.E2E_BIN, DIR = process.env.E2E_DIR, SHOTS = process.env.SHOTS;
if (!BIN || !DIR) { console.error('E2E_BIN and E2E_DIR are needed'); process.exit(2); }
if (fs.existsSync(DIR)) { console.error('E2E_DIR exists: give a fresh path'); process.exit(2); }
const APP = 'http://127.0.0.1:17797';
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
    isMobile: !!opts.phone, hasTouch: !!opts.phone, reducedMotion: opts.still ? 'reduce' : 'no-preference' });
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
  try { await fetch(APP + '/robots.txt'); console.error(`a server already answers on ${APP}: not testing against it`); process.exit(2); } catch (e) {}
  fs.mkdirSync(DIR, { recursive: true });
  fs.writeFileSync(path.join(DIR, 'config.json'), JSON.stringify({ listen: '127.0.0.1:17797', data: path.join(DIR, 'data'), rpc_url: 'http://127.0.0.1:17795' }));
  await new Promise(r => rpc.listen(17795, '127.0.0.1', r));
  await new Promise(r => site.listen(17796, '127.0.0.1', r));
  const srv = spawn(BIN, ['-config', path.join(DIR, 'config.json')], { stdio: ['ignore', fs.openSync(path.join(DIR, 'server.log'), 'a'), fs.openSync(path.join(DIR, 'server.log'), 'a')] });
  let browser;
  const done = async code => { if (browser) await browser.close(); srv.kill(); rpc.close(); site.close(); process.exit(code); };
  try {
    for (let i = 0; ; i++) {
      try { await fetch(APP + '/robots.txt'); break; } catch (e) { if (i > 50) throw new Error('the server did not start'); await new Promise(r => setTimeout(r, 100)); }
    }
    browser = await chromium.launch({ args: ['--host-resolver-rules=MAP shop.example 127.0.0.1:17796,MAP *.shop.example 127.0.0.1:17796,' +
      'MAP mirror.example 127.0.0.1:17796,MAP other.example 127.0.0.1:17796'] });

    // ---- signed out, at every scale and on a phone ----
    for (const v of [{ dpr: 1 }, { dpr: 1.5 }, { dpr: 2 }, { dpr: 3, phone: true, viewport: { width: 390, height: 844 } }]) {
      const { ctx, page } = await context(browser, [], v);
      await page.goto(APP + '/');
      const b = await box(page, '#signin'), tag = v.phone ? 'phone' : 'dpr' + v.dpr;
      ok(Math.round(b.height) === 20, `${tag}: Sign in with Solana is 20px high`, b);
      ok(await noScroll(page), `${tag}: the welcome page does not scroll sideways`);
      // the hero: a bitmap a device pixel a pixel, with the day drawn on it
      const hero = await page.evaluate(() => {
        const c = document.getElementById('hero'), r = c.getBoundingClientRect();
        const d = c.getContext('2d').getImageData(0, 0, c.width, c.height).data;
        let inked = 0;
        for (let i = 0; i < d.length; i += 4) if (d[i] !== 255 || d[i + 1] !== 255 || d[i + 2] !== 255) inked++;
        return { w: c.width, h: c.height, cw: r.width, ch: r.height, inked };
      });
      ok(hero.w === Math.round(hero.cw * (v.dpr || 1)) && hero.h === Math.round(hero.ch * (v.dpr || 1)) && hero.ch === 96 && hero.inked > 0,
        `${tag}: the hero is drawn at the screen's own pixels`, hero);
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

    // ---- the narrowest phone, in Verdana's width (an iPhone's face here;
    // headless Chromium's own fallback is narrower): the tiers fit their
    // window, their units in the header and the mint shown by its ends ----
    {
      const { ctx, page } = await context(browser, [], { dpr: 2, phone: true, viewport: { width: 320, height: 640 } });
      await page.goto(APP + '/');
      await page.addStyleTag({ content: 'body, th, td, .statusbar { font-family: "DejaVu Sans" !important }' });
      const out = await page.evaluate(() => {
        const f = document.querySelector('#tiers .frame').getBoundingClientRect();
        return [...document.querySelectorAll('#tiers .frame *')].filter(e => e.getBoundingClientRect().right > f.right - 1).map(e => e.tagName + '.' + e.className);
      });
      ok(out.length === 0 && await noScroll(page), '320px: the tiers stay inside their window', out);
      ok(await page.locator('#tiers th').nth(1).innerText() === 'Hold\n$V2EX' && !(await page.locator('#tiers td').nth(1).innerText()).includes('$'),
        '320px: the token is in the header, not in every row');
      ok(/^\S{10}…\S{10}$/.test(await page.locator('#tiers code').innerText()), '320px: the mint shows its ends');
      await shot(page, 'tiers-320');
      await ctx.close();
    }

    // ---- the hero holds still for a reader who asks for less motion ----
    {
      const { ctx, page } = await context(browser, [], { still: true });
      await page.goto(APP + '/');
      const frame = () => page.evaluate(() => document.getElementById('hero').toDataURL());
      const a = await frame();
      await page.waitForTimeout(3000);
      ok(a === await frame(), 'reduced motion: the hero is one still frame');
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
    ok(signed === '' || signed.startsWith(new URL(APP).host + ' wants you to sign in with your Solana account:\n' + keys.Rich.addr), 'the signed text names the host and the address', signed);

    // ---- add a site, get the snippet ----
    await page.fill('.add .field', 'https://www.shop.example/some/page');
    await Promise.all([page.waitForNavigation(), page.click('.add .btn')]);
    ok(/\/sites\/[0-9a-f]{12}\/settings$/.test(page.url()), "Add Site leads on to the new site's settings", page.url());
    const snippet = await page.locator('#snip').inputValue();
    siteId = (snippet.match(/data-site="([0-9a-f]{12})"/) || [])[1] || '';
    ok(siteId && snippet === `<script defer data-site="${siteId}" src="${APP}/js/s.js"></script>`, 'its settings hand out its snippet', snippet);
    await page.goto(APP + '/');
    ok(await page.locator('#sites .snip, #sites .copy').count() === 0, 'the Sites window carries no snippet');
    ok((await page.locator('.site .name').innerText()) === 'shop.example', 'the domain is read out of a pasted address');
    ok((await page.locator('.site .st').innerText()).trim() === '· Counting', 'the site counts, said after its day', await page.locator('.site .st').innerText());
    ok((await page.locator('.site .tv').innerText()) === '0 views today · 0 online', "the site's day is empty yet, and nobody is on it", await page.locator('.site .tv').innerText());
    for (const sel of ['.add .field', '.add .btn', '.site .buttons .btn']) {
      const b = await box(page, sel);
      ok(Math.round(b.height) === 20, sel + ' is 20px high', b);
    }
    ok((await page.locator('#sites .statusbar').innerText()).startsWith('1 of 2 sites'), 'the status line counts the sites');
    const tz = await page.evaluate(() => document.getElementById('tz').value);
    ok(tz && tz !== '', 'the form carries the time zone', tz);

    // ---- its settings: the setup check waits for the first view ----
    const sHref = await page.locator('.site .buttons a:has-text("Settings…")').getAttribute('href');
    ok(sHref === `/sites/${siteId}/settings`, 'Settings… leads to the site\'s settings', sHref);
    // the views to come fall into the day as dots: count each one made
    await page.evaluate(() => {
      window.__dots = 0;
      new MutationObserver(ms => ms.forEach(m => m.addedNodes.forEach(n => { if (n.className === 'dot') window.__dots++; })))
        .observe(document.getElementById('sites'), { subtree: true, childList: true });
    });
    const set = await rich.ctx.newPage();
    set.errors = [];
    set.on('pageerror', e => set.errors.push(String(e)));
    await set.goto(APP + sHref);
    ok(await set.locator('#setup.ok').count() === 0, 'before any view the check is not green');
    ok((await set.locator('#setup-line').innerText()).startsWith('Waiting for the first page view.'), 'and the line says it waits', await set.locator('#setup-line').innerText());
    ok(await set.locator('#snip').inputValue() === snippet, 'the settings carry the same snippet');
    const tickWait = await box(set, '#setup .tick'), snipWait = await box(set, '#snip');
    ok(tickWait.width === 16 && tickWait.height === 16, 'the check mark is 16px', tickWait);
    const dim = await set.evaluate(() => getComputedStyle(document.querySelector('.tick .mark')).fill);
    ok(dim === 'rgb(204, 204, 204)' && await set.locator('.tick .shadow').first().isHidden(), 'and dim, without its shadow', dim);
    await shot(set, 'settings-wait-dpr1');

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

    // ---- the settings page, left open, turns green by itself ----
    await set.waitForSelector('#setup.ok', { timeout: 10000 }).catch(() => {});
    ok(await set.locator('#setup.ok').count() === 1, 'a view on the site turns the check green without a reload');
    ok((await set.locator('#setup-line').innerText()) === 'The snippet works: the last page view came just now.', 'and the line says the snippet works', await set.locator('#setup-line').innerText());
    const green = await set.evaluate(() => getComputedStyle(document.querySelector('.tick .mark')).fill);
    ok(green === 'rgb(31, 158, 51)' && await set.locator('.tick .shadow').first().isVisible(), 'the check is the pass green, with its shadow', green);
    const snipOk = await box(set, '#snip');
    ok(snipOk.y === snipWait.y, 'nothing under the line moved', [snipWait, snipOk]);
    ok(set.errors.length === 0, 'no script errors on the settings page', set.errors);
    await shot(set, 'settings-ok-dpr1');
    await set.close();

    // ---- the Sites window, left open, counts the views as they come ----
    const liveDay = await page.waitForFunction(() => document.querySelector('.site .tv').textContent === '3 views today · 1 online'
      && getComputedStyle(document.querySelector('.site .bars i.now'), '::after').height === '40px'
      && /^3 of /.test(document.getElementById('used').parentElement.textContent), null, { timeout: 10000 }).then(() => true).catch(() => false);
    ok(liveDay, "the open page's day, its bar and the month's count follow the views without a reload",
      await page.evaluate(() => [document.querySelector('.site .tv').textContent, document.getElementById('used').textContent]));
    await page.waitForFunction(() => !document.querySelector('.site .dot'), null, { timeout: 5000 }).catch(() => {});
    const fell = await page.evaluate(() => [window.__dots, document.querySelectorAll('.site .dot').length]);
    ok(fell[0] >= 1 && fell[0] <= 3 && fell[1] === 0, 'the views fell into the hour as dots, a dot a view, and every one has landed', fell);

    // ---- the Sites window draws the day: a bar an hour, the views in the hour still filling ----
    await page.reload();
    const day = await page.evaluate(() => {
      const bars = [...document.querySelectorAll('.site .bars i')], b = document.querySelector('.site .bars').getBoundingClientRect();
      const row = document.querySelector('.site').getBoundingClientRect(), line = document.querySelector('.site .sline').getBoundingClientRect();
      const tv = document.querySelector('.site .tv').getBoundingClientRect(), st = document.querySelector('.site .st').getBoundingClientRect();
      const after = x => getComputedStyle(x, '::after'), before = x => getComputedStyle(x, '::before');
      return { n: bars.length, now: bars.filter(x => x.className === 'now').map(x => [after(x).height, after(x).borderTopColor, after(x).backgroundColor]),
        past: bars.filter(x => !x.className).map(x => [after(x).height, after(x).borderTopColor]).slice(0, 1),
        ghost: [before(bars[0]).backgroundColor, before(bars[0]).height],
        to: bars.filter(x => x.className === 'to').length, w: b.width, rw: row.width, h: b.height, floor: getComputedStyle(document.querySelector('.site .bars')).borderBottom,
        first: bars[0].getBoundingClientRect().left - b.left, last: b.right - bars[bars.length - 1].getBoundingClientRect().right,
        sky: bars[0].getBoundingClientRect().top - b.top, slot: bars[0].getBoundingClientRect().height,
        oneLine: Math.round(line.height) === 16 && Math.round(tv.top) === Math.round(st.top) && st.right <= line.right + 0.5, tv: document.querySelector('.site .tv').textContent };
    });
    ok(day.n >= 23 && day.n <= 25 && day.now.length === 1 && day.now[0][0] === '40px' && day.now[0][1] === 'rgb(255, 102, 0)' && day.now[0][2] === 'rgb(255, 204, 153)',
      "the day is a bar an hour, this hour's the busiest and still filling, in the hero's orange", day);
    ok(day.ghost[0] === 'rgb(235, 235, 255)' && day.ghost[1] === '0px', 'yesterday stands pale behind each hour: empty, as nothing came yesterday', day.ghost);
    ok(day.tv === '3 views today · 1 online' && day.oneLine, "its views and who is on it in words, on the name's line before Counting", day);
    ok(Math.round(day.w) === Math.round(day.rw) && day.first === 0 && day.last === 0 && Math.round(day.h) === 49 && day.sky === 8 && day.slot === 40 && day.floor.startsWith('1px solid'),
      'the day is the row wide, 40px under 8px of sky and over a floor line', day);
    ok(await page.locator('.site .bars').getAttribute('href') === `/sites/${siteId}` && await page.locator('.site .bars').evaluate(e => getComputedStyle(e).cursor) === 'pointer',
      'the day is a press to the desk, and the pointer says so');
    const btnCursors = await page.locator('.site .buttons .btn').evaluateAll(bs => bs.map(b => getComputedStyle(b).cursor));
    ok(btnCursors.length === 2 && btnCursors.every(c => c === 'default'), "the row's buttons wear no hand", btnCursors);
    await Promise.all([page.waitForNavigation(), page.click('.site .bars')]);
    ok(page.url() === `${APP}/sites/${siteId}`, 'a press on the day opens the desk', page.url());
    await Promise.all([page.waitForNavigation(), page.goBack()]);

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

    // ---- hosts: a blog on a subdomain counts once it is listed ----
    const hp = await rich.ctx.newPage();
    hp.errors = [];
    hp.on('pageerror', e => hp.errors.push(String(e)));
    const blog = await context(browser, []), said = [];
    blog.page.on('response', r => { if (r.url() === APP + '/e') said.push(r.headers()['x-exe-stats'] || ''); });
    await blog.page.goto('http://blog.shop.example/post');
    await blog.page.waitForTimeout(300);
    ok(said[0] === "not this site's page", 'a subdomain not listed is not counted', said);
    await hp.goto(`${APP}/sites/${siteId}/settings`);
    const why = await hp.locator('.notice span').innerText().catch(() => '');
    ok(why.startsWith('A view came from blog.shop.example') && why.endsWith('it is not one of the hosts below.'), 'the settings say why', why);
    ok(await hp.locator('#remove-host').isDisabled(), 'Remove waits for a host to be chosen');
    const addHost = async h => {
      await hp.fill('#hosts .add .field', h);
      await Promise.all([hp.waitForNavigation(), hp.click('#hosts .add .btn')]);
      return hp.locator('#hosts-said').innerText();
    };
    ok(await addHost('blog.shop.example') === 'The host counts now.' && new URL(hp.url()).hash === '#hosts', 'Add Host lists it and comes back to the hosts', hp.url());
    ok(await hp.locator('.list .row').count() === 2, 'the list has the domain and the host');
    ok(await hp.locator('.notice').count() === 0, 'and the note about its refused view is gone');
    ok(await addHost('www.shop.example') === 'That host counts already.' && await hp.locator('#hosts-said.fault').count() === 1, 'a host that counts already is refused, and says so');
    await blog.page.goto('http://blog.shop.example/post');
    await blog.page.waitForTimeout(300);
    ok(said[said.length - 1] === 'counted', 'the listed subdomain counts', said);
    await hp.click('.list label.row:has-text("blog.shop.example")');
    ok(await hp.locator('#remove-host').isEnabled(), 'choosing a host lets Remove take it');
    const chosen = await hp.locator('.list label.row').evaluate(x => getComputedStyle(x).backgroundColor);
    ok(chosen === 'rgb(51, 51, 153)', 'the chosen row wears the selection colour', chosen);
    await shot(hp, 'hosts-chosen-dpr1');
    await Promise.all([hp.waitForNavigation(), hp.click('#remove-host')]);
    ok(await hp.locator('#hosts-said').innerText() === 'That host no longer counts.' && await hp.locator('.list .row').count() === 1, 'Remove takes it off the list');

    // ---- a host the owner cannot help: Ignore, and it is noted no more ----
    const lastSaid = async u => { await blog.page.goto(u); await blog.page.waitForTimeout(300); return said[said.length - 1]; };
    ok(await lastSaid('http://mirror.example/tea') === "not this site's page", 'a mirror is not counted', said);
    await hp.goto(`${APP}/sites/${siteId}/settings`);
    ok((await hp.locator('.notice span').innerText()).startsWith('A view came from mirror.example') && await hp.locator('.notice #ignore').isVisible(), 'its note has Ignore');
    await shot(hp, 'ignore-note-dpr1');
    await Promise.all([hp.waitForNavigation(), hp.click('#ignore')]);
    ok(new URL(hp.url()).hash === '#setup-section' && await hp.locator('#setup-said').innerText() === 'That host is ignored: its views are no longer noted here.' &&
      await hp.locator('.notice').count() === 0, 'Ignore drops the note, and says so in its place', hp.url());
    ok((await hp.locator('#ignored .list .row').allInnerTexts()).join() === 'mirror.example' && await hp.locator('#remove-ignored').isDisabled(),
      'the ignored host is listed, its Remove waiting for a choice');
    await shot(hp, 'ignored-dpr1');
    ok(await lastSaid('http://mirror.example/more') === "not this site's page", 'its next view is refused as before', said);
    await hp.goto(`${APP}/sites/${siteId}/settings`);
    ok(await hp.locator('.notice').count() === 0, 'and not noted');
    await hp.click('#ignored .list label.row');
    ok(await hp.locator('#remove-ignored').isEnabled() && await hp.locator('#remove-host').isDisabled(), "choosing it wakes the list's own Remove only");
    await Promise.all([hp.waitForNavigation(), hp.click('#remove-ignored')]);
    ok(new URL(hp.url()).hash === '#ignored' && await hp.locator('#ignored-said').innerText() === "That host's views are noted again." &&
      await hp.locator('#ignored .list').count() === 0, 'Remove takes it off the list', hp.url());
    await lastSaid('http://mirror.example/again');
    await hp.goto(`${APP}/sites/${siteId}/settings`);
    ok(await hp.locator('#ignore').count() === 1 && await hp.locator('#ignored').count() === 0, 'its views are noted again, and the empty list is gone');
    await Promise.all([hp.waitForNavigation(), hp.click('#ignore')]);
    // another host's note stays, for the checks at every scale below
    await lastSaid('http://other.example/');
    ok(await addHost('*.shop.example') === 'The host counts now.' && (await hp.locator('.list label.row .note').innerText()) === 'Every subdomain', 'a *. host counts every subdomain');
    ok(hp.errors.length === 0 && blog.page.errors.length === 0, 'no script errors with hosts', [hp.errors, blog.page.errors]);
    await blog.ctx.close();
    await hp.close();

    // ---- the badge: none until a style is chosen, then the site's own ----
    const bp = await rich.ctx.newPage();
    bp.errors = [];
    bp.on('pageerror', e => bp.errors.push(String(e)));
    bp.on('console', m => { if (m.type() === 'error') bp.errors.push(m.text()); });
    const gifURL = `${APP}/badge/${siteId}.gif`;
    ok((await bp.request.get(gifURL)).status() === 404, 'a new site shows no badge');
    await bp.goto(`${APP}/sites/${siteId}/settings`);
    ok(await bp.locator('#badge-style').inputValue() === '0' && await bp.locator('#badge-html').count() === 0, 'the badge waits for a style');
    const popup = await box(bp, '#badge .popup');
    ok(Math.round(popup.height) === 19, 'the style is a 19px pop-up', popup);
    await Promise.all([bp.waitForNavigation(), bp.selectOption('#badge-style', '1')]);
    ok(new URL(bp.url()).hash === '#badge' && await bp.locator('#badge-said').innerText() === 'The badge is on: any page may show it.', 'choosing Hit Counter puts it on, and says so', bp.url());
    ok(await bp.locator('#hosts-said').innerText() === '', 'and says it only there');
    await bp.waitForFunction(() => [...document.querySelectorAll('.badges img')].every(i => i.complete && i.naturalWidth === 88));
    ok(await bp.locator('#badge-html').inputValue() === `<a href="${APP}/"><img src="${gifURL}" width="88" height="31" alt="shop.example page views, counted by exe-stats"></a>`, "the HTML shows the site's own badge", await bp.locator('#badge-html').inputValue());
    const res = await bp.request.get(gifURL);
    ok(res.status() === 200 && res.headers()['content-type'] === 'image/gif' && res.headers()['cache-control'] === 'no-cache', 'and it is there for any page, asked again every time', res.headers());
    await bp.click('.copy[data-for="badge-html"]');
    await bp.waitForFunction(() => document.getElementById('badge-said').textContent.startsWith('The HTML'), null, { timeout: 3000 }).catch(() => {});
    const copied = await bp.locator('#badge-said').innerText();
    ok(/^The HTML is (on your clipboard|selected for you to copy)\.$/.test(copied), 'Copy HTML says what it did', copied);
    ok(bp.errors.length === 0, 'no script errors with the badge', bp.errors);
    await shot(bp, 'badge-on-dpr1');
    await Promise.all([bp.waitForNavigation(), bp.click('.crumbs a:has-text("shop.example")')]);
    ok(new URL(bp.url()).pathname === `/sites/${siteId}`, "the trail's site leads to its desk (at the range last pressed)", bp.url());
    await bp.goBack();
    await Promise.all([bp.waitForNavigation(), bp.click('.crumbs a:has-text("Sites")')]);
    ok(bp.url() === `${APP}/`, 'and Sites leads home', bp.url());
    await bp.close();

    // ---- the account at every scale and on a phone ----
    const cookies = await rich.ctx.cookies();
    ok(cookies.length === 1 && cookies[0].httpOnly && cookies[0].sameSite === 'Lax', 'the session is one HttpOnly, SameSite=Lax cookie');
    for (const v of [{ dpr: 1 }, { dpr: 1.25 }, { dpr: 1.5 }, { dpr: 2 }, { dpr: 3, phone: true, viewport: { width: 390, height: 844 } }]) {
      const c = await context(browser, ['Rich'], v);
      await c.ctx.addCookies(cookies);
      await c.page.goto(APP + '/');
      const tag = v.phone ? 'phone' : 'dpr' + v.dpr;
      ok(await c.page.locator('#account').count() === 1 && await noScroll(c.page), `${tag}: the account page does not scroll sideways`);
      const b = await box(c.page, '#account .buttons .btn');
      ok(Math.round(b.height) === 20, `${tag}: account buttons are 20px high`, b);
      const tops = await c.page.locator('.site .buttons .btn').evaluateAll(bs => bs.filter(x => x.offsetParent).map(x => Math.round(x.getBoundingClientRect().top)));
      ok(tops.length === 2 && tops.every(t => t === tops[0]), `${tag}: a site's buttons are one row`, tops);
      const bars = await c.page.evaluate(() => [...document.querySelectorAll('.site .bars i')].map(x => x.getBoundingClientRect().width));
      ok(bars.length > 0 && bars.every(w => w === bars[0]) && bars[0] >= 6 && bars[0] % 2 === 0, `${tag}: a site's bars are one width, in whole 2px steps`, bars.slice(0, 3));
      const words = await c.page.evaluate(() => { const l = document.querySelector('.site .sline').getBoundingClientRect(), t = document.querySelector('.site .tv').getBoundingClientRect(); return [t.left >= l.left - 0.5 && t.right <= l.right + 0.5, Math.round(l.height)]; });
      ok(words[0] && words[1] === (v.phone ? 36 : 16), `${tag}: the day's words fit the row's line (under the name on a phone)`, words);
      await shot(c.page, 'account-' + tag);
      await c.page.goto(`${APP}/sites/${siteId}/settings`);
      ok(await c.page.locator('#setup.ok').count() === 1 && await noScroll(c.page), `${tag}: the settings show the green check and do not scroll sideways`);
      const heights = await c.page.locator('#settings .btn:not(.tbox)').evaluateAll(bs => bs.map(x => Math.round(x.getBoundingClientRect().height)));
      ok(heights.length === 9 && heights.every(h => h === 20), `${tag}: settings buttons are 20px high`, heights);
      // the note: its words, then Ignore at the right, inside the box
      const note = await c.page.evaluate(() => {
        const n = document.querySelector('.notice.ask'), b = n.querySelector('.btn'), t = n.querySelector('span');
        const r = n.getBoundingClientRect(), br = b.getBoundingClientRect(), tr = t.getBoundingClientRect();
        return { right: r.right - br.right, top: br.top - r.top, bottom: r.bottom - br.bottom, beside: br.left >= tr.right, text: t.textContent };
      });
      ok(note.text.startsWith('A view came from other.example') && note.right > 8 && note.top >= 6 && note.bottom >= 6, `${tag}: Ignore sits inside the note`, note);
      // the trail, in the exe documentation's toolbar: 33px, the trail 8px
      // down it, and on a phone the steps nearest this page only
      const trail = await c.page.evaluate(() => {
        const s = document.querySelector('.strip.tools'), n = s.querySelector('.crumbs'), r = s.getBoundingClientRect();
        return { h: r.height, top: n.getBoundingClientRect().top - r.top,
          shown: [...n.querySelectorAll('.crumb, b')].filter(x => x.offsetParent).map(x => x.textContent),
          links: [...n.querySelectorAll('a')].map(a => a.getAttribute('href')) };
      });
      ok(Math.round(trail.h) === 33 && Math.abs(trail.top - 8) < 0.5 && trail.links.join() === `/,/sites/${siteId}` &&
        trail.shown.join(' > ') === (v.phone ? 'shop.example > Settings' : 'Sites > shop.example > Settings'), `${tag}: the trail is the docs' toolbar`, trail);
      // the enlarged badge stands on whole screen pixels, the same number
      // for each of its own
      await c.page.waitForFunction(() => document.querySelector('.badges .big').complete);
      const big = await c.page.evaluate(() => {
        const b = document.querySelector('.badges .big'), r = b.getBoundingClientRect(), d = devicePixelRatio;
        return { w: r.width * d, x: (r.left + scrollX) * d, y: (r.top + scrollY) * d, done: b.complete && b.naturalWidth === 88 };
      });
      const whole = v => Math.abs(v - Math.round(v)) < 0.02;
      ok(big.done && whole(big.w / 88) && whole(big.x) && whole(big.y), `${tag}: the enlarged badge is on whole screen pixels`, big);
      const rows = await c.page.locator('.list .row').evaluateAll(rs => rs.map(x => Math.round(x.getBoundingClientRect().height)));
      ok(rows.length === 3 && rows.every(h => h === (v.phone ? 24 : 18)), `${tag}: the hosts and ignored lists have their rows at their height`, rows);
      // the line keeps the room of its longest wording, so the check's turn
      // moves nothing — measured in DejaVu Sans, Verdana's width, which is
      // where an iPhone and Windows land in the font stack
      const room = await c.page.evaluate(lines => {
        const p = document.getElementById('setup-line'), keep = p.textContent, min = parseFloat(getComputedStyle(p).minHeight);
        p.style.minHeight = '0';
        p.style.fontFamily = '"DejaVu Sans"';
        const hs = lines.map(t => { p.textContent = t; return p.getBoundingClientRect().height; });
        p.textContent = keep; p.style.minHeight = p.style.fontFamily = '';
        return { min, hs };
      }, ['Waiting for the first page view. Paste the snippet into the site\'s head, then open a page.',
        'A view came from staging.shop.example 59 min ago and was not counted: it is not one of the hosts below.',
        'A view of shop.example was sent from translate.goog 59 min ago, and was not counted.']);
      ok(room.hs.every(h => h <= room.min), `${tag}: the line's room holds its wordings`, room);
      await shot(c.page, 'settings-' + tag);
      await c.page.locator('#ignored').evaluate(x => x.scrollIntoView({ block: 'center' }));
      await shot(c.page, 'settings-ignored-' + tag);
      await c.page.locator('#danger').evaluate(x => x.scrollIntoView({ block: 'end' }));
      await shot(c.page, 'settings-danger-' + tag);
      await c.page.locator('#badge').evaluate(x => x.scrollIntoView({ block: 'end' }));
      await shot(c.page, 'settings-badge-' + tag);
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

    // ---- remove: from the foot of the site's settings, asked first, then gone ----
    ok(await page.locator('.site .btn:has-text("Remove")').count() === 0, 'the Sites window has no Remove');
    await page.goto(`${APP}/sites/${siteId}/settings`);
    ok(await page.evaluate(() => document.querySelector('#danger') === [...document.querySelectorAll('section')].pop()), 'the Danger Zone is the last section');
    await Promise.all([page.waitForNavigation(), page.click('#remove-site')]);
    ok((await page.locator('#danger .say').innerText()).startsWith('Remove shop.example and its numbers?') && new URL(page.url()).hash === '#danger', 'Remove Site… asks first', page.url());
    await shot(page, 'danger-ask-dpr1');
    await Promise.all([page.waitForNavigation(), page.click('#danger a:has-text("Cancel")')]);
    ok(await page.locator('#danger .say').count() === 0 && await page.locator('#remove-site').count() === 1, 'Cancel keeps the site');
    await Promise.all([page.waitForNavigation(), page.click('#remove-site')]);
    await Promise.all([page.waitForNavigation(), page.click('#danger button:has-text("Remove")')]);
    ok(new URL(page.url()).pathname === '/' && await page.locator('.site').count() === 0 &&
      (await page.locator('#account .flash').innerText()) === 'The site and its numbers are removed.', 'Remove takes the site away, and says so at home', page.url());
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
