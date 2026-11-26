const assert = require('node:assert/strict');
const { randomBytes } = require('node:crypto');
const { readFile } = require('node:fs/promises');
const { performance } = require('node:perf_hooks');

const timeout = 60_000;
const suffix = randomBytes(5).toString('hex');
const filename = `kamui-browser-${suffix}.txt`;
const value = `kamui-saved-${suffix}`;
let password;

async function command(page, label) {
  await page.keyboard.press('F1');
  const input = page.locator('.quick-input-widget input');
  await input.fill('>' + label);
  await page.getByRole('option').getByText(label, { exact: true }).first().click();
  await input.waitFor({ state: 'hidden' });
}

async function openFile(page, name) {
  const row = page.locator('.explorer-viewlet .monaco-list-row').filter({ hasText: name });
  const explorer = page.getByRole('tab', { name: /Explorer/ }).first();
  if (!(await row.isVisible()) && await explorer.isVisible() &&
      await explorer.getAttribute('aria-selected') !== 'true') {
    await explorer.click();
  }
  await row.dblclick();
  await page.locator('.part.editor .tabs-container').getByText(name, { exact: true }).waitFor();
}

async function openIDE(context, dashboard) {
  const link = dashboard.getByRole('link', { name: 'Open IDE' });
  await link.waitFor({ timeout });
  const path = await link.getAttribute('href');
  assert.match(path, /^\/workspace\/ws-[a-f0-9]{8,32}\/$/);
  const page = await context.newPage();
  const devtools = await context.newCDPSession(page);
  await devtools.send('Network.enable');
  const sockets = new Set();
  const upgrades = [];
  devtools.on('Network.webSocketCreated', event => {
    if (new URL(event.url).pathname.startsWith(path)) sockets.add(event.requestId);
  });
  devtools.on('Network.webSocketHandshakeResponseReceived', event => {
    if (sockets.has(event.requestId) && event.response.status === 101) upgrades.push(event.requestId);
  });
  await page.goto(new URL(path, dashboard.url()).href);
  await page.locator('.monaco-workbench').waitFor({ timeout });
  await page.waitForFunction(() => !!document.querySelector('.activitybar'));
  await waitUntil(() => upgrades.length > 0, 'No code-server WebSocket handshake returned 101');
  const trust = page.getByRole('button', { name: /Yes, I trust the authors/ });
  if (await trust.isVisible()) await trust.click();
  return { page, upgrades };
}

async function waitUntil(check, failure, waitMs = 15_000) {
  const deadline = performance.now() + waitMs;
  while (performance.now() < deadline) {
    if (await check()) return;
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error(failure);
}

async function terminal(page, cmd) {
  const helper = page.locator('.terminal-wrapper.active .xterm-helper-textarea').last();
  await helper.waitFor();
  await helper.focus();
  await page.keyboard.type(cmd);
  await page.keyboard.press('Enter');
}

async function assertTerminalOutput(page, expected) {
  // VS Code's accessible view reads the terminal buffer, including command output.
  const terminalInput = page.locator('.terminal-wrapper.active .xterm-helper-textarea').last();
  await terminalInput.focus();
  const linuxClient = await page.evaluate(() => navigator.platform.includes('Linux'));
  const shortcut = linuxClient ? 'Shift+Alt+F2' : 'Alt+F2';
  const view = page.locator('.accessible-view');
  await waitUntil(async () => {
    await terminalInput.focus();
    await page.keyboard.press(shortcut);
    await view.waitFor();
    const found = (await view.textContent()).includes(expected);
    await page.keyboard.press('Escape');
    await view.waitFor({ state: 'hidden' });
    return found;
  }, 'Expected terminal output is missing');
}

async function login(context, base) {
  const dashboard = await context.newPage();
  dashboard.setDefaultTimeout(timeout);
  await dashboard.goto(new URL('/login', base).href);
  await dashboard.getByLabel('Admin Password').fill(password);
  const started = performance.now();
  await dashboard.getByRole('button', { name: 'Sign In' }).click();
  await dashboard.getByRole('heading', { name: 'Workspaces', exact: true }).waitFor();
  return { dashboard, elapsedMs: Math.round(performance.now() - started) };
}

async function main() {
  const base = new URL(process.env.KAMUI_TEST_URL || 'https://127.0.0.1:18081');
  assert.ok(base.protocol === 'https:' && ['127.0.0.1', 'localhost', '[::1]'].includes(base.hostname),
    'The test requires a loopback HTTPS URL');
  if (!process.env.KAMUI_TEST_ENV) throw new Error('Set KAMUI_TEST_ENV to the isolated fixture env file');
  const env = await readFile(process.env.KAMUI_TEST_ENV, 'utf8');
  password = /^KAMUI_ADMIN_PASSWORD=(\S+)\s*$/m.exec(env)?.[1];
  assert.ok(password && password.length >= 24, 'The fixture must have a strong admin password');
  const workspaceName = process.env.KAMUI_TEST_WORKSPACE_NAME || process.env.KAMUI_TEST_CREATE_NAME || `Browser fixture ${suffix}`;
  const { chromium } = require(process.env.KAMUI_TEST_PLAYWRIGHT || 'playwright');
  const browser = await chromium.launch(process.env.KAMUI_TEST_BROWSER ? { executablePath: process.env.KAMUI_TEST_BROWSER } : {});
  let activePage;
  const pageErrors = [];
  try {
    const desktop = await browser.newContext({ viewport: { width: 1366, height: 768 }, ignoreHTTPSErrors: true });
    const { dashboard, elapsedMs: desktopLoginMs } = await login(desktop, base);
    if (!process.env.KAMUI_TEST_WORKSPACE_NAME) {
      await dashboard.getByRole('button', { name: /Create Workspace|New Workspace/ }).first().click();
      await dashboard.getByLabel('Workspace Name').fill(workspaceName);
      await dashboard.getByLabel('Repository URL').fill(process.env.KAMUI_TEST_REPO || 'https://github.com/octocat/Hello-World');
      await dashboard.getByRole('button', { name: 'Create Workspace' }).click();
    }
    const card = dashboard.locator('div').filter({
      has: dashboard.getByRole('heading', { name: workspaceName, exact: true }),
      hasText: /Last active/,
    }).filter({ has: dashboard.getByRole('button', { name: 'Remove workspace' }) }).last();
    await card.waitFor();
    const started = performance.now();
    if (await card.getByRole('button', { name: 'Start', exact: true }).isVisible()) {
      await card.getByRole('button', { name: 'Start', exact: true }).click();
    }
    await card.getByRole('link', { name: 'Open IDE' }).waitFor({ timeout });
    const workspaceReadyMs = Math.round(performance.now() - started);
    const { page: ide, upgrades } = await openIDE(desktop, dashboard);
    activePage = ide;
    ide.on('pageerror', error => pageErrors.push(error.message));
    ide.setDefaultTimeout(15_000);
    const primary = await ide.evaluate(() => navigator.platform.includes('Mac') ? 'Meta' : 'Control');
    const explorerToggle = ide.getByRole('tab', { name: /Explorer/ }).first();
    if (await explorerToggle.isVisible() && await explorerToggle.getAttribute('aria-selected') !== 'true') {
      await explorerToggle.click();
    }
    await ide.locator('.explorer-viewlet .monaco-list-row').first().waitFor();
    if (process.env.KAMUI_TEST_SCREENSHOT_DIR) {
      await ide.screenshot({ path: process.env.KAMUI_TEST_SCREENSHOT_DIR + '/desktop-ide.png' });
    }
    await command(ide, 'Terminal: Create New Terminal');
    const trustTerminal = ide.getByRole('button', { name: 'Trust Folder & Continue', exact: true });
    if (await trustTerminal.waitFor({ timeout: 2000 }).then(() => true, () => false)) {
      await trustTerminal.click();
    }
    await terminal(ide, `printf 'placeholder\\n' > '${filename}'`);
    await openFile(ide, filename);
    const editor = ide.locator('.part.editor .view-lines');
    await editor.getByText('placeholder', { exact: true }).click();
    await ide.keyboard.press(primary + '+a');
    await ide.keyboard.insertText(value);
    await editor.getByText(value, { exact: true }).waitFor();
    const dirtyTab = ide.locator('.part.editor .tabs-container .tab.active.dirty').filter({ hasText: filename });
    // code-server can autosave before the test observes a dirty tab. Save
    // explicitly when needed, then corroborate on-disk state in the terminal.
    if (await dirtyTab.isVisible()) {
      await command(ide, 'File: Save');
      await dirtyTab.waitFor({ state: 'detached' });
    }
    if (process.env.KAMUI_TEST_SCREENSHOT_DIR) {
      await ide.screenshot({ path: process.env.KAMUI_TEST_SCREENSHOT_DIR + '/desktop-saved.png' });
    }
    const outputPrefix = 'verified-' + suffix;
    // A split prefix prevents matching the shell's echoed input as output.
    await terminal(ide, `for i in 1 2 3 4 5; do content=$(cat '${filename}'); case "$content" in kamui-saved-*) break;; esac; sleep 1; done; printf 'verified-%s:%s\\n' '${suffix}' "$content"`);
    await assertTerminalOutput(ide, outputPrefix + ':' + value);
    const desktopUpgrades = upgrades.length;
    await ide.close();
    await card.getByRole('button', { name: 'Restart' }).click();
    await card.getByRole('link', { name: 'Open IDE' }).waitFor({ timeout });

    const mobile = await browser.newContext({
      viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, ignoreHTTPSErrors: true,
    });
    const { dashboard: phone, elapsedMs: mobileLoginMs } = await login(mobile, base);
    assert.equal(await phone.evaluate(() => document.documentElement.scrollWidth > window.innerWidth), false);
    const { page: reopened, upgrades: mobileUpgrades } = await openIDE(mobile, phone);
    activePage = reopened;
    reopened.on('pageerror', error => pageErrors.push(error.message));
    reopened.setDefaultTimeout(timeout);
    await openFile(reopened, filename);
    await reopened.locator('.view-lines').getByText(value, { exact: true }).first().waitFor();
    if (!process.env.KAMUI_TEST_WORKSPACE_NAME || process.env.KAMUI_TEST_REMOVE_WORKSPACE === '1') {
      dashboard.once('dialog', dialog => dialog.accept());
      await card.getByRole('button', { name: 'Remove workspace' }).click();
      await dashboard.getByRole('heading', { name: workspaceName, exact: true }).waitFor({ state: 'detached' });
    }
    assert.deepEqual(pageErrors, [], 'Unexpected code-server page exceptions');
    console.log(JSON.stringify({
      desktopLoginMs, mobileLoginMs, workspaceReadyMs, desktopWebSocket101: desktopUpgrades,
      mobileWebSocket101: mobileUpgrades.length, fileExplorer: 'PASS', editorSave: 'PASS',
      terminalOutput: 'PASS', persistedAfterRestart: 'PASS', workspaceName,
    }));
  } catch (error) {
    if (activePage && !activePage.isClosed() && process.env.KAMUI_TEST_SCREENSHOT_DIR) {
      await activePage.screenshot({ path: process.env.KAMUI_TEST_SCREENSHOT_DIR + '/ide-failure.png' }).catch(() => {});
    }
    if (pageErrors.length) error.message += ': ' + pageErrors.join(' | ');
    throw error;
  } finally {
    await browser.close();
  }
}

main().catch(error => {
  let message = String(error.stack || error);
  if (password) message = message.replaceAll(password, '[redacted]');
  console.error(message);
  process.exitCode = 1;
});
