// The forge's store: every repo, issue, pull request, comment, label and
// ref lives in memory in Maps. Nothing touches disk (the repo is mounted
// read-only in the trial) and no wall clock or unseeded randomness is used:
// every user-visible timestamp comes from a deterministic counter seeded by
// MAKEFUN_TRIAL_NOW, so a replayed trial leaves byte-identical state.

import { VIEWER } from './scenario.ts';
import {
  ZERO_OID,
  buildCommit,
  buildTree,
  collectReachable,
  hashObject,
  isAncestor,
  isObjectId,
  mergeBase,
  parseCommit,
  parseTag,
  parseTree,
} from './gitobj.ts';
import type { GitObject, GitObjectType, TreeEntry } from './gitobj.ts';

export { ZERO_OID };

export interface Label {
  readonly name: string;
  readonly color: string;
}

export interface IssueComment {
  readonly id: number;
  readonly body: string;
  readonly created_at: string;
  readonly updated_at: string;
  readonly user: { login: string; id: number };
}

export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  comments: IssueComment[];
  readonly created_at: string;
  updated_at: string;
  readonly user: { login: string; id: number };
}

export type PullState = 'open' | 'closed';

export interface PullRequest {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: PullState;
  merged: boolean;
  merged_at: string | null;
  merge_commit_sha: string | null;
  readonly head_ref: string;
  readonly base_ref: string;
  readonly head_sha: string;
  readonly base_sha: string;
  readonly created_at: string;
  updated_at: string;
  readonly user: { login: string; id: number };
}

export interface Repo {
  readonly id: number;
  readonly databaseId: number;
  readonly name: string;
  readonly owner: string;
  description: string;
  private: boolean;
  readonly default_branch: string;
  readonly created_at: string;
  updated_at: string;
  readonly objects: Map<string, GitObject>;
  readonly refs: Map<string, string>;
  readonly labels: Map<string, Label>;
  readonly issues: Map<number, Issue>;
  nextIssueNumber: number;
  nextCommentId: number;
  readonly pulls: Map<number, PullRequest>;
  nextPullNumber: number;
  readonly shallowBoundaries: Set<string>;
  /** Bumped on every mutation; cached answers keyed by it stay consistent. */
  version: number;
  /** Flat path -> blob oid of the default branch tip (rebuilt on commit). */
  filesCache: Map<string, string> | null;
  filesCacheCommit: string | null;
}

export class Store {
  readonly repos = new Map<string, Repo>();
  private nextRepoId = 1;
  /** Seconds since the trial's reference "now"; every timestamp derives from it. */
  private clockSeconds: number;
  private idCounter = 1;

  constructor() {
    const rawNow = process.env.MAKEFUN_TRIAL_NOW ?? '';
    const parsed = rawNow ? Date.parse(rawNow) : Number.NaN;
    this.clockSeconds = Number.isFinite(parsed) ? Math.floor(parsed / 1000) : 1_700_000_000;
  }

  /** The next deterministic timestamp, one second later than the previous. */
  nowIso(): string {
    this.clockSeconds += 1;
    return new Date(this.clockSeconds * 1000).toISOString();
  }

  /** A strictly increasing number for synthetic ids that must never repeat. */
  nextId(): number {
    this.idCounter += 1;
    return this.idCounter;
  }

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  listReposFor(owner: string): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === owner) out.push(repo);
    return out.sort((a, b) => a.id - b.id);
  }

  createRepo(owner: string, name: string, description = '', isPrivate = false): Repo {
    const id = this.nextRepoId;
    this.nextRepoId += 1;
    const stamp = this.nowIso();
    const repo: Repo = {
      id,
      databaseId: id,
      name,
      owner,
      description,
      private: isPrivate,
      default_branch: 'main',
      created_at: stamp,
      updated_at: stamp,
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      nextIssueNumber: 1,
      nextCommentId: 1,
      pulls: new Map(),
      nextPullNumber: 1,
      shallowBoundaries: new Set(),
      version: 1,
      filesCache: null,
      filesCacheCommit: null,
    };
    this.repos.set(this.repoKey(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  touch(repo: Repo): void {
    repo.version += 1;
    repo.updated_at = this.nowIso();
  }

  // -------------------------------------------------------------------------------------------
  // git objects and refs
  // -------------------------------------------------------------------------------------------

  putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  lookup(repo: Repo): (oid: string) => GitObject | undefined {
    return (oid) => repo.objects.get(oid);
  }

  headRefName(repo: Repo): string {
    return `refs/heads/${repo.default_branch}`;
  }

  headCommit(repo: Repo): string | null {
    return repo.refs.get(this.headRefName(repo)) ?? null;
  }

  refTip(repo: Repo, ref: string): string | null {
    if (ref === 'HEAD') return this.headCommit(repo);
    const direct = repo.refs.get(ref);
    if (direct) return direct;
    const byShort = repo.refs.get(`refs/heads/${ref}`);
    return byShort ?? null;
  }

  /** Flat path -> blob oid of the tree at `commitOid` (cached per tip). */
  filesOfCommit(repo: Repo, commitOid: string): Map<string, string> {
    if (repo.filesCacheCommit === commitOid && repo.filesCache) return repo.filesCache;
    const out = new Map<string, string>();
    const root = repo.objects.get(commitOid);
    if (root?.type === 'commit') {
      const walk = (treeOid: string, prefix: string): void => {
        const tree = repo.objects.get(treeOid);
        if (tree?.type !== 'tree') return;
        for (const entry of parseTree(tree.data)) {
          const path = prefix ? `${prefix}/${entry.name}` : entry.name;
          if (entry.mode === '40000') walk(entry.oid, path);
          else if (entry.mode !== '160000') out.set(path, entry.oid);
        }
      };
      walk(parseCommit(root.data).tree, '');
    }
    repo.filesCache = out;
    repo.filesCacheCommit = commitOid;
    return out;
  }

  /** Path -> tree oid of every directory in the tree at `commitOid`. */
  treeOidsOfCommit(repo: Repo, commitOid: string): Map<string, string> {
    const out = new Map<string, string>();
    const root = repo.objects.get(commitOid);
    if (root?.type !== 'commit') return out;
    const walk = (treeOid: string, prefix: string): void => {
      const tree = repo.objects.get(treeOid);
      if (tree?.type !== 'tree') return;
      for (const entry of parseTree(tree.data)) {
        if (entry.mode === '40000') {
          const path = prefix ? `${prefix}/${entry.name}` : entry.name;
          out.set(path, entry.oid);
          walk(entry.oid, path);
        }
      }
    };
    walk(parseCommit(root.data).tree, '');
    return out;
  }

  /** Build (and store) a tree from a flat path -> blob oid map. */
  buildTreeFromFiles(repo: Repo, files: ReadonlyMap<string, string>): string {
    const buildLevel = (prefix: string): string => {
      const entries: TreeEntry[] = [];
      const dirs = new Set<string>();
      for (const [path, oid] of files) {
        if (prefix && !path.startsWith(`${prefix}/`)) continue;
        const rest = prefix ? path.slice(prefix.length + 1) : path;
        const slash = rest.indexOf('/');
        if (slash < 0) entries.push({ mode: '100644', name: rest, oid });
        else dirs.add(rest.slice(0, slash));
      }
      for (const dir of [...dirs].sort()) {
        entries.push({ mode: '40000', name: dir, oid: buildLevel(prefix ? `${prefix}/${dir}` : dir) });
      }
      return this.putObject(repo, 'tree', buildTree(entries));
    };
    return buildLevel('');
  }

  /** Create a commit on a branch (or on the default branch when it is unborn). */
  commitFiles(
    repo: Repo,
    branch: string,
    message: string,
    apply: (files: Map<string, string>) => void,
  ): { commitOid: string; treeOid: string } {
    const ref = `refs/heads/${branch}`;
    const tip = repo.refs.get(ref) ?? null;
    const files = new Map<string, string>(tip ? this.filesOfCommit(repo, tip) : []);
    apply(files);
    const treeOid = this.buildTreeFromFiles(repo, files);
    const author = `${VIEWER.name} <${VIEWER.email}>`;
    const stamp = Math.floor(Date.parse(this.nowIso()) / 1000);
    const commitOid = this.putObject(
      repo,
      'commit',
      buildCommit({
        tree: treeOid,
        parents: tip ? [tip] : [],
        message: message.endsWith('\n') ? message : `${message}\n`,
        author,
        committer: author,
        timestamp: stamp,
      }),
    );
    repo.refs.set(ref, commitOid);
    this.touch(repo);
    return { commitOid, treeOid };
  }

  // -------------------------------------------------------------------------------------------
  // diffs and merges
  // -------------------------------------------------------------------------------------------

  lineCounts(oldContent: Buffer | null, newContent: Buffer | null): { additions: number; deletions: number } {
    const oldLines = oldContent ? oldContent.toString('utf8').split('\n') : [];
    const newLines = newContent ? newContent.toString('utf8').split('\n') : [];
    const oldCounts = new Map<string, number>();
    for (const line of oldLines) oldCounts.set(line, (oldCounts.get(line) ?? 0) + 1);
    const newCounts = new Map<string, number>();
    for (const line of newLines) newCounts.set(line, (newCounts.get(line) ?? 0) + 1);
    let additions = 0;
    let deletions = 0;
    for (const [line, count] of newCounts) additions += Math.max(0, count - (oldCounts.get(line) ?? 0));
    for (const [line, count] of oldCounts) deletions += Math.max(0, count - (newCounts.get(line) ?? 0));
    return { additions, deletions };
  }

  diffFiles(repo: Repo, baseCommit: string | null, headCommit: string | null): {
    filename: string;
    status: 'added' | 'removed' | 'modified';
    additions: number;
    deletions: number;
    changes: number;
  }[] {
    const base = baseCommit ? this.filesOfCommit(repo, baseCommit) : new Map<string, string>();
    const head = headCommit ? this.filesOfCommit(repo, headCommit) : new Map<string, string>();
    const paths = new Set<string>([...base.keys(), ...head.keys()]);
    const out: {
      filename: string;
      status: 'added' | 'removed' | 'modified';
      additions: number;
      deletions: number;
      changes: number;
    }[] = [];
    for (const path of [...paths].sort()) {
      const baseOid = base.get(path);
      const headOid = head.get(path);
      if (baseOid === headOid) continue;
      const baseBlob = baseOid ? repo.objects.get(baseOid)?.data ?? null : null;
      const headBlob = headOid ? repo.objects.get(headOid)?.data ?? null : null;
      const { additions, deletions } = this.lineCounts(baseBlob, headBlob);
      out.push({
        filename: path,
        status: baseOid === undefined ? 'added' : headOid === undefined ? 'removed' : 'modified',
        additions,
        deletions,
        changes: additions + deletions,
      });
    }
    return out;
  }

  /** True when head can be squash-merged onto base without a conflict. */
  mergeable(repo: Repo, baseSha: string, headSha: string): boolean {
    if (baseSha === headSha) return true;
    const base = mergeBase(baseSha, headSha, this.lookup(repo));
    if (!base) return false;
    const baseFiles = this.filesOfCommit(repo, base);
    const baseTip = this.filesOfCommit(repo, baseSha);
    const headTip = this.filesOfCommit(repo, headSha);
    const paths = new Set<string>([...baseFiles.keys(), ...baseTip.keys(), ...headTip.keys()]);
    for (const path of paths) {
      const original = baseFiles.get(path);
      const onBase = baseTip.get(path);
      const onHead = headTip.get(path);
      const baseChanged = original !== onBase;
      const headChanged = original !== onHead;
      if (baseChanged && headChanged && onBase !== onHead) return false;
    }
    return true;
  }

  // -------------------------------------------------------------------------------------------
  // issues, pulls, comments
  // -------------------------------------------------------------------------------------------

  createIssue(repo: Repo, title: string, body = '', labelNames: readonly string[] = []): Issue {
    const labels: Label[] = [];
    for (const name of labelNames) {
      const label = repo.labels.get(name);
      if (!label) throw new Error(`label ${name} does not exist`);
      labels.push(label);
    }
    const number = repo.nextIssueNumber;
    repo.nextIssueNumber += 1;
    const stamp = this.nowIso();
    const issue: Issue = {
      id: repo.databaseId * 100000 + number,
      number,
      title,
      body,
      state: 'open',
      labels,
      comments: [],
      created_at: stamp,
      updated_at: stamp,
      user: { login: VIEWER.login, id: VIEWER.id },
    };
    repo.issues.set(number, issue);
    this.touch(repo);
    return issue;
  }

  addComment(repo: Repo, issue: Issue, body: string): IssueComment {
    const stamp = this.nowIso();
    const comment: IssueComment = {
      id: repo.nextCommentId,
      body,
      created_at: stamp,
      updated_at: stamp,
      user: { login: VIEWER.login, id: VIEWER.id },
    };
    repo.nextCommentId += 1;
    issue.comments.push(comment);
    this.touch(repo);
    return comment;
  }

  createPull(
    repo: Repo,
    title: string,
    body: string,
    headRef: string,
    baseRef: string,
    headSha: string,
    baseSha: string,
  ): PullRequest {
    const number = repo.nextPullNumber;
    repo.nextPullNumber += 1;
    const stamp = this.nowIso();
    const pull: PullRequest = {
      id: repo.databaseId * 100000 + number,
      number,
      title,
      body,
      state: 'open',
      merged: false,
      merged_at: null,
      merge_commit_sha: null,
      head_ref: headRef,
      base_ref: baseRef,
      head_sha: headSha,
      base_sha: baseSha,
      created_at: stamp,
      updated_at: stamp,
      user: { login: VIEWER.login, id: VIEWER.id },
    };
    repo.pulls.set(number, pull);
    this.touch(repo);
    return pull;
  }

  /**
   * Squash-merge an open pull request onto its base branch: the merge commit
   * carries the head tree and the base tip as its only parent.
   */
  mergePull(repo: Repo, pull: PullRequest): { ok: true; sha: string } | { ok: false; reason: string } {
    if (pull.state !== 'open') return { ok: false, reason: 'already merged or closed' };
    const baseRef = `refs/heads/${pull.base_ref}`;
    const baseTip = repo.refs.get(baseRef);
    if (!baseTip) return { ok: false, reason: 'base branch is missing' };
    if (!this.mergeable(repo, baseTip, pull.head_sha)) return { ok: false, reason: 'conflict' };
    const headFiles = this.filesOfCommit(repo, pull.head_sha);
    const treeOid = this.buildTreeFromFiles(repo, headFiles);
    const author = `${VIEWER.name} <${VIEWER.email}>`;
    const stamp = Math.floor(Date.parse(this.nowIso()) / 1000);
    const message = `Merge pull request #${pull.number} from ${pull.head_ref}\n\n${pull.title}\n`;
    const sha = this.putObject(
      repo,
      'commit',
      buildCommit({ tree: treeOid, parents: [baseTip], message, author, committer: author, timestamp: stamp }),
    );
    repo.refs.set(baseRef, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.merged_at = this.nowIso();
    pull.merge_commit_sha = sha;
    pull.updated_at = pull.merged_at;
    this.touch(repo);
    return { ok: true, sha };
  }

  // -------------------------------------------------------------------------------------------
  // global ids and reachability helpers used by several surfaces
  // -------------------------------------------------------------------------------------------

  static globalId(kind: string, parts: readonly (string | number)[]): string {
    return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64url');
  }

  static decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
    try {
      const text = Buffer.from(id, 'base64url').toString('utf8');
      const [kind, ...parts] = text.split(':');
      if (!kind || parts.length === 0) return null;
      return { kind, parts };
    } catch {
      return null;
    }
  }

  commitHistory(repo: Repo, fromSha: string | null, limit = 100): { sha: string; message: string; author: string; committer: string }[] {
    const out: { sha: string; message: string; author: string; committer: string }[] = [];
    if (!fromSha) return out;
    const seen = new Set<string>();
    const queue: { sha: string; depth: number }[] = [{ sha: fromSha, depth: 0 }];
    while (queue.length && out.length < limit) {
      const { sha } = queue.shift() as { sha: string; depth: number };
      if (seen.has(sha)) continue;
      seen.add(sha);
      const object = repo.objects.get(sha);
      if (object?.type !== 'commit') continue;
      const parsed = parseCommit(object.data);
      out.push({ sha, message: parsed.message, author: parsed.author, committer: parsed.committer });
      for (const parent of parsed.parents) queue.push({ sha: parent, depth: 0 });
    }
    return out;
  }

  reachableObjects(repo: Repo, roots: Iterable<string>): Set<string> {
    return collectReachable(roots, this.lookup(repo));
  }

  isAncestorOf(repo: Repo, ancestor: string, tip: string): boolean {
    return isAncestor(ancestor, tip, this.lookup(repo));
  }

  /** A public JSON view of a repository (the shape every surface returns). */
  repoView(repo: Repo): Record<string, unknown> {
    return {
      id: repo.id,
      databaseId: repo.databaseId,
      name: repo.name,
      full_name: `${repo.owner}/${repo.name}`,
      owner: { login: repo.owner, id: repo.owner === VIEWER.login ? VIEWER.id : 1001 },
      private: repo.private,
      description: repo.description,
      default_branch: repo.default_branch,
      created_at: repo.created_at,
      updated_at: repo.updated_at,
      has_issues: true,
      has_projects: true,
      has_wiki: true,
      has_pages: false,
      forks_count: 0,
      open_issues_count: [...repo.issues.values()].filter((issue) => issue.state === 'open').length,
      watchers_count: 0,
      stargazers_count: 0,
    };
  }

  issueView(issue: Issue, repo: Repo): Record<string, unknown> {
    return {
      id: issue.id,
      number: issue.number,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      labels: issue.labels.map((label) => ({ name: label.name, color: label.color })),
      comments: issue.comments.length,
      created_at: issue.created_at,
      updated_at: issue.updated_at,
      user: issue.user,
      repository: { id: repo.id, name: repo.name, full_name: `${repo.owner}/${repo.name}` },
    };
  }

  pullView(pull: PullRequest, repo: Repo): Record<string, unknown> {
    return {
      id: pull.id,
      number: pull.number,
      title: pull.title,
      body: pull.body,
      state: pull.state,
      merged: pull.merged,
      merged_at: pull.merged_at,
      merge_commit_sha: pull.merge_commit_sha,
      head: { ref: pull.head_ref, sha: pull.head_sha },
      base: { ref: pull.base_ref, sha: pull.base_sha },
      created_at: pull.created_at,
      updated_at: pull.updated_at,
      user: pull.user,
      repository: { id: repo.id, name: repo.name, full_name: `${repo.owner}/${repo.name}` },
    };
  }

  commentView(comment: IssueComment): Record<string, unknown> {
    return {
      id: comment.id,
      body: comment.body,
      created_at: comment.created_at,
      updated_at: comment.updated_at,
      user: comment.user,
    };
  }
}

export { parseCommit, parseTag, parseTree };
