// graphql.ts — a small GraphQL executor for the GitHub-flavoured surface the
// suite exercises: repository/issues/pullRequests queries, node(id:) global
// IDs, Relay-style pagination, mutations, and introspection for __type.
// Only the documents the suite sends are supported; unknown fields produce a
// GraphQL errors array (HTTP 200), unknown repos produce data.repository=null.
import { diffTrees, type IssueRec, type PullRec, type RepoRec, type Store } from './store.ts';

/* ------------------------------------------------------------------ */
/* global IDs (opaque, stable): base64url of "Kind:..."                */
/* ------------------------------------------------------------------ */

export function encodeGlobalId(kind: string, ...parts: (string | number)[]): string {
  return Buffer.from([kind, ...parts.map(String)].join(':'), 'utf8').toString('base64url');
}

export function decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const [kind, ...parts] = text.split(':');
    if (kind === undefined || parts.length === 0) return null;
    return { kind, parts };
  } catch {
    return null;
  }
}

export function repoGlobalId(repo: RepoRec): string {
  return encodeGlobalId('Repository', repo.id);
}

export function issueGlobalId(repo: RepoRec, issue: IssueRec): string {
  return encodeGlobalId('Issue', repo.id, issue.number);
}

export function pullGlobalId(repo: RepoRec, pull: PullRec): string {
  return encodeGlobalId('PullRequest', repo.id, pull.number);
}

export function commentGlobalId(commentId: number): string {
  return encodeGlobalId('IssueComment', commentId);
}

/* ------------------------------------------------------------------ */
/* cursor pagination                                                   */
/* ------------------------------------------------------------------ */

function encodeCursor(index: number): string {
  return Buffer.from(`cursor:${index}`, 'utf8').toString('base64url');
}

function decodeCursor(cursor: string | null | undefined): number {
  if (cursor === null || cursor === undefined) return -1;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    return m === null ? -1 : parseInt(m[1], 10);
  } catch {
    return -1;
  }
}

/* ------------------------------------------------------------------ */
/* lexer + parser (subset: operations, fragments, aliases, variables) */
/* ------------------------------------------------------------------ */

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'punct'; value: string }
  | { kind: 'spread'; value: '...' }
  | { kind: 'var'; value: string };

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && src.startsWith('...', i)) {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let value = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1]!;
          value += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          value += src[j];
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      const m = /^-?\d+/.exec(src.slice(i));
      if (m !== null) {
        tokens.push({ kind: 'int', value: parseInt(m[0], 10) });
        i += m[0].length;
        continue;
      }
    }
    if (/[A-Za-z_]/.test(ch)) {
      const m = /^[A-Za-z_][A-Za-z0-9_]*/.exec(src.slice(i));
      tokens.push({ kind: 'name', value: m![0] });
      i += m![0].length;
      continue;
    }
    if (ch === '$') {
      const m = /^\$[A-Za-z_][A-Za-z0-9_]*/.exec(src.slice(i));
      if (m !== null) {
        tokens.push({ kind: 'var', value: m[0].slice(1) });
        i += m[0].length;
        continue;
      }
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} at ${i}`);
  }
  return tokens;
}

export interface FieldNode {
  name: string;
  alias: string | null;
  args: Record<string, unknown>;
  selection: FieldNode[] | null;
  /** Inline fragment type condition, for `... on Type { ... }` nodes. */
  fragmentType: string | null;
}

export interface OperationNode {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: Record<string, unknown>;
  selection: FieldNode[];
}

class Parser {
  private pos = 0;
  constructor(private tokens: Token[]) {}

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const t = this.tokens[this.pos];
    if (t === undefined) throw new Error('unexpected end of query');
    this.pos += 1;
    return t;
  }

  private expectPunct(value: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== value) throw new Error(`expected ${value}, got ${JSON.stringify(t)}`);
  }

  private peekPunct(value: string): boolean {
    const t = this.peek();
    return t !== undefined && t.kind === 'punct' && t.value === value;
  }

  parseDocument(): OperationNode[] {
    const ops: OperationNode[] = [];
    while (this.pos < this.tokens.length) {
      ops.push(this.parseOperation());
    }
    return ops;
  }

  private parseOperation(): OperationNode {
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const t = this.peek();
    if (t?.kind === 'name' && (t.value === 'query' || t.value === 'mutation')) {
      kind = t.value as 'query' | 'mutation';
      this.next();
      const n = this.peek();
      if (n?.kind === 'name') {
        name = n.value;
        this.next();
      }
    }
    // Variable definitions: ($name: Type! = default, ...)
    const variables: Record<string, unknown> = {};
    if (this.peekPunct('(')) {
      this.next();
      while (!this.peekPunct(')')) {
        const v = this.next();
        if (v.kind !== 'var') throw new Error('expected variable in definition');
        this.expectPunct(':');
        this.skipType();
        if (this.peekPunct('=')) {
          this.next();
          variables[v.value] = this.parseValue({});
        }
        if (this.peekPunct(',')) this.next();
      }
      this.expectPunct(')');
    }
    const selection = this.parseSelectionSet();
    return { kind, name, variables, selection };
  }

  private skipType(): void {
    // Type name, possibly wrapped in [ ] and suffixed with !.
    if (this.peekPunct('[')) {
      this.next();
      this.skipType();
      this.expectPunct(']');
    } else {
      const t = this.next();
      if (t.kind !== 'name') throw new Error('expected type name');
    }
    if (this.peekPunct('!')) this.next();
  }

  private parseSelectionSet(): FieldNode[] {
    this.expectPunct('{');
    const fields: FieldNode[] = [];
    while (!this.peekPunct('}')) {
      fields.push(...this.parseField());
    }
    this.expectPunct('}');
    return fields;
  }

  private parseField(): FieldNode[] {
    const t = this.next();
    if (t.kind === 'spread') {
      // Inline fragment `... on Type { ... }` or named fragment (unsupported).
      const n = this.peek();
      if (n?.kind === 'name' && n.value === 'on') {
        this.next();
        const typeTok = this.next();
        if (typeTok.kind !== 'name') throw new Error('expected type in fragment');
        const selection = this.parseSelectionSet();
        return selection.map((f) => ({ ...f, fragmentType: typeTok.value }));
      }
      throw new Error('named fragments are not supported');
    }
    if (t.kind !== 'name') throw new Error('expected field name');
    let name = t.value;
    let alias: string | null = null;
    if (this.peekPunct(':')) {
      this.next();
      const real = this.next();
      if (real.kind !== 'name') throw new Error('expected field name after alias');
      alias = name;
      name = real.value;
    }
    const args: Record<string, unknown> = {};
    if (this.peekPunct('(')) {
      this.next();
      while (!this.peekPunct(')')) {
        const argName = this.next();
        if (argName.kind !== 'name') throw new Error('expected argument name');
        this.expectPunct(':');
        args[argName.value] = this.parseValue({});
        if (this.peekPunct(',')) this.next();
      }
      this.expectPunct(')');
    }
    let selection: FieldNode[] | null = null;
    if (this.peekPunct('{')) selection = this.parseSelectionSet();
    return [{ name, alias, args, selection, fragmentType: null }];
  }

  private parseValue(variables: Record<string, unknown>): unknown {
    const t = this.next();
    if (t.kind === 'var') return { __variable: t.value };
    if (t.kind === 'string') return t.value;
    if (t.kind === 'int') return t.value;
    if (t.kind === 'name') {
      if (t.value === 'true') return true;
      if (t.value === 'false') return false;
      if (t.value === 'null') return null;
      return { __enum: t.value };
    }
    if (t.kind === 'punct' && t.value === '[') {
      const list: unknown[] = [];
      while (!this.peekPunct(']')) {
        list.push(this.parseValue(variables));
        if (this.peekPunct(',')) this.next();
      }
      this.expectPunct(']');
      return list;
    }
    if (t.kind === 'punct' && t.value === '{') {
      const obj: Record<string, unknown> = {};
      while (!this.peekPunct('}')) {
        const key = this.next();
        if (key.kind !== 'name') throw new Error('expected object key');
        this.expectPunct(':');
        obj[key.value] = this.parseValue(variables);
        if (this.peekPunct(',')) this.next();
      }
      this.expectPunct('}');
      return obj;
    }
    throw new Error(`unexpected value token ${JSON.stringify(t)}`);
  }
}

/** Substitute $variables and unwrap enums into plain JS values. */
function resolveValue(value: unknown, variables: Record<string, unknown>): unknown {
  if (value !== null && typeof value === 'object') {
    if ('__variable' in (value as Record<string, unknown>)) {
      return variables[(value as Record<string, unknown>).__variable as string];
    }
    if ('__enum' in (value as Record<string, unknown>)) {
      return (value as Record<string, unknown>).__enum as string;
    }
    if (Array.isArray(value)) return value.map((v) => resolveValue(v, variables));
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) out[k] = resolveValue(v, variables);
    return out;
  }
  return value;
}

function resolveArgs(field: FieldNode, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(field.args)) out[k] = resolveValue(v, variables);
  return out;
}

/* ------------------------------------------------------------------ */
/* schema (fields the suite may ask for)                               */
/* ------------------------------------------------------------------ */

interface TypeDef {
  fields: Record<string, true>;
}

const SCHEMA: Record<string, TypeDef> = {
  Query: { fields: { viewer: true, repository: true, node: true, __type: true } },
  User: { fields: { login: true, id: true, __typename: true } },
  Repository: {
    fields: {
      id: true, databaseId: true, name: true, nameWithOwner: true, owner: true,
      defaultBranchRef: true, viewerPermission: true, hasIssuesEnabled: true,
      issue: true, issues: true, pullRequests: true, pullRequest: true, __typename: true,
    },
  },
  Ref: { fields: { name: true, __typename: true } },
  Issue: {
    fields: {
      id: true, number: true, title: true, state: true, body: true,
      createdAt: true, updatedAt: true, __typename: true,
    },
  },
  PullRequest: {
    fields: {
      id: true, number: true, title: true, state: true, merged: true,
      headRefName: true, baseRefName: true, mergeable: true, files: true, __typename: true,
    },
  },
  IssueConnection: { fields: { nodes: true, pageInfo: true, totalCount: true, __typename: true } },
  PullRequestConnection: { fields: { nodes: true, pageInfo: true, totalCount: true, __typename: true } },
  PullRequestChangedFileConnection: { fields: { nodes: true, pageInfo: true, __typename: true } },
  PullRequestChangedFile: { fields: { path: true, __typename: true } },
  IssueComment: { fields: { id: true, body: true, __typename: true } },
  IssueCommentConnection: { fields: { nodes: true, pageInfo: true, __typename: true } },
  PageInfo: { fields: { hasNextPage: true, endCursor: true, __typename: true } },
  __Type: { fields: { name: true, fields: true, enumValues: true, __typename: true } },
  __Field: { fields: { name: true, __typename: true } },
  __EnumValue: { fields: { name: true, __typename: true } },
  CreateIssuePayload: { fields: { issue: true, __typename: true } },
  CloseIssuePayload: { fields: { issue: true, __typename: true } },
  CreatePullRequestPayload: { fields: { pullRequest: true, __typename: true } },
  MergePullRequestPayload: { fields: { pullRequest: true, __typename: true } },
  AddCommentPayload: { fields: { commentEdge: true, __typename: true } },
  IssueCommentEdge: { fields: { node: true, __typename: true } },
};

/** Introspection data: type name -> field names / enum values. */
const INTROSPECTION: Record<string, { fields: string[]; enumValues: string[] }> = {
  Issue: {
    fields: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt'],
    enumValues: [],
  },
  PullRequest: {
    fields: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files'],
    enumValues: [],
  },
  Repository: {
    fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
    enumValues: [],
  },
  SearchType: { fields: [], enumValues: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'CODE', 'COMMIT'] },
  IssueState: { fields: [], enumValues: ['OPEN', 'CLOSED'] },
  PullRequestState: { fields: [], enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
  MergeableState: { fields: [], enumValues: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
};

/* ------------------------------------------------------------------ */
/* runtime values                                                      */
/* ------------------------------------------------------------------ */

type Value =
  | { __typename: 'Query' }
  | { __typename: 'User'; login: string }
  | { __typename: 'Repository'; repo: RepoRec }
  | { __typename: 'Ref'; name: string }
  | { __typename: 'Issue'; repo: RepoRec; issue: IssueRec }
  | { __typename: 'PullRequest'; repo: RepoRec; pull: PullRec }
  | { __typename: 'IssueConnection'; items: { __typename: 'Issue'; repo: RepoRec; issue: IssueRec }[]; pageInfo: { hasNextPage: boolean; endCursor: string | null }; totalCount: number }
  | { __typename: 'PullRequestConnection'; items: { __typename: 'PullRequest'; repo: RepoRec; pull: PullRec }[]; pageInfo: { hasNextPage: boolean; endCursor: string | null }; totalCount: number }
  | { __typename: 'PullRequestChangedFileConnection'; items: { __typename: 'PullRequestChangedFile'; path: string }[]; pageInfo: { hasNextPage: boolean; endCursor: string | null } }
  | { __typename: 'PullRequestChangedFile'; path: string }
  | { __typename: 'IssueComment'; id: string; body: string }
  | { __typename: 'PageInfo'; hasNextPage: boolean; endCursor: string | null }
  | { __typename: '__Type'; name: string }
  | { __typename: '__Field'; name: string }
  | { __typename: '__EnumValue'; name: string }
  | { __typename: 'CreateIssuePayload'; issue: Value | null }
  | { __typename: 'CloseIssuePayload'; issue: Value | null }
  | { __typename: 'CreatePullRequestPayload'; pullRequest: Value | null }
  | { __typename: 'MergePullRequestPayload'; pullRequest: Value | null }
  | { __typename: 'AddCommentPayload'; commentEdge: { __typename: 'IssueCommentEdge'; node: Value } | null }
  | { __typename: 'IssueCommentEdge'; node: Value }
  | null;

class GraphQLError extends Error {
  path: (string | number)[];
  constructor(message: string, path: (string | number)[] = []) {
    super(message);
    this.path = path;
  }
}

function pageInfoFor(total: number, start: number, end: number): { hasNextPage: boolean; endCursor: string | null } {
  return {
    hasNextPage: end + 1 < total,
    endCursor: end >= start ? encodeCursor(end) : null,
  };
}

function paginate<T>(items: T[], args: Record<string, unknown>): { page: T[]; pageInfo: { hasNextPage: boolean; endCursor: string | null } } {
  const first = typeof args.first === 'number' ? args.first : items.length;
  const afterIndex = decodeCursor(args.after as string | null | undefined);
  const start = afterIndex + 1;
  const page = items.slice(start, start + first);
  const end = page.length > 0 ? start + page.length - 1 : start - 1;
  return { page, pageInfo: pageInfoFor(items.length, start, end) };
}

function pullMergeable(store: Store, repo: RepoRec, pull: PullRec): string {
  const baseTip = repo.branches.get(pull.base.ref);
  const headTip = repo.branches.get(pull.head.ref);
  if (baseTip === undefined || headTip === undefined) return 'UNKNOWN';
  const baseCommit = repo.commits.get(baseTip);
  const headCommit = repo.commits.get(headTip);
  if (baseCommit === undefined || headCommit === undefined) return 'UNKNOWN';
  const baseSince = diffTrees(pull.baseTreeAtCreation, baseCommit.tree);
  const prChanges = diffTrees(pull.baseTreeAtCreation, headCommit.tree);
  const changedOnBase = new Map<string, string>();
  for (const f of baseSince) {
    const entry = baseCommit.tree.get(f.filename);
    changedOnBase.set(f.filename, entry === undefined ? '' : entry.blobSha);
  }
  for (const f of prChanges) {
    const onBase = changedOnBase.get(f.filename);
    if (onBase !== undefined) {
      const headEntry = headCommit.tree.get(f.filename);
      const headSha = headEntry === undefined ? '' : headEntry.blobSha;
      if (headSha !== onBase) return 'CONFLICTING';
    }
  }
  return 'MERGEABLE';
}

function findRepoByGlobalId(store: Store, globalId: string): RepoRec | null {
  const decoded = decodeGlobalId(globalId);
  if (decoded === null || decoded.kind !== 'Repository' || decoded.parts.length !== 1) return null;
  const id = parseInt(decoded.parts[0]!, 10);
  for (const repo of store.repos.values()) {
    if (repo.id === id) return repo;
  }
  return null;
}

/* ------------------------------------------------------------------ */
/* executor                                                            */
/* ------------------------------------------------------------------ */

class Executor {
  constructor(private store: Store) {}

  executeOperation(op: OperationNode, variables: Record<string, unknown>): { data: Record<string, unknown> | null; errors: { message: string; path?: (string | number)[] }[] } {
    const root: Value = { __typename: 'Query' };
    const errors: { message: string; path?: (string | number)[] }[] = [];
    const data: Record<string, unknown> = {};
    for (const field of op.selection) {
      try {
        data[field.alias ?? field.name] = this.resolveField(root, field, variables, []);
      } catch (err) {
        if (err instanceof GraphQLError) {
          errors.push({ message: err.message, path: err.path });
          data[field.alias ?? field.name] = null;
        } else {
          throw err;
        }
      }
    }
    return { data: errors.length > 0 && Object.keys(data).length === 0 ? null : data, errors };
  }

  private resolveField(parent: Value, field: FieldNode, variables: Record<string, unknown>, path: (string | number)[]): unknown {
    if (parent === null) return null;
    const typeName = parent.__typename;
    // Inline fragment type condition: skip when it does not match.
    if (field.fragmentType !== null && field.fragmentType !== typeName) return undefined;
    const def = SCHEMA[typeName];
    if (def === undefined) throw new GraphQLError(`Unknown type "${typeName}"`, path);
    if (field.name === '__typename') return typeName;
    if (def.fields[field.name] !== true) {
      throw new GraphQLError(`Cannot query field "${field.name}" on type "${typeName}".`, [...path, field.name]);
    }
    const args = resolveArgs(field, variables);
    const value = this.fieldValue(parent, field.name, args, [...path, field.alias ?? field.name]);
    return this.resolveSelection(value, field.selection, variables, [...path, field.alias ?? field.name]);
  }

  private resolveSelection(value: unknown, selection: FieldNode[] | null, variables: Record<string, unknown>, path: (string | number)[]): unknown {
    if (selection === null || selection === undefined) return value;
    if (value === null || value === undefined) return null;
    if (Array.isArray(value)) {
      return value.map((v, i) => this.resolveSelection(v, selection, variables, [...path, i]));
    }
    if (typeof value === 'object' && '__typename' in (value as Record<string, unknown>)) {
      const out: Record<string, unknown> = {};
      for (const field of selection) {
        // Fragment gating happens in resolveField; a fragment field that does
        // not match returns undefined and is omitted.
        const result = this.resolveField(value as Value, field, variables, path);
        if (result !== undefined) out[field.alias ?? field.name] = result;
      }
      return out;
    }
    return value;
  }

  private fieldValue(parent: Value, name: string, args: Record<string, unknown>, path: (string | number)[]): unknown {
    switch (parent.__typename) {
      case 'Query': {
        if (name === 'viewer') {
          return { __typename: 'User', login: this.store.viewerLogin ?? '' } as Value;
        }
        if (name === 'repository') {
          const repo = this.store.findRepo(String(args.owner ?? ''), String(args.name ?? ''));
          return repo === null ? null : ({ __typename: 'Repository', repo } as Value);
        }
        if (name === 'node') {
          const decoded = decodeGlobalId(String(args.id ?? ''));
          if (decoded === null) return null;
          if (decoded.kind === 'Repository') {
            const repo = findRepoByGlobalId(this.store, String(args.id ?? ''));
            return repo === null ? null : ({ __typename: 'Repository', repo } as Value);
          }
          if (decoded.kind === 'Issue' && decoded.parts.length === 2) {
            const repo = findRepoByGlobalId(this.store, encodeGlobalId('Repository', decoded.parts[0]!));
            if (repo === null) return null;
            const issue = repo.issues.get(parseInt(decoded.parts[1]!, 10));
            return issue === undefined ? null : ({ __typename: 'Issue', repo, issue } as Value);
          }
          if (decoded.kind === 'PullRequest' && decoded.parts.length === 2) {
            const repo = findRepoByGlobalId(this.store, encodeGlobalId('Repository', decoded.parts[0]!));
            if (repo === null) return null;
            const pull = repo.pulls.get(parseInt(decoded.parts[1]!, 10));
            return pull === undefined ? null : ({ __typename: 'PullRequest', repo, pull } as Value);
          }
          return null;
        }
        if (name === '__type') {
          const typeName = String(args.name ?? '');
          return INTROSPECTION[typeName] === undefined ? null : ({ __typename: '__Type', name: typeName } as Value);
        }
        return null;
      }
      case 'User': {
        if (name === 'login') return parent.login;
        if (name === 'id') return 1000;
        return null;
      }
      case 'Repository': {
        const repo = parent.repo;
        switch (name) {
          case 'id': return repoGlobalId(repo);
          case 'databaseId': return repo.id;
          case 'name': return repo.name;
          case 'nameWithOwner': return `${repo.owner ?? ''}/${repo.name}`;
          case 'owner': return { __typename: 'User', login: repo.owner ?? '' } as Value;
          case 'defaultBranchRef': {
            const tip = repo.branches.get(repo.default_branch);
            return tip === undefined ? null : ({ __typename: 'Ref', name: repo.default_branch } as Value);
          }
          case 'viewerPermission': return 'ADMIN';
          case 'hasIssuesEnabled': return true;
          case 'issue': {
            const issue = repo.issues.get(Number(args.number));
            return issue === undefined ? null : ({ __typename: 'Issue', repo, issue } as Value);
          }
          case 'issues': {
            let issues = [...repo.issues.values()].sort((a, b) => a.seq - b.seq);
            const states = args.states as string[] | undefined;
            if (states !== undefined && states.length > 0) {
              const want = new Set(states.map((s) => s.toLowerCase()));
              issues = issues.filter((i) => want.has(i.state));
            }
            const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
            if (orderBy !== undefined && orderBy.field === 'CREATED_AT') {
              issues.sort((a, b) => (orderBy.direction === 'DESC' ? b.seq - a.seq : a.seq - b.seq));
            }
            const { page, pageInfo } = paginate(issues, args);
            return {
              __typename: 'IssueConnection',
              items: page.map((issue) => ({ __typename: 'Issue', repo, issue } as const)),
              pageInfo,
              totalCount: issues.length,
            } as Value;
          }
          case 'pullRequests': {
            const pulls = [...repo.pulls.values()].sort((a, b) => a.number - b.number);
            const { page, pageInfo } = paginate(pulls, args);
            return {
              __typename: 'PullRequestConnection',
              items: page.map((pull) => ({ __typename: 'PullRequest', repo, pull } as const)),
              pageInfo,
              totalCount: pulls.length,
            } as Value;
          }
          case 'pullRequest': {
            const pull = repo.pulls.get(Number(args.number));
            return pull === undefined ? null : ({ __typename: 'PullRequest', repo, pull } as Value);
          }
          default: return null;
        }
      }
      case 'Ref': {
        return name === 'name' ? parent.name : null;
      }
      case 'Issue': {
        const issue = parent.issue;
        switch (name) {
          case 'id': return issueGlobalId(parent.repo, issue);
          case 'number': return issue.number;
          case 'title': return issue.title;
          case 'state': return issue.state.toUpperCase();
          case 'body': return issue.body;
          case 'createdAt': return issue.created_at;
          case 'updatedAt': return issue.updated_at;
          default: return null;
        }
      }
      case 'PullRequest': {
        const pull = parent.pull;
        switch (name) {
          case 'id': return pullGlobalId(parent.repo, pull);
          case 'number': return pull.number;
          case 'title': return pull.title;
          case 'state': return pull.merged ? 'MERGED' : pull.state.toUpperCase();
          case 'merged': return pull.merged;
          case 'headRefName': return pull.head.ref;
          case 'baseRefName': return pull.base.ref;
          case 'mergeable': return pullMergeable(this.store, parent.repo, pull);
          case 'files': {
            const files = pull.files.map((f) => ({ __typename: 'PullRequestChangedFile', path: f.filename } as const));
            const { page, pageInfo } = paginate(files, args);
            return {
              __typename: 'PullRequestChangedFileConnection',
              items: page,
              pageInfo,
            } as Value;
          }
          default: return null;
        }
      }
      case 'IssueConnection':
      case 'PullRequestConnection':
      case 'PullRequestChangedFileConnection': {
        if (name === 'nodes') return parent.items;
        if (name === 'pageInfo') return { __typename: 'PageInfo', ...parent.pageInfo } as Value;
        if (name === 'totalCount' && 'totalCount' in parent) return parent.totalCount;
        return null;
      }
      case 'PullRequestChangedFile': {
        return name === 'path' ? parent.path : null;
      }
      case 'IssueComment': {
        if (name === 'id') return parent.id;
        if (name === 'body') return parent.body;
        return null;
      }
      case 'PageInfo': {
        if (name === 'hasNextPage') return parent.hasNextPage;
        if (name === 'endCursor') return parent.endCursor;
        return null;
      }
      case '__Type': {
        const info = INTROSPECTION[parent.name];
        if (info === undefined) return null;
        if (name === 'name') return parent.name;
        if (name === 'fields') return info.fields.map((f) => ({ __typename: '__Field', name: f } as Value));
        if (name === 'enumValues') return info.enumValues.map((v) => ({ __typename: '__EnumValue', name: v } as Value));
        return null;
      }
      case '__Field':
      case '__EnumValue': {
        return name === 'name' ? parent.name : null;
      }
      default: return null;
    }
  }
}

/* ------------------------------------------------------------------ */
/* mutations                                                           */
/* ------------------------------------------------------------------ */

function executeMutation(store: Store, op: OperationNode, variables: Record<string, unknown>): { data: Record<string, unknown> | null; errors: { message: string; path?: (string | number)[] }[] } {
  const executor = new Executor(store);
  // Mutations run through the same field machinery, but the root fields are
  // resolved by hand here because they mutate state.
  const errors: { message: string; path?: (string | number)[] }[] = [];
  const data: Record<string, unknown> = {};
  for (const field of op.selection) {
    const args = resolveArgs(field, variables);
    const key = field.alias ?? field.name;
    try {
      let payload: Value | null = null;
      if (field.name === 'createIssue') {
        const input = args.input as { repositoryId: string; title: string; body?: string };
        const repo = findRepoByGlobalId(store, String(input.repositoryId));
        if (repo === null) throw new GraphQLError('Could not resolve to a Repository with the global id.', [key]);
        const issue = store.createIssue(repo, { title: String(input.title), body: input.body === undefined ? '' : String(input.body), labels: [] });
        payload = { __typename: 'CreateIssuePayload', issue: { __typename: 'Issue', repo, issue } } as Value;
      } else if (field.name === 'closeIssue') {
        const input = args.input as { issueId: string };
        const decoded = decodeGlobalId(String(input.issueId));
        if (decoded === null || decoded.kind !== 'Issue' || decoded.parts.length !== 2) {
          throw new GraphQLError('Could not resolve to an Issue with the global id.', [key]);
        }
        const repo = findRepoByGlobalId(store, encodeGlobalId('Repository', decoded.parts[0]!));
        const issue = repo?.issues.get(parseInt(decoded.parts[1]!, 10));
        if (repo === undefined || repo === null || issue === undefined) {
          throw new GraphQLError('Could not resolve to an Issue with the global id.', [key]);
        }
        store.patchIssue(repo, issue.number, { state: 'closed' });
        payload = { __typename: 'CloseIssuePayload', issue: { __typename: 'Issue', repo, issue } } as Value;
      } else if (field.name === 'createPullRequest') {
        const input = args.input as { repositoryId: string; baseRefName: string; headRefName: string; title: string; body?: string };
        const repo = findRepoByGlobalId(store, String(input.repositoryId));
        if (repo === null) throw new GraphQLError('Could not resolve to a Repository with the global id.', [key]);
        const pull = store.createPull(repo, {
          title: String(input.title),
          body: input.body === undefined ? '' : String(input.body),
          head: String(input.headRefName),
          base: String(input.baseRefName),
        });
        payload = { __typename: 'CreatePullRequestPayload', pullRequest: { __typename: 'PullRequest', repo, pull } } as Value;
      } else if (field.name === 'mergePullRequest') {
        const input = args.input as { pullRequestId: string };
        const decoded = decodeGlobalId(String(input.pullRequestId));
        if (decoded === null || decoded.kind !== 'PullRequest' || decoded.parts.length !== 2) {
          throw new GraphQLError('Could not resolve to a PullRequest with the global id.', [key]);
        }
        const repo = findRepoByGlobalId(store, encodeGlobalId('Repository', decoded.parts[0]!));
        const pull = repo?.pulls.get(parseInt(decoded.parts[1]!, 10));
        if (repo === undefined || repo === null || pull === undefined) {
          throw new GraphQLError('Could not resolve to a PullRequest with the global id.', [key]);
        }
        store.mergePull(repo, pull.number);
        payload = { __typename: 'MergePullRequestPayload', pullRequest: { __typename: 'PullRequest', repo, pull } } as Value;
      } else if (field.name === 'addComment') {
        const input = args.input as { subjectId: string; body: string };
        const decoded = decodeGlobalId(String(input.subjectId));
        if (decoded === null || decoded.kind !== 'Issue' || decoded.parts.length !== 2) {
          throw new GraphQLError('Could not resolve to an Issue with the global id.', [key]);
        }
        const repo = findRepoByGlobalId(store, encodeGlobalId('Repository', decoded.parts[0]!));
        const issue = repo?.issues.get(parseInt(decoded.parts[1]!, 10));
        if (repo === undefined || repo === null || issue === undefined) {
          throw new GraphQLError('Could not resolve to an Issue with the global id.', [key]);
        }
        const comment = store.createComment(repo, issue.number, String(input.body));
        payload = {
          __typename: 'AddCommentPayload',
          commentEdge: {
            __typename: 'IssueCommentEdge',
            node: { __typename: 'IssueComment', id: commentGlobalId(comment.id), body: comment.body },
          },
        } as Value;
      } else {
        throw new GraphQLError(`Cannot query field "${field.name}" on type "Mutation".`, [key]);
      }
      data[key] = executor['resolveSelection'](payload, field.selection, variables, [key]);
    } catch (err) {
      if (err instanceof GraphQLError) {
        errors.push({ message: err.message, path: err.path });
        data[key] = null;
      } else if (err instanceof Error) {
        // Store ApiError (e.g. merge conflict, missing branch): GraphQL-level error.
        errors.push({ message: err.message, path: [key] });
        data[key] = null;
      } else {
        throw err;
      }
    }
  }
  return { data, errors };
}

/* ------------------------------------------------------------------ */
/* public entry point                                                  */
/* ------------------------------------------------------------------ */

export interface GraphQLEnvelope {
  data: Record<string, unknown> | null;
  errors?: { message: string; path?: (string | number)[] }[];
}

/** Execute one GraphQL request body (single operation object or an array for batching). */
export function executeGraphQL(store: Store, body: unknown): GraphQLEnvelope | GraphQLEnvelope[] {
  if (Array.isArray(body)) {
    return body.map((entry) => executeOne(store, entry));
  }
  return executeOne(store, body);
}

function executeOne(store: Store, entry: unknown): GraphQLEnvelope {
  const record = entry as Record<string, unknown>;
  const query = typeof record.query === 'string' ? record.query : '';
  const variables = (record.variables ?? {}) as Record<string, unknown>;
  const operationName = typeof record.operationName === 'string' ? record.operationName : null;
  let operations: OperationNode[];
  try {
    operations = new Parser(tokenize(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] };
  }
  if (operations.length === 0) return { data: null, errors: [{ message: 'no operation' }] };
  let op: OperationNode | undefined;
  if (operationName !== null) {
    op = operations.find((o) => o.name === operationName);
    if (op === undefined) return { data: null, errors: [{ message: `unknown operation ${operationName}` }] };
  } else {
    op = operations[0];
  }
  const mergedVariables = { ...op.variables, ...variables };
  if (op.kind === 'mutation') return executeMutation(store, op, mergedVariables);
  return new Executor(store).executeOperation(op, mergedVariables);
}
