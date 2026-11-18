// Runs before WhatsApp / Telegram's own code in every page and call window.
// It exposes the message bridge and runs the shared page script (ui/service.js)
// in the page itself, so window.Notification and window.open are replaced
// before the page uses them.
const { contextBridge, ipcRenderer, webFrame } = require('electron');

contextBridge.exposeInMainWorld('twincxtBridge', {
  postMessage: (msg) => ipcRenderer.send('svc-msg', String(msg)),
});

// The main process knows whether this is the WhatsApp or Telegram view, or a
// call window, and returns the script with that filled in.
const code = ipcRenderer.sendSync('service-init');
if (code) webFrame.executeJavaScript(code);
