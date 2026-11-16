// Git smart-HTTP: info/refs, upload-pack (protocol v2 and v0) and
// receive-pack (v0). The exact wire shapes the conformance suite's git
// client speaks, documented in the kit: pkt-line framing, v2 capability
// advertisement, ls-refs/fetch commands, shallow/deepen handling, sideband
// packfiles, report-status. Standard library only.

import { cachedParseCommit, cachedParseTree, parseTag } from './gitobj.ts';
import {
  PKT_FLUSH,
  ZERO_OID,
  decodePktLines,
  isObjectId,
  pktLine,
  pktLines,
  readPack,
  startsWithPktLine,
  writePack,
} from './gitobj.ts';
import type { GitObject, GitObjectType } from './gitobj.ts';
import type { Repo, Store } from './store.ts';

const UPLOAD_V0_CAPS =
  'multi_ack thin-pack side-band-64k side-band ofs-delta shallow symref=HEAD:refs/heads/main agent=forge/1.0';
const RECEIVE_V0_CAPS = 'report-status side-band-64k delete-refs ofs-delta symref=HEAD:refs/heads/main agent=forge/1.0';

interface CachedAdvertisements {
  version: number;
  v0Upload: Buffer;
  v2Upload: Buffer;
  v0Receive: Buffer;
}

const advertisementCache = new WeakMap<Repo, CachedAdvertisements>();
const packCache = new WeakMap<Repo, Map<string, Buffer>>();
/** Whole fetch/ls-refs response bodies, keyed by repo version + request. */
const responseCache = new WeakMap<Repo, Map<string, Buffer>>();

function cachedResponse(repo: Repo, key: string, build: () => Buffer): Buffer {
  let cache = responseCache.get(repo);
  if (!cache) {
    cache = new Map();
    responseCache.set(repo, cache);
  }
  const hit = cache.get(key);
  if (hit) return hit;
  const built = build();
  if (cache.size > 64) cache.clear();
  cache.set(key, built);
  return built;
}

function sortedRefs(repo: Repo): { name: string; oid: string }[] {
  return [...repo.refs.entries()]
    .map(([name, oid]) => ({ name, oid }))
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

function peeledOid(repo: Repo, ref: string, oid: string): string | null {
  if (!ref.startsWith('refs/tags/')) return null;
  const object = repo.objects.get(oid);
  if (object?.type !== 'tag') return null;
  const target = parseTag(object.data).object;
  return isObjectId(target) ? target : null;
}

function v0RefLines(repo: Repo, caps: string): string[] {
  const refs = sortedRefs(repo);
  const head = repo.refs.get(`refs/heads/${repo.default_branch}`) ?? null;
  if (refs.length === 0) return [`${ZERO_OID} capabilities^{}\0${caps}`];
  const lines: string[] = [`${head ?? ZERO_OID} HEAD\0${caps}`];
  for (const ref of refs) {
    lines.push(`${ref.oid} ${ref.name}`);
    const peeled = peeledOid(repo, ref.name, ref.oid);
    if (peeled) lines.push(`${peeled} ${ref.name}^{}`);
  }
  return lines;
}

function advertisements(repo: Repo): CachedAdvertisements {
  const cached = advertisementCache.get(repo);
  if (cached && cached.version === repo.version) return cached;
  const built: CachedAdvertisements = {
    version: repo.version,
    v0Upload: pktLines(['# service=git-upload-pack\n', ...v0RefLines(repo, UPLOAD_V0_CAPS), 'flush']),
    v2Upload: pktLines(['version 2\n', 'agent=forge/1.0\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n', 'flush']),
    v0Receive: pktLines(['# service=git-receive-pack\n', ...v0RefLines(repo, RECEIVE_V0_CAPS), 'flush']),
  };
  advertisementCache.set(repo, built);
  return built;
}

/** Split a sideband pack into pkt-lines (band 1, at most 65515 data bytes each). */
function sidebandPackets(pack: Buffer): Buffer[] {
  const out: Buffer[] = [];
  for (let offset = 0; offset < pack.length; offset += 65515) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + 65515));
    const framed = Buffer.allocUnsafe(chunk.length + 1);
    framed[0] = 1;
    chunk.copy(framed, 1);
    out.push(pktLine(framed));
  }
  return out;
}

interface FetchPlan {
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
}

/**
 * Decide what a fetch sends: reachable(wants) minus reachable(haves), with a
 * depth limit. Commit distance counts generations from the nearest want; the
 * boundary generation is listed as shallow. `oldBoundaries` are the shallow
 * tips the client already holds — any it now fully receives becomes unshallow.
 */
function planFetch(
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | null,
  oldBoundaries: string[],
): FetchPlan {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const distance = new Map<string, number>();
  const queue: { oid: string; depth: number }[] = [];
  // Index pointer, not shift(): the queue is a FIFO and shift() is O(n) per
  // pop, which made a 200-commit fetch walk quadratic.
  let queueHead = 0;
  for (const want of wants) {
    const object = lookup(want);
    if (!object) continue;
    if (object.type === 'tag') {
      const target = parseTag(object.data).object;
      if (isObjectId(target)) queue.push({ oid: target, depth: 0 });
    } else {
      queue.push({ oid: want, depth: 0 });
    }
  }
  const includedCommits = new Set<string>();
  while (queueHead < queue.length) {
    const { oid, depth: d } = queue[queueHead++] as { oid: string; depth: number };
    if (distance.has(oid)) continue;
    const object = lookup(oid);
    if (!object) continue;
    distance.set(oid, d);
    if (depth !== null && d >= depth) continue;
    includedCommits.add(oid);
    if (object.type === 'commit') {
      for (const parent of cachedParseCommit(object.data).parents) queue.push({ oid: parent, depth: d + 1 });
    }
  }
  const shallow: string[] = [];
  if (depth !== null) {
    for (const [oid, d] of distance) if (d === depth - 1 && includedCommits.has(oid)) shallow.push(oid);
  }
  const shallowSet = new Set(shallow);
  const unshallow: string[] = [];
  for (const boundary of oldBoundaries) {
    if (includedCommits.has(boundary) && !shallowSet.has(boundary)) unshallow.push(boundary);
  }
  // Objects the client already holds. A have that is one of the client's
  // shallow boundaries counts only for itself and its tree: a shallow clone
  // holds the boundary commit but none of its parents, so the walk must not
  // cross a boundary or a deepen would exclude the very parent it must send.
  const oldBoundarySet = new Set(oldBoundaries);
  const held = new Set<string>();
  {
    const stack: string[] = [...haves];
    while (stack.length) {
      const oid = stack.pop() as string;
      if (held.has(oid)) continue;
      const object = lookup(oid);
      if (!object) continue;
      held.add(oid);
      if (object.type === 'commit') {
        const commit = cachedParseCommit(object.data);
        stack.push(commit.tree);
        if (!oldBoundarySet.has(oid)) {
          for (const parent of commit.parents) stack.push(parent);
        }
      } else if (object.type === 'tag') {
        stack.push(parseTag(object.data).object);
      } else if (object.type === 'tree') {
        for (const entry of cachedParseTree(object.data)) {
          if (entry.mode !== '160000') stack.push(entry.oid);
        }
      }
    }
  }
  // Walk the included commits without crossing the depth boundary: a commit
  // contributes its tree and blobs, and its parents only when they are
  // themselves included. (collectReachable would walk the whole history.)
  const wanted = new Set<string>();
  const stack: string[] = [...wants];
  while (stack.length) {
    const oid = stack.pop() as string;
    if (wanted.has(oid)) continue;
    const object = lookup(oid);
    if (!object) continue;
    wanted.add(oid);
    if (object.type === 'commit') {
      const commit = cachedParseCommit(object.data);
      stack.push(commit.tree);
      for (const parent of commit.parents) if (includedCommits.has(parent)) stack.push(parent);
    } else if (object.type === 'tag') {
      stack.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of cachedParseTree(object.data)) {
        if (entry.mode !== '160000') stack.push(entry.oid);
      }
    }
  }
  const objects: GitObject[] = [];
  for (const oid of wanted) {
    if (held.has(oid)) continue;
    const object = lookup(oid);
    if (object) objects.push(object);
  }
  return { objects, shallow: shallow.sort(), unshallow: unshallow.sort() };
}

function packFor(repo: Repo, key: string, build: () => Buffer): Buffer {
  let cache = packCache.get(repo);
  if (!cache) {
    cache = new Map();
    packCache.set(repo, cache);
  }
  const hit = cache.get(key);
  if (hit) return hit;
  const built = build();
  if (cache.size > 64) cache.clear();
  cache.set(key, built);
  return built;
}

// ---------------------------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------------------------

export function handleInfoRefs(repo: Repo, service: string, protocolVersion: string | null): { status: number; contentType: string; body: Buffer } {
  const ads = advertisements(repo);
  if (service === 'git-upload-pack') {
    if (protocolVersion === '2') {
      return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: ads.v2Upload };
    }
    return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: ads.v0Upload };
  }
  if (service === 'git-receive-pack') {
    return { status: 200, contentType: 'application/x-git-receive-pack-advertisement', body: ads.v0Receive };
  }
  return { status: 404, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8') };
}

// ---------------------------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------------------------

function lsRefsResponse(
  repo: Repo,
  opts: { peel: boolean; symrefs: boolean; unborn: boolean; prefixes: string[] },
): Buffer {
  const lines: string[] = [];
  const head = repo.refs.get(`refs/heads/${repo.default_branch}`) ?? null;
  const matchPrefix = (name: string): boolean => opts.prefixes.length === 0 || opts.prefixes.some((p) => name.startsWith(p));
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    if (opts.unborn) lines.push('unborn HEAD symref-target:refs/heads/main');
    return pktLines([...lines, 'flush']);
  }
  if (matchPrefix('HEAD') && head) {
    lines.push(opts.symrefs ? `${head} HEAD symref-target:refs/heads/main` : `${head} HEAD`);
  }
  for (const ref of refs) {
    if (!matchPrefix(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (opts.peel) {
      const peeled = peeledOid(repo, ref.name, ref.oid);
      if (peeled) line += ` peeled:${peeled}`;
    }
    lines.push(line);
  }
  return pktLines([...lines, 'flush']);
}

function ackLines(repo: Repo, haves: string[]): string[] {
  for (const have of haves) {
    if (repo.objects.has(have)) return [`ACK ${have}\n`];
  }
  return ['NAK\n'];
}

function v2UploadPack(repo: Repo, body: Buffer): { status: number; contentType: string; body: Buffer } {
  const { packets } = decodePktLines(body);
  const lines = packets.filter((p): p is { kind: 'data'; payload: Buffer } => p.kind === 'data').map((p) => p.payload.toString('utf8'));
  const commandLine = lines[0] ?? '';
  const command = commandLine.startsWith('command=') ? commandLine.slice(8).trim() : '';
  if (command === 'ls-refs') {
    const opts = { peel: false, symrefs: false, unborn: false, prefixes: [] as string[] };
    for (const line of lines.slice(1)) {
      const text = line.trim();
      if (text === 'peel') opts.peel = true;
      else if (text === 'symrefs') opts.symrefs = true;
      else if (text === 'unborn') opts.unborn = true;
      else if (text.startsWith('ref-prefix ')) opts.prefixes.push(text.slice('ref-prefix '.length));
    }
    const key = `${repo.version}:ls-refs:${opts.peel}:${opts.symrefs}:${opts.unborn}:${opts.prefixes.join(',')}`;
    const responseBody = cachedResponse(repo, key, () => lsRefsResponse(repo, opts));
    return { status: 200, contentType: 'application/x-git-upload-pack-result', body: responseBody };
  }
  if (command === 'fetch') {
    const wants: string[] = [];
    const haves: string[] = [];
    const oldBoundaries: string[] = [];
    let depth: number | null = null;
    for (const line of lines.slice(1)) {
      const text = line.trim();
      if (text.startsWith('want ')) wants.push(text.slice(5).trim());
      else if (text.startsWith('have ')) haves.push(text.slice(5).trim());
      else if (text.startsWith('shallow ')) oldBoundaries.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) {
        const parsed = Number.parseInt(text.slice(7).trim(), 10);
        if (Number.isFinite(parsed)) depth = parsed;
      }
    }
    const fetchKey = `${repo.version}:v2fetch:${wants.join(',')}|${haves.join(',')}|${depth ?? ''}|${oldBoundaries.join(',')}`;
    const responseBody = cachedResponse(repo, fetchKey, () => {
      const plan = planFetch(repo, wants, haves, depth, oldBoundaries);
      const parts: Buffer[] = [];
      parts.push(pktLine('acknowledgments\n'));
      for (const line of ackLines(repo, haves)) parts.push(pktLine(line));
      const hasSections = plan.shallow.length > 0 || plan.unshallow.length > 0 || plan.objects.length > 0;
      if (hasSections) {
        parts.push(Buffer.from('0001', 'ascii'));
        if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
          parts.push(pktLine('shallow-info\n'));
          for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
          for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
          parts.push(Buffer.from('0001', 'ascii'));
        }
        if (plan.objects.length > 0) {
          parts.push(pktLine('packfile\n'));
          const key = `${repo.version}:v2:${wants.join(',')}:${haves.join(',')}:${depth ?? ''}`;
          parts.push(...sidebandPackets(packFor(repo, key, () => writePack(plan.objects))));
        }
      }
      parts.push(PKT_FLUSH);
      return Buffer.concat(parts);
    });
    return { status: 200, contentType: 'application/x-git-upload-pack-result', body: responseBody };
  }
  return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'unknown command' }), 'utf8') };
}

function v0UploadPack(repo: Repo, body: Buffer): { status: number; contentType: string; body: Buffer } {
  const { packets, consumed } = decodePktLines(body);
  const dataLines = packets.filter((p): p is { kind: 'data'; payload: Buffer } => p.kind === 'data').map((p) => p.payload.toString('utf8'));
  const wants: string[] = [];
  const haves: string[] = [];
  const oldBoundaries: string[] = [];
  let depth: number | null = null;
  let seenFlush = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      seenFlush = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8');
    if (!seenFlush) {
      if (text.startsWith('want ')) {
        const rest = text.slice(5).trim();
        wants.push(rest.split(' ')[0] as string);
      } else if (text.startsWith('shallow ')) oldBoundaries.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) {
        const parsed = Number.parseInt(text.slice(7).trim(), 10);
        if (Number.isFinite(parsed)) depth = parsed;
      }
    } else if (text.startsWith('have ')) {
      haves.push(text.slice(5).trim());
    }
  }
  if (wants.length === 0 || dataLines.length === 0) {
    return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'bad request' }), 'utf8') };
  }
  const fetchKey = `${repo.version}:v0fetch:${wants.join(',')}|${haves.join(',')}|${depth ?? ''}|${oldBoundaries.join(',')}`;
  const responseBody = cachedResponse(repo, fetchKey, () => {
    const plan = planFetch(repo, wants, haves, depth, oldBoundaries);
    const parts: Buffer[] = [];
    for (const line of ackLines(repo, haves)) parts.push(pktLine(line));
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    if (plan.objects.length > 0) {
      const key = `${repo.version}:v0:${wants.join(',')}:${haves.join(',')}:${depth ?? ''}`;
      parts.push(...sidebandPackets(packFor(repo, key, () => writePack(plan.objects))));
    }
    parts.push(PKT_FLUSH);
    return Buffer.concat(parts);
  });
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: responseBody };
}

export function handleUploadPack(
  repo: Repo,
  body: Buffer,
  protocolVersion: string | null,
): { status: number; contentType: string; body: Buffer } {
  if (!startsWithPktLine(body)) {
    return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'bad request' }), 'utf8') };
  }
  if (protocolVersion === '2') return v2UploadPack(repo, body);
  return v0UploadPack(repo, body);
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

interface RefCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function handleReceivePack(
  store: Store,
  repo: Repo,
  body: Buffer,
): { status: number; contentType: string; body: Buffer } {
  if (!startsWithPktLine(body)) {
    return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'bad request' }), 'utf8') };
  }
  const { packets, consumed } = decodePktLines(body);
  const commands: RefCommand[] = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let text = packet.payload.toString('utf8');
    const nul = text.indexOf('\0');
    if (nul >= 0) text = text.slice(0, nul);
    const parts = text.trim().split(' ');
    if (parts.length >= 3 && isObjectId(parts[0] as string) && isObjectId(parts[1] as string)) {
      commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts[2] as string });
    }
  }
  if (commands.length === 0) {
    return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'bad request' }), 'utf8') };
  }
  const packBytes = body.subarray(consumed);
  if (packBytes.length > 0) {
    try {
      const { objects } = readPack(packBytes, (oid) => repo.objects.get(oid));
      for (const [oid, object] of objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, object);
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : 'unpack failed';
      return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message }), 'utf8') };
    }
  }
  const results: string[] = ['unpack ok\n'];
  let changed = false;
  for (const command of commands) {
    const current = repo.refs.get(command.ref) ?? null;
    const isTag = command.ref.startsWith('refs/tags/');
    const fail = (reason: string): void => {
      results.push(`ng ${command.ref} ${reason}\n`);
    };
    if (command.newOid === ZERO_OID) {
      if (current === null) fail('no such ref');
      else if (command.oldOid !== ZERO_OID && command.oldOid !== current) fail('non-fast-forward');
      else {
        repo.refs.delete(command.ref);
        results.push(`ok ${command.ref}\n`);
        changed = true;
      }
      continue;
    }
    const object = repo.objects.get(command.newOid);
    if (!object) {
      fail('object not found');
      continue;
    }
    if (command.oldOid === ZERO_OID) {
      if (current !== null) fail('already exists');
      else {
        repo.refs.set(command.ref, command.newOid);
        results.push(`ok ${command.ref}\n`);
        changed = true;
      }
      continue;
    }
    if (current === null) {
      fail('no such ref');
      continue;
    }
    if (command.oldOid !== current) {
      fail('non-fast-forward');
      continue;
    }
    const fastForward =
      isTag ||
      object.type !== 'commit' ||
      store.isAncestorOf(repo, command.oldOid, command.newOid);
    if (!fastForward) {
      fail('non-fast-forward');
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    results.push(`ok ${command.ref}\n`);
    changed = true;
  }
  if (changed) store.touch(repo);
  const parts: Buffer[] = [];
  for (const line of results) {
    const framed = Buffer.concat([Buffer.from([1]), Buffer.from(line, 'utf8')]);
    parts.push(pktLine(framed));
  }
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-receive-pack-result', body: Buffer.concat(parts) };
}
