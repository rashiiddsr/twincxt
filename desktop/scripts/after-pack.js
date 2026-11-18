// macOS: give the app an ad-hoc signature after packaging.
//
// Twincxt has no Apple Developer ID, so the build is not signed by Apple.
// Packaging changes Electron's bundle (name, icon, Info.plist), which breaks
// the signature Electron ships with; on Apple Silicon an app with a broken
// signature is reported as "damaged" and cannot be opened at all. An ad-hoc
// signature fixes that. macOS will still ask once to confirm opening an app
// from an unidentified developer (see the release notes).
const { execFileSync } = require('child_process');
const path = require('path');

exports.default = async function afterPack(context) {
  if (context.electronPlatformName !== 'darwin') return;
  const app = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  console.log('ad-hoc signing', app);
  execFileSync('codesign', ['--force', '--deep', '--sign', '-', app], { stdio: 'inherit' });
  execFileSync('codesign', ['--verify', '--deep', '--strict', app], { stdio: 'inherit' });
};
