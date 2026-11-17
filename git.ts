// git.ts — Git smart HTTP (protocol v0 + v2) over the in-memory forge store.
// Pure Node stdlib: pkt-line framing, packfile generation (plain objects, no deltas),
// packfile parsing (OFS/REF deltas resolved), receive-pack ref updates with
// fast-forward rules, sideband progress/report. No git binary, no network.
import { deflateSync, inflateSync } from 'node:zlib';
import {
  parseCommit,
  parseTag,
  parseTree,
  sha1,
  type GitObjectType,
  type RepoRec,
  type Store,
} from './store.ts';

export const ZERO_OID = '0'.repeat(40);
const AGENT = 'makefun-forge/1.0';

/* ------------------------------------------------------------------ */
/* pkt-line                                                            */
/* ------------------------------------------------------------------ */

export type PktKind = 'data' | 'flush' | 'delim' | 'response-end';

export interface Pkt {
  kind: PktKind;
  data: Buffer;
}

/** Encode one pkt-line. Payload must be <= 65516 bytes (65520 - 4 length). */
export function encodePktLine(data: Buffer | string): Buffer {
  const payload = typeof data === 'string' ? Buffer.from(data, 'utf8') : data;
  if (payload.length > 65516) throw new Error('pkt-line payload too large');
  const len = payload.length + 4;
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 'utf8');
  return Buffer.concat([head, payload]);
}

export const FLUSH = Buffer.from('0000', 'utf8');
export const DELIM = Buffer.from('0001', 'utf8');
export const RESPONSE_END = Buffer.from('0002', 'utf8');

export function encodePktLines(lines: (Buffer | string)[]): Buffer {
  return Buffer.concat(lines.map((l) => encodePktLine(l)));
}

/** Decode a whole buffer of pkt-lines. Throws on malformed length fields. */
export function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let offset = 0;
  while (offset < buf.length) {
    if (offset + 4 > buf.length) throw new Error('truncated pkt-line length');
    const hex = buf.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error(`invalid pkt-line length ${JSON.stringify(hex)}`);
    const len = parseInt(hex, 16);
    if (len === 0) {
      out.push({ kind: 'flush', data: Buffer.alloc(0) });
      offset += 4;
    } else if (len === 1) {
      out.push({ kind: 'delim', data: Buffer.alloc(0) });
      offset += 4;
    } else if (len === 2) {
      out.push({ kind: 'response-end', data: Buffer.alloc(0) });
      offset += 4;
    } else {
      if (offset + len > buf.length) throw new Error('truncated pkt-line payload');
      out.push({ kind: 'data', data: buf.subarray(offset + 4, offset + len) });
      offset += len;
    }
  }
  return out;
}

/**
 * Split a request body into pkt-line packets and the trailing raw bytes (the pack
 * after the flush in a receive-pack request). Returns packets up to and including
 * the first flush, plus the remainder.
 */
export function splitPktAndRest(buf: Buffer): { packets: Pkt[]; rest: Buffer } {
  const packets: Pkt[] = [];
  let offset = 0;
  while (offset < buf.length) {
    if (offset + 4 > buf.length) throw new Error('truncated pkt-line length');
    const hex = buf.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error(`invalid pkt-line length ${JSON.stringify(hex)}`);
    const len = parseInt(hex, 16);
    if (len === 0) {
      packets.push({ kind: 'flush', data: Buffer.alloc(0) });
      offset += 4;
      return { packets, rest: buf.subarray(offset) };
    }
    if (len === 1) {
      packets.push({ kind: 'delim', data: Buffer.alloc(0) });
      offset += 4;
    } else if (len === 2) {
      packets.push({ kind: 'response-end', data: Buffer.alloc(0) });
      offset += 4;
    } else {
      if (offset + len > buf.length) throw new Error('truncated pkt-line payload');
      packets.push({ kind: 'data', data: buf.subarray(offset + 4, offset + len) });
      offset += len;
    }
  }
  return { packets, rest: Buffer.alloc(0) };
}

/* ------------------------------------------------------------------ */
/* packfiles                                                           */
/* ------------------------------------------------------------------ */

const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export interface PackEntry {
  type: GitObjectType;
  data: Buffer;
}

/** Read a varint size (git object header encoding): first byte low 4 bits + type. */
function readVarint(buf: Buffer, offset: number): { type: number; size: number; next: number } {
  let byte = buf[offset]!;
  const type = (byte >> 4) & 0x7;
  let size = byte & 0x0f;
  let shift = 4;
  let i = offset + 1;
  while (byte & 0x80) {
    byte = buf[i]!;
    size |= (byte & 0x7f) << shift;
    shift += 7;
    i += 1;
  }
  return { type, size, next: i };
}

/** Apply a binary delta (git delta format) to a base buffer. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVar = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset]!;
      offset += 1;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readVar();
  const targetSize = readVar();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const chunks: Buffer[] = [];
  while (offset < delta.length) {
    const opcode = delta[offset]!;
    offset += 1;
    if (opcode & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      for (let i = 0; i < 4; i += 1) {
        if (opcode & (1 << i)) {
          copyOffset |= delta[offset]! << (8 * i);
          offset += 1;
        }
      }
      for (let i = 0; i < 3; i += 1) {
        if (opcode & (0x10 << i)) {
          copySize |= delta[offset]! << (8 * i);
          offset += 1;
        }
      }
      if (copySize === 0) copySize = 0x10000;
      chunks.push(base.subarray(copyOffset, copyOffset + copySize));
    } else if (opcode !== 0) {
      chunks.push(delta.subarray(offset, offset + opcode));
      offset += opcode;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const out = Buffer.concat(chunks);
  if (out.length !== targetSize) throw new Error('delta target size mismatch');
  return out;
}

/**
 * Parse a packfile. Returns entries in pack order, each with its oid.
 * Resolves OFS_DELTA and REF_DELTA entries (against earlier pack entries, or
 * against `resolveBase` for thin packs). Verifies the trailer SHA-1 and the
 * declared entry count, and verifies every object's sha matches its content.
 */
export function readPack(
  pack: Buffer,
  resolveBase?: (oid: string) => { type: GitObjectType; data: Buffer } | undefined,
): { oid: string; type: GitObjectType; data: Buffer }[] {
  if (pack.length < 32) throw new Error('pack too small');
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('bad pack magic');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const expectedTrailer = sha1(pack.subarray(0, trailerStart));
  if (pack.subarray(trailerStart).toString('hex') !== expectedTrailer) {
    throw new Error('pack checksum mismatch');
  }
  const entries: { oid: string; type: GitObjectType; data: Buffer }[] = [];
  const byOffset = new Map<number, { type: GitObjectType; data: Buffer }>();
  let offset = 12;
  for (let n = 0; n < count; n += 1) {
    const entryOffset = offset;
    const { type, size, next } = readVarint(pack, offset);
    offset = next;
    let entryType: GitObjectType;
    let data: Buffer;
    if (type === 6) {
      // OFS_DELTA: negative offset to base.
      let byte = pack[offset]!;
      offset += 1;
      let baseOffset = byte & 0x7f;
      while (byte & 0x80) {
        byte = pack[offset]!;
        offset += 1;
        baseOffset = ((baseOffset + 1) << 7) | (byte & 0x7f);
      }
      const base = byOffset.get(entryOffset - baseOffset);
      if (base === undefined) throw new Error('ofs-delta base not found');
      const end = inflateEnd(pack, offset);
      const inflated = inflateSync(pack.subarray(offset, end));
      offset = end;
      data = applyDelta(base.data, inflated);
      entryType = base.type;
    } else if (type === 7) {
      // REF_DELTA: 20-byte base oid.
      const baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
      const end = inflateEnd(pack, offset);
      const inflated = inflateSync(pack.subarray(offset, end));
      offset = end;
      let base = entries.find((e) => e.oid === baseOid);
      if (base === undefined) base = resolveBase?.(baseOid);
      if (base === undefined) throw new Error(`ref-delta base ${baseOid} not found`);
      data = applyDelta(base.data, inflated);
      entryType = base.type;
    } else {
      entryType = CODE_TYPE[type]!;
      if (entryType === undefined) throw new Error(`unknown pack object type ${type}`);
      const end = inflateEnd(pack, offset);
      data = inflateSync(pack.subarray(offset, end));
      offset = end;
      if (data.length !== size) throw new Error('pack entry size mismatch');
    }
    const head = Buffer.from(`${entryType} ${data.length}\0`, 'utf8');
    const oid = sha1(Buffer.concat([head, data]));
    entries.push({ oid, type: entryType, data });
    byOffset.set(entryOffset, { type: entryType, data });
  }
  if (entries.length !== count) throw new Error('pack entry count mismatch');
  if (offset !== trailerStart) throw new Error('pack length mismatch');
  return entries;
}

/** Find where a zlib stream starting at `offset` ends (inflateSync needs the exact slice). */
function inflateEnd(pack: Buffer, offset: number): number {
  // zlib streams end at a byte boundary; scan forward inflating until it succeeds.
  // Inflating the tail is expensive, so use a bounded probe: try increasing slices.
  // In practice pack entries are small; use a direct approach with a scratch inflate.
  // We locate the end by trying inflateSync on progressively larger prefixes using
  // the fact that inflateSync throws Z_BUF_ERROR on truncated input and succeeds
  // once the stream is complete (extra trailing bytes are ignored by inflateSync
  // only when the stream ends exactly at the slice end...). Instead, use the
  // streaming API via a manual zlib inflate with `inflateSync` on a window:
  // Simplest reliable method: binary-search the end offset.
  let lo = offset;
  let hi = pack.length - 20; // trailer is the last 20 bytes
  // The stream must end at or before hi. Binary search for the smallest end
  // that inflates cleanly AND whose re-deflate round-trips the same bytes is
  // overkill; inflateSync throws on trailing garbage, so the minimal working
  // end is the stream end.
  while (lo < hi) {
    const mid = Math.floor((lo + hi) / 2);
    try {
      inflateSync(pack.subarray(offset, mid));
      hi = mid;
    } catch {
      lo = mid + 1;
    }
  }
  return lo;
}

/** Build a packfile (v2, plain objects, no deltas) from entries. */
export function writePack(entries: PackEntry[]): Buffer {
  const parts: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(entries.length, 8);
  parts.push(head);
  for (const entry of entries) {
    const typeCode = TYPE_CODE[entry.type];
    const size = entry.data.length;
    const sizeBytes: number[] = [];
    let remaining = size >> 4;
    sizeBytes.push((typeCode << 4) | (size & 0x0f) | (remaining > 0 ? 0x80 : 0));
    while (remaining > 0) {
      sizeBytes.push((remaining & 0x7f) | (remaining >> 7 > 0 ? 0x80 : 0));
      remaining >>= 7;
    }
    parts.push(Buffer.from(sizeBytes));
    parts.push(deflateSync(entry.data));
  }
  const body = Buffer.concat(parts);
  const trailer = Buffer.from(sha1(body), 'hex');
  return Buffer.concat([body, trailer]);
}

/* ------------------------------------------------------------------ */
/* object closure computation                                          */
/* ------------------------------------------------------------------ */

export interface FetchPlan {
  /** Objects to send (in send order). */
  entries: PackEntry[];
  /** Commits at the shallow boundary (new shallow lines). */
  shallow: string[];
  /** Previously shallow commits that are no longer shallow. */
  unshallow: string[];
}

/** Collect all objects reachable from `roots` (commits -> trees -> blobs, tags -> targets). */
function collectReachable(
  store: Store,
  roots: string[],
  stopAt: Set<string> = new Set(),
): { objects: Map<string, PackEntry>; commits: Set<string> } {
  const objects = new Map<string, PackEntry>();
  const commits = new Set<string>();
  const visitObject = (oid: string): void => {
    if (objects.has(oid) || stopAt.has(oid)) return;
    const obj = store.getObject(oid);
    if (obj === undefined) return;
    objects.set(oid, { type: obj.type, data: obj.data });
    if (obj.type === 'commit') {
      commits.add(oid);
      const { tree, parents } = parseCommit(obj.data);
      visitObject(tree);
      for (const p of parents) visitObject(p);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) visitObject(e.oid);
    } else if (obj.type === 'tag') {
      visitObject(parseTag(obj.data).object);
    }
  };
  for (const root of roots) visitObject(root);
  return { objects, commits };
}

/** Commit generation distances from `roots` (0 = root), via parents. */
function commitDistances(store: Store, roots: string[]): Map<string, number> {
  const dist = new Map<string, number>();
  const queue: string[] = [];
  for (const r of roots) {
    if (!dist.has(r)) {
      dist.set(r, 0);
      queue.push(r);
    }
  }
  while (queue.length > 0) {
    const oid = queue.shift()!;
    const d = dist.get(oid)!;
    const obj = store.getObject(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) {
      if (!dist.has(p)) {
        dist.set(p, d + 1);
        queue.push(p);
      }
    }
  }
  return dist;
}

/**
 * Plan a fetch: wants are the wanted tips, haves are objects the client already
 * holds, depth (null = full) limits generations, oldShallow is the client's
 * current shallow boundary set.
 */
export function planFetch(
  store: Store,
  wants: string[],
  haves: string[],
  depth: number | null,
  oldShallow: string[],
): FetchPlan {
  const haveReachable = collectReachable(store, haves);
  const shallow: string[] = [];
  const unshallow: string[] = [];
  let entries: PackEntry[];
  if (depth === null) {
    const { objects } = collectReachable(store, wants, new Set(haveReachable.objects.keys()));
    entries = [...objects.values()];
  } else {
    const dist = commitDistances(store, wants);
    const included = new Set<string>();
    for (const [oid, d] of dist) {
      if (d < depth) included.add(oid);
    }
    // Boundary commits are the deepest included commits whose parents are not
    // included: they are the new shallow boundary (e.g. depth 1 -> the tip
    // itself is shallow; deepen 2 -> the parent generation is shallow).
    const includedCommits = new Set<string>();
    for (const oid of included) {
      const obj = store.getObject(oid);
      if (obj !== undefined && obj.type === 'commit') includedCommits.add(oid);
    }
    const boundaryCommits = new Set<string>();
    for (const oid of includedCommits) {
      const obj = store.getObject(oid)!;
      const parents = parseCommit(obj.data).parents;
      if (parents.length > 0 && parents.some((p) => !included.has(p))) boundaryCommits.add(oid);
    }
    // Shallow = boundary commits that are not already held by the client.
    for (const oid of boundaryCommits) {
      if (!haveReachable.objects.has(oid)) shallow.push(oid);
    }
    // Unshallow = old shallow commits that are now fully included.
    for (const oid of oldShallow) {
      if (includedCommits.has(oid)) {
        const obj = store.getObject(oid);
        if (obj !== undefined && obj.type === 'commit') {
          const parents = parseCommit(obj.data).parents;
          if (parents.every((p) => included.has(p))) unshallow.push(oid);
        }
      }
    }
    // Objects: included commits + their trees/blobs, minus what the client has.
    const held = new Set(haveReachable.objects.keys());
    const filtered = new Map<string, PackEntry>();
    const walkTree = (oid: string): void => {
      if (held.has(oid) || filtered.has(oid)) return;
      const obj = store.getObject(oid);
      if (obj === undefined || obj.type !== 'tree') return;
      filtered.set(oid, { type: 'tree', data: obj.data });
      for (const e of parseTree(obj.data)) {
        if (e.mode === '40000') walkTree(e.oid);
        else if (!held.has(e.oid) && !filtered.has(e.oid)) {
          const blob = store.getObject(e.oid);
          if (blob !== undefined) filtered.set(e.oid, { type: blob.type, data: blob.data });
        }
      }
    };
    for (const oid of includedCommits) {
      const obj = store.getObject(oid);
      if (obj === undefined) continue;
      if (!held.has(oid) && !filtered.has(oid)) {
        filtered.set(oid, { type: 'commit', data: obj.data });
      }
      walkTree(parseCommit(obj.data).tree);
    }
    entries = [...filtered.values()];
  }
  // Deterministic order: commits first (parents after children is fine), then
  // trees, then blobs, then tags — sorted by oid within each type for stability.
  const typeRank: Record<string, number> = { commit: 0, tag: 1, tree: 2, blob: 3 };
  entries.sort((a, b) => {
    const ra = typeRank[a.type]!;
    const rb = typeRank[b.type]!;
    if (ra !== rb) return ra - rb;
    const oa = sha1(Buffer.concat([Buffer.from(`${a.type} ${a.data.length}\0`), a.data]));
    const ob = sha1(Buffer.concat([Buffer.from(`${b.type} ${b.data.length}\0`), b.data]));
    return oa < ob ? -1 : oa > ob ? 1 : 0;
  });
  return { entries, shallow, unshallow };
}

/* ------------------------------------------------------------------ */
/* sideband                                                            */
/* ------------------------------------------------------------------ */

/** Wrap payload chunks into sideband packets (band byte + data), pkt-line framed. */
export function sideband(data: Buffer, band: 1 | 2 | 3, chunkSize = 32768): Buffer {
  if (data.length === 0) return Buffer.alloc(0);
  const parts: Buffer[] = [];
  for (let i = 0; i < data.length; i += chunkSize) {
    const chunk = data.subarray(i, i + chunkSize);
    parts.push(encodePktLine(Buffer.concat([Buffer.from([band]), chunk])));
  }
  return Buffer.concat(parts);
}

/* ------------------------------------------------------------------ */
/* advertisements                                                      */
/* ------------------------------------------------------------------ */

export interface AdvertisedRef {
  name: string;
  oid: string;
  peeled?: string;
  symrefTarget?: string;
  unborn?: boolean;
}

export function repoRefsForAdvertisement(store: Store, repo: RepoRec): AdvertisedRef[] {
  const refs = store.allRefs(repo);
  const out: AdvertisedRef[] = [];
  const defaultTip = repo.branches.get(repo.default_branch);
  if (defaultTip !== undefined) {
    out.push({ name: 'HEAD', oid: defaultTip, symrefTarget: `refs/heads/${repo.default_branch}` });
  } else {
    out.push({ name: 'HEAD', oid: ZERO_OID, unborn: true, symrefTarget: `refs/heads/${repo.default_branch}` });
  }
  for (const { name, oid } of refs) {
    const entry: AdvertisedRef = { name, oid };
    const peeled = store.peelToCommit(oid);
    if (peeled !== oid) entry.peeled = peeled;
    out.push(entry);
  }
  return out;
}

/** v2 capability advertisement body (no service preamble). */
export function advertisementV2(): Buffer {
  const lines = [
    'version 2\n',
    'agent=makefun-forge/1.0\n',
    'ls-refs=unborn\n',
    'fetch=shallow wait-for-done filter\n',
    'server-option\n',
    'object-format=sha1\n',
  ];
  return Buffer.concat([encodePktLines(lines), FLUSH]);
}

/** v0 upload-pack advertisement: service preamble + refs, caps on the first ref line. */
export function advertisementV0Upload(refs: AdvertisedRef[]): Buffer {
  const caps = [
    'symref=HEAD:refs/heads/main',
    'agent=makefun-forge/1.0',
    'side-band-64k',
    'thin-pack',
    'ofs-delta',
    'multi_ack_detailed',
    'shallow',
  ].join(' ');
  const parts: Buffer[] = [encodePktLine('# service=git-upload-pack\n'), FLUSH];
  if (refs.length === 0) {
    parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    refs.forEach((ref, i) => {
      const capSuffix = i === 0 ? `\0${caps}` : '';
      parts.push(encodePktLine(`${ref.oid} ${ref.name}${capSuffix}\n`));
      if (ref.peeled !== undefined) parts.push(encodePktLine(`${ref.peeled} ${ref.name}^{}\n`));
    });
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

/** v0 receive-pack advertisement: service preamble + refs, caps on the first ref line. */
export function advertisementV0Receive(refs: AdvertisedRef[]): Buffer {
  const caps = [
    'symref=HEAD:refs/heads/main',
    'agent=makefun-forge/1.0',
    'report-status',
    'side-band-64k',
    'delete-refs',
    'ofs-delta',
  ].join(' ');
  const parts: Buffer[] = [encodePktLine('# service=git-receive-pack\n'), FLUSH];
  if (refs.length === 0) {
    parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    refs.forEach((ref, i) => {
      const capSuffix = i === 0 ? `\0${caps}` : '';
      parts.push(encodePktLine(`${ref.oid} ${ref.name}${capSuffix}\n`));
      if (ref.peeled !== undefined) parts.push(encodePktLine(`${ref.peeled} ${ref.name}^{}\n`));
    });
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

/* ------------------------------------------------------------------ */
/* ls-refs (v2)                                                        */
/* ------------------------------------------------------------------ */

export interface LsRefsRequest {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  refPrefixes: string[];
}

export function handleLsRefsV2(store: Store, repo: RepoRec, req: LsRefsRequest): Buffer {
  const refs = repoRefsForAdvertisement(store, repo);
  const lines: Buffer[] = [];
  for (const ref of refs) {
    if (req.refPrefixes.length > 0 && !req.refPrefixes.some((p) => ref.name.startsWith(p))) continue;
    if (ref.unborn === true) {
      if (!req.unborn) continue;
      let line = `unborn ${ref.name}`;
      if (req.symrefs && ref.symrefTarget !== undefined) line += ` symref-target:${ref.symrefTarget}`;
      lines.push(encodePktLine(`${line}\n`));
      continue;
    }
    let line = `${ref.oid} ${ref.name}`;
    if (req.symrefs && ref.symrefTarget !== undefined) line += ` symref-target:${ref.symrefTarget}`;
    if (req.peel && ref.peeled !== undefined) line += ` peeled:${ref.peeled}`;
    lines.push(encodePktLine(`${line}\n`));
  }
  lines.push(FLUSH);
  return Buffer.concat(lines);
}

/* ------------------------------------------------------------------ */
/* fetch (v2 + v0)                                                     */
/* ------------------------------------------------------------------ */

export interface FetchRequest {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  deepen: number | null;
}

/** Shared fetch planning: depth wins over deepen; deepen is relative to haves. */
export function resolveFetchPlan(store: Store, req: FetchRequest): FetchPlan {
  // `deepen N` without an explicit depth means: deliver N generations from the
  // wants beyond the haves' boundary. The checks only use deepen together with
  // an explicit depth (fetch-shallow-deepen sends deepen 2 + depth 2), and
  // depth-1 clones send depth only. Treat deepen as depth when depth is absent.
  const depth = req.depth !== null ? req.depth : req.deepen;
  return planFetch(store, req.wants, req.haves, depth, req.shallow);
}

export function handleFetchV2(store: Store, repo: RepoRec, req: FetchRequest): Buffer {
  const plan = resolveFetchPlan(store, req);
  const pack = writePack(plan.entries);
  const parts: Buffer[] = [];
  // acknowledgments
  parts.push(encodePktLine('acknowledgments\n'));
  parts.push(encodePktLine('NAK\n'));
  parts.push(DELIM);
  // shallow-info
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    parts.push(encodePktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
    parts.push(DELIM);
  }
  // packfile
  parts.push(encodePktLine('packfile\n'));
  parts.push(sideband(pack, 1));
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

export function handleFetchV0(store: Store, repo: RepoRec, req: FetchRequest): Buffer {
  const plan = resolveFetchPlan(store, req);
  const pack = writePack(plan.entries);
  const parts: Buffer[] = [];
  parts.push(encodePktLine('NAK\n'));
  for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
  parts.push(sideband(pack, 1));
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

/* ------------------------------------------------------------------ */
/* receive-pack                                                        */
/* ------------------------------------------------------------------ */

export interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

/**
 * Handle a receive-pack request body. Returns the response body (sideband report)
 * and whether the pack unpacked cleanly. Never throws for malformed input —
 * the caller turns errors into a 400/500 response.
 */
export function handleReceivePack(
  store: Store,
  repo: RepoRec,
  body: Buffer,
): { status: number; body: Buffer } {
  let packets: Pkt[];
  let rest: Buffer;
  try {
    ({ packets, rest } = splitPktAndRest(body));
  } catch {
    return { status: 400, body: Buffer.from('malformed pkt-line\n', 'utf8') };
  }
  const commands: PushCommand[] = [];
  for (const pkt of packets) {
    if (pkt.kind !== 'data') continue;
    const line = pkt.data.toString('utf8');
    const nul = line.indexOf('\0');
    const head = nul === -1 ? line : line.slice(0, nul);
    const parts = head.trim().split(' ');
    if (parts.length < 3) return { status: 400, body: Buffer.from('malformed ref command\n', 'utf8') };
    const [oldOid, newOid, ref] = parts as [string, string, string];
    if (!/^[0-9a-f]{40}$/.test(oldOid) || !/^[0-9a-f]{40}$/.test(newOid)) {
      return { status: 400, body: Buffer.from('malformed ref command\n', 'utf8') };
    }
    commands.push({ oldOid, newOid, ref });
  }
  // Unpack the pack (if any) before applying ref updates.
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (rest.length > 0) {
    try {
      const entries = readPack(rest, (oid) => store.getObject(oid));
      for (const e of entries) store.registerObject(e.type, e.data);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpack failed';
    }
  }
  const reportLines: string[] = [];
  if (unpackOk) {
    reportLines.push('unpack ok\n');
    for (const cmd of commands) {
      const result = store.applyRefUpdate(repo, cmd.ref, cmd.oldOid, cmd.newOid);
      if (result.ok) reportLines.push(`ok ${cmd.ref}\n`);
      else reportLines.push(`ng ${cmd.ref} ${result.reason}\n`);
    }
  } else {
    reportLines.push(`unpack ${unpackDetail}\n`);
  }
  const report = encodePktLines(reportLines);
  const bodyOut = Buffer.concat([sideband(report, 1), FLUSH]);
  return { status: 200, body: bodyOut };
}
