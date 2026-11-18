// git.go — Git smart HTTP (protocol v0 + v2) over the in-memory forge store.
//
// Faithful port of git.ts: handleGitHttp, handleLsRefsV2, handleFetchV2,
// handleFetchV0, handleReceivePack, plus the fetch planning helpers
// (collectReachable / commitDistances / collectHeld / planFetch).
// Packfile work lives in pack.go.
package main

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// urlPathUnescape decodes one path segment the way TS decodeURIComponent
// does ('+' stays '+').
func urlPathUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}

const gitAgent = "makefun-forge/1.0"

// ------------------------------------------------------------------
// advertisements
// ------------------------------------------------------------------

type AdvertisedRef struct {
	Name         string
	Oid          string
	Peeled       string
	HasPeeled    bool
	SymrefTarget string
	Unborn       bool
}

func repoRefsForAdvertisement(s *Store, repo *RepoRec) []AdvertisedRef {
	refs := s.AllRefs(repo)
	out := make([]AdvertisedRef, 0, len(refs)+1)
	if tip, ok := repo.Branches[repo.DefaultBranch]; ok {
		out = append(out, AdvertisedRef{Name: "HEAD", Oid: tip, SymrefTarget: "refs/heads/" + repo.DefaultBranch})
	} else {
		out = append(out, AdvertisedRef{Name: "HEAD", Oid: ZeroOID, Unborn: true, SymrefTarget: "refs/heads/" + repo.DefaultBranch})
	}
	for _, r := range refs {
		entry := AdvertisedRef{Name: r.Name, Oid: r.Oid}
		if peeled := s.PeelToCommit(r.Oid); peeled != r.Oid {
			entry.Peeled = peeled
			entry.HasPeeled = true
		}
		out = append(out, entry)
	}
	return out
}

// advertisementV2 is the v2 capability advertisement body (no service preamble).
func advertisementV2() []byte {
	lines := []string{
		"version 2\n",
		"agent=" + gitAgent + "\n",
		"ls-refs=unborn\n",
		"fetch=shallow wait-for-done filter\n",
		"server-option\n",
		"object-format=sha1\n",
	}
	out := encodePktLineStrings(lines)
	return append(out, FlushPacket...)
}

func advertisementV0(refs []AdvertisedRef, service string, caps string) []byte {
	parts := [][]byte{
		mustPktLineString("# service=" + service + "\n"),
		FlushPacket,
	}
	if len(refs) == 0 {
		parts = append(parts, mustPktLineString(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		for i, ref := range refs {
			capSuffix := ""
			if i == 0 {
				capSuffix = "\x00" + caps
			}
			parts = append(parts, mustPktLineString(ref.Oid+" "+ref.Name+capSuffix+"\n"))
			if ref.HasPeeled {
				parts = append(parts, mustPktLineString(ref.Peeled+" "+ref.Name+"^{}\n"))
			}
		}
	}
	parts = append(parts, FlushPacket)
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

const v0UploadCaps = "symref=HEAD:refs/heads/main agent=" + gitAgent + " side-band-64k thin-pack ofs-delta multi_ack_detailed shallow"
const v0ReceiveCaps = "symref=HEAD:refs/heads/main agent=" + gitAgent + " report-status side-band-64k delete-refs ofs-delta"

// advertisementV0Upload: v0 upload-pack advertisement (service preamble + refs).
func advertisementV0Upload(refs []AdvertisedRef) []byte {
	return advertisementV0(refs, "git-upload-pack", v0UploadCaps)
}

// advertisementV0Receive: v0 receive-pack advertisement (service preamble + refs).
func advertisementV0Receive(refs []AdvertisedRef) []byte {
	return advertisementV0(refs, "git-receive-pack", v0ReceiveCaps)
}

// ------------------------------------------------------------------
// ls-refs (v2)
// ------------------------------------------------------------------

type LsRefsRequest struct {
	Peel        bool
	Symrefs     bool
	Unborn      bool
	RefPrefixes []string
}

func handleLsRefsV2(s *Store, repo *RepoRec, req LsRefsRequest) []byte {
	refs := repoRefsForAdvertisement(s, repo)
	var lines [][]byte
	for _, ref := range refs {
		if len(req.RefPrefixes) > 0 {
			matched := false
			for _, p := range req.RefPrefixes {
				if strings.HasPrefix(ref.Name, p) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if ref.Unborn {
			if !req.Unborn {
				continue
			}
			line := "unborn " + ref.Name
			if req.Symrefs && ref.SymrefTarget != "" {
				line += " symref-target:" + ref.SymrefTarget
			}
			lines = append(lines, mustPktLineString(line+"\n"))
			continue
		}
		line := ref.Oid + " " + ref.Name
		if req.Symrefs && ref.SymrefTarget != "" {
			line += " symref-target:" + ref.SymrefTarget
		}
		if req.Peel && ref.HasPeeled {
			line += " peeled:" + ref.Peeled
		}
		lines = append(lines, mustPktLineString(line+"\n"))
	}
	var out []byte
	for _, l := range lines {
		out = append(out, l...)
	}
	return append(out, FlushPacket...)
}

// ------------------------------------------------------------------
// fetch planning
// ------------------------------------------------------------------

type FetchRequest struct {
	Wants   []string
	Haves   []string
	Shallow []string
	Depth   *int
	Deepen  *int
}

type FetchPlan struct {
	Entries   []PackEntry
	Shallow   []string
	Unshallow []string
}

// collectReachable collects all objects reachable from roots
// (commits -> trees -> blobs, tags -> targets), stopping at stopAt.
func collectReachable(s *Store, roots []string, stopAt map[string]bool) (map[string]PackEntry, map[string]bool) {
	objects := make(map[string]PackEntry)
	commits := make(map[string]bool)
	var visit func(oid string)
	visit = func(oid string) {
		if _, ok := objects[oid]; ok || stopAt[oid] {
			return
		}
		obj, ok := s.GetObject(oid)
		if !ok {
			return
		}
		objects[oid] = PackEntry{Type: obj.Type, Data: obj.Data}
		switch obj.Type {
		case "commit":
			commits[oid] = true
			parsed := ParseCommit(obj.Data)
			visit(parsed.Tree)
			for _, p := range parsed.Parents {
				visit(p)
			}
		case "tree":
			for _, e := range ParseTree(obj.Data) {
				visit(e.Oid)
			}
		case "tag":
			visit(ParseTag(obj.Data).Object)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return objects, commits
}

// commitDistances returns commit generation distances from roots (0 = root),
// via parents, plus the BFS discovery order of the commits.
func commitDistances(s *Store, roots []string) (map[string]int, []string) {
	dist := make(map[string]int)
	var order []string
	queue := make([]string, 0)
	for _, r := range roots {
		if _, ok := dist[r]; !ok {
			dist[r] = 0
			queue = append(queue, r)
			order = append(order, r)
		}
	}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		d := dist[oid]
		obj, ok := s.GetObject(oid)
		if !ok || obj.Type != "commit" {
			continue
		}
		for _, p := range ParseCommit(obj.Data).Parents {
			if _, ok := dist[p]; !ok {
				dist[p] = d + 1
				queue = append(queue, p)
				order = append(order, p)
			}
		}
	}
	return dist, order
}

// collectHeld is the set of objects a fetch client already holds, from its
// haves plus its current shallow boundary. The walk stops at a shallow
// commit's parents: a shallow clone holds the boundary commit, its tree and
// its blobs, but NOT the history below it.
func collectHeld(s *Store, haves []string, shallow []string) map[string]PackEntry {
	held := make(map[string]PackEntry)
	shallowSet := make(map[string]bool, len(shallow))
	for _, o := range shallow {
		shallowSet[o] = true
	}
	var visit func(oid string)
	visit = func(oid string) {
		if _, ok := held[oid]; ok {
			return
		}
		obj, ok := s.GetObject(oid)
		if !ok {
			return
		}
		held[oid] = PackEntry{Type: obj.Type, Data: obj.Data}
		switch obj.Type {
		case "commit":
			parsed := ParseCommit(obj.Data)
			visit(parsed.Tree)
			if !shallowSet[oid] {
				for _, p := range parsed.Parents {
					visit(p)
				}
			}
		case "tree":
			for _, e := range ParseTree(obj.Data) {
				visit(e.Oid)
			}
		case "tag":
			visit(ParseTag(obj.Data).Object)
		}
	}
	for _, have := range haves {
		visit(have)
	}
	return held
}

// planFetch plans a fetch: wants are the wanted tips, haves are objects the
// client already holds, depth (nil = full) limits generations, oldShallow is
// the client's current shallow boundary set.
func planFetch(s *Store, wants, haves []string, depth *int, oldShallow []string) FetchPlan {
	haveReachable := collectHeld(s, haves, oldShallow)
	var shallow []string
	var unshallow []string
	var entries []PackEntry
	if depth == nil {
		stopAt := make(map[string]bool, len(haveReachable))
		for k := range haveReachable {
			stopAt[k] = true
		}
		objects, _ := collectReachable(s, wants, stopAt)
		entries = make([]PackEntry, 0, len(objects))
		for _, e := range objects {
			entries = append(entries, e)
		}
	} else {
		dist, order := commitDistances(s, wants)
		included := make(map[string]bool)
		for oid, d := range dist {
			if d < *depth {
				included[oid] = true
			}
		}
		// Included commits, in BFS discovery order (matches TS Set order).
		var includedCommits []string
		includedSet := make(map[string]bool)
		for _, oid := range order {
			if !included[oid] {
				continue
			}
			obj, ok := s.GetObject(oid)
			if ok && obj.Type == "commit" {
				includedCommits = append(includedCommits, oid)
				includedSet[oid] = true
			}
		}
		// Boundary commits: deepest included commits whose parents are not
		// included. Shallow = boundary commits not already held.
		for _, oid := range includedCommits {
			obj, _ := s.GetObject(oid)
			parents := ParseCommit(obj.Data).Parents
			boundary := false
			if len(parents) > 0 {
				for _, p := range parents {
					if !included[p] {
						boundary = true
						break
					}
				}
			}
			if boundary {
				if _, held := haveReachable[oid]; !held {
					shallow = append(shallow, oid)
				}
			}
		}
		// Unshallow = old shallow commits that are now fully included.
		for _, oid := range oldShallow {
			if !includedSet[oid] {
				continue
			}
			obj, ok := s.GetObject(oid)
			if !ok || obj.Type != "commit" {
				continue
			}
			parents := ParseCommit(obj.Data).Parents
			allIncluded := true
			for _, p := range parents {
				if !included[p] {
					allIncluded = false
					break
				}
			}
			if allIncluded {
				unshallow = append(unshallow, oid)
			}
		}
		// Objects: included commits + their trees/blobs, minus held.
		held := make(map[string]bool, len(haveReachable))
		for k := range haveReachable {
			held[k] = true
		}
		filtered := make(map[string]PackEntry)
		var walkTree func(oid string)
		walkTree = func(oid string) {
			if held[oid] {
				return
			}
			if _, ok := filtered[oid]; ok {
				return
			}
			obj, ok := s.GetObject(oid)
			if !ok || obj.Type != "tree" {
				return
			}
			filtered[oid] = PackEntry{Type: "tree", Data: obj.Data}
			for _, e := range ParseTree(obj.Data) {
				if e.Mode == "40000" {
					walkTree(e.Oid)
				} else if !held[e.Oid] {
					if _, ok := filtered[e.Oid]; !ok {
						if blob, ok := s.GetObject(e.Oid); ok {
							filtered[e.Oid] = PackEntry{Type: blob.Type, Data: blob.Data}
						}
					}
				}
			}
		}
		for _, oid := range includedCommits {
			obj, ok := s.GetObject(oid)
			if !ok {
				continue
			}
			if !held[oid] {
				if _, ok := filtered[oid]; !ok {
					filtered[oid] = PackEntry{Type: "commit", Data: obj.Data}
				}
			}
			walkTree(ParseCommit(obj.Data).Tree)
		}
		entries = make([]PackEntry, 0, len(filtered))
		for _, e := range filtered {
			entries = append(entries, e)
		}
	}
	// Deterministic order: commits first, then tags, trees, blobs — sorted
	// by oid within each type.
	typeRank := map[string]int{"commit": 0, "tag": 1, "tree": 2, "blob": 3}
	type ranked struct {
		entry PackEntry
		oid   string
	}
	rankedEntries := make([]ranked, len(entries))
	for i, e := range entries {
		rankedEntries[i] = ranked{entry: e, oid: objectSha(e.Type, e.Data)}
	}
	sort.Slice(rankedEntries, func(i, j int) bool {
		ri, rj := typeRank[rankedEntries[i].entry.Type], typeRank[rankedEntries[j].entry.Type]
		if ri != rj {
			return ri < rj
		}
		return rankedEntries[i].oid < rankedEntries[j].oid
	})
	entries = entries[:0]
	for _, r := range rankedEntries {
		entries = append(entries, r.entry)
	}
	return FetchPlan{Entries: entries, Shallow: shallow, Unshallow: unshallow}
}

// resolveFetchPlan: depth wins over deepen; deepen is relative to haves.
func resolveFetchPlan(s *Store, req FetchRequest) FetchPlan {
	depth := req.Depth
	if depth == nil {
		depth = req.Deepen
	}
	return planFetch(s, req.Wants, req.Haves, depth, req.Shallow)
}

// ------------------------------------------------------------------
// fetch response cache
// ------------------------------------------------------------------

// The key covers everything a fetch response depends on: the repo, the
// protocol version, the wants, the haves, the depth and the client's
// shallow set. Git objects are immutable and content addressed, and a new
// object only becomes reachable through a ref move — which changes the
// wanted tip and therefore the key — so no explicit invalidation is needed.
var fetchCache = make(map[string][]byte)

const fetchCacheMax = 32

func fetchCacheKey(repo *RepoRec, protocol string, req FetchRequest) string {
	sorted := func(in []string) string {
		cp := append([]string(nil), in...)
		sort.Strings(cp)
		return strings.Join(cp, ",")
	}
	depth := ""
	if req.Depth != nil {
		depth = itoa(*req.Depth)
	}
	deepen := ""
	if req.Deepen != nil {
		deepen = itoa(*req.Deepen)
	}
	return strings.Join([]string{
		itoa(repo.ID),
		protocol,
		sorted(req.Wants),
		sorted(req.Haves),
		depth,
		deepen,
		sorted(req.Shallow),
	}, "|")
}

func fetchCacheSet(key string, value []byte) {
	if len(fetchCache) >= fetchCacheMax {
		fetchCache = make(map[string][]byte)
	}
	fetchCache[key] = value
}

func handleFetchV2(s *Store, repo *RepoRec, req FetchRequest) []byte {
	key := fetchCacheKey(repo, "v2", req)
	if hit, ok := fetchCache[key]; ok {
		return hit
	}
	out := buildFetchV2(s, repo, req)
	fetchCacheSet(key, out)
	return out
}

func buildFetchV2(s *Store, repo *RepoRec, req FetchRequest) []byte {
	plan := resolveFetchPlan(s, req)
	pack := WritePack(plan.Entries)
	var parts [][]byte
	// The acknowledgments section is only sent when the client sent 'have'
	// lines: a client that sent no haves expects the response to start with
	// shallow-info or packfile, and real git aborts otherwise. `ready`
	// closes the section: negotiation is always over after the acks.
	if len(req.Haves) > 0 {
		parts = append(parts, mustPktLineString("acknowledgments\n"))
		acked := false
		for _, have := range req.Haves {
			for _, want := range req.Wants {
				if s.IsAncestor(have, want) {
					parts = append(parts, mustPktLineString("ACK "+have+"\n"))
					acked = true
					break
				}
			}
		}
		if !acked {
			parts = append(parts, mustPktLineString("NAK\n"))
		}
		parts = append(parts, mustPktLineString("ready\n"))
		parts = append(parts, DelimPacket)
	}
	// shallow-info
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		parts = append(parts, mustPktLineString("shallow-info\n"))
		for _, oid := range plan.Shallow {
			parts = append(parts, mustPktLineString("shallow "+oid+"\n"))
		}
		for _, oid := range plan.Unshallow {
			parts = append(parts, mustPktLineString("unshallow "+oid+"\n"))
		}
		parts = append(parts, DelimPacket)
	}
	// packfile
	parts = append(parts, mustPktLineString("packfile\n"))
	parts = append(parts, Sideband(pack, 1, 32768))
	parts = append(parts, FlushPacket)
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func handleFetchV0(s *Store, repo *RepoRec, req FetchRequest) []byte {
	key := fetchCacheKey(repo, "v0", req)
	if hit, ok := fetchCache[key]; ok {
		return hit
	}
	out := buildFetchV0(s, repo, req)
	fetchCacheSet(key, out)
	return out
}

func buildFetchV0(s *Store, repo *RepoRec, req FetchRequest) []byte {
	plan := resolveFetchPlan(s, req)
	pack := WritePack(plan.Entries)
	var parts [][]byte
	parts = append(parts, mustPktLineString("NAK\n"))
	for _, oid := range plan.Shallow {
		parts = append(parts, mustPktLineString("shallow "+oid+"\n"))
	}
	for _, oid := range plan.Unshallow {
		parts = append(parts, mustPktLineString("unshallow "+oid+"\n"))
	}
	parts = append(parts, Sideband(pack, 1, 32768))
	parts = append(parts, FlushPacket)
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ------------------------------------------------------------------
// HTTP glue (routing, auth, body)
// ------------------------------------------------------------------

const jsonContentType = "application/json; charset=utf-8"

func gitHeaders(status int, contentType string, bodyLen int, reqID string, extra ...[2]string) [][2]string {
	h := [][2]string{
		{"content-type", contentType},
		{"content-length", itoa(bodyLen)},
		{"x-github-request-id", reqID},
	}
	return append(h, extra...)
}

// sendGitBuffer mirrors TS sendBuffer: ordered headers, body as given.
func sendGitBuffer(status int, contentType string, body []byte, reqID string, extra ...[2]string) *Response {
	return &Response{Status: status, Header: gitHeaders(status, contentType, len(body), reqID, extra...), Body: body}
}

// sendJSONGit mirrors TS sendJsonGit: JSON body with NO trailing newline
// (unlike sendJson, which appends '\n').
func sendJSONGit(status int, v any, reqID string) *Response {
	body := marshalJSON(v)
	return sendGitBuffer(status, jsonContentType, body, reqID)
}

func gitAuthorized(req *Request, token string) bool {
	if token == "" {
		return true
	}
	return req.Header("authorization") == "Bearer "+token
}

type gitPath struct {
	Owner string
	Repo  string
	Tail  string
}

func parseGitPath(path string) *gitPath {
	parts := make([]string, 0)
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 3 {
		return nil
	}
	owner, repoGit := parts[0], parts[1]
	tail := parts[2:]
	if !strings.HasSuffix(repoGit, ".git") {
		return nil
	}
	repo, err := urlPathUnescape(repoGit[:len(repoGit)-4])
	if err != nil {
		return nil
	}
	ownerDecoded, err := urlPathUnescape(owner)
	if err != nil {
		return nil
	}
	return &gitPath{Owner: ownerDecoded, Repo: repo, Tail: strings.Join(tail, "/")}
}

type uploadPackRequestV2 struct {
	kind   string // "ls-refs" | "fetch"
	lsRefs *LsRefsRequest
	fetch  *FetchRequest
}

func parseUploadPackBody(body []byte, isV2 bool) (*uploadPackRequestV2, error) {
	packets, err := DecodePktLines(body)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, pkt := range packets {
		if pkt.Kind == PktData {
			lines = append(lines, string(pkt.Data))
		}
	}
	if isV2 {
		first := ""
		if len(lines) > 0 {
			first = strings.TrimSpace(lines[0])
		}
		if first == "command=ls-refs" {
			req := &LsRefsRequest{}
			for _, line := range lines[1:] {
				t := strings.TrimSpace(line)
				switch {
				case t == "peel":
					req.Peel = true
				case t == "symrefs":
					req.Symrefs = true
				case t == "unborn":
					req.Unborn = true
				case strings.HasPrefix(t, "ref-prefix "):
					req.RefPrefixes = append(req.RefPrefixes, t[len("ref-prefix "):])
				}
			}
			return &uploadPackRequestV2{kind: "ls-refs", lsRefs: req}, nil
		}
		if first == "command=fetch" {
			return &uploadPackRequestV2{kind: "fetch", fetch: parseFetchArgs(lines[1:])}, nil
		}
		return nil, errUnknownCommand(first)
	}
	return &uploadPackRequestV2{kind: "fetch", fetch: parseFetchArgs(lines)}, nil
}

func errUnknownCommand(first string) error {
	return &unknownCommandError{first: first}
}

type unknownCommandError struct{ first string }

func (e *unknownCommandError) Error() string { return "unknown v2 command " + e.first }

func parseFetchArgs(lines []string) *FetchRequest {
	req := &FetchRequest{}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "want "):
			req.Wants = append(req.Wants, strings.Split(line[5:], " ")[0])
		case strings.HasPrefix(line, "have "):
			req.Haves = append(req.Haves, strings.Split(line[5:], " ")[0])
		case strings.HasPrefix(line, "shallow "):
			req.Shallow = append(req.Shallow, strings.Split(line[8:], " ")[0])
		case strings.HasPrefix(line, "deepen "):
			if n, ok := parseNonNegativeInt(strings.TrimSpace(line[7:])); ok {
				req.Deepen = &n
			}
		case strings.HasPrefix(line, "deepen-not "):
			// Not used by the suite; ignored.
		case strings.HasPrefix(line, "depth "):
			if n, ok := parseNonNegativeInt(strings.TrimSpace(line[6:])); ok {
				req.Depth = &n
			}
		}
	}
	return req
}

// parseNonNegativeInt parses a decimal integer the way TS parseInt does for
// the deepen/depth lines: leading digits, finite. Returns false on no digits.
func parseNonNegativeInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	i := 0
	neg := false
	if s[0] == '-' {
		neg = true
		i = 1
	} else if s[0] == '+' {
		i = 1
	}
	n := 0
	digits := 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// HandleGit handles one git smart-HTTP request. It returns (handled, resp):
// handled=false means the path is not a git path and the dispatcher should
// fall through to 404.
func HandleGit(req *Request, s *Store, token string) (bool, *Response) {
	parsed := parseGitPath(req.Path)
	if parsed == nil {
		return false, nil
	}
	if parsed.Tail != "info/refs" && parsed.Tail != "git-upload-pack" && parsed.Tail != "git-receive-pack" {
		return false, nil
	}
	if !gitAuthorized(req, token) {
		return true, sendJSONGit(401, map[string]string{"message": "Bad credentials"}, req.ID)
	}
	repo, ok := s.FindRepo(parsed.Owner, parsed.Repo)
	if !ok {
		return true, sendJSONGit(404, map[string]string{"message": "Not Found"}, req.ID)
	}
	refs := repoRefsForAdvertisement(s, repo)
	if parsed.Tail == "info/refs" {
		if req.Method != "GET" {
			return true, sendJSONGit(405, map[string]string{"message": "Method Not Allowed"}, req.ID)
		}
		service := req.Query.Get("service")
		if service == "git-upload-pack" {
			isV2 := req.Header("git-protocol") == "version=2"
			var body []byte
			if isV2 {
				body = advertisementV2()
			} else {
				body = advertisementV0Upload(refs)
			}
			return true, sendGitBuffer(200, "application/x-git-upload-pack-advertisement", body, req.ID)
		}
		if service == "git-receive-pack" {
			body := advertisementV0Receive(refs)
			return true, sendGitBuffer(200, "application/x-git-receive-pack-advertisement", body, req.ID)
		}
		return true, sendJSONGit(400, map[string]string{"message": "Missing service parameter"}, req.ID)
	}
	// POST endpoints.
	if req.Method != "POST" {
		return true, sendJSONGit(405, map[string]string{"message": "Method Not Allowed"}, req.ID)
	}
	// The transport has already read the full body (Content-Length or
	// chunked); a read failure cannot surface here, so the TS 400
	// 'Could not read request body' branch is unreachable in Go.
	body := req.Body
	if parsed.Tail == "git-upload-pack" {
		isV2 := req.Header("git-protocol") == "version=2"
		parsedReq, err := parseUploadPackBody(body, isV2)
		if err != nil {
			return true, sendGitBuffer(400, "text/plain; charset=utf-8", []byte("malformed pkt-line\n"), req.ID)
		}
		var out []byte
		if parsedReq.kind == "ls-refs" {
			out = handleLsRefsV2(s, repo, *parsedReq.lsRefs)
		} else if isV2 {
			out = handleFetchV2(s, repo, *parsedReq.fetch)
		} else {
			out = handleFetchV0(s, repo, *parsedReq.fetch)
		}
		return true, sendGitBuffer(200, "application/x-git-upload-pack-result", out, req.ID)
	}
	// git-receive-pack
	status, resultBody := handleReceivePack(s, repo, body)
	// Real git reads the report-status response to EOF and treats a
	// kept-alive connection after a complete report as a hangup, so close
	// this response's connection explicitly — but only for real git
	// (User-Agent `git/...`). Other clients reuse connections.
	var extra [][2]string
	if strings.HasPrefix(req.Header("user-agent"), "git/") {
		extra = append(extra, [2]string{"connection", "close"})
	}
	return true, sendGitBuffer(status, "application/x-git-receive-pack-result", resultBody, req.ID, extra...)
}

type PushCommand struct {
	OldOid string
	NewOid string
	Ref    string
}

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// handleReceivePack handles a receive-pack request body. It returns the
// response status and body (sideband report). It never panics on malformed
// input — errors become a 400 response.
func handleReceivePack(s *Store, repo *RepoRec, body []byte) (int, []byte) {
	packets, rest, err := SplitPktAndRest(body)
	if err != nil {
		return 400, []byte("malformed pkt-line\n")
	}
	var commands []PushCommand
	for _, pkt := range packets {
		if pkt.Kind != PktData {
			continue
		}
		line := string(pkt.Data)
		if nul := strings.IndexByte(line, 0); nul != -1 {
			line = line[:nul]
		}
		parts := strings.Split(strings.TrimSpace(line), " ")
		if len(parts) < 3 {
			return 400, []byte("malformed ref command\n")
		}
		oldOid, newOid, ref := parts[0], parts[1], parts[2]
		if !oidRe.MatchString(oldOid) || !oidRe.MatchString(newOid) {
			return 400, []byte("malformed ref command\n")
		}
		commands = append(commands, PushCommand{OldOid: oldOid, NewOid: newOid, Ref: ref})
	}
	// Unpack the pack (if any) before applying ref updates.
	unpackOk := true
	unpackDetail := "ok"
	if len(rest) > 0 {
		entries, err := ReadPack(rest, func(oid string) (*GitObject, bool) {
			return s.GetObject(oid)
		})
		if err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		} else {
			for _, e := range entries {
				s.RegisterObject(e.Type, e.Data)
			}
		}
	}
	var reportLines []string
	if unpackOk {
		reportLines = append(reportLines, "unpack ok\n")
		for _, cmd := range commands {
			ok, reason := s.ApplyRefUpdate(repo, cmd.Ref, cmd.OldOid, cmd.NewOid)
			if ok {
				reportLines = append(reportLines, "ok "+cmd.Ref+"\n")
			} else {
				reportLines = append(reportLines, "ng "+cmd.Ref+" "+reason+"\n")
			}
		}
	} else {
		reportLines = append(reportLines, "unpack "+unpackDetail+"\n")
	}
	report := encodePktLineStrings(reportLines)
	// The report ends with a flush packet INSIDE the band-1 payload, then a
	// second flush terminating the sideband stream itself.
	reportWithFlush := append(report, FlushPacket...)
	bodyOut := append(Sideband(reportWithFlush, 1, 32768), FlushPacket...)
	return 200, bodyOut
}
