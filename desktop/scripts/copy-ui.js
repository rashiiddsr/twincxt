// Copies the UI shared with the Windows app (../ui) into this folder before a
// run or a build, so all three systems use the same shell and page script.
const fs = require('fs');
const path = require('path');

const from = path.join(__dirname, '..', '..', 'ui');
const to = path.join(__dirname, '..', 'ui');

fs.rmSync(to, { recursive: true, force: true });
fs.mkdirSync(path.join(to, 'icons'), { recursive: true });
for (const f of ['shell.html', 'service.js']) {
  fs.copyFileSync(path.join(from, f), path.join(to, f));
}
for (const f of fs.readdirSync(path.join(from, 'icons'))) {
  fs.copyFileSync(path.join(from, 'icons', f), path.join(to, 'icons', f));
}
console.log('UI copied from', from);
