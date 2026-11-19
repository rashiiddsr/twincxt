// Injected into every WhatsApp / Telegram document before the page's own code.
// Shared by Windows (WebView2), macOS (WKWebView) and Linux (WebKitGTK). The host replaces
// the service id and the call-window flag below.
(function () {
  var SVC = '__SVC__';
  // WebView2 on Windows; on macOS and Linux the host defines window.twincxtBridge.
  var bridge = (window.chrome && window.chrome.webview) || window.twincxtBridge || null;
  function post(o) {
    try { if (bridge) bridge.postMessage(JSON.stringify(o)); } catch (e) {}
  }

  // ---- notifications -------------------------------------------------------
  function NativeNotification(title, options) {
    options = options || {};
    post({ type: 'notify', title: String(title || ''), body: String(options.body || '') });
    this.title = title;
    this.body = options.body || '';
    this.tag = options.tag || '';
    this.data = options.data;
    this.onclick = this.onclose = this.onerror = this.onshow = null;
    var self = this;
    setTimeout(function () { if (typeof self.onshow === 'function') self.onshow(); }, 0);
  }
  NativeNotification.prototype.close = function () {};
  NativeNotification.prototype.addEventListener = function () {};
  NativeNotification.prototype.removeEventListener = function () {};
  NativeNotification.prototype.dispatchEvent = function () { return true; };
  NativeNotification.permission = 'granted';
  NativeNotification.maxActions = 0;
  NativeNotification.requestPermission = function (cb) {
    if (typeof cb === 'function') cb('granted');
    return Promise.resolve('granted');
  };
  try {
    Object.defineProperty(window, 'Notification', { value: NativeNotification, writable: true, configurable: true });
  } catch (e) { window.Notification = NativeNotification; }
  if (window.ServiceWorkerRegistration) {
    ServiceWorkerRegistration.prototype.showNotification = function (title, options) {
      post({ type: 'notify', title: String(title || ''), body: String((options && options.body) || '') });
      return Promise.resolve();
    };
    ServiceWorkerRegistration.prototype.getNotifications = function () { return Promise.resolve([]); };
  }
  if (navigator.permissions && navigator.permissions.query) {
    var query = navigator.permissions.query.bind(navigator.permissions);
    navigator.permissions.query = function (desc) {
      if (desc && desc.name === 'notifications') {
        return Promise.resolve({ state: 'granted', status: 'granted', onchange: null,
          addEventListener: function () {}, removeEventListener: function () {} });
      }
      return query(desc);
    };
  }

  if (window.top !== window || __POPUP__) return;

  // ---- privacy blur ----------------------------------------------------------
  // Blurs parts of the page until the pointer is over them. The host calls
  // window.twincxtPrivacy({ blurMessages, blurPreviews, blurNames, blurPhotos,
  // blurMedia }) after every page load and whenever a setting changes; the last
  // value is kept in this site's localStorage so the blur is applied from the
  // first paint of the next load.
  var BLUR = {
    wa: {
      blurMessages: '#main .copyable-text, #main .selectable-text, #main .quoted-mention',
      blurPreviews: '#pane-side span[title]:not([dir="auto"]), #pane-side span[dir="ltr"]',
      blurNames: '#pane-side span[dir="auto"][title], #main header span[dir="auto"], #main .message-in span[aria-label][dir="auto"]',
      blurPhotos: 'img[src*="pps.whatsapp.net"], #pane-side [role="gridcell"] img, #main header img',
      blurMedia: '#main img[src^="blob:"], #main img[src^="data:"], #main video, #main [data-testid*="media"], #main canvas'
    },
    tg: {
      blurMessages: '.Message .text-content, .Message .message-content .text, .bubble .message, .bubble .reply-subtitle',
      blurPreviews: '.ListItem .last-message, .ListItem .subtitle, .chatlist-chat .row-subtitle, .chatlist-chat .dialog-subtitle',
      blurNames: '.ListItem .fullName, .ListItem .title h3, .MiddleHeader .fullName, .chatlist-chat .peer-title, .chat-info .peer-title, .message-title-name',
      blurPhotos: '.Avatar, .avatar, .avatar-like, .chat-info .avatar',
      blurMedia: '.Message .media-inner, .Message .Photo, .Message .Video, .Message video, .bubble .media-container, .bubble .media-photo, .bubble video, .bubble .media-video'
    }
  }[SVC] || {};
  var privacyStyle = null;
  function applyPrivacy(p) {
    p = p || {};
    var parts = [];
    Object.keys(BLUR).forEach(function (k) {
      if (!p[k]) return;
      parts.push(BLUR[k].split(',').map(function (s) {
        s = s.trim();
        return s + ':not(:hover)';
      }).join(',\n') + ' { filter: blur(7px) !important; transition: filter .12s ease !important; }');
    });
    if (!privacyStyle) {
      privacyStyle = document.createElement('style');
      privacyStyle.id = 'twincxt-privacy';
    }
    privacyStyle.textContent = parts.join('\n');
    var parent = document.head || document.documentElement;
    if (parent && privacyStyle.parentNode !== parent) parent.appendChild(privacyStyle);
  }
  window.twincxtPrivacy = function (p) {
    try { localStorage.setItem('twincxt-privacy', JSON.stringify(p || {})); } catch (e) {}
    applyPrivacy(p);
  };
  try { applyPrivacy(JSON.parse(localStorage.getItem('twincxt-privacy') || '{}')); } catch (e) {}
  // The page may replace <head>; put the style back if it goes.
  document.addEventListener('DOMContentLoaded', function () {
    var p = {};
    try { p = JSON.parse(localStorage.getItem('twincxt-privacy') || '{}'); } catch (e) {}
    applyPrivacy(p);
    new MutationObserver(function () {
      if (privacyStyle && !privacyStyle.isConnected) (document.head || document.documentElement).appendChild(privacyStyle);
    }).observe(document.documentElement, { childList: true });
  });

  // ---- call windows only on request -----------------------------------------
  // WhatsApp opens its call window as soon as a call starts. In Twincxt the
  // call stays in the main window; a separate window opens only when you
  // click "Move to new window". External links are not affected.
  if (SVC === 'wa') {
    var popoutUntil = 0, lastLabel = '';
    var POPOUT = /new window|jendela baru|nueva ventana|nova janela|neues fenster|nouvelle fen/i;
    function noteClick(e) {
      var el = e.target && e.target.closest && e.target.closest('button,[role="button"],a,[aria-label],[title]');
      if (!el) return;
      lastLabel = ((el.getAttribute('aria-label') || '') + ' ' + (el.getAttribute('title') || '') + ' ' +
        (el.textContent || '')).replace(/\s+/g, ' ').trim().slice(0, 80);
      popoutUntil = POPOUT.test(lastLabel) ? Date.now() + 3000 : 0;
    }
    document.addEventListener('pointerdown', noteClick, true);
    document.addEventListener('click', noteClick, true);
    var nativeOpen = window.open;
    window.open = function (url) {
      var u = url == null ? '' : String(url);
      var internal = u === '' || u.indexOf('about:') === 0 || u.charAt(0) === '/' ||
        u.indexOf(location.origin) === 0;
      if (internal && Date.now() > popoutUntil) {
        post({ type: 'popupBlocked', label: lastLabel });
        return null;
      }
      return nativeOpen.apply(window, arguments);
    };
  }

  // ---- title strip colour --------------------------------------------------
  var last = '';
  function parse(c) {
    var m = /rgba?\(([^)]+)\)/.exec(c || '');
    if (!m) return null;
    var p = m[1].split(',').map(parseFloat);
    if (p.length > 3 && p[3] < 0.5) return null;
    return [Math.round(p[0]), Math.round(p[1]), Math.round(p[2])];
  }
  function colorAt(x, y) {
    var el = document.elementFromPoint(x, y);
    while (el && el.nodeType === 1) {
      var c = parse(getComputedStyle(el).backgroundColor);
      if (c) return c;
      el = el.parentElement;
    }
    return null;
  }
  function sync() {
    if (!document.body) return;
    var w = window.innerWidth, votes = {}, best = null, n = 0;
    [0.15, 0.4, 0.7, 0.92].forEach(function (f) {
      var c = colorAt(Math.floor(w * f), 1);
      if (!c) return;
      var k = c.join(',');
      votes[k] = (votes[k] || 0) + 1;
      if (votes[k] > n) { n = votes[k]; best = k; }
    });
    if (!best) {
      var b = parse(getComputedStyle(document.body).backgroundColor);
      if (b) best = b.join(',');
    }
    if (!best || best === last) return;
    last = best;
    var c = best.split(',').map(Number);
    post({ type: 'frame', r: c[0], g: c[1], b: c[2] });
  }
  function start() {
    sync();
    new MutationObserver(sync).observe(document.documentElement, { attributes: true, subtree: false });
    new MutationObserver(sync).observe(document.body, { attributes: true, attributeFilter: ['class', 'style'] });
    setInterval(sync, 1500);
    window.addEventListener('resize', sync);
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
