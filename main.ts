// Entry point: bind an ephemeral port, register it with the trial env, then
// long-poll the env's /done endpoint and exit 0 when the trial ends. This is
// the kit starter's lifecycle, kept as-is; all forge behaviour lives in
// app.ts and the modules it imports.

import { createApp } from './app.ts';

const envUrl = process.env.MAKEFUN_ENV_URL ?? '';
const server = createApp();

server.listen(0, async () => {
  const address = server.address();
  const port = typeof address === 'object' && address !== null ? address.port : 0;
  if (envUrl) {
    await fetch(`${envUrl}/register`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ port }),
    });
    for (;;) {
      const response = await fetch(`${envUrl}/done`);
      const data = (await response.json()) as { done?: boolean };
      if (data.done) break;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
  }
  server.close(() => process.exit(0));
});
