// GitHub REST v3 surface: repositories, refs, contents, labels, issues,
// comments and pull requests. Shapes follow the conformance suite's
// expectations: JSON everywhere, X-GitHub-Request-Id on every reply (added
// by the router), 404 for foreign owners, 422 "already exists" on duplicate
// repos, 409 on stale content shas, 405 on conflicted merges.

import { VIEWER } from './scenario.ts';
import { isObjectId, parseCommit } from './gitobj.ts';
import type { Repo, Store } from './store.ts';

export interface RestRequest {
  method: string;
  segments: string[];
  query: URLSearchParams;
  body: unknown;
  bodyParseError: boolean;
  baseUrl: string;
}

export interface RestResponse {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

type JsonBody = Record<string, unknown>;

function json(status: number, body: unknown, headers?: Record<string, string>): RestResponse {
  return { status, body, headers };
}

function error(status: number, message: string): RestResponse {
  return { status, body: { message } };
}

function asObject(value: unknown): JsonBody {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as JsonBody) : {};
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: unknown, fallback: number): number {
  const parsed = typeof value === 'number' ? value : Number.parseInt(String(value ?? ''), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function paginate<T>(items: readonly T[], query: URLSearchParams, baseUrl: string): { page: T[]; headers: Record<string, string> } {
  const perPage = Math.min(100, asNumber(query.get('per_page'), 30));
  const page = Math.max(1, asNumber(query.get('page'), 1));
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const slice = items.slice((page - 1) * perPage, page * perPage);
  const headers: Record<string, string> = {};
  if (totalPages > 1) {
    const links: string[] = [];
    const urlFor = (target: number): string => {
      const params = new URLSearchParams(query);
      params.set('page', String(target));
      params.set('per_page', String(perPage));
      return `${baseUrl}?${params.toString()}`;
    };
    if (page < totalPages) links.push(`<${urlFor(page + 1)}>; rel="next"`);
    links.push(`<${urlFor(totalPages)}>; rel="last"`);
    if (page > 1) links.push(`<${urlFor(page - 1)}>; rel="prev"`);
    links.push(`<${urlFor(1)}>; rel="first"`);
    headers.link = links.join(', ');
  }
  return { page: slice, headers };
}

function repoOr404(store: Store, owner: string, name: string): Repo | null {
  if (owner !== VIEWER.login) return null;
  return store.getRepo(owner, name) ?? null;
}

function issueLabelNames(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  const names: string[] = [];
  for (const item of value) {
    if (typeof item === 'string') names.push(item);
    else if (item !== null && typeof item === 'object') {
      const label = item as JsonBody;
      if (typeof label.name === 'string') names.push(label.name);
    }
  }
  return names;
}

function commitSummary(repo: Repo, sha: string): Record<string, unknown> {
  const object = repo.objects.get(sha);
  const parsed = object?.type === 'commit' ? parseCommit(object.data) : null;
  const identity = (raw: string): { name: string; email: string; date: string } => {
    const match = /^(.*?) <([^>]*)> (\d+) [+-]\d{4}$/.exec(raw);
    if (!match) return { name: raw, email: '', date: '' };
    return { name: match[1] as string, email: match[2] as string, date: new Date(Number(match[3]) * 1000).toISOString() };
  };
  const author = identity(parsed?.author ?? '');
  const committer = identity(parsed?.committer ?? '');
  // GitHub's API presents the commit message without its trailing newline.
  const message = (parsed?.message ?? '').replace(/\n$/, '');
  return {
    sha,
    commit: { message, author, committer },
    author: { login: VIEWER.login, id: VIEWER.id },
    committer: { login: VIEWER.login, id: VIEWER.id },
    parents: (parsed?.parents ?? []).map((parent) => ({ sha: parent })),
  };
}

export function handleRest(store: Store, req: RestRequest): RestResponse {
  const { method, segments, query } = req;
  // The full request URL: pagination Link headers must point back at this
  // route, not at the API root.
  const requestUrl = segments.length ? `${req.baseUrl}/${segments.join('/')}` : req.baseUrl;
  if (req.bodyParseError && (method === 'POST' || method === 'PATCH' || method === 'PUT' || method === 'DELETE')) {
    return error(400, 'Invalid JSON in request body');
  }
  const body = asObject(req.body);

  // ---- root and meta (no repo context) ------------------------------------------------------
  if (segments.length === 0) {
    if (method !== 'GET') return error(404, 'Not Found');
    return json(
      200,
      {
        current_user_url: `${req.baseUrl}/user`,
        user_url: `${req.baseUrl}/users/{user}`,
        repository_url: `${req.baseUrl}/repos/{owner}/{repo}`,
        meta: { verifiable_password_authentication: true },
      },
      { 'x-oauth-scopes': 'repo, read:org' },
    );
  }
  if (segments[0] === 'meta' && segments.length === 1) {
    return method === 'GET' ? json(200, { installed_version: '1.0.0' }) : error(404, 'Not Found');
  }

  // ---- /user -------------------------------------------------------------------------------
  if (segments[0] === 'user') {
    if (segments.length === 1) {
      if (method !== 'GET') return error(404, 'Not Found');
      return json(200, { login: VIEWER.login, id: VIEWER.id, name: VIEWER.name, email: VIEWER.email, type: 'User' });
    }
    if (segments[1] === 'repos') {
      if (method === 'POST') {
        const name = asString(body.name).trim();
        if (!name) return error(422, 'name is required');
        if (store.getRepo(VIEWER.login, name)) return error(422, 'Repository already exists');
        const repo = store.createRepo(
          VIEWER.login,
          name,
          asString(body.description),
          body.private === true,
        );
        return json(201, store.repoView(repo));
      }
      if (method === 'GET') {
        const repos = store.listReposFor(VIEWER.login);
        const { page, headers } = paginate(repos.map((repo) => store.repoView(repo)), query, requestUrl);
        return json(200, page, headers);
      }
      return error(404, 'Not Found');
    }
    return error(404, 'Not Found');
  }

  // ---- /repos/{owner}/{repo}/... ------------------------------------------------------------
  if (segments[0] === 'repos' && segments.length >= 3) {
    const owner = segments[1] as string;
    const repoName = segments[2] as string;
    const repo = repoOr404(store, owner, repoName);
    if (!repo) return error(404, 'Not Found');
    const rest = segments.slice(3);

    if (rest.length === 0) {
      if (method === 'GET') return json(200, store.repoView(repo));
      if (method === 'PATCH') {
        if (typeof body.description === 'string') repo.description = body.description;
        if (typeof body.private === 'boolean') repo.private = body.private;
        store.touch(repo);
        return json(200, store.repoView(repo));
      }
      if (method === 'DELETE') {
        store.deleteRepo(owner, repoName);
        return { status: 204, body: null };
      }
      return error(404, 'Not Found');
    }

    if (rest[0] === 'branches') {
      if (rest.length === 1 && method === 'GET') {
        const branches = [...repo.refs.keys()]
          .filter((ref) => ref.startsWith('refs/heads/'))
          .map((ref) => ref.slice('refs/heads/'.length))
          .sort()
          .map((name) => ({ name, commit: { sha: repo.refs.get(`refs/heads/${name}`) as string }, protected: false }));
        return json(200, branches);
      }
      if (rest.length === 2 && method === 'GET') {
        const sha = repo.refs.get(`refs/heads/${rest[1]}`);
        if (!sha) return error(404, 'Not Found');
        return json(200, { name: rest[1], commit: { sha }, protected: false });
      }
      return error(404, 'Not Found');
    }

    if (rest[0] === 'commits' && rest.length === 1 && method === 'GET') {
      const ref = query.get('sha');
      const from = ref ? (isObjectId(ref) ? ref : store.refTip(repo, ref)) : store.headCommit(repo);
      const history = store.commitHistory(repo, from, 100).map((entry) => commitSummary(repo, entry.sha));
      const { page, headers } = paginate(history, query, requestUrl);
      return json(200, page, headers);
    }

    if (rest[0] === 'git' && rest[1] === 'refs') {
      if (method === 'POST' && rest.length === 2) {
        const ref = asString(body.ref);
        const sha = asString(body.sha);
        if (!ref.startsWith('refs/') || !isObjectId(sha)) return error(422, 'ref and sha are required');
        if (!repo.objects.has(sha)) return error(422, 'sha does not exist');
        if (repo.refs.has(ref)) return error(422, 'Reference already exists');
        repo.refs.set(ref, sha);
        store.touch(repo);
        return json(201, { ref, object: { sha, type: 'commit' } });
      }
      if (method === 'GET' && rest.length === 4 && rest[2] === 'heads') {
        const sha = repo.refs.get(`refs/heads/${rest[3]}`);
        if (!sha) return error(404, 'Not Found');
        return json(200, { ref: `refs/heads/${rest[3]}`, object: { sha, type: 'commit' } });
      }
      return error(404, 'Not Found');
    }

    if (rest[0] === 'contents') {
      const path = rest.slice(1).map((segment) => decodeURIComponent(segment)).join('/');
      if (method === 'GET') {
        const ref = query.get('ref');
        const from = ref ? (isObjectId(ref) ? ref : store.refTip(repo, ref)) : store.headCommit(repo);
        if (!from) return error(404, 'Not Found');
        const files = store.filesOfCommit(repo, from);
        const blobOid = files.get(path);
        if (blobOid) {
          const blob = repo.objects.get(blobOid);
          const data = blob?.data ?? Buffer.alloc(0);
          return json(200, {
            type: 'file',
            encoding: 'base64',
            size: data.length,
            name: path.split('/').pop() ?? path,
            path,
            content: data.toString('base64'),
            sha: blobOid,
          });
        }
        const prefix = path ? `${path}/` : '';
        const treeOids = store.treeOidsOfCommit(repo, from);
        const entries: Record<string, unknown>[] = [];
        const seenDirs = new Set<string>();
        for (const [filePath, oid] of files) {
          if (!filePath.startsWith(prefix)) continue;
          const remainder = filePath.slice(prefix.length);
          const slash = remainder.indexOf('/');
          if (slash < 0) {
            entries.push({ type: 'file', name: remainder, path: filePath, sha: oid, size: repo.objects.get(oid)?.data.length ?? 0 });
          } else {
            const dir = remainder.slice(0, slash);
            if (!seenDirs.has(dir)) {
              seenDirs.add(dir);
              entries.push({ type: 'dir', name: dir, path: `${prefix}${dir}`, sha: treeOids.get(`${prefix}${dir}`) ?? '' });
            }
          }
        }
        if (entries.length === 0) return error(404, 'Not Found');
        entries.sort((a, b) => String(a.name).localeCompare(String(b.name)));
        return json(200, entries);
      }
      if (method === 'PUT') {
        const message = asString(body.message) || `Update ${path}`;
        const content = asString(body.content).replace(/\s/g, '');
        const branch = asString(body.branch) || repo.default_branch;
        const sha = asString(body.sha);
        const tip = repo.refs.get(`refs/heads/${branch}`);
        const files = tip ? store.filesOfCommit(repo, tip) : new Map<string, string>();
        const existing = files.get(path);
        if (existing) {
          if (!sha) return error(409, 'sha is required to update a file');
          if (sha !== existing) return error(409, 'sha does not match the current file');
        } else if (sha) {
          return error(422, 'sha does not match a file that does not exist');
        }
        if (repo.refs.size > 0 && !tip) return error(422, 'branch does not exist');
        const blobOid = store.putObject(repo, 'blob', Buffer.from(content, 'base64'));
        const { commitOid } = store.commitFiles(repo, branch, message, (next) => {
          next.set(path, blobOid);
        });
        const status = existing ? 200 : 201;
        return json(status, {
          content: { name: path.split('/').pop() ?? path, path, sha: blobOid, size: Buffer.from(content, 'base64').length, encoding: 'base64' },
          commit: { sha: commitOid, message },
        });
      }
      if (method === 'DELETE') {
        const message = asString(body.message) || `Delete ${path}`;
        const sha = asString(body.sha);
        const tip = store.headCommit(repo);
        if (!tip) return error(404, 'Not Found');
        const files = store.filesOfCommit(repo, tip);
        const existing = files.get(path);
        if (!existing) return error(404, 'Not Found');
        if (sha && sha !== existing) return error(409, 'sha does not match the current file');
        const { commitOid } = store.commitFiles(repo, repo.default_branch, message, (next) => {
          next.delete(path);
        });
        return json(200, { commit: { sha: commitOid, message } });
      }
      return error(404, 'Not Found');
    }

    if (rest[0] === 'labels') {
      if (method === 'POST' && rest.length === 1) {
        const name = asString(body.name).trim();
        const color = asString(body.color) || '000000';
        if (!name) return error(422, 'name is required');
        const existing = repo.labels.get(name);
        if (existing) return json(201, { name: existing.name, color: existing.color });
        const label = { name, color };
        repo.labels.set(name, label);
        store.touch(repo);
        return json(201, { name, color });
      }
      if (method === 'GET' && rest.length === 1) {
        return json(200, [...repo.labels.values()].map((label) => ({ name: label.name, color: label.color })));
      }
      return error(404, 'Not Found');
    }

    if (rest[0] === 'issues') {
      if (rest.length === 1) {
        if (method === 'POST') {
          const title = asString(body.title).trim();
          if (!title) return error(422, 'title is required');
          const labelNames = issueLabelNames(body.labels);
          for (const name of labelNames) {
            if (!repo.labels.has(name)) return error(422, `label ${name} does not exist`);
          }
          const issue = store.createIssue(repo, title, asString(body.body), labelNames);
          return json(201, store.issueView(issue, repo));
        }
        if (method === 'GET') {
          const state = query.get('state') ?? 'open';
          const sort = query.get('sort') ?? 'created';
          const direction = query.get('direction') ?? 'desc';
          let issues = [...repo.issues.values()];
          if (state === 'open') issues = issues.filter((issue) => issue.state === 'open');
          else if (state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
          const key = sort === 'updated' ? 'updated_at' : 'created_at';
          issues.sort((a, b) => {
            const byTime = a[key].localeCompare(b[key]);
            if (byTime !== 0) return direction === 'asc' ? byTime : -byTime;
            return direction === 'asc' ? a.number - b.number : b.number - a.number;
          });
          const { page, headers } = paginate(issues.map((issue) => store.issueView(issue, repo)), query, requestUrl);
          return json(200, page, headers);
        }
        return error(404, 'Not Found');
      }
      const number = Number.parseInt(rest[1] as string, 10);
      if (!Number.isInteger(number)) return error(404, 'Not Found');
      const issue = repo.issues.get(number);
      if (!issue) return error(404, 'Not Found');
      if (rest.length === 2) {
        if (method === 'GET') return json(200, store.issueView(issue, repo));
        if (method === 'PATCH') {
          if (typeof body.state === 'string' && (body.state === 'open' || body.state === 'closed')) issue.state = body.state;
          if (typeof body.title === 'string') issue.title = body.title;
          if (typeof body.body === 'string') issue.body = body.body;
          issue.updated_at = store.nowIso();
          store.touch(repo);
          return json(200, store.issueView(issue, repo));
        }
        return error(404, 'Not Found');
      }
      if (rest[2] === 'comments') {
        if (method === 'POST') {
          const commentBody = asString(body.body);
          if (!commentBody) return error(422, 'body is required');
          const comment = store.addComment(repo, issue, commentBody);
          return json(201, store.commentView(comment));
        }
        if (method === 'GET') {
          return json(200, issue.comments.map((comment) => store.commentView(comment)));
        }
        return error(404, 'Not Found');
      }
      return error(404, 'Not Found');
    }

    if (rest[0] === 'pulls') {
      if (rest.length === 1) {
        if (method === 'POST') {
          const title = asString(body.title).trim();
          const head = asString(body.head);
          const base = asString(body.base);
          if (!title || !head || !base) return error(422, 'title, head and base are required');
          const headSha = store.refTip(repo, head);
          const baseSha = store.refTip(repo, base);
          if (!headSha || !baseSha) return error(422, 'head or base branch does not exist');
          const duplicate = [...repo.pulls.values()].find(
            (pull) => pull.state === 'open' && pull.head_ref === head && pull.base_ref === base,
          );
          if (duplicate) return error(422, 'A pull request already exists for these branches');
          const pull = store.createPull(repo, title, asString(body.body), head, base, headSha, baseSha);
          return json(201, store.pullView(pull, repo));
        }
        if (method === 'GET') {
          const state = query.get('state') ?? 'open';
          let pulls = [...repo.pulls.values()];
          if (state === 'open') pulls = pulls.filter((pull) => pull.state === 'open');
          else if (state === 'closed') pulls = pulls.filter((pull) => pull.state === 'closed');
          pulls.sort((a, b) => b.number - a.number);
          const { page, headers } = paginate(pulls.map((pull) => store.pullView(pull, repo)), query, requestUrl);
          return json(200, page, headers);
        }
        return error(404, 'Not Found');
      }
      const number = Number.parseInt(rest[1] as string, 10);
      if (!Number.isInteger(number)) return error(404, 'Not Found');
      const pull = repo.pulls.get(number);
      if (!pull) return error(404, 'Not Found');
      if (rest.length === 2) {
        if (method === 'GET') return json(200, store.pullView(pull, repo));
        return error(404, 'Not Found');
      }
      if (rest[2] === 'files' && method === 'GET') {
        const files = store.diffFiles(repo, pull.base_sha, pull.head_sha).map((entry) => ({
          filename: entry.filename,
          status: entry.status === 'removed' ? 'changed' : entry.status,
          additions: entry.additions,
          deletions: entry.deletions,
          changes: entry.changes,
        }));
        return json(200, files);
      }
      if (rest[2] === 'merge' && method === 'PUT') {
        const methodName = asString(body.merge_method) || 'squash';
        if (methodName !== 'squash' && methodName !== 'merge' && methodName !== 'rebase') {
          return error(422, 'merge_method must be merge, squash or rebase');
        }
        const result = store.mergePull(repo, pull);
        if (!result.ok) {
          if (result.reason === 'conflict') return error(405, 'Merge conflict');
          return error(405, result.reason);
        }
        return json(200, { sha: result.sha, merged: true, message: 'Pull Request successfully merged' });
      }
      return error(404, 'Not Found');
    }

    return error(404, 'Not Found');
  }

  return error(404, 'Not Found');
}
