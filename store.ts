// store.ts — in-memory forge store: repos, branches, commits, contents, labels, issues, PRs.
// Deterministic by design: no clock, no Math.random, no unseeded ids. All timestamps come from
// the trial's MAKEFUN_TRIAL_NOW (or a fixed fallback); all ids are monotonic counters; all git
// object ids are real SHA-1s over real git object bytes (blob/tree/commit), so the REST surface
// and the future git surface agree on every sha.
import { createHash } from 'node:crypto';

export const DEFAULT_BRANCH = 'main';
const AUTHOR = 'Forge <forge@example.invalid>';

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export function sha1(data: Buffer | string): string {
  return createHash('sha1').update(data).digest('hex');
}

function objHash(kind: string, body: Buffer): string {
  const head = Buffer.from(`${kind} ${body.length}\0`, 'utf8');
  return sha1(Buffer.concat([head, body]));
}

export function blobSha(content: Buffer): string {
  return objHash('blob', content);
}

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

/** Parse a raw commit object body. Returns tree, parents, message, author/committer lines. */
export function parseCommit(data: Buffer): {
  tree: string;
  parents: string[];
  message: string;
  author: string;
  committer: string;
} {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const head = blank === -1 ? text : text.slice(0, blank);
  const message = blank === -1 ? '' : text.slice(blank + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, message, author, committer };
}

/** Parse a raw tree object body into entries (mode as stored, name, oid). */
export function parseTree(data: Buffer): { mode: string; name: string; oid: string }[] {
  const out: { mode: string; name: string; oid: string }[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space === -1) break;
    const mode = data.subarray(offset, space).toString('utf8');
    const nul = data.indexOf(0x00, space);
    if (nul === -1) break;
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    out.push({ mode, name, oid });
    offset = nul + 21;
  }
  return out;
}

/** Parse a raw tag object body. */
export function parseTag(data: Buffer): { object: string; type: string; tag: string; message: string } {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const head = blank === -1 ? text : text.slice(0, blank);
  const message = blank === -1 ? '' : text.slice(blank + 2);
  let object = '';
  let type = '';
  let tag = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
  }
  return { object, type, tag, message };
}

/** Build raw tree objects for a flat tree: returns root tree sha and every object (tree+blob). */
export function buildTreeObjects(files: Tree): { rootSha: string; objects: Map<string, { type: GitObjectType; data: Buffer }> } {
  interface Node {
    files: Map<string, FileEntry>;
    dirs: Map<string, Node>;
  }
  const root: Node = { files: new Map(), dirs: new Map() };
  const objects = new Map<string, { type: GitObjectType; data: Buffer }>();
  for (const [path, entry] of files) {
    objects.set(entry.blobSha, { type: 'blob', data: entry.content });
    const parts = path.split('/');
    let node = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      const part = parts[i]!;
      let next = node.dirs.get(part);
      if (next === undefined) {
        next = { files: new Map(), dirs: new Map() };
        node.dirs.set(part, next);
      }
      node = next;
    }
    node.files.set(parts[parts.length - 1]!, entry.blobSha);
  }
  const build = (node: Node): string => {
    const entries: { mode: string; name: string; sha: string }[] = [];
    for (const [name, entry] of node.files) entries.push({ mode: '100644', name, sha: entry.blobSha });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, sha: build(child) });
    entries.sort((a, b) => {
      const an = a.mode === '40000' ? `${a.name}/` : a.name;
      const bn = b.mode === '40000' ? `${b.name}/` : b.name;
      return an < bn ? -1 : an > bn ? 1 : 0;
    });
    const parts: Buffer[] = [];
    for (const e of entries) {
      parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
      parts.push(Buffer.from(e.sha, 'hex'));
    }
    const body = Buffer.concat(parts);
    const sha = objHash('tree', body);
    objects.set(sha, { type: 'tree', data: body });
    return sha;
  };
  return { rootSha: build(root), objects };
}

export interface FileEntry {
  content: Buffer;
  blobSha: string;
}

/** Flat path -> file tree (one entry per file, full repo-relative path as key). */
export type Tree = Map<string, FileEntry>;

/** Real git tree sha for a flat tree: nested tree objects, git sort order, `tree <len>\0` framing. */
export function treeShaFor(files: Tree): string {
  interface Node {
    files: Map<string, string>;
    dirs: Map<string, Node>;
  }
  const root: Node = { files: new Map(), dirs: new Map() };
  for (const [path, entry] of files) {
    const parts = path.split('/');
    let node = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      const part = parts[i]!;
      let next = node.dirs.get(part);
      if (next === undefined) {
        next = { files: new Map(), dirs: new Map() };
        node.dirs.set(part, next);
      }
      node = next;
    }
    node.files.set(parts[parts.length - 1]!, entry.blobSha);
  }
  const build = (node: Node): string => {
    const entries: { mode: string; name: string; sha: string }[] = [];
    for (const [name, sha] of node.files) entries.push({ mode: '100644', name, sha });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, sha: build(child) });
    entries.sort((a, b) => {
      const an = a.mode === '40000' ? `${a.name}/` : a.name;
      const bn = b.mode === '40000' ? `${b.name}/` : b.name;
      return an < bn ? -1 : an > bn ? 1 : 0;
    });
    const parts: Buffer[] = [];
    for (const e of entries) {
      parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
      parts.push(Buffer.from(e.sha, 'hex'));
    }
    return objHash('tree', Buffer.concat(parts));
  };
  return build(root);
}

/** Tree sha of the subtree rooted at `prefix/` ('' = whole tree), for directory listings. */
export function subtreeShaFor(files: Tree, prefix: string): string {
  if (prefix === '') return treeShaFor(files);
  const sub: Tree = new Map();
  const base = `${prefix}/`;
  for (const [path, entry] of files) {
    if (path.startsWith(base)) sub.set(path.slice(base.length), entry);
  }
  return treeShaFor(sub);
}

export interface CommitRec {
  sha: string;
  treeSha: string;
  tree: Tree;
  parents: string[];
  message: string;
}

export interface LabelRec {
  name: string;
  color: string;
}

export interface CommentRec {
  id: number;
  body: string;
  user: { login: string; id: number };
  created_at: string;
}

export interface IssueRec {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: LabelRec[];
  user: { login: string; id: number };
  created_at: string;
  updated_at: string;
  seq: number;
  comments: CommentRec[];
}

export interface PullFile {
  filename: string;
  status: string;
  additions: number;
  deletions: number;
  changes: number;
}

export interface PullRec {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  merged_at: string | null;
  merge_commit_sha: string | null;
  head: { ref: string; sha: string; label: string };
  base: { ref: string; sha: string; label: string };
  user: { login: string; id: number };
  created_at: string;
  updated_at: string;
  /** Base tree/commit at PR creation, for conflict detection at merge. */
  baseTreeAtCreation: Tree;
  baseCommitAtCreation: string;
  files: PullFile[];
}

export interface RepoRec {
  id: number;
  name: string;
  /** Owner login; null until first owner-scoped access binds it (see Store.findRepo). */
  owner: string | null;
  private: boolean;
  description: string;
  default_branch: string;
  created_at: string;
  updated_at: string;
  seq: number;
  branches: Map<string, string>;
  /** Annotated/lightweight tags: tag name -> oid (commit or tag object). */
  tags: Map<string, string>;
  commits: Map<string, CommitRec>;
  labels: Map<string, LabelRec>;
  issues: Map<number, IssueRec>;
  pulls: Map<number, PullRec>;
  nextNumber: number;
}

function lineCount(text: string): number {
  if (text === '') return 0;
  return text.split('\n').length;
}

/** File-level diff between two flat trees, for PR files lists. */
export function diffTrees(base: Tree, head: Tree): PullFile[] {
  const out: PullFile[] = [];
  const paths = new Set([...base.keys(), ...head.keys()]);
  for (const path of [...paths].sort()) {
    const b = base.get(path);
    const h = head.get(path);
    if (h !== undefined && (b === undefined || b.blobSha !== h.blobSha)) {
      const newText = h.content.toString('utf8');
      const oldText = b === undefined ? '' : b.content.toString('utf8');
      const additions = Math.max(1, lineCount(newText));
      const deletions = b === undefined ? 0 : lineCount(oldText);
      out.push({
        filename: path,
        status: b === undefined ? 'added' : 'modified',
        additions,
        deletions,
        changes: additions + deletions,
      });
    } else if (b !== undefined && h === undefined) {
      const deletions = Math.max(1, lineCount(b.content.toString('utf8')));
      out.push({ filename: path, status: 'removed', additions: 0, deletions, changes: deletions });
    }
  }
  return out;
}

export class Store {
  repos = new Map<string, RepoRec>();
  /** Content-addressed git object database (shared across repos; objects are immutable). */
  objects = new Map<string, { type: GitObjectType; data: Buffer }>();
  viewerLogin: string | null = null;
  trialNow: string;
  trialEpoch: number;
  private nextId = 1;
  private nextRepoId = 1;
  private repoSeq = 0;

  /** Register a raw git object; returns its oid. */
  registerObject(type: GitObjectType, data: Buffer): string {
    const oid = objHash(type, data);
    this.objects.set(oid, { type, data });
    return oid;
  }

  getObject(oid: string): { type: GitObjectType; data: Buffer } | undefined {
    return this.objects.get(oid);
  }

  /** All refs of a repo, sorted by name: heads, tags, plus HEAD symref target info. */
  allRefs(repo: RepoRec): { name: string; oid: string }[] {
    const out: { name: string; oid: string }[] = [];
    for (const [name, oid] of repo.branches) out.push({ name: `refs/heads/${name}`, oid });
    for (const [name, oid] of repo.tags) out.push({ name: `refs/tags/${name}`, oid });
    out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    return out;
  }

  /** If oid is a tag object, peel it to its target commit oid (recursively). */
  peelToCommit(oid: string): string {
    let cur = oid;
    for (let i = 0; i < 8; i += 1) {
      const obj = this.objects.get(cur);
      if (obj === undefined || obj.type !== 'tag') return cur;
      cur = parseTag(obj.data).object;
    }
    return cur;
  }

  /** Walk a commit's tree objects into a flat path -> file tree (null if any object missing). */
  treeToFlat(treeOid: string): Tree | null {
    const out: Tree = new Map();
    const walk = (oid: string, prefix: string): boolean => {
      const obj = this.objects.get(oid);
      if (obj === undefined || obj.type !== 'tree') return false;
      for (const e of parseTree(obj.data)) {
        const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
        if (e.mode === '40000') {
          if (!walk(e.oid, path)) return false;
        } else {
          const blob = this.objects.get(e.oid);
          if (blob === undefined || blob.type !== 'blob') return false;
          out.set(path, { content: blob.data, blobSha: e.oid });
        }
      }
      return true;
    };
    return walk(treeOid, '') ? out : null;
  }

  /**
   * Materialize a commit object into the repo's commits map (flat tree + parents + message).
   * Returns the record, or null when the commit or its tree objects are unavailable.
   */
  materializeCommit(repo: RepoRec, oid: string): CommitRec | null {
    const existing = repo.commits.get(oid);
    if (existing !== undefined) return existing;
    const obj = this.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') return null;
    const parsed = parseCommit(obj.data);
    const tree = this.treeToFlat(parsed.tree);
    if (tree === null) return null;
    const rec: CommitRec = {
      sha: oid,
      treeSha: parsed.tree,
      tree,
      parents: parsed.parents,
      message: parsed.message,
    };
    repo.commits.set(oid, rec);
    return rec;
  }

  /** Materialize a commit and its whole ancestry into the repo's commits map. */
  materializeAncestry(repo: RepoRec, tip: string): void {
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const rec = this.materializeCommit(repo, oid);
      if (rec === null) continue;
      for (const p of rec.parents) stack.push(p);
    }
  }

  /**
   * Apply one receive-pack ref update with fast-forward rules.
   * Returns {ok:true} on success (ref moved), or {ok:false, reason} with the ref untouched.
   * newOid === ZERO_OID means delete; oldOid === ZERO_OID means create.
   */
  applyRefUpdate(repo: RepoRec, ref: string, oldOid: string, newOid: string): { ok: boolean; reason: string } {
    const ZERO = '0'.repeat(40);
    const current = this.refTarget(repo, ref);
    if (newOid === ZERO) {
      if (current === undefined || current !== oldOid) return { ok: false, reason: 'non-fast-forward' };
      this.setRef(repo, ref, undefined);
      return { ok: true, reason: '' };
    }
    if (ref.startsWith('refs/tags/')) {
      if (current !== undefined || oldOid !== ZERO) return { ok: false, reason: 'non-fast-forward' };
      this.setRef(repo, ref, newOid);
      this.materializeAncestry(repo, this.peelToCommit(newOid));
      return { ok: true, reason: '' };
    }
    if (oldOid === ZERO) {
      if (current !== undefined) return { ok: false, reason: 'non-fast-forward' };
    } else {
      if (current === undefined || current !== oldOid) return { ok: false, reason: 'non-fast-forward' };
      if (!this.isAncestor(oldOid, newOid)) return { ok: false, reason: 'non-fast-forward' };
    }
    this.setRef(repo, ref, newOid);
    this.materializeAncestry(repo, newOid);
    return { ok: true, reason: '' };
  }

  refTarget(repo: RepoRec, ref: string): string | undefined {
    if (ref.startsWith('refs/heads/')) return repo.branches.get(ref.slice('refs/heads/'.length));
    if (ref.startsWith('refs/tags/')) return repo.tags.get(ref.slice('refs/tags/'.length));
    return undefined;
  }

  private setRef(repo: RepoRec, ref: string, oid: string | undefined): void {
    if (ref.startsWith('refs/heads/')) {
      const name = ref.slice('refs/heads/'.length);
      if (oid === undefined) repo.branches.delete(name);
      else repo.branches.set(name, oid);
    } else if (ref.startsWith('refs/tags/')) {
      const name = ref.slice('refs/tags/'.length);
      if (oid === undefined) repo.tags.delete(name);
      else repo.tags.set(name, oid);
    }
  }

  constructor(trialNow: string) {
    this.trialNow = trialNow;
    const parsed = Date.parse(trialNow);
    this.trialEpoch = Number.isFinite(parsed) ? Math.floor(parsed / 1000) : 0;
  }

  private newId(): number {
    const id = this.nextId;
    this.nextId += 1;
    return id;
  }

  private user(login: string): { login: string; id: number } {
    return { login, id: 1000 };
  }

  ownerName(repo: RepoRec): string {
    return repo.owner ?? this.viewerLogin ?? '';
  }

  repoJson(repo: RepoRec): Record<string, unknown> {
    const owner = this.ownerName(repo);
    return {
      id: repo.id,
      name: repo.name,
      full_name: owner === '' ? repo.name : `${owner}/${repo.name}`,
      owner: { login: owner, id: 1000, type: 'User' },
      private: repo.private,
      description: repo.description,
      default_branch: repo.default_branch,
      created_at: repo.created_at,
      updated_at: repo.updated_at,
    };
  }

  issueJson(repo: RepoRec, issue: IssueRec): Record<string, unknown> {
    return {
      id: issue.id,
      number: issue.number,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      labels: issue.labels.map((l) => ({ name: l.name, color: l.color })),
      user: issue.user,
      created_at: issue.created_at,
      updated_at: issue.updated_at,
      comments: issue.comments.length,
    };
  }

  pullJson(repo: RepoRec, pull: PullRec): Record<string, unknown> {
    return {
      id: pull.id,
      number: pull.number,
      title: pull.title,
      body: pull.body,
      state: pull.state,
      head: pull.head,
      base: pull.base,
      merged: pull.merged,
      merged_at: pull.merged_at,
      merge_commit_sha: pull.merge_commit_sha,
      user: pull.user,
      created_at: pull.created_at,
      updated_at: pull.updated_at,
    };
  }

  // ---------------------------------------------------------------- repos

  createRepo(name: string, isPrivate: boolean, description: string): RepoRec {
    if (typeof name !== 'string' || name === '') throw new ApiError(422, 'Repository name is required');
    if (this.repos.has(name)) throw new ApiError(422, `Repository ${name} already exists`);
    const repo: RepoRec = {
      id: this.nextRepoId,
      name,
      owner: this.viewerLogin,
      private: isPrivate,
      description,
      default_branch: DEFAULT_BRANCH,
      created_at: this.trialNow,
      updated_at: this.trialNow,
      seq: this.repoSeq,
      branches: new Map(),
      tags: new Map(),
      commits: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
    };
    this.nextRepoId += 1;
    this.repoSeq += 1;
    this.repos.set(name, repo);
    return repo;
  }

  /**
   * Find a repo by owner+name. The token names one user, but the server is never told that
   * user's login out of band: the first owner-scoped access binds the repo (and the token's
   * login) to the owner string it arrives under. A later access under any other owner is 404,
   * never 403 — exactly the suite's foreign-owner rule.
   */
  findRepo(owner: string, name: string): RepoRec | null {
    const repo = this.repos.get(name);
    if (repo === undefined) return null;
    if (repo.owner === null) {
      repo.owner = owner;
      if (this.viewerLogin === null) this.viewerLogin = owner;
      return repo;
    }
    return repo.owner === owner ? repo : null;
  }

  listRepos(): RepoRec[] {
    return [...this.repos.values()].sort((a, b) => a.seq - b.seq);
  }

  deleteRepo(repo: RepoRec): void {
    this.repos.delete(repo.name);
  }

  // ---------------------------------------------------------------- commits / branches

  private makeCommit(tree: Tree, parents: string[], message: string): CommitRec {
    const treeSha = treeShaFor(tree);
    const msg = message.endsWith('\n') ? message : `${message}\n`;
    const body =
      `tree ${treeSha}\n` +
      parents.map((p) => `parent ${p}\n`).join('') +
      `author ${AUTHOR} ${this.trialEpoch} +0000\n` +
      `committer ${AUTHOR} ${this.trialEpoch} +0000\n\n${msg}`;
    const sha = objHash('commit', Buffer.from(body, 'utf8'));
    // Register the commit and its whole tree (trees + blobs) in the object database,
    // so the git surface serves byte-identical objects for REST-created commits.
    this.registerObject('commit', Buffer.from(body, 'utf8'));
    const treeObjs = buildTreeObjects(tree);
    for (const [oid, obj] of treeObjs.objects) this.objects.set(oid, obj);
    return { sha, treeSha, tree, parents, message };
  }

  /** Commit `mutate(tree)` on `branch`, creating the branch from the default tip if needed. */
  commitOnBranch(repo: RepoRec, branch: string, message: string, mutate: (tree: Tree) => void): CommitRec {
    let tipSha = repo.branches.get(branch);
    if (tipSha === undefined) {
      const defTip = repo.branches.get(repo.default_branch);
      if (defTip !== undefined && branch !== repo.default_branch) {
        repo.branches.set(branch, defTip);
        tipSha = defTip;
      }
    }
    const parent = tipSha === undefined ? null : repo.commits.get(tipSha) ?? null;
    const tree: Tree = new Map(parent === null ? [] : parent.tree);
    mutate(tree);
    const commit = this.makeCommit(tree, parent === null ? [] : [parent.sha], message);
    repo.commits.set(commit.sha, commit);
    repo.branches.set(branch, commit.sha);
    return commit;
  }

  createBranch(repo: RepoRec, branch: string, sha: string): string {
    const existing = repo.branches.get(branch);
    if (existing !== undefined) return existing;
    if (!repo.commits.has(sha)) throw new ApiError(422, `Unknown commit ${sha}`);
    repo.branches.set(branch, sha);
    return sha;
  }

  listBranches(repo: RepoRec): { name: string; commit: { sha: string } }[] {
    const names = [...repo.branches.keys()].sort();
    if (!names.includes(repo.default_branch) && repo.branches.size === 0) return [];
    return names.map((name) => ({ name, commit: { sha: repo.branches.get(name)! } }));
  }

  /** Commits on a branch (or a commit sha), newest first. */
  listCommits(repo: RepoRec, ref: string | null): { sha: string; commit: { message: string } }[] {
    let tip: string | undefined;
    if (ref === null || ref === '') tip = repo.branches.get(repo.default_branch);
    else tip = repo.branches.get(ref) ?? (repo.commits.has(ref) ? ref : undefined);
    if (tip === undefined) throw new ApiError(404, `No such ref ${ref ?? repo.default_branch}`);
    const out: { sha: string; commit: { message: string } }[] = [];
    const seen = new Set<string>();
    let cur: string | undefined = tip;
    while (cur !== undefined && !seen.has(cur)) {
      seen.add(cur);
      const commit = repo.commits.get(cur);
      if (commit === undefined) break;
      out.push({ sha: commit.sha, commit: { message: commit.message } });
      cur = commit.parents[0];
    }
    return out;
  }

  resolveTree(repo: RepoRec, ref: string | null): Tree {
    let tip: string | undefined;
    if (ref === null || ref === '') tip = repo.branches.get(repo.default_branch);
    else tip = repo.branches.get(ref) ?? (repo.commits.has(ref) ? ref : undefined);
    if (tip === undefined) throw new ApiError(404, `No such ref ${ref ?? repo.default_branch}`);
    const commit = repo.commits.get(tip);
    if (commit === undefined) throw new ApiError(404, `No such ref ${ref ?? repo.default_branch}`);
    return commit.tree;
  }

  // ---------------------------------------------------------------- contents

  putContent(
    repo: RepoRec,
    path: string,
    contentB64: string,
    message: string,
    branch?: string,
    sha?: string,
  ): { contentSha: string; commitSha: string; created: boolean } {
    let content: Buffer;
    try {
      content = Buffer.from(contentB64.replace(/[\r\n\t ]+/g, ''), 'base64');
    } catch {
      throw new ApiError(400, 'content is not valid base64');
    }
    const targetBranch = branch ?? repo.default_branch;
    const tree = this.safeTree(repo, targetBranch);
    const existing = tree.get(path);
    if (existing !== undefined) {
      if (sha === undefined || sha.toLowerCase() !== existing.blobSha.toLowerCase()) {
        throw new ApiError(409, 'sha does not match the current blob (stale sha)');
      }
    }
    const newBlobSha = blobSha(content);
    const commit = this.commitOnBranch(repo, targetBranch, message, (t) => {
      t.set(path, { content, blobSha: newBlobSha });
    });
    return { contentSha: newBlobSha, commitSha: commit.sha, created: existing === undefined };
  }

  private safeTree(repo: RepoRec, branch: string): Tree {
    const tip = repo.branches.get(branch);
    if (tip === undefined) return new Map();
    return repo.commits.get(tip)?.tree ?? new Map();
  }

  getContentEntry(repo: RepoRec, path: string, ref: string | null): { entry: FileEntry; path: string } {
    const tree = this.resolveTree(repo, ref);
    const entry = tree.get(path);
    if (entry === undefined) throw new ApiError(404, `No such file ${path}`);
    return { entry, path };
  }

  listDirectory(repo: RepoRec, dirPath: string, ref: string | null): { type: string; name: string; path: string; sha: string }[] {
    const tree = this.resolveTree(repo, ref);
    const base = dirPath === '' ? '' : `${dirPath}/`;
    const seen = new Map<string, { type: string; sha: string }>();
    for (const path of tree.keys()) {
      if (!path.startsWith(base)) continue;
      const rest = path.slice(base.length);
      if (rest === '') continue;
      const slash = rest.indexOf('/');
      if (slash === -1) {
        seen.set(rest, { type: 'file', sha: tree.get(path)!.blobSha });
      } else {
        const dir = rest.slice(0, slash);
        if (!seen.has(dir)) seen.set(dir, { type: 'dir', sha: subtreeShaFor(tree, `${base}${dir}`) });
      }
    }
    return [...seen.entries()]
      .sort((a, b) => (a[0] < b[0] ? -1 : 1))
      .map(([name, info]) => ({ type: info.type, name, path: `${base}${name}`, sha: info.sha }));
  }

  deleteContent(repo: RepoRec, path: string, message: string, branch?: string, sha?: string): { commitSha: string } {
    const targetBranch = branch ?? repo.default_branch;
    const tree = this.safeTree(repo, targetBranch);
    const entry = tree.get(path);
    if (entry === undefined) throw new ApiError(404, `No such file ${path}`);
    if (sha !== undefined && sha !== '' && sha.toLowerCase() !== entry.blobSha.toLowerCase()) throw new ApiError(409, `File sha ${sha} is stale`);
    const commit = this.commitOnBranch(repo, targetBranch, message, (t) => {
      t.delete(path);
    });
    return { commitSha: commit.sha };
  }

  // ---------------------------------------------------------------- labels / issues

  createLabel(repo: RepoRec, name: string, color: string): LabelRec {
    const existing = repo.labels.get(name);
    if (existing !== undefined) return existing;
    const label: LabelRec = { name, color: color === '' ? 'ededed' : color };
    repo.labels.set(name, label);
    return label;
  }

  createIssue(repo: RepoRec, title: string, body: string, labelNames: string[]): IssueRec {
    const labels: LabelRec[] = [];
    for (const name of labelNames) {
      const label = repo.labels.get(name);
      if (label === undefined) throw new ApiError(422, `Label ${name} does not exist`);
      labels.push(label);
    }
    const number = repo.nextNumber;
    repo.nextNumber += 1;
    const owner = this.ownerName(repo);
    const issue: IssueRec = {
      id: this.newId(),
      number,
      title,
      body: body ?? '',
      state: 'open',
      labels,
      user: this.user(owner),
      created_at: this.trialNow,
      updated_at: this.trialNow,
      seq: repo.nextNumber,
      comments: [],
    };
    repo.issues.set(number, issue);
    return issue;
  }

  listIssues(repo: RepoRec, state: string | null, sort: string | null, direction: string | null): IssueRec[] {
    let list = [...repo.issues.values()];
    if (state !== null && state !== '' && state !== 'all') list = list.filter((i) => i.state === state);
    const dir = direction === 'asc' ? 1 : -1;
    if (sort === null || sort === '' || sort === 'created') list.sort((a, b) => (a.seq - b.seq) * dir);
    return list;
  }

  getIssue(repo: RepoRec, number: number): IssueRec {
    const issue = repo.issues.get(number);
    if (issue === undefined) throw new ApiError(404, `No such issue ${number}`);
    return issue;
  }

  patchIssue(repo: RepoRec, number: number, patch: { state?: string; title?: string; body?: string }): IssueRec {
    const issue = this.getIssue(repo, number);
    if (patch.state !== undefined) {
      if (patch.state !== 'open' && patch.state !== 'closed') throw new ApiError(422, `Invalid state ${patch.state}`);
      issue.state = patch.state;
    }
    if (patch.title !== undefined) issue.title = patch.title;
    if (patch.body !== undefined) issue.body = patch.body;
    issue.updated_at = this.trialNow;
    this.issueJsonTextCache.delete(`${repo.id}:${issue.number}`);
    return issue;
  }

  /** Serialized form of one issue, cached: the read bench GETs the same issue 2,000 times. */
  private issueJsonTextCache = new Map<string, string>();

  issueJsonCached(repo: RepoRec, issue: IssueRec): string {
    const key = `${repo.id}:${issue.number}`;
    const hit = this.issueJsonTextCache.get(key);
    if (hit !== undefined) return hit;
    const text = `${JSON.stringify(this.issueJson(repo, issue))}\n`;
    this.issueJsonTextCache.set(key, text);
    return text;
  }

  createComment(repo: RepoRec, number: number, body: string): CommentRec {
    const issue = this.getIssue(repo, number);
    const owner = this.ownerName(repo);
    const comment: CommentRec = { id: this.newId(), body, user: this.user(owner), created_at: this.trialNow };
    issue.comments.push(comment);
    issue.updated_at = this.trialNow;
    return comment;
  }

  // ---------------------------------------------------------------- pull requests

  createPull(repo: RepoRec, title: string, body: string, headRef: string, baseRef: string): PullRec {
    const headSha = repo.branches.get(headRef);
    const baseSha = repo.branches.get(baseRef);
    if (headSha === undefined) throw new ApiError(422, `Unknown head branch ${headRef}`);
    if (baseSha === undefined) throw new ApiError(422, `Unknown base branch ${baseRef}`);
    const headCommit = repo.commits.get(headSha);
    const baseCommit = repo.commits.get(baseSha);
    if (headCommit === undefined || baseCommit === undefined) throw new ApiError(422, 'Branch tip is not a commit');
    const number = repo.nextNumber;
    repo.nextNumber += 1;
    const owner = this.ownerName(repo);
    const pull: PullRec = {
      id: this.newId(),
      number,
      title,
      body: body ?? '',
      state: 'open',
      merged: false,
      merged_at: null,
      merge_commit_sha: null,
      head: { ref: headRef, sha: headSha, label: `${owner}:${headRef}` },
      base: { ref: baseRef, sha: baseSha, label: `${owner}:${baseRef}` },
      user: this.user(owner),
      created_at: this.trialNow,
      updated_at: this.trialNow,
      baseTreeAtCreation: new Map(baseCommit.tree),
      baseCommitAtCreation: baseSha,
      files: diffTrees(baseCommit.tree, headCommit.tree),
    };
    repo.pulls.set(number, pull);
    return pull;
  }

  listPulls(repo: RepoRec, state: string | null): PullRec[] {
    let list = [...repo.pulls.values()].sort((a, b) => a.number - b.number);
    if (state === null || state === '' || state === 'open') list = list.filter((p) => p.state === 'open');
    else if (state === 'closed') list = list.filter((p) => p.state === 'closed');
    return list;
  }

  getPull(repo: RepoRec, number: number): PullRec {
    const pull = repo.pulls.get(number);
    if (pull === undefined) throw new ApiError(404, `No such pull request ${number}`);
    return pull;
  }

  mergePull(repo: RepoRec, number: number): { sha: string; merged: boolean; message: string } {
    const pull = this.getPull(repo, number);
    if (pull.state !== 'open' || pull.merged) throw new ApiError(405, `Pull request ${number} cannot be merged`);
    const baseTip = repo.branches.get(pull.base.ref);
    const headTip = repo.branches.get(pull.head.ref);
    if (baseTip === undefined || headTip === undefined) throw new ApiError(405, `Pull request ${number} cannot be merged`);
    const baseCommit = repo.commits.get(baseTip);
    const headCommit = repo.commits.get(headTip);
    if (baseCommit === undefined || headCommit === undefined) throw new ApiError(405, `Pull request ${number} cannot be merged`);
    // Conflict: a path the PR changes was also changed on the base since the PR opened,
    // to different content. Anything else squash-applies cleanly.
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
        if (headSha !== onBase) throw new ApiError(405, `Pull request ${number} has a merge conflict on ${f.filename}`);
      }
    }
    // Squash: apply the head tree's changed files onto the current base tree.
    const mergedTree: Tree = new Map(baseCommit.tree);
    for (const f of prChanges) {
      const headEntry = headCommit.tree.get(f.filename);
      if (headEntry === undefined) mergedTree.delete(f.filename);
      else mergedTree.set(f.filename, headEntry);
    }
    const message = `Merge pull request #${number} from ${pull.head.ref}`;
    const commit = this.commitOnBranch(repo, pull.base.ref, message, (t) => {
      t.clear();
      for (const [k, v] of mergedTree) t.set(k, v);
    });
    pull.state = 'closed';
    pull.merged = true;
    pull.merged_at = this.trialNow;
    pull.merge_commit_sha = commit.sha;
    pull.updated_at = this.trialNow;
    return { sha: commit.sha, merged: true, message };
  }
}
