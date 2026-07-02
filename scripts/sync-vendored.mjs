#!/usr/bin/env node
/**
 * Sync the vendored copies this repo carries from a nanohype checkout — the
 * single source of truth for runtime modules, org tooling config, the
 * tenant-chart-base library chart, and this script itself.
 *
 * Driven by the manifest next to this file (`scripts/vendored.json`): each
 * entry declares a source path relative to the nanohype checkout and a
 * destination path relative to the repo root; `"dir": true` entries sync a
 * whole directory tree. Directories listed in `exclusiveDirs` may contain
 * only manifest-listed files — anything else is drift, so unconsumed modules
 * can't accumulate.
 *
 * The script is itself a manifest entry (`library/scripts/sync-vendored.mjs`
 * upstream), so fixes to the sync machinery propagate outward like every
 * other vendored surface.
 *
 *   node scripts/sync-vendored.mjs            # (re)write the vendored copies
 *   node scripts/sync-vendored.mjs --check    # CI gate: exit 1 if any copy drifted
 *
 * The nanohype checkout is resolved from $NANOHYPE_DIR, defaulting to a
 * sibling checkout at ../nanohype (CI checks out nanohype/nanohype and points
 * NANOHYPE_DIR at it). Copies are byte-identical to their source — behavior
 * changes land upstream with their tests, then re-sync; a copy that drifts
 * from the source is the defect.
 */
import { cp, copyFile, mkdir, readdir, readFile, rm, stat } from 'node:fs/promises';
import { dirname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const ROOT = join(SCRIPT_DIR, '..');
const NANOHYPE_DIR = process.env.NANOHYPE_DIR ?? join(ROOT, '..', 'nanohype');
const CHECK = process.argv.includes('--check');

/** Recursively list files under a dir, relative to it (sorted). */
async function listFiles(dir, base = dir) {
  const out = [];
  for (const e of await readdir(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) out.push(...(await listFiles(p, base)));
    else out.push(relative(base, p));
  }
  return out.sort();
}

async function readOrNull(path) {
  try {
    return await readFile(path, 'utf8');
  } catch {
    return null;
  }
}

/** @returns {Promise<number>} drift count for one file entry. */
async function syncFile(srcPath, destPath) {
  const rel = relative(ROOT, destPath);
  if (CHECK) {
    const src = await readFile(srcPath, 'utf8');
    if (src === (await readOrNull(destPath))) {
      console.log(`ok  ${rel}`);
      return 0;
    }
    console.error(`DRIFT  ${rel} — run \`npm run sync:vendored\``);
    return 1;
  }
  await mkdir(dirname(destPath), { recursive: true });
  await copyFile(srcPath, destPath);
  console.log(`vendored ${rel}`);
  return 0;
}

/** @returns {Promise<number>} drift count for one directory entry. */
async function syncDir(srcDir, destDir) {
  const rel = relative(ROOT, destDir);
  if (CHECK) {
    const srcFiles = await listFiles(srcDir);
    let copyFiles;
    try {
      copyFiles = await listFiles(destDir);
    } catch {
      copyFiles = null;
    }
    const sameList = copyFiles !== null && copyFiles.join('\n') === srcFiles.join('\n');
    const sameBytes =
      sameList &&
      (
        await Promise.all(
          srcFiles.map(
            async (f) =>
              (await readFile(join(srcDir, f), 'utf8')) === (await readOrNull(join(destDir, f))),
          ),
        )
      ).every(Boolean);
    if (sameBytes) {
      console.log(`ok  ${rel}`);
      return 0;
    }
    console.error(`DRIFT  ${rel} — run \`npm run sync:vendored\``);
    return 1;
  }
  await rm(destDir, { recursive: true, force: true });
  await cp(srcDir, destDir, { recursive: true });
  console.log(`vendored ${rel}`);
  return 0;
}

/** @returns {Promise<number>} drift count for unlisted files in an exclusive dir. */
async function checkExclusive(dir, allowedDests) {
  const abs = join(ROOT, dir);
  let present = [];
  try {
    present = await listFiles(abs);
  } catch {
    present = [];
  }
  let drift = 0;
  for (const f of present) {
    const rel = [dir, f].join(sep);
    if (!allowedDests.has(rel)) {
      console.error(`DRIFT  ${rel} — not in the vendored manifest (scripts/vendored.json)`);
      drift++;
    }
  }
  return drift;
}

async function main() {
  try {
    await stat(NANOHYPE_DIR);
  } catch {
    console.error(`nanohype checkout not found at ${NANOHYPE_DIR} — set NANOHYPE_DIR`);
    process.exit(2);
  }

  const manifest = JSON.parse(await readFile(join(SCRIPT_DIR, 'vendored.json'), 'utf8'));
  const entries = manifest.entries ?? [];
  const exclusiveDirs = manifest.exclusiveDirs ?? [];

  let drift = 0;

  if (!CHECK) {
    for (const dir of exclusiveDirs) {
      await rm(join(ROOT, dir), { recursive: true, force: true });
    }
  }

  for (const entry of entries) {
    const src = join(NANOHYPE_DIR, entry.src);
    const dest = join(ROOT, entry.dest);
    drift += entry.dir ? await syncDir(src, dest) : await syncFile(src, dest);
  }

  if (CHECK) {
    const allowedDests = new Set(entries.map((e) => e.dest.split('/').join(sep)));
    for (const dir of exclusiveDirs) {
      drift += await checkExclusive(dir, allowedDests);
    }
    if (drift > 0) process.exit(1);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
