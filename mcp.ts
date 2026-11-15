// MCP over Streamable HTTP (JSON-RPC 2.0, plain JSON answers): the six
// tools the suite requires plus two convenience tools, all acting on the
// same in-memory store as REST and GraphQL. The tools array is built once
// so two tools/list calls answer byte-identical JSON.

import { VIEWER } from './scenario.ts';
import type { Store } from './store.ts';

interface McpTool {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

const TOOLS: McpTool[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
      additionalProperties: false,
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
      additionalProperties: false,
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
      additionalProperties: false,
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state: open, closed or all', enum: ['open', 'closed', 'all'] },
      },
      required: ['owner', 'repo'],
      additionalProperties: false,
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch onto a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
      additionalProperties: false,
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with the squash method.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
      additionalProperties: false,
    },
  },
  {
    name: 'list_repos',
    description: 'List repositories owned by a user (defaults to the authenticated user).',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
      },
      required: [],
      additionalProperties: false,
    },
  },
  {
    name: 'get_issue',
    description: 'Get one issue by repository and number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        issue_number: { type: 'integer', description: 'Issue number' },
      },
      required: ['owner', 'repo', 'issue_number'],
      additionalProperties: false,
    },
  },
];

const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25'];
const SERVER_INFO = { name: 'forge-mcp', version: '1.0.0' };

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: unknown): number {
  return typeof value === 'number' ? value : Number.parseInt(String(value ?? ''), 10);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

export interface McpResponse {
  status: number;
  body: unknown;
}

function envelope(id: unknown, result: unknown): McpResponse {
  return { status: 200, body: { jsonrpc: '2.0', id: id ?? null, result } };
}

function errorEnvelope(id: unknown, code: number, message: string, status = 200): McpResponse {
  return { status, body: { jsonrpc: '2.0', id: id ?? null, error: { code, message } } };
}

function toolResult(data: unknown): McpResponse['body'] {
  return {
    content: [{ type: 'text', text: JSON.stringify(data) }],
    structuredContent: data,
  };
}

function toolError(message: string): McpResponse['body'] {
  return {
    content: [{ type: 'text', text: JSON.stringify({ error: message }) }],
    isError: true,
  };
}

function missingArgs(args: Record<string, unknown>, required: readonly string[]): string | null {
  for (const key of required) {
    if (args[key] === undefined || args[key] === null || args[key] === '') return `Missing required argument: ${key}`;
  }
  return null;
}

function callTool(store: Store, name: string, args: Record<string, unknown>): unknown {
  const owner = asString(args.owner) || VIEWER.login;
  const repoName = asString(args.repo);
  switch (name) {
    case 'create_repo': {
      const missing = missingArgs(args, ['name']);
      if (missing) return toolError(missing);
      const repoNameArg = asString(args.name);
      if (store.getRepo(VIEWER.login, repoNameArg)) return toolError('Repository already exists');
      const repo = store.createRepo(VIEWER.login, repoNameArg, asString(args.description), args.private === true);
      return toolResult(store.repoView(repo));
    }
    case 'get_repo': {
      const missing = missingArgs(args, ['owner', 'repo']);
      if (missing) return toolError(missing);
      if (owner !== VIEWER.login) return toolError('Not Found');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError('Not Found');
      return toolResult(store.repoView(repo));
    }
    case 'create_issue': {
      const missing = missingArgs(args, ['owner', 'repo', 'title']);
      if (missing) return toolError(missing);
      if (owner !== VIEWER.login) return toolError('Not Found');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError('Not Found');
      const issue = store.createIssue(repo, asString(args.title), asString(args.body));
      return toolResult(store.issueView(issue, repo));
    }
    case 'list_issues': {
      const missing = missingArgs(args, ['owner', 'repo']);
      if (missing) return toolError(missing);
      if (owner !== VIEWER.login) return toolError('Not Found');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError('Not Found');
      const state = asString(args.state) || 'open';
      const issues = [...repo.issues.values()].filter((issue) => state === 'all' || issue.state === state);
      return toolResult({ issues: issues.map((issue) => store.issueView(issue, repo)) });
    }
    case 'create_pull_request': {
      const missing = missingArgs(args, ['owner', 'repo', 'title', 'head', 'base']);
      if (missing) return toolError(missing);
      if (owner !== VIEWER.login) return toolError('Not Found');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError('Not Found');
      const head = asString(args.head);
      const base = asString(args.base);
      const headSha = store.refTip(repo, head);
      const baseSha = store.refTip(repo, base);
      if (!headSha || !baseSha) return toolError('head or base branch does not exist');
      const duplicate = [...repo.pulls.values()].find(
        (pull) => pull.state === 'open' && pull.head_ref === head && pull.base_ref === base,
      );
      if (duplicate) return toolError('A pull request already exists for these branches');
      const pull = store.createPull(repo, asString(args.title), '', head, base, headSha, baseSha);
      return toolResult(store.pullView(pull, repo));
    }
    case 'merge_pull_request': {
      const missing = missingArgs(args, ['owner', 'repo', 'pull_number']);
      if (missing) return toolError(missing);
      if (owner !== VIEWER.login) return toolError('Not Found');
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError('Not Found');
      const pull = repo.pulls.get(asNumber(args.pull_number));
      if (!pull) return toolError('Not Found');
      const result = store.mergePull(repo, pull);
      if (!result.ok) return toolError(result.reason);
      return toolResult({ merged: true, number: pull.number, sha: result.sha });
    }
    case 'list_repos': {
      const repos = store.listReposFor(owner);
      return toolResult({ repositories: repos.map((repo) => store.repoView(repo)) });
    }
    case 'get_issue': {
      const missing = missingArgs(args, ['owner', 'repo', 'issue_number']);
      if (missing) return toolError(missing);
      if (owner !== VIEWER.login) return toolError('Not Found');
      const repo = store.getRepo(owner, repoName);
      const issue = repo?.issues.get(asNumber(args.issue_number));
      if (!repo || !issue) return toolError('Not Found');
      return toolResult(store.issueView(issue, repo));
    }
    default:
      return null;
  }
}

export function handleMcp(
  store: Store,
  body: unknown,
  bodyParseError: boolean,
): McpResponse {
  if (bodyParseError) {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } } };
  }
  if (!isRecord(body) || typeof body.method !== 'string') {
    return errorEnvelope(null, -32600, 'Invalid Request');
  }
  const id = body.id ?? null;
  const isNotification = body.id === undefined;
  const params = isRecord(body.params) ? body.params : {};

  if (body.method === 'notifications/initialized') {
    return { status: 202, body: null };
  }
  if (body.method === 'ping') {
    return envelope(id, {});
  }
  if (body.method === 'server/discover') {
    return envelope(id, {
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: SERVER_INFO,
      _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
    });
  }
  if (body.method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    return envelope(id, {
      protocolVersion: SUPPORTED_VERSIONS.includes(requested) ? requested : '2026-07-28',
      capabilities: { tools: {} },
      serverInfo: SERVER_INFO,
    });
  }
  if (body.method === 'tools/list') {
    return envelope(id, { tools: TOOLS });
  }
  if (body.method === 'tools/call') {
    const name = asString(params.name);
    const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    if (!name) return errorEnvelope(id, -32602, 'Tool name is required');
    if (!TOOLS.some((tool) => tool.name === name)) return errorEnvelope(id, -32601, `Unknown tool: ${name}`);
    const result = callTool(store, name, args);
    if (result === null) return errorEnvelope(id, -32601, `Unknown tool: ${name}`);
    return envelope(id, result);
  }
  if (isNotification) return { status: 202, body: null };
  return errorEnvelope(id, -32601, `Method not found: ${body.method}`);
}
