#!/usr/bin/env node
// Assemble a per-platform npm package around a freshly built Go binary.
//
// Usage:
//   node scripts/make-platform-pkg.mjs \
//     --platform darwin --arch arm64 --binary ./remote-au --version 1.2.3 --outdir npm
//
// Produces: <outdir>/remote-au-<platform>-<arch>/{package.json, bin/remote-au[.exe]}
// The package declares "os"/"cpu" so npm only installs it on the matching host,
// and is referenced by the main "remote-au" package via optionalDependencies.
import { mkdirSync, copyFileSync, writeFileSync, chmodSync } from 'node:fs';
import { join } from 'node:path';

function arg(name) {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 ? process.argv[i + 1] : undefined;
}

const platform = arg('platform');
const arch = arg('arch');
const binary = arg('binary');
const version = arg('version');
const outdir = arg('outdir') ?? 'npm';

for (const [k, v] of Object.entries({ platform, arch, binary, version })) {
  if (!v) {
    console.error(`make-platform-pkg: missing --${k}`);
    process.exit(1);
  }
}

const exe = platform === 'win32' ? 'remote-au.exe' : 'remote-au';
const pkgName = `remote-au-${platform}-${arch}`;
const pkgDir = join(outdir, pkgName);
const binDir = join(pkgDir, 'bin');
mkdirSync(binDir, { recursive: true });

const dest = join(binDir, exe);
copyFileSync(binary, dest);
chmodSync(dest, 0o755);

const pkg = {
  name: pkgName,
  version,
  description: `remote-au prebuilt binary for ${platform}-${arch}`,
  os: [platform],
  cpu: [arch],
  files: [`bin/${exe}`],
  license: 'MIT',
  repository: { type: 'git', url: 'git+https://github.com/leaperone/remote-au.git' },
};
writeFileSync(join(pkgDir, 'package.json'), JSON.stringify(pkg, null, 2) + '\n');
console.log(`wrote ${pkgDir} (${pkgName}@${version})`);
