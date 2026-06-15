#!/usr/bin/env node
// Download the prebuilt remote-au binary for the host platform from the matching
// GitHub Release and place it at vendor/<exe>. Runs as the package's postinstall.
//
// The release tag is derived from this package's version (v<version>), and the
// asset name from the host's platform/arch. The repository is public, so the
// download needs no authentication.
//
// Skipped (exit 0) when: REMOTE_AU_SKIP_DOWNLOAD is set, the version is the
// 0.0.0 dev sentinel, the binary already exists, or the platform is unsupported
// (the launcher then reports a clear runtime error).
import { createWriteStream } from 'node:fs';
import { mkdir, chmod, stat, rm } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';

const here = dirname(fileURLToPath(import.meta.url));
const pkgRoot = join(here, '..');
const { version } = createRequire(import.meta.url)(join(pkgRoot, 'package.json'));

const SUPPORTED = new Set(['darwin-arm64', 'linux-x64', 'linux-arm64', 'win32-x64']);
const platform = process.platform;
const arch = process.arch;
const key = `${platform}-${arch}`;
const exe = platform === 'win32' ? 'remote-au.exe' : 'remote-au';
const asset = platform === 'win32' ? `remote-au-${key}.exe` : `remote-au-${key}`;
const dest = join(pkgRoot, 'vendor', exe);
const url = `https://github.com/leaperone/remote-au/releases/download/v${version}/${asset}`;

function skip(reason) {
  console.log(`remote-au: skipping binary download (${reason}).`);
  process.exit(0);
}

async function exists(p) {
  try { await stat(p); return true; } catch { return false; }
}

async function main() {
  if (process.env.REMOTE_AU_SKIP_DOWNLOAD) skip('REMOTE_AU_SKIP_DOWNLOAD set');
  if (version === '0.0.0') skip('dev version 0.0.0 has no published release');
  if (!SUPPORTED.has(key)) skip(`unsupported platform ${key}`);
  if (await exists(dest)) skip(`binary already present at ${dest}`);

  console.log(`remote-au: downloading ${asset} (v${version})...`);
  const res = await fetch(url, { redirect: 'follow' });
  if (!res.ok || !res.body) {
    throw new Error(`download failed: HTTP ${res.status} ${res.statusText} for ${url}`);
  }

  await mkdir(dirname(dest), { recursive: true });
  const tmp = `${dest}.download`;
  try {
    await pipeline(Readable.fromWeb(res.body), createWriteStream(tmp));
    if (platform !== 'win32') await chmod(tmp, 0o755);
    const { rename } = await import('node:fs/promises');
    await rename(tmp, dest);
  } catch (err) {
    await rm(tmp, { force: true });
    throw err;
  }
  console.log(`remote-au: installed binary at ${dest}`);
}

main().catch((err) => {
  console.error(`remote-au: failed to download prebuilt binary: ${err.message}`);
  console.error(`  tried: ${url}`);
  console.error('  You can retry the install, set REMOTE_AU_SKIP_DOWNLOAD=1 to skip,');
  console.error('  or build from source: https://github.com/leaperone/remote-au');
  process.exit(1);
});
