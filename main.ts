// Entry point: spawns the vendored Go core (./forge) and waits for it.
// The Go binary owns the whole lifecycle: it binds an ephemeral port (never
// 8080), POSTs $MAKEFUN_ENV_URL/register {"port": N}, polls
// GET $MAKEFUN_ENV_URL/done until {"done": true}, then exits 0. This file
// only forwards its exit code. No network, no model call, stdlib only.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const binary = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
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
