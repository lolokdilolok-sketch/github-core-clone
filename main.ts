// Entry point: spawns the vendored Go core (./forge) and waits for it.
// The Go binary owns the whole lifecycle: it binds an ephemeral port (never
// 8080), POSTs $MAKEFUN_ENV_URL/register {"port": N}, polls
// GET $MAKEFUN_ENV_URL/done until {"done": true}, then exits 0. This file
// only forwards its exit code. No network, no model call, stdlib only.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { accessSync, chmodSync, copyFileSync, constants, mkdtempSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const binary = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

// The forge binary is uploaded to the repo through the GitHub web UI, which
// stores every file with mode 100644 and strips the executable bit. Worse,
// the grading pod mounts the repository read-only (/bot), so a plain
// chmodSync(binary, 0o755) fails there with EROFS and the spawn then fails
// with EACCES — the entry never registers and scores 0 on every column
// (reproduced locally under a read-only bind mount: "[entry] warning: could
// not chmod forge: EROFS" followed by "failed to start forge: EACCES").
// So: first try to re-apply the exec bit in place (works when the checkout
// is writable). If the binary is still not executable afterwards, copy it to
// a fresh writable directory under os.tmpdir(), chmod the copy, and spawn
// the copy instead. The copy is byte-identical to the vendored binary; the
// entry's behaviour does not change, only where the executable lives.
function resolveExecutable(): string {
  try {
    chmodSync(binary, 0o755);
  } catch (err) {
    process.stderr.write(`[entry] warning: could not chmod forge: ${(err as Error).message}\n`);
  }
  try {
    accessSync(binary, constants.X_OK);
    return binary;
  } catch {
    // Not executable in place (read-only checkout): fall back to a copy.
  }
  try {
    const dir = mkdtempSync(path.join(os.tmpdir(), 'forge-'));
    const copy = path.join(dir, 'forge');
    copyFileSync(binary, copy);
    chmodSync(copy, 0o755);
    process.stderr.write(`[entry] forge copied to writable ${copy}\n`);
    return copy;
  } catch (err) {
    // Last resort: try the original path anyway so the real error surfaces.
    process.stderr.write(`[entry] warning: could not stage a writable forge copy: ${(err as Error).message}\n`);
    return binary;
  }
}

const executable = resolveExecutable();

const child = spawn(executable, [], { stdio: 'inherit', env: process.env });
child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start forge: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
