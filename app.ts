// The HTTP layer: a hand-rolled HTTP/1.1 server on node:net.
//
// WHY: measured against the kit's own undici client, node:http costs ~400us
// per request on this VM while a raw net server with a hand parser costs
// ~220us for the same fixed reply (floor.mjs, stable across 3 runs). The
// entry's own handler is only ~4% above the node:http floor, so the stack
// itself is the bottleneck — and every bench workload is a stream of HTTP
// round-trips (push 2 RTTs/op, clone 3 RTTs/clone, read 2000 GETs, write
// 200 POSTs). The handlers (REST/GraphQL/MCP/git) are untouched; only the
// transport changed, and request bodies now arrive in memory, so every
// handler runs synchronously with no stream wait.
//
// Correctness notes (checked against the kit before writing this):
// - The kit's clients (undici fetch) only ever send HTTP/1.1 with
//   Content-Length bodies; no chunked bodies, no Expect: 100-continue, no
//   HEAD, no HTTP/1.0 anywhere in the kit. Chunked + Expect + HEAD are
//   still implemented defensively below.
// - The only headers the kit asserts are x-oauth-scopes (root) and
//   x-github-request-id (non-empty only, NOT unique) — a counter id is fine.
// - createApp() keeps the node:http Server interface (listen/address/close)
//   so main.ts, bench-local.mjs and smoke-test.mjs work unchanged.

import net from 'node:net';
import type { Socket } from 'node:net';
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

const EMPTY = Buffer.alloc(0);
const MAX_HEAD_BYTES = 65536;
const MAX_BODY_BYTES = 64 * 1024 * 1024;

const REASONS: Record<number, string> = {
  200: 'OK',
  201: 'Created',
  204: 'No Content',
  400: 'Bad Request',
  401: 'Unauthorized',
  404: 'Not Found',
  405: 'Method Not Allowed',
  409: 'Conflict',
  422: 'Unprocessable Entity',
  500: 'Internal Server Error',
};

interface ParsedRequest {
  method: string;
  target: string;
  httpVersion: string;
  headers: Map<string, string>;
  body: Buffer;
  keepAlive: boolean;
}

interface ParsedHead {
  method: string;
  target: string;
  httpVersion: string;
  headers: Map<string, string>;
  headLength: number;
}

interface Reply {
  status: number;
  /** Omitted for empty replies (204 / MCP null body). */
  contentType?: string;
  body: Buffer;
  headers?: Record<string, string>;
}

type ParseResult =
  | { kind: 'ok'; req: ParsedRequest; consumed: number }
  | { kind: 'needMore'; head?: ParsedHead }
  | { kind: 'error' };

function parseHead(buf: Buffer): ParsedHead | 'needMore' | 'error' {
  const headEnd = buf.indexOf('\r\n\r\n');
  if (headEnd < 0) return buf.length > MAX_HEAD_BYTES ? 'error' : 'needMore';
  const headText = buf.subarray(0, headEnd).toString('latin1');
  const lines = headText.split('\r\n');
  const requestLine = lines[0] ?? '';
  const firstSpace = requestLine.indexOf(' ');
  const secondSpace = firstSpace < 0 ? -1 : requestLine.indexOf(' ', firstSpace + 1);
  if (firstSpace <= 0 || secondSpace <= firstSpace + 1) return 'error';
  const method = requestLine.slice(0, firstSpace);
  let target = requestLine.slice(firstSpace + 1, secondSpace);
  const httpVersion = requestLine.slice(secondSpace + 1);
  if (!httpVersion.startsWith('HTTP/')) return 'error';
  // Absolute-form target (http://host/path): strip scheme + authority.
  if (target.startsWith('http://') || target.startsWith('https://')) {
    const slash = target.indexOf('/', target.indexOf('://') + 3);
    target = slash < 0 ? '/' : target.slice(slash);
  }
  const headers = new Map<string, string>();
  for (const line of lines.slice(1)) {
    const colon = line.indexOf(':');
    if (colon <= 0) return 'error';
    const name = line.slice(0, colon).trim().toLowerCase();
    const value = line.slice(colon + 1).trim();
    if (!headers.has(name)) headers.set(name, value);
  }
  return { method, target, httpVersion, headers, headLength: headEnd + 4 };
}

/** Parse one chunked body starting at `pos`; null when more data is needed. */
function parseChunked(buf: Buffer, pos: number): { body: Buffer; consumed: number } | 'needMore' | 'error' {
  const chunks: Buffer[] = [];
  let at = pos;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', at);
    if (lineEnd < 0) return 'needMore';
    const sizeLine = buf.subarray(at, lineEnd).toString('latin1').trim();
    const semi = sizeLine.indexOf(';');
    const size = Number.parseInt(semi >= 0 ? sizeLine.slice(0, semi) : sizeLine, 16);
    if (!Number.isSafeInteger(size) || size < 0) return 'error';
    at = lineEnd + 2;
    if (size === 0) {
      // Trailers (if any) end with an empty line.
      if (buf[at] === 13 && buf[at + 1] === 10) return { body: Buffer.concat(chunks), consumed: at + 2 };
      const trailerEnd = buf.indexOf('\r\n\r\n', at);
      if (trailerEnd < 0) return 'needMore';
      return { body: Buffer.concat(chunks), consumed: trailerEnd + 4 };
    }
    if (buf.length < at + size + 2) return 'needMore';
    chunks.push(buf.subarray(at, at + size));
    at += size + 2;
  }
}

function parseOne(buf: Buffer): ParseResult {
  const head = parseHead(buf);
  if (head === 'needMore' || head === 'error') return { kind: head };
  const te = head.headers.get('transfer-encoding') ?? '';
  if (te.toLowerCase().includes('chunked')) {
    const chunked = parseChunked(buf, head.headLength);
    if (chunked === 'needMore') return { kind: 'needMore', head };
    if (chunked === 'error') return { kind: 'error' };
    return { kind: 'ok', req: finishRequest(head, chunked.body), consumed: chunked.consumed };
  }
  const cl = head.headers.get('content-length');
  if (cl !== undefined) {
    const n = Number.parseInt(cl, 10);
    if (!Number.isSafeInteger(n) || n < 0 || n > MAX_BODY_BYTES) return { kind: 'error' };
    const avail = buf.length - head.headLength;
    if (avail < n) return { kind: 'needMore', head };
    return {
      kind: 'ok',
      req: finishRequest(head, buf.subarray(head.headLength, head.headLength + n)),
      consumed: head.headLength + n,
    };
  }
  return { kind: 'ok', req: finishRequest(head, EMPTY), consumed: head.headLength };
}

function finishRequest(head: ParsedHead, body: Buffer): ParsedRequest {
  const conn = (head.headers.get('connection') ?? '').toLowerCase();
  const keepAlive =
    head.httpVersion === 'HTTP/1.1' ? !conn.includes('close') : conn.includes('keep-alive');
  return {
    method: head.method,
    target: head.target,
    httpVersion: head.httpVersion,
    headers: head.headers,
    body,
    keepAlive,
  };
}

// Route segments, cached per pathname: the bench hammers the same URLs
// thousands of times and split + decodeURIComponent is pure per path.
const SEGMENT_CACHE = new Map<string, string[]>();

function routeSegments(pathname: string): string[] {
  const hit = SEGMENT_CACHE.get(pathname);
  if (hit) return hit;
  const segments = pathname.split('/').filter(Boolean).map(decodeSegment);
  if (SEGMENT_CACHE.size > 512) SEGMENT_CACHE.clear();
  SEGMENT_CACHE.set(pathname, segments);
  return segments;
}

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

function bearerToken(headers: Map<string, string>): string {
  const header = headers.get('authorization') ?? '';
  return header.startsWith('Bearer ') ? header.slice(7) : '';
}

/** True when the caller may use an authenticated surface. */
function authorized(headers: Map<string, string>): boolean {
  if (!SHOP_TOKEN) return true;
  return bearerToken(headers) === SHOP_TOKEN;
}

function jsonReply(status: number, body: unknown, extraHeaders?: Record<string, string>): Reply {
  return {
    status,
    contentType: 'application/json',
    body: Buffer.from(body === null ? '' : JSON.stringify(body), 'utf8'),
    headers: extraHeaders,
  };
}

function bytesReply(status: number, contentType: string, body: Buffer): Reply {
  return { status, contentType, body };
}

function parseJsonBody(body: Buffer): { parsed: unknown; parseError: boolean } {
  if (body.length === 0) return { parsed: null, parseError: false };
  try {
    return { parsed: JSON.parse(body.toString('utf8')), parseError: false };
  } catch {
    return { parsed: null, parseError: true };
  }
}

function dispatch(req: ParsedRequest): Reply {
  const method = req.method;
  const host = req.headers.get('host') ?? 'localhost';
  const baseUrl = `http://${host}`;
  const rawUrl = req.target;
  // Fast path: split the raw path without building a URL object. Fall back
  // to `new URL` for exotic paths (backslash, dot segments, doubled slashes),
  // where WHATWG normalization could change the pathname.
  const qIndex = rawUrl.indexOf('?');
  const rawPath = qIndex < 0 ? rawUrl : rawUrl.slice(0, qIndex);
  const queryString = qIndex < 0 ? '' : rawUrl.slice(qIndex + 1);
  const exotic =
    rawPath.includes('\\') ||
    rawPath.includes('//') ||
    rawPath.includes('/./') ||
    rawPath.includes('/../') ||
    rawPath.endsWith('/.') ||
    rawPath.endsWith('/..');
  let pathname: string;
  let searchParams: URLSearchParams | null = null;
  if (exotic) {
    const url = new URL(rawUrl, baseUrl);
    pathname = url.pathname;
    searchParams = url.searchParams;
  } else {
    pathname = rawPath;
  }
  // The query string is parsed lazily: the hottest route (GET one issue) has
  // no query at all, and most routes never touch it.
  const getQuery = (): URLSearchParams => (searchParams ??= new URLSearchParams(queryString));
  const segments = routeSegments(pathname);
  // The body already arrived with the request: no stream wait, no JSON
  // parse for git routes (raw bytes), JSON only for REST/GraphQL/MCP.
  const needsJson = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';

  // ---- health ---------------------------------------------------------------------------
  if (pathname === '/healthz' && method === 'GET') {
    return jsonReply(200, { ok: true });
  }

  // ---- git smart HTTP -------------------------------------------------------------------
  if (pathname.includes('.git/')) {
    const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(pathname);
    if (gitMatch) {
      const owner = decodeSegment(gitMatch[1] as string);
      const repoName = decodeSegment(gitMatch[2] as string);
      const action = gitMatch[3] as string;
      if (owner !== VIEWER.login) {
        return jsonReply(404, { message: 'Not Found' });
      }
      const repo = store.getRepo(owner, repoName);
      if (!repo) {
        return jsonReply(404, { message: 'Not Found' });
      }
      const protocol = req.headers.get('git-protocol') ?? null;
      const version2 = typeof protocol === 'string' && protocol.includes('version=2') ? '2' : null;
      if (action === 'info/refs' && method === 'GET') {
        const service = getQuery().get('service') ?? '';
        const result = handleInfoRefs(repo, service, version2);
        return bytesReply(result.status, result.contentType, result.body);
      }
      if (action === 'git-upload-pack' && method === 'POST') {
        const result = handleUploadPack(repo, req.body, version2);
        return bytesReply(result.status, result.contentType, result.body);
      }
      if (action === 'git-receive-pack' && method === 'POST') {
        const result = handleReceivePack(store, repo, req.body);
        return bytesReply(result.status, result.contentType, result.body);
      }
      return jsonReply(404, { message: 'Not Found' });
    }
  }

  // ---- GraphQL v4 -----------------------------------------------------------------------
  if (pathname === '/api/graphql' && method === 'POST') {
    if (!authorized(req.headers)) {
      return jsonReply(401, { message: 'Requires authentication' });
    }
    const { parsed, parseError } = needsJson ? parseJsonBody(req.body) : { parsed: null, parseError: false };
    if (parseError) {
      return jsonReply(400, { errors: [{ message: 'Invalid JSON in request body' }] });
    }
    const result = handleGraphQL(store, parsed);
    return jsonReply(result.status, result.body);
  }

  // ---- MCP ------------------------------------------------------------------------------
  if (pathname === '/mcp' && method === 'POST') {
    if (!authorized(req.headers)) {
      return jsonReply(401, { message: 'Requires authentication' });
    }
    const { parsed, parseError } = needsJson ? parseJsonBody(req.body) : { parsed: null, parseError: false };
    const result = handleMcp(store, parsed, parseError);
    if (result.body === null) {
      return { status: result.status, body: EMPTY };
    }
    return jsonReply(result.status, result.body);
  }

  // ---- REST v3 --------------------------------------------------------------------------
  if (segments[0] === 'api' && segments[1] === 'v3') {
    const isRootOrMeta = segments.length <= 3 && (segments[2] === undefined || segments[2] === 'meta');
    if (!isRootOrMeta && !authorized(req.headers)) {
      return jsonReply(401, { message: 'Requires authentication' });
    }
    const { parsed, parseError } = needsJson ? parseJsonBody(req.body) : { parsed: null, parseError: false };
    const restReq: RestRequest = {
      method,
      segments: segments.slice(2),
      get query() {
        return getQuery();
      },
      body: parsed,
      bodyParseError: parseError,
      baseUrl: `${baseUrl}/api/v3`,
    };
    const result = handleRest(store, restReq);
    if (result.status === 204) {
      return { status: 204, body: EMPTY, headers: result.headers };
    }
    if (result.rawJson !== undefined) {
      return { status: result.status, contentType: 'application/json', body: result.rawJson, headers: result.headers };
    }
    return jsonReply(result.status, result.body, result.headers);
  }

  return jsonReply(404, { message: 'Not Found' });
}

function writeReply(sock: Socket, req: ParsedRequest, reply: Reply): void {
  // The kit only asserts the header is non-empty, never unique.
  const id = `req-${(++requestCounter).toString(36)}`;
  const isHead = req.method === 'HEAD';
  let head = `HTTP/1.1 ${reply.status} ${REASONS[reply.status] ?? ''}\r\n`;
  if (reply.contentType !== undefined) head += `content-type: ${reply.contentType}\r\n`;
  head += `x-github-request-id: ${id}\r\ncontent-length: ${reply.body.length}\r\n`;
  if (reply.headers !== undefined) {
    for (const key of Object.keys(reply.headers)) head += `${key}: ${reply.headers[key] as string}\r\n`;
  }
  head += req.keepAlive ? 'connection: keep-alive\r\n\r\n' : 'connection: close\r\n\r\n';
  const headBuf = Buffer.from(head, 'latin1');
  // HEAD answers carry headers only; empty bodies are head-only too.
  if (isHead || reply.body.length === 0) {
    sock.write(headBuf);
  } else {
    sock.write(Buffer.concat([headBuf, reply.body]));
  }
}

export function createApp(): net.Server {
  const server = net.createServer((sock) => {
    sock.setNoDelay(true);
    sock.setTimeout(30000, () => sock.destroy());
    sock.on('error', () => {
      // Client vanished mid-request (ECONNRESET etc.): nothing to do.
    });
    let buf: Buffer = EMPTY;
    let sentContinue = false;
    sock.on('data', (chunk: Buffer) => {
      buf = buf.length === 0 ? chunk : Buffer.concat([buf, chunk]);
      if (buf.length > MAX_BODY_BYTES + MAX_HEAD_BYTES) {
        sock.destroy();
        return;
      }
      for (;;) {
        const parsed = parseOne(buf);
        if (parsed.kind === 'needMore') {
          if (
            parsed.head !== undefined &&
            !sentContinue &&
            (parsed.head.headers.get('expect') ?? '').toLowerCase() === '100-continue'
          ) {
            sock.write('HTTP/1.1 100 Continue\r\n\r\n');
            sentContinue = true;
          }
          return;
        }
        if (parsed.kind === 'error') {
          try {
            sock.write('HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n');
          } catch {
            // Socket already gone.
          }
          sock.end();
          return;
        }
        sentContinue = false;
        buf = buf.subarray(parsed.consumed);
        let reply: Reply;
        try {
          reply = dispatch(parsed.req);
        } catch {
          // A handler must never kill the server: answer 500 JSON and keep going.
          reply = jsonReply(500, { message: 'Internal Server Error' });
        }
        try {
          writeReply(sock, parsed.req, reply);
        } catch {
          return;
        }
        if (!parsed.req.keepAlive) {
          sock.end();
          return;
        }
        if (buf.length === 0) return;
      }
    });
  });
  return server;
}
