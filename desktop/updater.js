// Built-in updater for macOS and Linux (the Windows app has the same flow in
// updater.go).
//
//   1. Check: the newest GitHub release is read 20 s after start, every 6 hours
//      and when "Periksa pembaruan" is pressed.
//   2. Download: with automatic checks on, a newer version downloads in the
//      background. Its SHA-256 must match the checksum GitHub lists for the
//      file (or a SHA256SUMS*.txt in the release), otherwise it is deleted.
//   3. Install, only when you press "Mulai ulang & perbarui":
//      - macOS: the new Twincxt.app replaces the running one in place (no .dmg
//        to drag), then opens again. Files downloaded by Twincxt itself carry
//        no quarantine flag, so macOS does not block the new version.
//      - Linux AppImage: the file replaces itself and starts again.
//      - Linux .deb: installed through the system's password prompt (pkexec),
//        or opened in the software installer when that is not available.
//   Logins and settings live in the user's data folder and are never touched.
//
// Minimum version: release notes may contain <!-- twincxt-min: 1.3.0 -->;
// older versions then must update (no skipping, services locked until done).

const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { spawn } = require('child_process');
const { app } = require('electron');

const REPO = 'rashiiddsr/twincxt';
const FIRST_CHECK_MS = 20 * 1000;
const CHECK_EVERY_MS = 6 * 60 * 60 * 1000;
const UA = { 'User-Agent': 'Twincxt-Updater', Accept: 'application/vnd.github+json' };

function newer(a, b) {
  const pa = (String(a).match(/\d+/g) || []).slice(0, 4).map(Number);
  const pb = (String(b).split('-')[0].match(/\d+/g) || []).slice(0, 4).map(Number);
  for (let i = 0; i < 4; i++) {
    const x = pa[i] || 0, y = pb[i] || 0;
    if (x !== y) return x > y;
  }
  return false;
}

function cleanNotes(body) {
  const out = [];
  for (let line of String(body || '').replace(/\r/g, '').split('\n')) {
    line = line.trim();
    if (!line || line.startsWith('<!--')) continue;
    line = line.replace(/^#+/, '').replace(/\*\*/g, '').replace(/`/g, '').trim();
    out.push(line);
    if (out.length >= 12) { out.push('…'); break; }
  }
  return out.join('\n');
}

function requiredBy(rel) {
  const m = /<!--\s*twincxt-min:\s*v?([0-9][0-9.]*)\s*-->/.exec(rel.body || '');
  return !!m && newer(m[1], app.getVersion());
}

function pickAsset(rel) {
  const assets = rel.assets || [];
  const find = (re) => assets.find((a) => re.test(a.name));
  if (process.platform === 'darwin') {
    return find(process.arch === 'arm64' ? /-mac-arm64\.zip$/i : /-mac-x64\.zip$/i);
  }
  if (process.platform === 'linux') {
    return process.env.APPIMAGE ? find(/-linux-x64\.AppImage$/i) : find(/-linux-x64\.deb$/i);
  }
  return null;
}

async function expectedSha256(rel, asset) {
  const d = String(asset.digest || '').toLowerCase().replace(/^sha256:/, '');
  if (/^[0-9a-f]{64}$/.test(d)) return d;
  for (const a of rel.assets || []) {
    if (!/^sha256sums.*\.txt$/i.test(a.name)) continue;
    const res = await fetch(a.browser_download_url, { headers: UA });
    if (!res.ok) continue;
    for (const line of (await res.text()).split('\n')) {
      const f = line.trim().split(/\s+/);
      if (f.length === 2 && f[1].replace(/^\*/, '') === asset.name && /^[0-9a-f]{64}$/i.test(f[0])) return f[0].toLowerCase();
    }
  }
  throw new Error('checksum tidak tersedia, file tidak dipasang');
}

function fileSha256(file) {
  return new Promise((resolve, reject) => {
    const h = crypto.createHash('sha256');
    fs.createReadStream(file).on('data', (d) => h.update(d)).on('end', () => resolve(h.digest('hex'))).on('error', reject);
  });
}

class Updater {
  constructor({ getConfig, saveConfig, onState, onRequired, notify, quit, log }) {
    Object.assign(this, { getConfig, saveConfig, onState, onRequired, notify, quit, log });
    this.state = { status: 'idle', version: '', progress: 0, notes: '', error: '', required: false };
    this.rel = null;
    this.asset = null;
    this.file = '';
    this.busy = false;
    this.announced = '';
    this.dir = path.join(app.getPath('userData'), 'updates');
  }

  start() {
    setTimeout(() => this.check(false), FIRST_CHECK_MS);
    setInterval(() => this.check(false), CHECK_EVERY_MS);
  }

  set(patch, replace) {
    this.state = replace ? { status: 'idle', version: '', progress: 0, notes: '', error: '', required: false, ...patch } : { ...this.state, ...patch };
    this.onState(this.state);
  }

  async check(manual) {
    if (this.busy) return;
    this.busy = true;
    if (manual) this.set({ status: 'checking' }, true);
    try {
      const res = await fetch(`https://api.github.com/repos/${REPO}/releases/latest`, { headers: UA });
      if (!res.ok) throw new Error(`GitHub menjawab ${res.status}`);
      const rel = await res.json();
      if (rel.draft || rel.prerelease) throw new Error('rilis terbaru bukan rilis final');
      const version = String(rel.tag_name || '').replace(/^v/, '');
      const asset = pickAsset(rel);
      if (!newer(version, app.getVersion()) || !asset) {
        if (manual) this.set({ status: 'latest' }, true);
        return;
      }
      const cfg = this.getConfig();
      const required = requiredBy(rel);
      if (!manual && !required && (!cfg.autoUpdate || cfg.skippedVersion === version)) return;
      this.rel = rel;
      this.asset = asset;
      this.set({ status: 'available', version, notes: cleanNotes(rel.body), required }, true);
      if (required) this.onRequired();
      if (required || (!manual && cfg.autoUpdate)) this.download(false);
    } catch (e) {
      this.log('update: check failed: ' + e.message);
      if (manual) this.set({ status: 'error', error: 'Tidak bisa memeriksa pembaruan. Periksa koneksi internet lalu coba lagi.' }, true);
    } finally {
      this.busy = false;
    }
  }

  async download(thenInstall) {
    const { rel, asset } = this;
    if (!rel || !asset || this.state.status === 'downloading') return;
    const version = String(rel.tag_name).replace(/^v/, '');
    const base = { version, notes: cleanNotes(rel.body), required: requiredBy(rel) };
    this.set({ ...base, status: 'downloading', progress: 0 }, true);
    try {
      const want = await expectedSha256(rel, asset);
      fs.mkdirSync(this.dir, { recursive: true });
      const final = path.join(this.dir, path.basename(asset.name));
      let ok = false;
      try { ok = (await fileSha256(final)) === want; } catch (_) { ok = false; }
      if (!ok) {
        for (const f of fs.readdirSync(this.dir)) fs.rmSync(path.join(this.dir, f), { recursive: true, force: true });
        const res = await fetch(asset.browser_download_url, { headers: { 'User-Agent': UA['User-Agent'] } });
        if (!res.ok || !res.body) throw new Error(`server menjawab ${res.status}`);
        const total = Number(res.headers.get('content-length')) || asset.size || 0;
        const partial = final + '.partial';
        const out = fs.createWriteStream(partial);
        const h = crypto.createHash('sha256');
        let done = 0, last = 0;
        for await (const chunk of res.body) {
          h.update(chunk);
          done += chunk.length;
          if (!out.write(chunk)) await new Promise((r) => out.once('drain', r));
          if (total && Date.now() - last > 200) { last = Date.now(); this.set({ progress: done / total }); }
        }
        await new Promise((r, j) => out.end((e) => (e ? j(e) : r())));
        if (h.digest('hex') !== want) {
          fs.rmSync(partial, { force: true });
          throw new Error('file rusak atau tidak cocok dengan checksum rilis');
        }
        fs.renameSync(partial, final);
      }
      this.file = final;
      this.set({ ...base, status: 'ready', progress: 1 }, true);
      if (thenInstall) return this.install();
      if (this.announced !== version) {
        this.announced = version;
        this.notify('Pembaruan Twincxt ' + version + ' siap', 'Buka Pengaturan untuk memasangnya.');
      }
    } catch (e) {
      this.log('update: download failed: ' + e.message);
      this.set({ ...base, status: 'error', error: 'Unduhan pembaruan gagal: ' + e.message }, true);
    }
  }

  async install() {
    if (!this.file || !fs.existsSync(this.file)) {
      this.file = '';
      return this.download(true);
    }
    try {
      if (process.platform === 'darwin') await this.installMac();
      else if (process.env.APPIMAGE) this.installAppImage();
      else this.installDeb();
    } catch (e) {
      this.log('update: install failed: ' + e.message);
      this.set({ status: 'error', error: e.message });
    }
  }

  // Replace the running Twincxt.app in place, after Twincxt has quit.
  async installMac() {
    const bundle = path.resolve(process.execPath, '..', '..', '..'); // …/Twincxt.app
    if (!bundle.endsWith('.app')) throw new Error('Twincxt tidak berjalan dari sebuah .app.');
    try {
      fs.accessSync(path.dirname(bundle), fs.constants.W_OK);
    } catch (_) {
      throw new Error('Folder ' + path.dirname(bundle) + ' tidak bisa ditulis. Pasang versi baru dari file .dmg di halaman rilis.');
    }
    const stage = path.join(this.dir, 'stage');
    fs.rmSync(stage, { recursive: true, force: true });
    fs.mkdirSync(stage, { recursive: true });
    await new Promise((resolve, reject) => {
      const p = spawn('/usr/bin/ditto', ['-x', '-k', this.file, stage]);
      p.on('exit', (code) => (code === 0 ? resolve() : reject(new Error('Gagal membuka arsip pembaruan.'))));
    });
    const fresh = fs.readdirSync(stage).find((n) => n.endsWith('.app'));
    if (!fresh) throw new Error('Arsip pembaruan tidak berisi aplikasi.');
    const script = path.join(this.dir, 'install.sh');
    fs.writeFileSync(script, [
      '#!/bin/bash',
      // Wait for this Twincxt to quit (logins are saved on quit), up to 60 s.
      `for i in $(seq 1 120); do kill -0 ${process.pid} 2>/dev/null || break; sleep 0.5; done`,
      `NEW=${JSON.stringify(path.join(stage, fresh))}`,
      `APP=${JSON.stringify(bundle)}`,
      'OLD="$APP.old-$$"',
      'mv "$APP" "$OLD" && mv "$NEW" "$APP" && rm -rf "$OLD" || { [ -d "$OLD" ] && mv "$OLD" "$APP"; }',
      'xattr -dr com.apple.quarantine "$APP" 2>/dev/null',
      'open "$APP"',
      '',
    ].join('\n'), { mode: 0o755 });
    spawn('/bin/bash', [script], { detached: true, stdio: 'ignore' }).unref();
    this.set({ status: 'installing' });
    setTimeout(() => this.quit(), 800);
  }

  installAppImage() {
    const current = process.env.APPIMAGE;
    const staged = path.join(path.dirname(current), '.' + path.basename(current) + '.new');
    fs.copyFileSync(this.file, staged);
    fs.chmodSync(staged, 0o755);
    fs.renameSync(staged, current); // atomic on the same disk; the running copy keeps its inode
    spawn(current, [], { detached: true, stdio: 'ignore' }).unref();
    this.set({ status: 'installing' });
    setTimeout(() => this.quit(), 800);
  }

  installDeb() {
    const deb = this.file;
    const hasPkexec = fs.existsSync('/usr/bin/pkexec');
    const script = path.join(this.dir, 'install.sh');
    fs.writeFileSync(script, hasPkexec ? [
      '#!/bin/bash',
      `for i in $(seq 1 120); do kill -0 ${process.pid} 2>/dev/null || break; sleep 0.5; done`,
      `if pkexec apt-get install -y ${JSON.stringify(deb)}; then nohup twincxt >/dev/null 2>&1 & else xdg-open ${JSON.stringify(deb)}; fi`,
      '',
    ].join('\n') : ['#!/bin/bash', `xdg-open ${JSON.stringify(deb)}`, ''].join('\n'), { mode: 0o755 });
    spawn('/bin/bash', [script], { detached: true, stdio: 'ignore' }).unref();
    this.set({ status: 'installing' });
    if (hasPkexec) setTimeout(() => this.quit(), 800);
  }

  skip() {
    if (!this.state.version || this.state.required) return;
    const cfg = this.getConfig();
    cfg.skippedVersion = this.state.version;
    this.saveConfig();
    this.set({}, true);
  }
}

module.exports = { Updater, newer };
