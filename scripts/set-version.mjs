#!/usr/bin/env node
// Sync the release version across the main package.json: its own "version" and
// every entry in "optionalDependencies" (the per-platform packages are published
// at the same version). Usage: node scripts/set-version.mjs <version>
import { readFileSync, writeFileSync } from 'node:fs';

const version = process.argv[2];
if (!version || !/^\d+\.\d+\.\d+(-[\w.]+)?$/.test(version)) {
  console.error(`set-version: invalid version ${JSON.stringify(version)}`);
  process.exit(1);
}

const path = new URL('../package.json', import.meta.url);
const pkg = JSON.parse(readFileSync(path, 'utf8'));
pkg.version = version;
for (const dep of Object.keys(pkg.optionalDependencies ?? {})) {
  pkg.optionalDependencies[dep] = version;
}
writeFileSync(path, JSON.stringify(pkg, null, 2) + '\n');
console.log(`set main package + ${Object.keys(pkg.optionalDependencies ?? {}).length} optional deps to ${version}`);
