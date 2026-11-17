// Entry point: launch the Go forge binary and wait for it.
//
// The forge itself is a statically linked Linux amd64 Go binary committed
// in this repository as vendored source (the kit allows a committed static
// binary to run from the repository). This file only spawns it — the binary
// binds the ephemeral port, registers with the trial env, serves the whole
// forge (git smart HTTP, REST v3, GraphQL v4, MCP) and exits 0 when the env
// says the trial is done. This process exits with the binary's code.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const dir = path.dirname(fileURLToPath(import.meta.url));
const child = spawn(path.join(dir, 'forge'), [], {
  stdio: 'inherit',
  env: process.env,
});

child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start the forge binary: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});

// If the harness stops this process, stop the forge too.
for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}
