// GraphQL v4: a small parser and executor for the exact documents the
// conformance suite sends (viewer, repository, issues/pulls connections,
// mutations, node lookup, __type introspection, batch operations). Unknown
// fields answer HTTP 200 with an errors array naming the field; unknown
// repositories answer data.repository null. No model calls, no network.

import { VIEWER } from './scenario.ts';
import type { Issue, PullRequest, Repo, Store } from './store.ts';
import { Store as StoreClass } from './store.ts';

// ---------------------------------------------------------------------------------------------
// lexer / parser
// ---------------------------------------------------------------------------------------------

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'punct'; value: string }
  | { kind: 'spread' };

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < source.length) {
    const ch = source[i] as string;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && source.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'spread' });
      i += 3;
      continue;
    }
    if ('{}():!@=|&$[]'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      let value = '';
      i += 1;
      while (i < source.length && source[i] !== '"') {
        if (source[i] === '\\' && i + 1 < source.length) {
          const escaped = source[i + 1] as string;
          value += escaped === 'n' ? '\n' : escaped === 't' ? '\t' : escaped === 'r' ? '\r' : escaped;
          i += 2;
        } else {
          value += source[i];
          i += 1;
        }
      }
      i += 1;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let text = '';
      while (i < source.length && /[0-9.eE+-]/.test(source[i] as string)) {
        text += source[i];
        i += 1;
      }
      const num = Number(text);
      tokens.push(text.includes('.') || text.includes('e') || text.includes('E') ? { kind: 'float', value: num } : { kind: 'int', value: num });
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let text = '';
      while (i < source.length && /[A-Za-z0-9_]/.test(source[i] as string)) {
        text += source[i];
        i += 1;
      }
      tokens.push({ kind: 'name', value: text });
      continue;
    }
    throw new Error(`unexpected character ${ch} in GraphQL query`);
  }
  return tokens;
}

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: { name: string; value: ValueNode }[] };

interface FieldNode {
  alias: string | null;
  name: string;
  arguments: { name: string; value: ValueNode }[];
  selectionSet: SelectionNode[] | null;
}

type SelectionNode =
  | { kind: 'field'; field: FieldNode }
  | { kind: 'inlineFragment'; typeCondition: string | null; selectionSet: SelectionNode[] }
  | { kind: 'fragmentSpread'; name: string };

interface OperationNode {
  kind: 'query' | 'mutation';
  name: string | null;
  variableDefinitions: { name: string; defaultValue: ValueNode | null }[];
  selectionSet: SelectionNode[];
}

interface FragmentNode {
  name: string;
  typeCondition: string;
  selectionSet: SelectionNode[];
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  peek(): Token | undefined {
    return this.tokens[this.pos];
  }
  next(): Token {
    const token = this.tokens[this.pos];
    if (!token) throw new Error('unexpected end of GraphQL query');
    this.pos += 1;
    return token;
  }
  expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected ${value}`);
  }
  acceptPunct(value: string): boolean {
    const token = this.peek();
    if (token?.kind === 'punct' && token.value === value) {
      this.pos += 1;
      return true;
    }
    return false;
  }
  expectName(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error('expected a name');
    return token.value;
  }
  acceptName(value: string): boolean {
    const token = this.peek();
    if (token?.kind === 'name' && token.value === value) {
      this.pos += 1;
      return true;
    }
    return false;
  }

  parseDocument(): { operations: OperationNode[]; fragments: Map<string, FragmentNode> } {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, FragmentNode>();
    while (this.pos < this.tokens.length) {
      const token = this.peek();
      if (token?.kind === 'punct' && token.value === '{') {
        operations.push({ kind: 'query', name: null, variableDefinitions: [], selectionSet: this.parseSelectionSet() });
        continue;
      }
      if (token?.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expectName(); // 'on'
        const typeCondition = this.expectName();
        fragments.set(name, { name, typeCondition, selectionSet: this.parseSelectionSet() });
        continue;
      }
      if (token?.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
        const kind = this.next().value as 'query' | 'mutation';
        let name: string | null = null;
        const next = this.peek();
        if (next?.kind === 'name') name = this.expectName();
        const variableDefinitions = this.parseVariableDefinitions();
        operations.push({ kind, name, variableDefinitions, selectionSet: this.parseSelectionSet() });
        continue;
      }
      throw new Error('unexpected token at top level of GraphQL document');
    }
    return { operations, fragments };
  }

  parseVariableDefinitions(): { name: string; defaultValue: ValueNode | null }[] {
    const defs: { name: string; defaultValue: ValueNode | null }[] = [];
    if (!this.acceptPunct('(')) return defs;
    while (!this.acceptPunct(')')) {
      this.expectPunct('$');
      const name = this.expectName();
      this.expectPunct(':');
      // Skip the type (name, brackets, !) — types are not needed to execute.
      let depth = 0;
      for (;;) {
        const token = this.next();
        if (token.kind === 'punct' && token.value === '[') depth += 1;
        else if (token.kind === 'punct' && token.value === ']') depth -= 1;
        else if (token.kind === 'punct' && (token.value === ',' || token.value === ')')) {
          if (depth === 0) {
            if (token.value === ')') this.pos -= 1;
            break;
          }
        }
      }
      let defaultValue: ValueNode | null = null;
      if (this.acceptPunct('=')) defaultValue = this.parseValue();
      defs.push({ name, defaultValue });
    }
    return defs;
  }

  parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!this.acceptPunct('}')) {
      const token = this.peek();
      if (token?.kind === 'spread') {
        this.next();
        const next = this.peek();
        if (next?.kind === 'name' && next.value === 'on') {
          this.next();
          const typeCondition = this.expectName();
          selections.push({ kind: 'inlineFragment', typeCondition, selectionSet: this.parseSelectionSet() });
        } else if (next?.kind === 'name') {
          selections.push({ kind: 'fragmentSpread', name: this.expectName() });
        } else {
          selections.push({ kind: 'inlineFragment', typeCondition: null, selectionSet: this.parseSelectionSet() });
        }
        continue;
      }
      selections.push({ kind: 'field', field: this.parseField() });
    }
    return selections;
  }

  parseField(): FieldNode {
    let name = this.expectName();
    let alias: string | null = null;
    if (this.acceptPunct(':')) {
      alias = name;
      name = this.expectName();
    }
    const args: { name: string; value: ValueNode }[] = [];
    if (this.acceptPunct('(')) {
      while (!this.acceptPunct(')')) {
        const argName = this.expectName();
        this.expectPunct(':');
        args.push({ name: argName, value: this.parseValue() });
      }
    }
    while (this.acceptPunct('@')) {
      this.expectName();
      if (this.acceptPunct('(')) {
        while (!this.acceptPunct(')')) this.parseValue();
      }
    }
    let selectionSet: SelectionNode[] | null = null;
    const next = this.peek();
    if (next?.kind === 'punct' && next.value === '{') selectionSet = this.parseSelectionSet();
    return { alias, name, arguments: args, selectionSet };
  }

  parseValue(): ValueNode {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') return { kind: 'variable', name: this.expectName() };
    if (token.kind === 'punct' && token.value === '[') {
      const values: ValueNode[] = [];
      while (!this.acceptPunct(']')) values.push(this.parseValue());
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: { name: string; value: ValueNode }[] = [];
      while (!this.acceptPunct('}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue() });
      }
      return { kind: 'object', fields };
    }
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'int') return { kind: 'int', value: token.value };
    if (token.kind === 'float') return { kind: 'float', value: token.value };
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new Error('unexpected value in GraphQL query');
  }
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

interface GqlContext {
  store: Store;
  variables: Record<string, unknown>;
  fragments: Map<string, FragmentNode>;
  errors: { message: string }[];
}

type GqlValue =
  | { __typename: 'Query' }
  | { __typename: 'Mutation' }
  | { __typename: 'User'; login: string; id: number; name: string }
  | { __typename: 'Repository'; repo: Repo }
  | { __typename: 'Issue'; repo: Repo; issue: Issue }
  | { __typename: 'PullRequest'; repo: Repo; pull: PullRequest }
  | { __typename: 'Ref'; name: string }
  | { __typename: 'IssueConnection'; nodes: GqlValue[]; pageInfo: GqlValue }
  | { __typename: 'PullRequestConnection'; nodes: GqlValue[]; pageInfo: GqlValue }
  | { __typename: 'PullRequestFileConnection'; nodes: GqlValue[] }
  | { __typename: 'PullRequestFile'; path: string }
  | { __typename: 'Comment'; id: string; body: string }
  | { __typename: 'CommentEdge'; node: GqlValue }
  | { __typename: 'PageInfo'; hasNextPage: boolean; endCursor: string | null }
  | { __typename: 'IssuePayload'; issue: GqlValue | null }
  | { __typename: 'PullRequestPayload'; pullRequest: GqlValue | null }
  | { __typename: 'CommentPayload'; commentEdge: GqlValue | null }
  | { __typename: 'Type'; name: string; kind: string; fields: { name: string }[] | null; enumValues: { name: string }[] | null };

function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name];
    case 'int':
    case 'float':
    case 'string':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((value) => resolveValue(value, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = resolveValue(field.value, variables);
      return out;
    }
  }
}

function argsOf(field: FieldNode, ctx: GqlContext): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of field.arguments) out[arg.name] = resolveValue(arg.value, ctx.variables);
  return out;
}

function cursorFor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64url');
}

function cursorNumber(cursor: string | null | undefined): number | null {
  if (!cursor) return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(text);
    return match ? Number(match[1]) : null;
  } catch {
    return null;
  }
}

function page<T extends { number: number }>(
  items: readonly T[],
  first: unknown,
  after: unknown,
  descending: boolean,
): { slice: T[]; hasNextPage: boolean; endCursor: string | null } {
  const afterNumber = cursorNumber(typeof after === 'string' ? after : null);
  let rest = items;
  if (afterNumber !== null) {
    // A cursor names the last item of the previous page; the next page holds
    // the items after it in the page's own order.
    rest = rest.filter((item) => (descending ? item.number < afterNumber : item.number > afterNumber));
  }
  const limit = typeof first === 'number' && first > 0 ? first : rest.length;
  const slice = rest.slice(0, limit);
  const hasNextPage = rest.length > slice.length;
  return { slice, hasNextPage, endCursor: slice.length ? cursorFor((slice[slice.length - 1] as T).number) : null };
}

const INTROSPECTION: Record<string, { kind: string; fields: string[] | null; enumValues: string[] | null }> = {
  Issue: { kind: 'OBJECT', fields: ['id', 'number', 'title', 'state', 'body'], enumValues: null },
  PullRequest: {
    kind: 'OBJECT',
    fields: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'],
    enumValues: null,
  },
  Repository: {
    kind: 'OBJECT',
    fields: [
      'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
      'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest',
    ],
    enumValues: null,
  },
  SearchType: { kind: 'ENUM', fields: null, enumValues: ['ISSUE', 'USER', 'REPOSITORY'] },
};

function repoValue(repo: Repo): GqlValue {
  return { __typename: 'Repository', repo };
}
function issueValue(repo: Repo, issue: Issue): GqlValue {
  return { __typename: 'Issue', repo, issue };
}
function pullValue(repo: Repo, pull: PullRequest): GqlValue {
  return { __typename: 'PullRequest', repo, pull };
}

function findRepoByDatabaseId(store: Store, databaseId: number): Repo | undefined {
  for (const repo of store.repos.values()) if (repo.databaseId === databaseId) return repo;
  return undefined;
}

function resolveField(value: GqlValue, field: FieldNode, ctx: GqlContext): unknown {
  const { store } = ctx;
  const args = argsOf(field, ctx);
  switch (value.__typename) {
    case 'Query': {
      if (field.name === 'viewer') {
        return { __typename: 'User', login: VIEWER.login, id: VIEWER.id, name: VIEWER.name } as GqlValue;
      }
      if (field.name === 'repository') {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        if (owner !== VIEWER.login) return null;
        const repo = store.getRepo(owner, name);
        return repo ? repoValue(repo) : null;
      }
      if (field.name === 'node') {
        const decoded = StoreClass.decodeGlobalId(String(args.id ?? ''));
        if (!decoded) return null;
        if (decoded.kind === 'Repository') {
          const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
          return repo ? repoValue(repo) : null;
        }
        if (decoded.kind === 'Issue') {
          const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
          const issue = repo?.issues.get(Number(decoded.parts[1]));
          return repo && issue ? issueValue(repo, issue) : null;
        }
        if (decoded.kind === 'PullRequest') {
          const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
          const pull = repo?.pulls.get(Number(decoded.parts[1]));
          return repo && pull ? pullValue(repo, pull) : null;
        }
        return null;
      }
      if (field.name === '__type') {
        const spec = INTROSPECTION[String(args.name ?? '')];
        if (!spec) return null;
        return {
          __typename: 'Type',
          name: String(args.name ?? ''),
          kind: spec.kind,
          fields: spec.fields?.map((name) => ({ name })) ?? null,
          enumValues: spec.enumValues?.map((name) => ({ name })) ?? null,
        } as GqlValue;
      }
      break;
    }
    case 'Mutation': {
      if (field.name === 'createIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = StoreClass.decodeGlobalId(String(input.repositoryId ?? ''));
        if (!decoded || decoded.kind !== 'Repository') return { __typename: 'IssuePayload', issue: null } as GqlValue;
        const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
        if (!repo) return { __typename: 'IssuePayload', issue: null } as GqlValue;
        const issue = store.createIssue(repo, String(input.title ?? ''), String(input.body ?? ''));
        return { __typename: 'IssuePayload', issue: issueValue(repo, issue) } as GqlValue;
      }
      if (field.name === 'closeIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = StoreClass.decodeGlobalId(String(input.issueId ?? ''));
        if (!decoded || decoded.kind !== 'Issue') return { __typename: 'IssuePayload', issue: null } as GqlValue;
        const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
        const issue = repo?.issues.get(Number(decoded.parts[1]));
        if (!repo || !issue) return { __typename: 'IssuePayload', issue: null } as GqlValue;
        issue.state = 'closed';
        issue.updated_at = store.nowIso();
        store.touch(repo);
        return { __typename: 'IssuePayload', issue: issueValue(repo, issue) } as GqlValue;
      }
      if (field.name === 'createPullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = StoreClass.decodeGlobalId(String(input.repositoryId ?? ''));
        if (!decoded || decoded.kind !== 'Repository') return { __typename: 'PullRequestPayload', pullRequest: null } as GqlValue;
        const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
        if (!repo) return { __typename: 'PullRequestPayload', pullRequest: null } as GqlValue;
        const headRef = String(input.headRefName ?? '');
        const baseRef = String(input.baseRefName ?? '');
        const headSha = store.refTip(repo, headRef);
        const baseSha = store.refTip(repo, baseRef);
        if (!headSha || !baseSha) return { __typename: 'PullRequestPayload', pullRequest: null } as GqlValue;
        const pull = store.createPull(repo, String(input.title ?? ''), '', headRef, baseRef, headSha, baseSha);
        return { __typename: 'PullRequestPayload', pullRequest: pullValue(repo, pull) } as GqlValue;
      }
      if (field.name === 'mergePullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = StoreClass.decodeGlobalId(String(input.pullRequestId ?? ''));
        if (!decoded || decoded.kind !== 'PullRequest') return { __typename: 'PullRequestPayload', pullRequest: null } as GqlValue;
        const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
        const pull = repo?.pulls.get(Number(decoded.parts[1]));
        if (!repo || !pull) return { __typename: 'PullRequestPayload', pullRequest: null } as GqlValue;
        store.mergePull(repo, pull);
        return { __typename: 'PullRequestPayload', pullRequest: pullValue(repo, pull) } as GqlValue;
      }
      if (field.name === 'addComment') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = StoreClass.decodeGlobalId(String(input.subjectId ?? ''));
        if (!decoded || decoded.kind !== 'Issue') return { __typename: 'CommentPayload', commentEdge: null } as GqlValue;
        const repo = findRepoByDatabaseId(store, Number(decoded.parts[0]));
        const issue = repo?.issues.get(Number(decoded.parts[1]));
        if (!repo || !issue) return { __typename: 'CommentPayload', commentEdge: null } as GqlValue;
        const comment = store.addComment(repo, issue, String(input.body ?? ''));
        const node: GqlValue = {
          __typename: 'Comment',
          id: StoreClass.globalId('IssueComment', [repo.databaseId, issue.number, comment.id]),
          body: comment.body,
        };
        return { __typename: 'CommentPayload', commentEdge: { __typename: 'CommentEdge', node } } as GqlValue;
      }
      break;
    }
    case 'User': {
      if (field.name === 'login') return value.login;
      if (field.name === 'id') return value.id;
      if (field.name === 'name') return value.name;
      break;
    }
    case 'Repository': {
      const repo = value.repo;
      if (field.name === 'id') return StoreClass.globalId('Repository', [repo.databaseId]);
      if (field.name === 'databaseId') return repo.databaseId;
      if (field.name === 'name') return repo.name;
      if (field.name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
      if (field.name === 'owner') return { __typename: 'User', login: repo.owner, id: repo.owner === VIEWER.login ? VIEWER.id : 1001, name: repo.owner } as GqlValue;
      if (field.name === 'defaultBranchRef') {
        const tip = store.headCommit(repo);
        return tip ? ({ __typename: 'Ref', name: repo.default_branch } as GqlValue) : null;
      }
      if (field.name === 'viewerPermission') return 'ADMIN';
      if (field.name === 'hasIssuesEnabled') return true;
      if (field.name === 'issue') {
        const issue = repo.issues.get(Number(args.number));
        return issue ? issueValue(repo, issue) : null;
      }
      if (field.name === 'issues') {
        const states = Array.isArray(args.states) ? (args.states as string[]) : null;
        const orderBy = (args.orderBy ?? {}) as Record<string, unknown>;
        let issues = [...repo.issues.values()];
        if (states) {
          const wanted = new Set(states.map((state) => String(state).toLowerCase()));
          issues = issues.filter((issue) => wanted.has(issue.state));
        }
        const direction = String(orderBy.direction ?? 'DESC');
        issues.sort((a, b) => (direction === 'ASC' ? a.number - b.number : b.number - a.number));
        const result = page(issues, args.first, args.after, direction !== 'ASC');
        return {
          __typename: 'IssueConnection',
          nodes: result.slice.map((issue) => issueValue(repo, issue)),
          pageInfo: { __typename: 'PageInfo', hasNextPage: result.hasNextPage, endCursor: result.endCursor } as GqlValue,
        } as GqlValue;
      }
      if (field.name === 'pullRequests') {
        const states = Array.isArray(args.states) ? (args.states as string[]) : null;
        let pulls = [...repo.pulls.values()];
        if (states) {
          const wanted = new Set(states.map((state) => String(state).toLowerCase()));
          pulls = pulls.filter((pull) => wanted.has(pull.state));
        } else {
          pulls = pulls.filter((pull) => pull.state === 'open');
        }
        pulls.sort((a, b) => b.number - a.number);
        const result = page(pulls, args.first, args.after, true);
        return {
          __typename: 'PullRequestConnection',
          nodes: result.slice.map((pull) => pullValue(repo, pull)),
          pageInfo: { __typename: 'PageInfo', hasNextPage: result.hasNextPage, endCursor: result.endCursor } as GqlValue,
        } as GqlValue;
      }
      if (field.name === 'pullRequest') {
        const pull = repo.pulls.get(Number(args.number));
        return pull ? pullValue(repo, pull) : null;
      }
      break;
    }
    case 'Issue': {
      const issue = value.issue;
      if (field.name === 'id') return StoreClass.globalId('Issue', [value.repo.databaseId, issue.number]);
      if (field.name === 'number') return issue.number;
      if (field.name === 'title') return issue.title;
      if (field.name === 'state') return issue.state === 'open' ? 'OPEN' : 'CLOSED';
      if (field.name === 'body') return issue.body;
      break;
    }
    case 'PullRequest': {
      const pull = value.pull;
      const repo = value.repo;
      if (field.name === 'id') return StoreClass.globalId('PullRequest', [repo.databaseId, pull.number]);
      if (field.name === 'number') return pull.number;
      if (field.name === 'headRefName') return pull.head_ref;
      if (field.name === 'baseRefName') return pull.base_ref;
      if (field.name === 'mergeable') {
        const baseTip = store.refTip(repo, pull.base_ref);
        if (!baseTip) return 'UNKNOWN';
        return store.mergeable(repo, baseTip, pull.head_sha) ? 'MERGEABLE' : 'CONFLICTING';
      }
      if (field.name === 'state') return pull.state === 'open' ? 'OPEN' : 'CLOSED';
      if (field.name === 'merged') return pull.merged;
      if (field.name === 'files') {
        const files = store.diffFiles(repo, pull.base_sha, pull.head_sha);
        const first = typeof args.first === 'number' && args.first > 0 ? args.first : files.length;
        return {
          __typename: 'PullRequestFileConnection',
          nodes: files.slice(0, first).map((entry) => ({ __typename: 'PullRequestFile', path: entry.filename } as GqlValue)),
        } as GqlValue;
      }
      break;
    }
    case 'Ref': {
      if (field.name === 'name') return value.name;
      break;
    }
    case 'IssueConnection':
    case 'PullRequestConnection': {
      if (field.name === 'nodes') return value.nodes;
      if (field.name === 'pageInfo') return value.pageInfo;
      break;
    }
    case 'PullRequestFileConnection': {
      if (field.name === 'nodes') return value.nodes;
      break;
    }
    case 'PullRequestFile': {
      if (field.name === 'path') return value.path;
      break;
    }
    case 'Comment': {
      if (field.name === 'id') return value.id;
      if (field.name === 'body') return value.body;
      break;
    }
    case 'CommentEdge': {
      if (field.name === 'node') return value.node;
      break;
    }
    case 'PageInfo': {
      if (field.name === 'hasNextPage') return value.hasNextPage;
      if (field.name === 'endCursor') return value.endCursor;
      if (field.name === 'hasPreviousPage') return false;
      if (field.name === 'startCursor') return null;
      break;
    }
    case 'IssuePayload': {
      if (field.name === 'issue') return value.issue;
      break;
    }
    case 'PullRequestPayload': {
      if (field.name === 'pullRequest') return value.pullRequest;
      break;
    }
    case 'CommentPayload': {
      if (field.name === 'commentEdge') return value.commentEdge;
      break;
    }
    case 'Type': {
      if (field.name === 'name') return value.name;
      if (field.name === 'kind') return value.kind;
      if (field.name === 'fields') return value.fields;
      if (field.name === 'enumValues') return value.enumValues;
      break;
    }
  }
  if (field.name === '__typename') return value.__typename;
  ctx.errors.push({ message: `Cannot query field "${field.name}" on type "${value.__typename}".` });
  return null;
}

function isGqlValue(value: unknown): value is GqlValue {
  return value !== null && typeof value === 'object' && '__typename' in (value as Record<string, unknown>);
}

function executeSelection(
  value: unknown,
  selections: SelectionNode[],
  ctx: GqlContext,
): unknown {
  if (Array.isArray(value)) {
    return value.map((item) => executeSelection(item, selections, ctx));
  }
  if (value === null || typeof value !== 'object') return value;
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'field') {
      const field = selection.field;
      const key = field.alias ?? field.name;
      let resolved: unknown;
      if (isGqlValue(value)) resolved = resolveField(value, field, ctx);
      else resolved = (value as Record<string, unknown>)[field.name] ?? null;
      if (field.selectionSet && resolved !== null && resolved !== undefined) {
        resolved = executeSelection(resolved, field.selectionSet, ctx);
      }
      out[key] = resolved;
    } else if (selection.kind === 'inlineFragment') {
      if (selection.typeCondition && isGqlValue(value) && selection.typeCondition !== value.__typename) continue;
      Object.assign(out, executeSelection(value, selection.selectionSet, ctx) as Record<string, unknown>);
    } else if (selection.kind === 'fragmentSpread') {
      const fragment = ctx.fragments.get(selection.name);
      if (!fragment) {
        ctx.errors.push({ message: `Unknown fragment "${selection.name}".` });
        continue;
      }
      if (fragment.typeCondition && isGqlValue(value) && fragment.typeCondition !== value.__typename) continue;
      Object.assign(out, executeSelection(value, fragment.selectionSet, ctx) as Record<string, unknown>);
    }
  }
  return out;
}

function executeOperation(store: Store, operation: OperationNode, variables: Record<string, unknown>, fragments: Map<string, FragmentNode>): Record<string, unknown> {
  const ctx: GqlContext = { store, variables, fragments, errors: [] };
  const root: GqlValue = operation.kind === 'mutation' ? { __typename: 'Mutation' } : { __typename: 'Query' };
  const data = executeSelection(root, operation.selectionSet, ctx);
  const envelope: Record<string, unknown> = { data };
  if (ctx.errors.length > 0) envelope.errors = ctx.errors;
  return envelope;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

/**
 * Handle one GraphQL request body (single operation or a batch array).
 * Always answers HTTP 200 with an envelope, except a non-object body.
 */
export function handleGraphQL(store: Store, body: unknown): { status: number; body: unknown } {
  const run = (item: unknown): Record<string, unknown> => {
    if (!isRecord(item) || typeof item.query !== 'string') {
      return { data: null, errors: [{ message: 'Request must contain a query string.' }] };
    }
    const variables = isRecord(item.variables) ? (item.variables as Record<string, unknown>) : {};
    let parsed;
    try {
      parsed = new Parser(lex(item.query)).parseDocument();
    } catch (error) {
      return { data: null, errors: [{ message: error instanceof Error ? error.message : 'GraphQL parse error' }] };
    }
    const operation = parsed.operations.find((op) => !item.operationName || op.name === item.operationName) ?? parsed.operations[0];
    if (!operation) return { data: null, errors: [{ message: 'No operation found in document.' }] };
    return executeOperation(store, operation, variables, parsed.fragments);
  };
  if (Array.isArray(body)) return { status: 200, body: body.map(run) };
  if (!isRecord(body)) return { status: 400, body: { errors: [{ message: 'Request body must be a JSON object.' }] } };
  return { status: 200, body: run(body) };
}
