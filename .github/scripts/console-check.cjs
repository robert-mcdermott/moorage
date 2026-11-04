// What console-check.sh runs: the console, driven as a person would, in one browser.
const { chromium, firefox, webkit } = require('playwright');
const { spawn } = require('node:child_process');

const browserName = process.env.BROWSER || 'chromium';
const base = process.env.CONSOLE_URL;
const passphrase = 'a long and unusual passphrase for the check';

let failures = 0;
function check(what, ok, detail) {
  console.log(`${ok ? 'ok   ' : 'FAIL '} ${browserName}: ${what}${!ok && detail ? ` (${detail})` : ''}`);
  if (!ok) { failures++; }
}

function moor(...args) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.env.CONSOLE_MOOR, [...args, '--server', process.env.CONSOLE_SERVER], { cwd: process.env.CONSOLE_APP_DIR, stdio: 'ignore' });
    child.on('exit', code => (code === 0 ? resolve() : reject(new Error(`moor ${args.join(' ')}: ${code}`))));
  });
}

(async () => {
  const types = { chromium, firefox, webkit };
  const browser = await types[browserName].launch();
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await context.newPage();

  // Anything a page complains of: above all what the Content-Security-Policy refuses.
  const problems = [];
  // Firefox says so when a page that is going away lets go of a stream: it is meant to.
  const letGo = text => /\/api\/v1\//.test(text) && /can.t establish a connection|was interrupted while the page was loading/.test(text);
  page.on('console', m => { if (m.type() === 'error' && !letGo(m.text())) { problems.push(m.text()); } });
  page.on('pageerror', e => problems.push(String(e)));
  const quiet = what => {
    check(`${what}: nothing went wrong in the page`, problems.length === 0, problems.join(' | '));
    problems.length = 0;
  };

  // Claiming the server.
  await page.goto(`${base}/setup?code=${process.env.CONSOLE_CLAIM}`);
  await page.fill('input[name=email]', 'owner@example.com');
  await page.fill('input[name=password]', passphrase);
  await page.fill('input[name=confirm]', passphrase);
  await page.click('button[type=submit]');
  await page.waitForURL('**/welcome');
  check('claiming the server goes to getting started', true);

  // Every page, wide and narrow, with nothing wider than the screen.
  const pages = ['/', '/activity', '/welcome', '/new', '/apps/shop', '/apps/shop/deployments', '/apps/shop/logs', '/apps/shop/variables',
    '/apps/shop/variables/edit', '/apps/shop/deploy', '/account', '/account/keys', '/account/tokens', '/account/sessions',
    '/settings', '/settings/users', '/settings/registries', '/settings/system', '/settings/audit', '/projects', '/projects/default'];
  for (const width of [1280, 375]) {
    await page.setViewportSize({ width, height: 800 });
    for (const path of pages) {
      const res = await page.goto(base + path);
      check(`${path} at ${width}px answers`, res.status() === 200, String(res.status()));
      const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      check(`${path} at ${width}px fits`, over <= 1, `${over}px too wide`);
    }
    quiet(`the pages at ${width}px`);
  }
  await page.setViewportSize({ width: 1280, height: 800 });

  // The palette, from the keyboard.
  await page.goto(base + '/');
  await page.keyboard.press('/');
  await page.waitForSelector('#palette[open]');
  await page.keyboard.type('logs shop');
  await page.keyboard.press('Enter');
  await page.waitForURL('**/apps/shop/logs');
  check('the palette goes where it is asked', true);

  // The theme stays chosen.
  await page.click('[data-theme-set=dark]');
  await page.reload();
  check('the theme stays', await page.evaluate(() => document.documentElement.getAttribute('data-theme')) === 'dark');
  await page.click('[data-theme-set=system]');

  // The logs page follows, pauses and filters.
  await page.goto(base + '/apps/shop/logs');
  await page.waitForSelector('[data-log-status]:not([hidden])');
  const lines = () => page.locator('[data-logs] .log-line').count();
  const before = await lines();
  await page.waitForFunction(n => document.querySelectorAll('[data-logs] .log-line').length > n, before, { timeout: 15000 });
  check('new lines arrive by themselves', true);
  check('a JSON line reads as its message', (await page.textContent('[data-logs]')).includes('processed a job  job='));
  check('an error stands out', await page.locator('[data-logs] .level-error').count() > 0);
  await page.click('[data-log-pause]');
  const held = await lines();
  await page.waitForFunction(() => /new\)/.test(document.querySelector('[data-log-pause]').textContent), null, { timeout: 15000 });
  check('pausing holds the lines', await lines() === held);
  await page.click('[data-log-pause]');
  await page.waitForFunction(n => document.querySelectorAll('[data-logs] .log-line').length > n, held, { timeout: 5000 });
  check('resuming brings what came', true);
  await page.fill('[data-log-search]', 'queue');
  await page.keyboard.press('Enter');
  await page.waitForURL('**/logs?*q=queue*');
  check('a search marks what it found', await page.locator('[data-logs] mark').count() > 0);
  await page.check('input[name=problems]');
  await page.waitForURL('**problems=1**');
  check('a filter applies by itself', true);
  quiet('the logs page');

  // A deployment, from the apps list that sees it start to its page as it goes.
  await page.goto(base + '/');
  await page.waitForTimeout(500); // the page's stream opens
  const badge = page.locator('[data-app-row=shop] .badge').first();
  check('the app is healthy', (await badge.textContent()).trim() === 'Healthy', await badge.textContent());
  const marker = await page.evaluate(() => { window.__same = true; return true; });
  const deploy = moor('deploy', '--detach');
  await page.waitForFunction(() => document.querySelector('[data-app-row=shop] .badge').textContent.trim() === 'Deploying', null, { timeout: 15000 });
  check('the list sees a deployment start, without reloading', marker && await page.evaluate(() => window.__same === true));
  await deploy;
  await page.waitForFunction(() => document.querySelector('[data-app-row=shop] .badge').textContent.trim() === 'Healthy', null, { timeout: 60000 });
  check('and sees it end', true);
  quiet('the apps list');

  await page.goto(base + '/apps/shop');
  await page.click('button:has-text("Redeploy")');
  await page.waitForURL('**/deployments/dep_*');
  await page.waitForFunction(() => document.querySelector('[data-log]') && document.querySelector('[data-log]').textContent.includes('✔ live'), null, { timeout: 60000 });
  check('a deployment\'s log arrives as it is written', true);
  await page.waitForFunction(() => document.querySelector('.stage.rollout, .stages') && document.querySelectorAll('.stage.done').length >= 3, null, { timeout: 15000 });
  check('its stages move by themselves', true);
  quiet('the deployment page');

  // What asks first asks, and takes no for an answer.
  await page.goto(base + '/apps/shop');
  const rollback = page.locator('button:has-text("Roll back")').first();
  await rollback.click();
  await page.waitForSelector('#confirm[open]');
  await page.click('[data-confirm-no]');
  check('no means no', !page.url().includes('/deployments/') && await page.locator('#confirm[open]').count() === 0);
  await rollback.click();
  await page.click('[data-confirm-yes]');
  await page.waitForURL('**/deployments/dep_*', { timeout: 60000 });
  check('yes rolls back', (await page.textContent('[data-log]')).includes('✔ live'));
  quiet('rolling back');

  // Scaling, from the page, with the instances to show for it.
  await page.goto(base + '/apps/shop');
  await page.fill('input[name="process.web"]', '2');
  await page.click('button:has-text("Scale")');
  await page.waitForURL('**/apps/shop**', { timeout: 60000 });
  await page.waitForSelector('text=web.2', { timeout: 15000 });
  check('scaling brings an instance', true);
  await page.waitForFunction(() => !location.search.includes('done='), null, { timeout: 5000 });
  check('what was done is said once', true);
  quiet('scaling');

  // Variables: added, then edited as text with a look at what would change.
  await page.goto(base + '/apps/shop/variables');
  await page.click('summary:has-text("Add a variable")');
  await page.fill('#var-name', 'API_URL');
  await page.fill('#var-value', 'https://example.com');
  await page.click('.add-var button[type=submit]');
  await page.waitForURL('**/variables?*', { timeout: 60000 });
  check('a variable is released', (await page.textContent('main')).includes('API_URL'));
  await page.goto(base + '/apps/shop/variables/edit');
  await page.fill('#vars-text', 'API_URL=https://new.example.com\nADDED=1\n');
  await page.click('button:has-text("See what changes")');
  await page.waitForURL('**/variables/preview');
  const preview = await page.textContent('main');
  check('the text says what it would change', preview.includes('changed') && preview.includes('added'));
  await page.click('button:has-text("Apply and release")');
  await page.waitForURL('**/variables?*', { timeout: 60000 });
  check('and does', (await page.textContent('main')).includes('ADDED'));
  quiet('the variables');

  // A token, shown once, with a button to copy it.
  await page.goto(base + '/account/tokens');
  await page.fill('#token-name', 'ci');
  await page.click('button:has-text("Make the token")');
  await page.waitForSelector('.secret-once');
  check('a token is shown once', (await page.textContent('.secret-once')).includes('moor_'));
  quiet('the tokens');

  // Signing out.
  await page.goto(base + '/account');
  await page.click('#signout button');
  await page.waitForURL(url => !url.pathname.startsWith('/account'));
  const res = await page.goto(base + '/apps/shop');
  check('signing out ends it', page.url().includes('/login') && res.status() === 200);
  quiet('signing out');

  await browser.close();
  if (failures > 0) {
    console.log(`${failures} failed`);
    process.exit(1);
  }
  console.log(`every check passed in ${browserName}`);
})().catch(err => {
  console.error(err);
  process.exit(1);
});
