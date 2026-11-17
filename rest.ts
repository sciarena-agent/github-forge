// rest.ts — GitHub REST v3 surface under /api/v3.
// Every reply: application/json, non-empty X-GitHub-Request-Id, errors as {message}.
// Auth: Bearer $MAKEFUN_SHOP_TOKEN required on every /api/v3 route (healthz excepted,
// handled in app.ts). Unknown route: 404 JSON, never HTML. Malformed JSON body: 400.
import type { IncomingMessage, ServerResponse } from 'node:http';
import { ApiError, DEFAULT_BRANCH, type RepoRec, type Store } from './store.ts';

export interface RestContext {
  store: Store;
  token: string;
  reqId: () => string;
}

type Body = Record<string, unknown>;

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function bool(v: unknown, fallback = false): boolean {
  return typeof v === 'boolean' ? v : fallback;
}

function num(v: unknown, fallback = 0): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : fallback;
}

/** Answer with a pre-serialized JSON body (no re-stringify on the hot path). */
export function sendRawJson(res: ServerResponse, status: number, text: string, reqId: string, extra: Record<string, string> = {}): void {
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': reqId,
    ...extra,
  });
  res.end(text);
}

export function sendJson(res: ServerResponse, status: number, body: unknown, reqId: string, extra: Record<string, string> = {}): void {
  if (status === 204) {
    res.writeHead(204, { 'x-github-request-id': reqId, ...extra });
    res.end();
    return;
  }
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': reqId,
    ...extra,
  });
  res.end(text);
}

function errJson(res: ServerResponse, status: number, message: string, reqId: string): void {
  sendJson(res, status, { message }, reqId);
}

/** Read the request body as text; null when empty. Throws ApiError(400) on malformed JSON. */
export async function readBody(req: IncomingMessage): Promise<{ text: string; json: Body | null }> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  const text = Buffer.concat(chunks).toString('utf8');
  if (text.trim() === '') return { text, json: null };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new ApiError(400, 'Request body is not valid JSON');
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new ApiError(400, 'Request body must be a JSON object');
  }
  return { text, json: parsed as Body };
}

function paginate<T>(items: T[], query: URLSearchParams): { page: T[]; total: number; pageNum: number; perPage: number } {
  const perPage = Math.max(1, Math.min(100, num(query.get('per_page') === null ? NaN : Number(query.get('per_page')), 30) || 30));
  const pageNum = Math.max(1, num(query.get('page') === null ? NaN : Number(query.get('page')), 1) || 1);
  const start = (pageNum - 1) * perPage;
  return { page: items.slice(start, start + perPage), total: items.length, pageNum, perPage };
}

function linkHeader(req: IncomingMessage, basePath: string, query: URLSearchParams, pageNum: number, perPage: number, total: number): string | null {
  const lastPage = Math.max(1, Math.ceil(total / perPage));
  if (pageNum >= lastPage) return null;
  const host = req.headers.host ?? 'localhost';
  const q = new URLSearchParams(query);
  q.set('per_page', String(perPage));
  const url = (page: number): string => {
    q.set('page', String(page));
    return `http://${host}${basePath}?${q.toString()}`;
  };
  return `<${url(pageNum + 1)}>; rel="next", <${url(lastPage)}>; rel="last"`;
}

function needRepo(ctx: RestContext, owner: string, name: string): RepoRec {
  const repo = ctx.store.findRepo(owner, name);
  if (repo === null) throw new ApiError(404, `Repository ${owner}/${name} not found`);
  return repo;
}

function asLabels(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  const out: string[] = [];
  for (const item of v) {
    if (typeof item === 'string') out.push(item);
    else if (typeof item === 'object' && item !== null && typeof (item as Body).name === 'string') out.push((item as Body).name as string);
  }
  return out;
}

/** Handle one /api/v3 request. `path` is the pathname after /api/v3 ('' or '/' = root). */
export async function handleRest(req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams, ctx: RestContext): Promise<void> {
  const reqId = ctx.reqId();
  const method = req.method ?? 'GET';
  try {
    // Auth: every /api/v3 route needs the exact bearer token (when one is configured).
    if (ctx.token !== '') {
      const auth = req.headers.authorization ?? '';
      if (auth !== `Bearer ${ctx.token}`) {
        errJson(res, 401, 'Bad credentials', reqId);
        return;
      }
    }
    const body = method === 'GET' || method === 'HEAD' ? { text: '', json: null } : await readBody(req);
    const json: Body = body.json ?? {};
    const segs = path.split('/').filter((s) => s !== '');
    const dec = (s: string): string => {
      try {
        return decodeURIComponent(s);
      } catch {
        return s;
      }
    };

    // GET /api/v3/ — root
    if (segs.length === 0) {
      if (method !== 'GET') throw new ApiError(404, 'Not Found');
      sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, reqId, {
        'x-oauth-scopes': 'repo, read:org',
      });
      return;
    }

    // GET /api/v3/meta
    if (segs.length === 1 && segs[0] === 'meta') {
      if (method !== 'GET') throw new ApiError(404, 'Not Found');
      sendJson(res, 200, { installed_version: '1.0.0' }, reqId);
      return;
    }

    // GET /api/v3/user — the token's login (learned); 404 while unknown (callers fall back).
    if (segs.length === 1 && segs[0] === 'user') {
      if (method !== 'GET') throw new ApiError(404, 'Not Found');
      const login = ctx.store.viewerLogin;
      if (login === null) {
        errJson(res, 404, 'Not Found', reqId);
        return;
      }
      sendJson(res, 200, { login, id: 1000, type: 'User' }, reqId);
      return;
    }

    // /api/v3/user/repos
    if (segs.length === 2 && segs[0] === 'user' && segs[1] === 'repos') {
      if (method === 'GET') {
        const repos = ctx.store.listRepos();
        const { page, total, pageNum, perPage } = paginate(repos, query);
        const extra: Record<string, string> = {};
        const link = linkHeader(req, '/api/v3/user/repos', query, pageNum, perPage, total);
        if (link !== null) extra.link = link;
        sendJson(res, 200, page.map((r) => ctx.store.repoJson(r)), reqId, extra);
        return;
      }
      if (method === 'POST') {
        const repo = ctx.store.createRepo(str(json.name), bool(json.private, false), str(json.description));
        sendJson(res, 201, ctx.store.repoJson(repo), reqId);
        return;
      }
      throw new ApiError(404, 'Not Found');
    }

    // /api/v3/repos/{owner}/{repo}[...]
    if (segs.length >= 3 && segs[0] === 'repos') {
      const owner = dec(segs[1]!);
      const name = dec(segs[2]!);
      const rest = segs.slice(3);
      await handleRepoRoute(req, res, method, owner, name, rest, query, json, ctx, reqId);
      return;
    }

    throw new ApiError(404, 'Not Found');
  } catch (err) {
    if (err instanceof ApiError) {
      errJson(res, err.status, err.message, reqId);
      return;
    }
    errJson(res, 500, 'Internal Server Error', reqId);
  }
}

async function handleRepoRoute(
  req: IncomingMessage,
  res: ServerResponse,
  method: string,
  owner: string,
  name: string,
  rest: string[],
  query: URLSearchParams,
  json: Body,
  ctx: RestContext,
  reqId: string,
): Promise<void> {
  const dec = (s: string): string => {
    try {
      return decodeURIComponent(s);
    } catch {
      return s;
    }
  };
  const store = ctx.store;

  // /repos/{o}/{r} itself
  if (rest.length === 0) {
    if (method === 'GET') {
      const repo = needRepo(ctx, owner, name);
      sendJson(res, 200, store.repoJson(repo), reqId);
      return;
    }
    if (method === 'PATCH') {
      const repo = needRepo(ctx, owner, name);
      if (json.description !== undefined) repo.description = str(json.description);
      repo.updated_at = store.trialNow;
      sendJson(res, 200, store.repoJson(repo), reqId);
      return;
    }
    if (method === 'DELETE') {
      const repo = needRepo(ctx, owner, name);
      store.deleteRepo(repo);
      sendJson(res, 204, null, reqId);
      return;
    }
    throw new ApiError(404, 'Not Found');
  }

  const sub = rest[0]!;
  const repo = needRepo(ctx, owner, name);

  // GET /branches, GET /branches/{branch}
  if (sub === 'branches') {
    if (method === 'GET' && rest.length === 1) {
      sendJson(res, 200, store.listBranches(repo), reqId);
      return;
    }
    if (method === 'GET' && rest.length === 2) {
      const branch = dec(rest[1]!);
      const tip = repo.branches.get(branch);
      if (tip === undefined) throw new ApiError(404, `No such branch ${branch}`);
      sendJson(res, 200, { name: branch, commit: { sha: tip } }, reqId);
      return;
    }
    throw new ApiError(404, 'Not Found');
  }

  // GET /commits[?sha=]
  if (sub === 'commits') {
    if (method === 'GET' && rest.length === 1) {
      const ref = query.get('sha');
      sendJson(res, 200, store.listCommits(repo, ref), reqId);
      return;
    }
    throw new ApiError(404, 'Not Found');
  }

  // /contents/{path...}
  if (sub === 'contents') {
    const filePath = rest.slice(1).map(dec).join('/');
    if (method === 'GET') {
      const ref = query.get('ref');
      if (filePath === '') {
        sendJson(res, 200, store.listDirectory(repo, '', ref), reqId);
        return;
      }
      // A directory lists its entries; a file returns the file object.
      // A named path that is neither is a 404 (GET-after-DELETE relies on this).
      const tree = store.resolveTree(repo, ref);
      if (!tree.has(filePath)) {
        const listing = store.listDirectory(repo, filePath, ref);
        if (listing.length === 0) throw new ApiError(404, `No such file or directory ${filePath}`);
        sendJson(res, 200, listing, reqId);
        return;
      }
      const { entry } = store.getContentEntry(repo, filePath, ref);
      const base = filePath.split('/').pop() ?? filePath;
      sendJson(
        res,
        200,
        {
          type: 'file',
          encoding: 'base64',
          size: entry.content.length,
          name: base,
          path: filePath,
          content: entry.content.toString('base64'),
          sha: entry.blobSha,
        },
        reqId,
      );
      return;
    }
    if (method === 'PUT') {
      if (filePath === '') throw new ApiError(404, 'Not Found');
      const result = store.putContent(repo, filePath, str(json.content), str(json.message), str(json.branch) === '' ? undefined : str(json.branch), str(json.sha) === '' ? undefined : str(json.sha));
      const base = filePath.split('/').pop() ?? filePath;
      sendJson(
        res,
        result.created ? 201 : 200,
        {
          content: { name: base, path: filePath, sha: result.contentSha, size: Buffer.from(str(json.content), 'base64').length, encoding: 'base64' },
          commit: { sha: result.commitSha },
        },
        reqId,
      );
      return;
    }
    if (method === 'DELETE') {
      if (filePath === '') throw new ApiError(404, 'Not Found');
      const result = store.deleteContent(repo, filePath, str(json.message), str(json.branch) === '' ? undefined : str(json.branch), str(json.sha) === '' ? undefined : str(json.sha));
      sendJson(res, 200, { commit: { sha: result.commitSha } }, reqId);
      return;
    }
    throw new ApiError(404, 'Not Found');
  }

  // POST /git/refs, GET /git/refs/heads/{branch}
  if (sub === 'git') {
    if (rest.length === 2 && rest[1] === 'refs' && method === 'POST') {
      const ref = str(json.ref);
      const sha = str(json.sha);
      const match = /^refs\/heads\/(.+)$/.exec(ref);
      if (match === null) throw new ApiError(422, `Invalid ref ${ref}`);
      const tip = store.createBranch(repo, match[1]!, sha);
      sendJson(res, 201, { ref, object: { sha: tip, type: 'commit' } }, reqId);
      return;
    }
    if (rest.length >= 4 && rest[1] === 'refs' && rest[2] === 'heads' && method === 'GET') {
      const branch = rest.slice(3).map(dec).join('/');
      const tip = repo.branches.get(branch);
      if (tip === undefined) throw new ApiError(404, `No such ref refs/heads/${branch}`);
      sendJson(res, 200, { ref: `refs/heads/${branch}`, object: { sha: tip, type: 'commit' } }, reqId);
      return;
    }
    throw new ApiError(404, 'Not Found');
  }

  // POST /labels, GET /labels
  if (sub === 'labels') {
    if (method === 'POST' && rest.length === 1) {
      const label = store.createLabel(repo, str(json.name), str(json.color, 'ededed'));
      sendJson(res, 201, { name: label.name, color: label.color }, reqId);
      return;
    }
    if (method === 'GET' && rest.length === 1) {
      sendJson(res, 200, [...repo.labels.values()].map((l) => ({ name: l.name, color: l.color })), reqId);
      return;
    }
    throw new ApiError(404, 'Not Found');
  }

  // /issues...
  if (sub === 'issues') {
    if (rest.length === 1) {
      if (method === 'GET') {
        // GitHub's default list state is open; an explicit state=all overrides it.
        const list = store.listIssues(repo, query.get('state') ?? 'open', query.get('sort'), query.get('direction'));
        const { page } = paginate(list, query);
        sendJson(res, 200, page.map((i) => store.issueJson(repo, i)), reqId);
        return;
      }
      if (method === 'POST') {
        const issue = store.createIssue(repo, str(json.title), str(json.body), asLabels(json.labels));
        sendJson(res, 201, store.issueJson(repo, issue), reqId);
        return;
      }
      throw new ApiError(404, 'Not Found');
    }
    const number = Number(rest[1]);
    if (!Number.isInteger(number) || number < 1) throw new ApiError(404, 'Not Found');
    if (rest.length === 2) {
      if (method === 'GET') {
        // Hot path (the read bench hits this 2,000 times): serve the cached serialization.
        sendRawJson(res, 200, store.issueJsonCached(repo, store.getIssue(repo, number)), reqId);
        return;
      }
      if (method === 'PATCH') {
        const patched = store.patchIssue(repo, number, {
          state: typeof json.state === 'string' ? json.state : undefined,
          title: typeof json.title === 'string' ? json.title : undefined,
          body: typeof json.body === 'string' ? json.body : undefined,
        });
        sendJson(res, 200, store.issueJson(repo, patched), reqId);
        return;
      }
      throw new ApiError(404, 'Not Found');
    }
    if (rest.length === 3 && rest[2] === 'comments') {
      if (method === 'GET') {
        const issue = store.getIssue(repo, number);
        sendJson(
          res,
          200,
          issue.comments.map((c) => ({ id: c.id, body: c.body, user: c.user, created_at: c.created_at })),
          reqId,
        );
        return;
      }
      if (method === 'POST') {
        const comment = store.createComment(repo, number, str(json.body));
        sendJson(res, 201, { id: comment.id, body: comment.body, user: comment.user, created_at: comment.created_at }, reqId);
        return;
      }
      throw new ApiError(404, 'Not Found');
    }
    throw new ApiError(404, 'Not Found');
  }

  // /pulls...
  if (sub === 'pulls') {
    if (rest.length === 1) {
      if (method === 'GET') {
        const list = store.listPulls(repo, query.get('state'));
        const { page } = paginate(list, query);
        sendJson(res, 200, page.map((p) => store.pullJson(repo, p)), reqId);
        return;
      }
      if (method === 'POST') {
        const pull = store.createPull(repo, str(json.title), str(json.body), str(json.head), str(json.base));
        sendJson(res, 201, store.pullJson(repo, pull), reqId);
        return;
      }
      throw new ApiError(404, 'Not Found');
    }
    const number = Number(rest[1]);
    if (!Number.isInteger(number) || number < 1) throw new ApiError(404, 'Not Found');
    if (rest.length === 2) {
      if (method === 'GET') {
        sendJson(res, 200, store.pullJson(repo, store.getPull(repo, number)), reqId);
        return;
      }
      throw new ApiError(404, 'Not Found');
    }
    if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
      const pull = store.getPull(repo, number);
      sendJson(res, 200, pull.files, reqId);
      return;
    }
    if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
      const result = store.mergePull(repo, number);
      sendJson(res, 200, result, reqId);
      return;
    }
    throw new ApiError(404, 'Not Found');
  }

  throw new ApiError(404, 'Not Found');
}
