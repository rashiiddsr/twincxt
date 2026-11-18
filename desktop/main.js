// Twincxt for macOS and Linux (Electron). The Windows app (Go + WebView2)
// does the same; both share ui/shell.html and ui/service.js.
//
// Layout: one window. The shell (sidebar, title strip, settings) covers it;
// the WhatsApp and Telegram views sit in the content area on top of it.

const {
  app, BaseWindow, WebContentsView, BrowserWindow, ipcMain, Menu, Tray, Notification,
  nativeTheme, nativeImage, shell, session, systemPreferences, screen, clipboard,
} = require('electron');
const fs = require('fs');
const path = require('path');
const os = require('os');
const { Updater } = require('./updater');

const IS_MAC = process.platform === 'darwin';
const PLATFORM = IS_MAC ? 'mac' : 'linux';
const SIDE_W = 76;
const STRIP_H = IS_MAC ? 38 : 36;
const MIN_W = 960;
const MIN_H = 620;
const WARM_MS = 10 * 60 * 1000;
const PARTITION = 'persist:twincxt';
const UI = path.join(__dirname, 'ui');
const NAMES = { wa: 'WhatsApp', tg: 'Telegram' };

app.setName('Twincxt');
// For tests: keep all data in another folder.
if (process.env.TWINCXT_USERDATA) app.setPath('userData', process.env.TWINCXT_USERDATA);
if (!app.requestSingleInstanceLock()) {
  app.quit();
  process.exit(0);
}

// ---- config -------------------------------------------------------------------

const CONFIG = path.join(app.getPath('userData'), 'config.json');
const DEFAULTS = {
  active: 'wa', theme: 'system', zoomWa: 100, zoomTg: 100,
  autostart: false, startHidden: false, closeAction: 'tray',
  notifyWa: true, notifyTg: true, notifyMode: 'toast', telegramVersion: 'a',
  autoUpdate: true, skippedVersion: '',
  window: { x: 0, y: 0, w: 1240, h: 800, maximized: false },
};
let cfg = loadConfig();

function loadConfig() {
  let c = {};
  try { c = JSON.parse(fs.readFileSync(CONFIG, 'utf8').replace(/^﻿/, '')); } catch (_) { /* first start */ }
  c = { ...DEFAULTS, ...c, window: { ...DEFAULTS.window, ...(c.window || {}) } };
  if (!['wa', 'tg'].includes(c.active)) c.active = 'wa';
  if (!['light', 'dark', 'system'].includes(c.theme)) c.theme = 'system';
  if (!['tray', 'quit'].includes(c.closeAction)) c.closeAction = 'tray';
  if (!['toast', 'attention', 'both'].includes(c.notifyMode)) c.notifyMode = 'toast';
  if (!['a', 'k'].includes(c.telegramVersion)) c.telegramVersion = 'a';
  for (const k of ['zoomWa', 'zoomTg']) if (!(c[k] >= 50 && c[k] <= 200)) c[k] = 100;
  return c;
}

function saveConfig() {
  try {
    fs.mkdirSync(path.dirname(CONFIG), { recursive: true });
    fs.writeFileSync(CONFIG + '.tmp', JSON.stringify(cfg, null, 2));
    fs.renameSync(CONFIG + '.tmp', CONFIG);
  } catch (e) { log('config: ' + e.message); }
}

function log(line) {
  if (!process.env.TWINCXT_DEBUG) return;
  try { fs.appendFileSync(path.join(app.getPath('userData'), 'debug.log'), new Date().toISOString().slice(11, 23) + '  ' + line + '\n'); } catch (_) {}
}

// ---- state ----------------------------------------------------------------------

let win = null;
let shellView = null;
let shellReady = false;
let tray = null;
let quitting = false;
let settingsOpen = false;
let locked = false; // a required update must be installed first
let fullSvc = '';
let updater = null;
const svcs = {
  wa: { id: 'wa', view: null, unread: 0, strip: null, ready: false, leftAt: 0, cold: false },
  tg: { id: 'tg', view: null, unread: 0, strip: null, ready: false, leftAt: 0, cold: false },
};
const popups = new Map(); // BrowserWindow -> { svc, closing }
const views = new Map(); // webContents.id -> { svc, popup }

function serviceURL(id) {
  return id === 'wa' ? 'https://web.whatsapp.com/' : `https://web.telegram.org/${cfg.telegramVersion}/`;
}

function isDark() {
  return cfg.theme === 'dark' || (cfg.theme === 'system' && nativeTheme.shouldUseDarkColors);
}

function trusted(raw) {
  try {
    const u = new URL(raw);
    if (u.protocol !== 'https:') return false;
    return ['whatsapp.com', 'whatsapp.net', 'telegram.org'].some((d) => u.hostname === d || u.hostname.endsWith('.' + d));
  } catch (_) { return false; }
}

// A Chrome user agent with this Electron's Chromium version: WhatsApp refuses
// browsers it does not recognise, and Electron's own agent names Electron.
const USER_AGENT = (() => {
  const v = process.versions.chrome.split('.')[0] + '.0.0.0';
  const osPart = IS_MAC ? 'Macintosh; Intel Mac OS X 10_15_7' : 'X11; Linux x86_64';
  return `Mozilla/5.0 (${osPart}) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${v} Safari/537.36`;
})();

const SERVICE_JS = fs.readFileSync(path.join(UI, 'service.js'), 'utf8');
function serviceScript(svc, popup) {
  return SERVICE_JS.split('__SVC__').join(svc).split('__POPUP__').join(popup ? 'true' : 'false');
}

// Clicks WhatsApp's own "Move here" so a call (and the camera) returns to the
// main window when you close the call window.
const MOVE_HERE_JS = `(function(){
  var re=/move here|pindahkan ke sini|pindah ke sini|mover aqu|mover para c|hierher|ici/i;
  var els=document.querySelectorAll('button,[role="button"]');
  for(var i=0;i<els.length;i++){
    var l=(els[i].getAttribute('aria-label')||'')+' '+(els[i].getAttribute('title')||'')+' '+(els[i].textContent||'');
    if(re.test(l)){els[i].click();return true;}
  }
  return false;
})()`;

// ---- shell ----------------------------------------------------------------------

function shellEval(js) {
  if (shellReady && shellView) shellView.webContents.executeJavaScript(js).catch(() => {});
}

function pushState() {
  shellEval('twincxt.state(' + JSON.stringify({
    platform: PLATFORM,
    version: app.getVersion(),
    active: cfg.active,
    theme: cfg.theme,
    zoom: { wa: cfg.zoomWa, tg: cfg.zoomTg },
    autostart: cfg.autostart,
    startHidden: cfg.startHidden,
    closeAction: cfg.closeAction,
    notifyWa: cfg.notifyWa,
    notifyTg: cfg.notifyTg,
    notifyMode: cfg.notifyMode,
    telegramVersion: cfg.telegramVersion,
    autoUpdate: cfg.autoUpdate,
    maximized: !!win && win.isMaximized(),
    settingsOpen,
  }) + ')');
}

function pushUnread(s) {
  shellEval(`twincxt.unread(${JSON.stringify(s.id)},${s.unread})`);
  const total = svcs.wa.unread + svcs.tg.unread;
  if (IS_MAC) app.dock.setBadge(total ? String(total) : '');
  else app.setBadgeCount(total);
  if (tray) tray.setToolTip(total ? `Twincxt · ${total} belum dibaca` : 'Twincxt');
}

function applyStrip() {
  const s = svcs[cfg.active];
  if (settingsOpen || !s.strip) return shellEval("twincxt.strip('', '')");
  const { r, g, b } = s.strip;
  const dark = (0.299 * r + 0.587 * g + 0.114 * b) / 255 < 0.5;
  shellEval(`twincxt.strip(${JSON.stringify(`rgb(${r},${g},${b})`)},${JSON.stringify(dark ? '#D1D7DB' : '#3B4A54')})`);
}

// ---- window and layout -------------------------------------------------------------

function contentSize() {
  const [w, h] = win.getContentSize();
  return { w, h };
}

// The visible service is on top; the other stays running underneath the
// shell ("warm") so switching is instant and calls keep working. After
// WARM_MS out of view (and with no call window or audio) it is paused.
function layout() {
  if (!win) return;
  const { w, h } = contentSize();
  shellView.setBounds({ x: 0, y: 0, width: w, height: h });
  const order = [];
  for (const id of ['wa', 'tg']) {
    const s = svcs[id];
    if (!s.view) continue;
    const full = fullSvc === id;
    s.view.setBounds(full ? { x: 0, y: 0, width: w, height: h } : { x: SIDE_W, y: STRIP_H, width: Math.max(0, w - SIDE_W), height: Math.max(0, h - STRIP_H) });
    const onTop = (!settingsOpen && id === cfg.active) || full;
    if (onTop) {
      s.leftAt = 0;
      setCold(s, false);
    } else if (!s.leftAt) {
      s.leftAt = Date.now();
    }
    order.push({ s, onTop });
  }
  // z-order: services out of view, then the shell, then the visible service.
  const cv = win.contentView;
  for (const { s, onTop } of order) if (!onTop) cv.addChildView(s.view, 0);
  cv.addChildView(shellView);
  for (const { s, onTop } of order) if (onTop) cv.addChildView(s.view);
}

function setCold(s, cold) {
  if (s.cold === cold || !s.view) return;
  s.cold = cold;
  log(`${s.id}: cold=${cold}`);
  s.view.setVisible(!cold);
  s.view.webContents.setBackgroundThrottling(cold);
}

function busy(s) {
  for (const p of popups.values()) if (p.svc === s.id) return true;
  return !!s.view && s.view.webContents.isCurrentlyAudible();
}

setInterval(() => {
  for (const s of Object.values(svcs)) {
    if (s.leftAt && !s.cold && Date.now() - s.leftAt >= WARM_MS && !busy(s)) setCold(s, true);
  }
}, Math.min(60000, WARM_MS / 2));

function focusActive() {
  if (settingsOpen) return shellView && shellView.webContents.focus();
  const s = svcs[cfg.active];
  if (s.view) s.view.webContents.focus();
}

function selectService(id, source) {
  if (!svcs[id]) return;
  if (locked) { settingsOpen = true; layout(); pushState(); return; }
  log(`select ${id} by ${source}`);
  cfg.active = id;
  settingsOpen = false;
  layout();
  pushState();
  applyStrip();
  focusActive();
  saveConfig();
}

function toggleSettings(on) {
  settingsOpen = locked || (on === undefined ? !settingsOpen : on);
  layout();
  pushState();
  applyStrip();
  focusActive();
}

function showWindow() {
  if (!win) return;
  if (win.isMinimized()) win.restore();
  win.show();
  win.focus();
  focusActive();
}

function lockForUpdate() {
  if (locked) return;
  locked = true;
  log('update: below the minimum version, services locked');
  settingsOpen = true;
  layout();
  pushState();
  applyStrip();
  showWindow();
}

function createWindow() {
  const wb = cfg.window;
  const visible = screen.getAllDisplays().some((d) => {
    const a = d.workArea;
    return wb.w >= MIN_W / 2 && wb.x < a.x + a.width && wb.x + wb.w > a.x && wb.y < a.y + a.height && wb.y + wb.h > a.y;
  });
  win = new BaseWindow({
    width: Math.max(MIN_W, wb.w || 1240),
    height: Math.max(MIN_H, wb.h || 800),
    ...(visible ? { x: wb.x, y: wb.y } : {}),
    minWidth: MIN_W,
    minHeight: MIN_H,
    show: false,
    title: 'Twincxt',
    backgroundColor: isDark() ? '#1A1A1A' : '#EBEBEB',
    ...(IS_MAC
      ? { titleBarStyle: 'hidden', trafficLightPosition: { x: 14, y: 13 } }
      : { autoHideMenuBar: true, icon: path.join(UI, 'icons', 'app.png') }),
  });

  shellView = new WebContentsView({
    webPreferences: { preload: path.join(__dirname, 'preload-shell.js'), contextIsolation: true, sandbox: true, spellcheck: false },
  });
  shellView.setBackgroundColor(isDark() ? '#1A1A1A' : '#EBEBEB');
  win.contentView.addChildView(shellView);
  shellView.webContents.loadFile(path.join(UI, 'shell.html'));
  shellView.webContents.on('will-navigate', (e) => e.preventDefault());
  shellView.webContents.on('before-input-event', (e, input) => onKey(e, input));

  for (const id of ['wa', 'tg']) createService(svcs[id]);

  win.on('resize', () => { layout(); pushState(); });
  win.on('maximize', pushState);
  win.on('unmaximize', pushState);
  win.on('focus', () => { win.flashFrame(false); focusActive(); });
  win.on('close', (e) => {
    saveBounds();
    if (!quitting && cfg.closeAction === 'tray') {
      e.preventDefault();
      if (IS_MAC && win.isFullScreen()) { win.once('leave-full-screen', () => win.hide()); win.setFullScreen(false); } else win.hide();
    }
  });
  win.on('closed', () => {
    win = null;
    // "Keluar" chosen for the close button: quit (macOS would otherwise keep
    // the app running without a window).
    if (!quitting) { quitting = true; app.quit(); }
  });

  layout();
  const autostarted = process.argv.includes('--autostart') || (IS_MAC && app.getLoginItemSettings().wasOpenedAtLogin);
  if (!(autostarted && cfg.startHidden)) {
    if (wb.maximized) win.maximize();
    win.show();
  }
}

function saveBounds() {
  if (!win || win.isMinimized() || win.isFullScreen()) return;
  const b = win.isMaximized() ? (cfg.window || {}) : win.getBounds();
  cfg.window = { x: b.x, y: b.y, w: b.width || b.w, h: b.height || b.h, maximized: win.isMaximized() };
  saveConfig();
}

// ---- services -----------------------------------------------------------------------

function webPrefs(extra) {
  return {
    preload: path.join(__dirname, 'preload-service.js'),
    contextIsolation: true,
    sandbox: true,
    partition: PARTITION,
    spellcheck: true,
    ...extra,
  };
}

function createService(s) {
  const view = new WebContentsView({ webPreferences: webPrefs() });
  s.view = view;
  const wc = view.webContents;
  views.set(wc.id, { svc: s.id, popup: false });
  view.setBackgroundColor(isDark() ? '#161717' : '#FFFFFF');
  wc.setUserAgent(USER_AGENT);
  wc.setVisualZoomLevelLimits(1, 1); // no pinch zoom
  wc.setZoomFactor((s.id === 'wa' ? cfg.zoomWa : cfg.zoomTg) / 100);
  wc.on('did-finish-load', () => wc.setZoomFactor((s.id === 'wa' ? cfg.zoomWa : cfg.zoomTg) / 100));
  configurePage(wc, s.id);

  wc.on('page-title-updated', (_e, title) => {
    const m = /\((\d+)\)/.exec(title || '');
    const n = m ? Number(m[1]) : 0;
    if (n !== s.unread) { s.unread = n; pushUnread(s); }
  });
  wc.on('did-start-navigation', (_e, url, inPage, isMain) => {
    if (isMain && !inPage && trusted(url)) { s.ready = false; shellEval(`twincxt.loading(${JSON.stringify(s.id)},true)`); }
  });
  wc.on('did-stop-loading', () => { s.ready = true; shellEval(`twincxt.loading(${JSON.stringify(s.id)},false)`); });
  wc.on('render-process-gone', (_e, d) => {
    log(`${s.id}: renderer gone (${d.reason})`);
    if (d.reason !== 'clean-exit') wc.reload();
  });
  wc.on('enter-html-full-screen', () => { fullSvc = s.id; win.setFullScreen(true); layout(); });
  wc.on('leave-html-full-screen', () => { fullSvc = ''; win.setFullScreen(false); layout(); });

  wc.loadURL(serviceURL(s.id));
}

// Settings shared by the service views and their call windows.
function configurePage(wc, svcId) {
  wc.setWindowOpenHandler(({ url, features }) => {
    if (trusted(url) || url === '' || url.startsWith('about:')) {
      // WhatsApp's call window: the page itself only opens it from "Move to
      // new window" (see ui/service.js).
      const size = {};
      for (const part of String(features || '').split(',')) {
        const [k, v] = part.split('=').map((x) => (x || '').trim());
        if ((k === 'width' || k === 'height') && Number(v) >= 200) size[k] = Number(v);
      }
      return {
        action: 'allow',
        overrideBrowserWindowOptions: {
          width: size.width || 960,
          height: size.height || 680,
          minWidth: 360,
          minHeight: 300,
          title: NAMES[svcId],
          autoHideMenuBar: true,
          backgroundColor: isDark() ? '#161717' : '#FFFFFF',
          icon: IS_MAC ? undefined : path.join(UI, 'icons', 'app.png'),
          webPreferences: webPrefs(),
        },
      };
    }
    if (/^(https?:|mailto:|tel:)/i.test(url)) shell.openExternal(url);
    return { action: 'deny' };
  });
  wc.on('did-create-window', (child) => setupPopup(child, svcId));
  wc.on('will-navigate', (e, url) => {
    if (trusted(url) || !/^https?:/i.test(url)) return;
    e.preventDefault();
    shell.openExternal(url);
  });
  wc.on('before-input-event', (e, input) => onKey(e, input));
  wc.on('context-menu', (_e, params) => contextMenu(wc, params));
}

function setupPopup(child, svcId) {
  popups.set(child, { svc: svcId, closing: false });
  views.set(child.webContents.id, { svc: svcId, popup: true });
  child.webContents.setUserAgent(USER_AGENT);
  child.webContents.setVisualZoomLevelLimits(1, 1);
  configurePage(child.webContents, svcId);
  log(`popup: opened for ${svcId}`);

  // Closing the call window hands the call back to the main window through
  // WhatsApp's own "Move here". Until WhatsApp has done so the window stays
  // "visible" to the page but invisible to you, so the camera keeps running.
  child.on('close', (e) => {
    const p = popups.get(child);
    if (!p || p.closing || svcId !== 'wa' || !svcs.wa.view || quitting) return;
    p.closing = true;
    e.preventDefault();
    if (IS_MAC) child.setOpacity(0); else child.setPosition(-32000, -32000);
    child.setIgnoreMouseEvents(true);
    child.setSkipTaskbar(true);
    log('popup: closed by you, asking WhatsApp to move the call back');
    svcs.wa.view.webContents.executeJavaScript(MOVE_HERE_JS).then((found) => {
      if (!found && !child.isDestroyed()) child.destroy();
    }).catch(() => { if (!child.isDestroyed()) child.destroy(); });
    setTimeout(() => { if (!child.isDestroyed()) child.destroy(); }, 3000);
  });
  child.on('closed', () => popups.delete(child));
}

function contextMenu(wc, p) {
  const items = [];
  for (const word of p.dictionarySuggestions || []) {
    items.push({ label: word, click: () => wc.replaceMisspelling(word) });
  }
  if (p.misspelledWord) {
    items.push({ label: 'Tambahkan ke kamus', click: () => wc.session.addWordToSpellCheckerDictionary(p.misspelledWord) });
    items.push({ type: 'separator' });
  }
  if (p.linkURL) {
    items.push({ label: 'Buka tautan di browser', click: () => shell.openExternal(p.linkURL) });
    items.push({ label: 'Salin tautan', click: () => clipboard.writeText(p.linkURL) });
    items.push({ type: 'separator' });
  }
  if (p.mediaType === 'image') {
    items.push({ label: 'Salin gambar', click: () => wc.copyImageAt(p.x, p.y) });
    items.push({ type: 'separator' });
  }
  const f = p.editFlags || {};
  if (p.isEditable) {
    items.push({ label: 'Batalkan', role: 'undo', enabled: f.canUndo });
    items.push({ label: 'Ulangi', role: 'redo', enabled: f.canRedo });
    items.push({ type: 'separator' });
    items.push({ label: 'Potong', role: 'cut', enabled: f.canCut });
  }
  if (p.isEditable || p.selectionText) items.push({ label: 'Salin', role: 'copy', enabled: f.canCopy });
  if (p.isEditable) items.push({ label: 'Tempel', role: 'paste', enabled: f.canPaste });
  if (p.isEditable || p.selectionText) items.push({ label: 'Pilih semua', role: 'selectAll' });
  while (items.length && items[items.length - 1].type === 'separator') items.pop();
  if (items.length) Menu.buildFromTemplate(items).popup();
}

// Shortcuts while any page has focus. macOS: ⌘1 / ⌘2 / ⌘,; Linux: Alt+1 / Alt+2 / Ctrl+,.
// Ctrl+Tab switches between WhatsApp and Telegram everywhere.
function onKey(e, input) {
  if (input.type !== 'keyDown') return;
  const mod = IS_MAC ? input.meta : input.alt;
  const settingsMod = IS_MAC ? input.meta : input.control;
  let handled = true;
  if (input.control && input.key === 'Tab') selectService(cfg.active === 'wa' ? 'tg' : 'wa', 'Ctrl+Tab');
  else if (mod && !input.control && input.key === '1') selectService('wa', 'shortcut');
  else if (mod && !input.control && input.key === '2') selectService('tg', 'shortcut');
  else if (settingsMod && input.key === ',') toggleSettings();
  else handled = false;
  if (handled) e.preventDefault();
}

// ---- messages from the shell and the pages ---------------------------------------------

ipcMain.on('shell-msg', (_e, raw) => {
  let m;
  try { m = JSON.parse(raw); } catch (_) { return; }
  log('shell: ' + raw);
  switch (m.type) {
    case 'ready':
      shellReady = true;
      pushState();
      for (const s of Object.values(svcs)) {
        pushUnread(s);
        shellEval(`twincxt.loading(${JSON.stringify(s.id)},${!s.ready})`);
      }
      applyStrip();
      if (updater) updater.onState(updater.state);
      break;
    case 'select': selectService(m.data, 'sidebar'); break;
    case 'settings': toggleSettings(!!m.data); break;
    case 'zoom': setZoom(m.data.svc, m.data.value); break;
    case 'set': setOption(m.data.key, m.data.value); break;
    case 'action': runAction(String(m.data)); break;
    case 'update':
      if (m.data === 'check') updater.check(true);
      else if (m.data === 'install') updater.install();
      else if (m.data === 'skip') updater.skip();
      break;
    case 'window':
      if (m.data === 'min') win.minimize();
      else if (m.data === 'max') win.isMaximized() ? win.unmaximize() : win.maximize();
      else if (m.data === 'close') win.close();
      break;
  }
});

ipcMain.on('service-init', (e) => {
  const v = views.get(e.sender.id);
  // A call window's page may load before did-create-window: treat an unknown
  // page as a WhatsApp call window.
  e.returnValue = v ? serviceScript(v.svc, v.popup) : serviceScript('wa', true);
});

ipcMain.on('svc-msg', (e, raw) => {
  const v = views.get(e.sender.id);
  if (!v) return;
  let m;
  try { m = JSON.parse(raw); } catch (_) { return; }
  const s = svcs[v.svc];
  log(`${v.svc}${v.popup ? ' popup' : ''}: ${raw.slice(0, 300)}`);
  if (m.type === 'frame' && !v.popup) {
    const c = { r: m.r | 0, g: m.g | 0, b: m.b | 0 };
    if (!s.strip || s.strip.r !== c.r || s.strip.g !== c.g || s.strip.b !== c.b) {
      s.strip = c;
      if (s.id === cfg.active) applyStrip();
    }
  } else if (m.type === 'notify') {
    notifyMessage(s, String(m.title || ''), String(m.body || ''));
  }
});

function setZoom(id, z) {
  z = z >= 50 && z <= 200 ? z : 100;
  if (id === 'wa') cfg.zoomWa = z; else cfg.zoomTg = z;
  if (svcs[id] && svcs[id].view) svcs[id].view.webContents.setZoomFactor(z / 100);
  saveConfig();
}

function setOption(key, value) {
  switch (key) {
    case 'theme': cfg.theme = value; nativeTheme.themeSource = value; break;
    case 'autostart': cfg.autostart = !!value; setAutostart(cfg.autostart); break;
    case 'startHidden': cfg.startHidden = !!value; break;
    case 'closeAction': cfg.closeAction = value === 'quit' ? 'quit' : 'tray'; break;
    case 'notifyWa': cfg.notifyWa = !!value; break;
    case 'notifyTg': cfg.notifyTg = !!value; break;
    case 'notifyMode': if (['toast', 'attention', 'both'].includes(value)) cfg.notifyMode = value; break;
    case 'autoUpdate': cfg.autoUpdate = !!value; if (cfg.autoUpdate) updater.check(false); break;
    case 'telegramVersion':
      if ((value === 'a' || value === 'k') && value !== cfg.telegramVersion) {
        cfg.telegramVersion = value;
        svcs.tg.view.webContents.loadURL(serviceURL('tg'));
      }
      break;
  }
  saveConfig();
  pushState();
}

function runAction(action) {
  const [what, id] = action.split(':');
  const s = svcs[id];
  if (!s || !s.view) return;
  if (what === 'reload') s.view.webContents.loadURL(serviceURL(id));
  if (what === 'reset') {
    const origin = new URL(serviceURL(id)).origin;
    s.view.webContents.session.clearStorageData({ origin }).then(() => {
      s.unread = 0;
      pushUnread(s);
      s.view.webContents.loadURL(serviceURL(id));
    });
  }
}

// ---- notifications ----------------------------------------------------------------------------

function iconFor(id) {
  const p = path.join(UI, 'icons', id + '.png');
  return fs.existsSync(p) ? p : path.join(UI, 'icons', 'app.png');
}

function notifyMessage(s, title, body) {
  if ((s.id === 'wa' && !cfg.notifyWa) || (s.id === 'tg' && !cfg.notifyTg)) return;
  const focused = win && win.isVisible() && win.isFocused() && !win.isMinimized();
  if (focused && s.id === cfg.active && !settingsOpen) return; // already looking at it
  if (cfg.notifyMode !== 'attention' && Notification.isSupported()) {
    const n = new Notification({ title: title || NAMES[s.id], body, icon: iconFor(s.id), silent: true });
    n.on('click', () => { selectService(s.id, 'notification'); showWindow(); });
    n.show();
  }
  // "Kedip taskbar" / "Pantulkan Dock" / "Sorot di taskbar".
  if (cfg.notifyMode !== 'toast' && !focused) {
    if (IS_MAC) app.dock.bounce('informational');
    else if (win) win.flashFrame(true);
  }
}

function notifyPlain(title, body) {
  if (!Notification.isSupported()) return;
  const n = new Notification({ title, body, icon: path.join(UI, 'icons', 'app.png'), silent: true });
  n.on('click', () => { toggleSettings(true); showWindow(); });
  n.show();
}

// ---- autostart -----------------------------------------------------------------------------------

function autostartFile() { return path.join(os.homedir(), '.config', 'autostart', 'twincxt.desktop'); }

function setAutostart(on) {
  if (IS_MAC) {
    app.setLoginItemSettings({ openAtLogin: on });
    return;
  }
  const exe = process.env.APPIMAGE || process.execPath;
  try {
    if (on) {
      fs.mkdirSync(path.dirname(autostartFile()), { recursive: true });
      fs.writeFileSync(autostartFile(), [
        '[Desktop Entry]', 'Type=Application', 'Name=Twincxt',
        `Exec="${exe}" --autostart`, 'X-GNOME-Autostart-enabled=true', '',
      ].join('\n'));
    } else {
      fs.rmSync(autostartFile(), { force: true });
    }
  } catch (e) { log('autostart: ' + e.message); }
}

function autostartEnabled() {
  if (IS_MAC) return app.getLoginItemSettings().openAtLogin;
  return fs.existsSync(autostartFile());
}

// ---- menus and tray ---------------------------------------------------------------------------------

function buildMenu() {
  const zoomBy = (dir) => {
    const id = cfg.active;
    const steps = [50, 67, 75, 80, 90, 100, 110, 125, 150, 175, 200];
    const cur = id === 'wa' ? cfg.zoomWa : cfg.zoomTg;
    const i = Math.max(0, steps.indexOf(cur));
    setZoom(id, dir === 0 ? 100 : steps[Math.max(0, Math.min(steps.length - 1, i + dir))]);
    pushState();
  };
  if (!IS_MAC) {
    // Linux: no menu bar (the shell has every control); shortcuts are handled per page.
    Menu.setApplicationMenu(null);
    return;
  }
  Menu.setApplicationMenu(Menu.buildFromTemplate([
    {
      label: 'Twincxt',
      submenu: [
        { label: 'Tentang Twincxt', role: 'about' },
        { label: 'Periksa Pembaruan…', click: () => { toggleSettings(true); showWindow(); updater.check(true); } },
        { type: 'separator' },
        { label: 'Pengaturan…', accelerator: 'Cmd+,', click: () => { toggleSettings(true); showWindow(); } },
        { type: 'separator' },
        { label: 'Sembunyikan Twincxt', role: 'hide' },
        { label: 'Sembunyikan Lainnya', role: 'hideOthers' },
        { label: 'Tampilkan Semua', role: 'unhide' },
        { type: 'separator' },
        { label: 'Keluar dari Twincxt', accelerator: 'Cmd+Q', click: () => { quitting = true; app.quit(); } },
      ],
    },
    {
      label: 'Edit',
      submenu: [
        { label: 'Batalkan', role: 'undo' },
        { label: 'Ulangi', role: 'redo' },
        { type: 'separator' },
        { label: 'Potong', role: 'cut' },
        { label: 'Salin', role: 'copy' },
        { label: 'Tempel', role: 'paste' },
        { label: 'Tempel dan Cocokkan Gaya', role: 'pasteAndMatchStyle' },
        { label: 'Pilih Semua', role: 'selectAll' },
      ],
    },
    {
      label: 'Tampilan',
      submenu: [
        { label: 'WhatsApp', accelerator: 'Cmd+1', click: () => selectService('wa', 'menu') },
        { label: 'Telegram', accelerator: 'Cmd+2', click: () => selectService('tg', 'menu') },
        { label: 'Ganti Layanan', accelerator: 'Ctrl+Tab', click: () => selectService(cfg.active === 'wa' ? 'tg' : 'wa', 'menu') },
        { type: 'separator' },
        { label: 'Perbesar', accelerator: 'Cmd+=', click: () => zoomBy(1) },
        { label: 'Perkecil', accelerator: 'Cmd+-', click: () => zoomBy(-1) },
        { label: 'Ukuran Asli', accelerator: 'Cmd+0', click: () => zoomBy(0) },
        { type: 'separator' },
        { label: 'Muat Ulang Layanan', accelerator: 'Cmd+R', click: () => runAction('reload:' + cfg.active) },
        { label: 'Layar Penuh', role: 'togglefullscreen' },
      ],
    },
    {
      label: 'Jendela',
      submenu: [
        { label: 'Perkecil', role: 'minimize' },
        { label: 'Zoom', role: 'zoom' },
        { label: 'Tutup Jendela', accelerator: 'Cmd+W', click: () => win && win.close() },
        { type: 'separator' },
        { label: 'Bawa Semua ke Depan', role: 'front' },
      ],
    },
    {
      label: 'Bantuan',
      role: 'help',
      submenu: [
        { label: 'Halaman Twincxt', click: () => shell.openExternal('https://github.com/rashiiddsr/twincxt') },
        { label: 'Catatan Rilis', click: () => shell.openExternal('https://github.com/rashiiddsr/twincxt/releases') },
      ],
    },
  ]));
}

function buildTray() {
  if (IS_MAC) return; // macOS: the Dock icon (with its unread badge) is the way back.
  const img = nativeImage.createFromPath(path.join(UI, 'icons', 'app.png')).resize({ width: 24, height: 24 });
  tray = new Tray(img);
  tray.setToolTip('Twincxt');
  const label = (name, n) => (n > 0 ? `${name}  (${n})` : name);
  const refresh = () => tray.setContextMenu(Menu.buildFromTemplate([
    { label: 'Buka Twincxt', click: showWindow },
    { type: 'separator' },
    { label: label('WhatsApp', svcs.wa.unread), click: () => { selectService('wa', 'tray'); showWindow(); } },
    { label: label('Telegram', svcs.tg.unread), click: () => { selectService('tg', 'tray'); showWindow(); } },
    { type: 'separator' },
    { label: 'Pengaturan', click: () => { toggleSettings(true); showWindow(); } },
    { label: 'Keluar', click: () => { quitting = true; app.quit(); } },
  ]));
  refresh();
  setInterval(refresh, 5000);
  tray.on('click', () => (win && win.isVisible() && win.isFocused() ? win.hide() : showWindow()));
}

// ---- downloads and permissions -----------------------------------------------------------------------

function setupSession() {
  const ses = session.fromPartition(PARTITION);
  ses.setUserAgent(USER_AGENT);
  ses.setPermissionRequestHandler(async (wc, permission, callback, details) => {
    const url = details.requestingUrl || (wc && wc.getURL()) || '';
    if (!trusted(url) || permission === 'notifications') return callback(false); // notifications go through Twincxt
    if (IS_MAC && permission === 'media') {
      const types = details.mediaTypes || [];
      if (types.includes('video') && !(await systemPreferences.askForMediaAccess('camera'))) return callback(false);
      if (types.includes('audio') && !(await systemPreferences.askForMediaAccess('microphone'))) return callback(false);
    }
    callback(true);
  });
  ses.setPermissionCheckHandler((_wc, permission, origin) => permission !== 'notifications' && trusted(origin || ''));

  ses.on('will-download', (_e, item) => {
    const dir = app.getPath('downloads');
    const ext = path.extname(item.getFilename());
    const base = path.basename(item.getFilename(), ext) || 'download';
    let target = path.join(dir, base + ext);
    for (let i = 1; fs.existsSync(target); i++) target = path.join(dir, `${base} (${i})${ext}`);
    item.setSavePath(target);
    item.once('done', (_ev, state) => {
      if (state !== 'completed' || !Notification.isSupported()) return;
      const n = new Notification({ title: 'Unduhan selesai', body: path.basename(target), icon: path.join(UI, 'icons', 'app.png'), silent: true });
      n.on('click', () => shell.showItemInFolder(target));
      n.show();
    });
  });
}

// ---- start -----------------------------------------------------------------------------------------

app.on('second-instance', () => showWindow());
app.on('activate', () => showWindow()); // Dock icon clicked (macOS)
app.on('before-quit', () => {
  quitting = true;
  saveBounds();
  // Write cookies and site data to disk before exiting, so logins are kept.
  session.fromPartition(PARTITION).flushStorageData();
});
app.on('window-all-closed', () => { if (!IS_MAC) app.quit(); });

app.whenReady().then(() => {
  nativeTheme.themeSource = cfg.theme;
  cfg.autostart = autostartEnabled();
  setupSession();
  buildMenu();
  createWindow();
  buildTray();
  nativeTheme.on('updated', () => { if (win) shellView.setBackgroundColor(isDark() ? '#1A1A1A' : '#EBEBEB'); });

  updater = new Updater({
    getConfig: () => cfg,
    saveConfig,
    onState: (st) => shellEval('twincxt.update(' + JSON.stringify(st) + ')'),
    onRequired: lockForUpdate,
    notify: notifyPlain,
    quit: () => { quitting = true; app.quit(); },
    log,
  });
  updater.start();

  // Test hook: TWINCXT_TEST_POPUP=1 opens a call-like window from a "Move to
  // new window" button, then closes it the way the window's ✕ would.
  if (process.env.TWINCXT_TEST_POPUP) {
    const wc = svcs.wa.view.webContents;
    wc.once('did-finish-load', () => setTimeout(() => {
      wc.executeJavaScript(`(function(){
        var auto=window.open('','auto','width=400,height=300'); console.log('auto popup', !!auto);
        var b=document.createElement('button');b.setAttribute('aria-label','Move to new window');
        b.onclick=function(){var w=window.open('','twincxt-test','width=520,height=360');
          w.document.title='Popup click';w.document.body.innerHTML='<h1>POPUP OK</h1>';window.__tw=w;};
        var m=document.createElement('button');m.textContent='Move here';
        m.onclick=function(){window.__tw&&window.__tw.close();};
        document.body.append(b,m);b.click();
      })()`, true);
      setTimeout(() => {
        log('test: popups open = ' + popups.size);
        for (const child of popups.keys()) child.close();
        setTimeout(() => log('test: popups after close = ' + popups.size), 1500);
      }, 3000);
    }, 3000));
  }

  // Test hook for CI: TWINCXT_SMOKE=<seconds> quits after that long.
  if (process.env.TWINCXT_SMOKE) setTimeout(() => { quitting = true; app.quit(); }, Number(process.env.TWINCXT_SMOKE) * 1000);
});
