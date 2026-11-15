// The HTTP router: one node:http server that dispatches to the git smart-HTTP
// surface, REST v3, GraphQL v4 and MCP, plus the lifecycle register/done
// endpoints the trial env expects. Every reply is JSON unless it is a git
// pack/advertisement, every reply carries X-GitHub-Request-Id, and the
// process never dies on a bad request body.

import { createServer } from 'node:http';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { handleGraphQL } from './graphql.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './githttp.ts';
import { handleMcp } from './mcp.ts';
import { handleRest } from './rest.ts';
import type { RestRequest } from './rest.ts';
import { VIEWER } from './scenario.ts';
import { Store } from './store.ts';

const SHOP_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const store = new Store();
let requestCounter = 0;

function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter.toString(36)}-${Date.now().toString(36)}`;
}

function sendJson(res: ServerResponse, status: number, body: unknown, extraHeaders: Record<string, string> = {}): void {
  const payload = body === null ? '' : JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json',
    'x-github-request-id': nextRequestId(),
    ...extraHeaders,
  });
  res.end(payload);
}

function sendBytes(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'x-github-request-id': nextRequestId() });
  res.end(body);
}

function readBody(req: IncomingMessage): Promise<{ raw: Buffer; parsed: unknown; parseError: boolean }> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => {
      const raw = Buffer.concat(chunks);
      if (raw.length === 0) return resolve({ raw, parsed: null, parseError: false });
      try {
        resolve({ raw, parsed: JSON.parse(raw.toString('utf8')), parseError: false });
      } catch {
        resolve({ raw, parsed: null, parseError: true });
      }
    });
    req.on('error', () => resolve({ raw: Buffer.alloc(0), parsed: null, parseError: true }));
  });
}

function bearerToken(req: IncomingMessage): string {
  const header = req.headers.authorization ?? '';
  return header.startsWith('Bearer ') ? header.slice(7) : '';
}

/** True when the caller may use an authenticated surface. */
function authorized(req: IncomingMessage): boolean {
  if (!SHOP_TOKEN) return true;
  return bearerToken(req) === SHOP_TOKEN;
}

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'localhost'}`);
  const method = req.method ?? 'GET';
  const host = req.headers.host ?? 'localhost';
  const baseUrl = `http://${host}`;
  const segments = url.pathname.split('/').filter(Boolean).map(decodeSegment);

  try {
    // ---- health ---------------------------------------------------------------------------
    if (url.pathname === '/healthz' && method === 'GET') {
      sendJson(res, 200, { ok: true });
      return;
    }

    // ---- git smart HTTP -------------------------------------------------------------------
    const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(url.pathname);
    if (gitMatch) {
      const owner = decodeSegment(gitMatch[1] as string);
      const repoName = decodeSegment(gitMatch[2] as string);
      const action = gitMatch[3] as string;
      if (owner !== VIEWER.login) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      const repo = store.getRepo(owner, repoName);
      if (!repo) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      const protocol = req.headers['git-protocol'] ?? null;
      const version2 = typeof protocol === 'string' && protocol.includes('version=2') ? '2' : null;
      if (action === 'info/refs' && method === 'GET') {
        const service = url.searchParams.get('service') ?? '';
        const result = handleInfoRefs(repo, service, version2);
        sendBytes(res, result.status, result.contentType, result.body);
        return;
      }
      if (action === 'git-upload-pack' && method === 'POST') {
        const { raw } = await readBody(req);
        const result = handleUploadPack(repo, raw, version2);
        sendBytes(res, result.status, result.contentType, result.body);
        return;
      }
      if (action === 'git-receive-pack' && method === 'POST') {
        const { raw } = await readBody(req);
        const result = handleReceivePack(store, repo, raw);
        sendBytes(res, result.status, result.contentType, result.body);
        return;
      }
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }

    // ---- GraphQL v4 -----------------------------------------------------------------------
    if (url.pathname === '/api/graphql' && method === 'POST') {
      if (!authorized(req)) {
        sendJson(res, 401, { message: 'Requires authentication' });
        return;
      }
      const { parsed, parseError } = await readBody(req);
      if (parseError) {
        sendJson(res, 400, { errors: [{ message: 'Invalid JSON in request body' }] });
        return;
      }
      const result = handleGraphQL(store, parsed);
      sendJson(res, result.status, result.body);
      return;
    }

    // ---- MCP ------------------------------------------------------------------------------
    if (url.pathname === '/mcp' && method === 'POST') {
      if (!authorized(req)) {
        sendJson(res, 401, { message: 'Requires authentication' });
        return;
      }
      const { parsed, parseError } = await readBody(req);
      const result = handleMcp(store, parsed, parseError);
      if (result.body === null) {
        res.writeHead(result.status, { 'x-github-request-id': nextRequestId() });
        res.end();
        return;
      }
      sendJson(res, result.status, result.body);
      return;
    }

    // ---- REST v3 --------------------------------------------------------------------------
    if (segments[0] === 'api' && segments[1] === 'v3') {
      const isRootOrMeta = segments.length <= 3 && (segments[2] === undefined || segments[2] === 'meta');
      if (!isRootOrMeta && !authorized(req)) {
        sendJson(res, 401, { message: 'Requires authentication' });
        return;
      }
      const { parsed, parseError } = await readBody(req);
      const restReq: RestRequest = {
        method,
        segments: segments.slice(2),
        query: url.searchParams,
        body: parsed,
        bodyParseError: parseError,
        baseUrl: `${baseUrl}/api/v3`,
      };
      const result = handleRest(store, restReq);
      if (result.status === 204) {
        res.writeHead(204, { 'x-github-request-id': nextRequestId(), ...(result.headers ?? {}) });
        res.end();
        return;
      }
      if (result.rawJson !== undefined) {
        res.writeHead(result.status, {
          'content-type': 'application/json',
          'x-github-request-id': nextRequestId(),
          ...(result.headers ?? {}),
        });
        res.end(result.rawJson);
        return;
      }
      sendJson(res, result.status, result.body, result.headers);
      return;
    }

    sendJson(res, 404, { message: 'Not Found' });
  } catch (error) {
    // A handler must never kill the server: answer 500 JSON and keep going.
    const message = error instanceof Error ? error.message : 'Internal Server Error';
    if (!res.headersSent) sendJson(res, 500, { message });
    else res.end();
  }
}

export function createApp(): ReturnType<typeof createServer> {
  return createServer((req, res) => {
    void handle(req, res);
  });
}
