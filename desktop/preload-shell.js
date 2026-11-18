// Bridge for the shell (sidebar, title strip, settings): the shared shell.html
// talks to its host through window.twincxtHost.postMessage, the same shape as
// WebView2's window.chrome.webview on Windows.
const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('twincxtHost', {
  postMessage: (msg) => ipcRenderer.send('shell-msg', String(msg)),
});
