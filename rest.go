// rest.go — GitHub REST v3 surface under /api/v3.
// Faithful port of github-forge-v6/rest.ts (handleRest + handleRepoRoute).
// Auth first, body-parse errors, routes, pagination, Link header, JSON field
// order (structs, never maps for fixed shapes), and the cached-issue hot path.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Body is a decoded JSON object body (numbers kept as json.Number so that
// 1e999 behaves like JS JSON.parse: Infinity, not a decode error).
type Body map[string]any

func strVal(v any, fallback ...string) string {
	fb := ""
	if len(fallback) > 0 {
		fb = fallback[0]
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fb
}

func boolVal(v any, fallback ...bool) bool {
	fb := false
	if len(fallback) > 0 {
		fb = fallback[0]
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return fb
}

// parseBody mirrors TS readBody: empty/whitespace-only body -> nil body, no
// error; malformed JSON -> 400 'Request body is not valid JSON'; valid JSON
// that is not an object (or is an array) -> 400 'Request body must be a JSON
// object'. A JSON null is not an object either (TS checks parsed === null).
func parseBody(body []byte) (Body, *ApiError) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, NewApiError(400, "Request body is not valid JSON")
	}
	// Trailing garbage after one JSON value: TS JSON.parse throws.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, NewApiError(400, "Request body is not valid JSON")
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, NewApiError(400, "Request body must be a JSON object")
	}
	return obj, nil
}

// dec mirrors TS decodeURIComponent per path segment: on a malformed escape
// the raw segment is kept. ('+' stays '+', unlike QueryUnescape.)
func dec(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// jsWhitespace is the JS WhiteSpace + LineTerminator set trimmed by Number()
// and String.prototype.trim.
const jsWhitespace = " \t\n\v\f\r\u00a0\ufeff\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

var jsDecimalRe = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// jsNumber mirrors JS Number(s) for strings: trims JS whitespace, then
// accepts decimal (with exponent), 0x hex, 0b binary, 0o octal, and
// +/-Infinity. Anything else is NaN (ok=false). Notable: "0x10" -> 16,
// " 12 " -> 12, "" -> 0, "1_000" -> NaN, "infinity" -> NaN.
func jsNumber(s string) (float64, bool) {
	t := strings.Trim(s, jsWhitespace)
	if t == "" {
		return 0, true
	}
	switch t {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	rest := t
	sign := 1.0
	if rest[0] == '+' {
		rest = rest[1:]
	} else if rest[0] == '-' {
		sign = -1
		rest = rest[1:]
	}
	if len(rest) >= 2 && rest[0] == '0' {
		switch rest[1] {
		case 'x', 'X':
			v, ok := jsIntBase(rest[2:], 16)
			return sign * v, ok
		case 'b', 'B':
			v, ok := jsIntBase(rest[2:], 2)
			return sign * v, ok
		case 'o', 'O':
			v, ok := jsIntBase(rest[2:], 8)
			return sign * v, ok
		}
	}
	if !jsDecimalRe.MatchString(t) {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			// 1e999 etc: JS yields +/-Infinity.
			return f, true
		}
		return 0, false
	}
	return f, true
}

// jsIntBase parses a run of digits in the given base; ok=false when empty or
// any digit is out of range. Accumulates in float64 so huge literals saturate
// to +/-Inf the way JS does instead of erroring.
func jsIntBase(s string, base float64) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v := 0.0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d float64
		switch {
		case c >= '0' && c <= '9':
			d = float64(c - '0')
		case c >= 'a' && c <= 'f':
			d = float64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = float64(c-'A') + 10
		default:
			return 0, false
		}
		if d >= base {
			return 0, false
		}
		v = v*base + d
	}
	return v, true
}

// jsNumStr renders a float the way JS String(number) does, for the small
// values that appear in pagination (page/per_page/lastPage). Integers print
// plain; non-integers use shortest-round-trip form.
func jsNumStr(f float64) string {
	if f == math.Trunc(f) && !math.IsInf(f, 0) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// --- pagination (mirrors TS paginate + linkHeader) ---

// queryNum mirrors TS num(query.get(k) === null ? NaN : Number(...), fb) || fb:
// absent/non-numeric/non-finite -> fb; 0 -> fb (the || step).
func queryNum(q url.Values, key string, fb float64) float64 {
	vals, ok := q[key]
	if !ok || len(vals) == 0 {
		return fb
	}
	f, isNum := jsNumber(vals[0])
	if !isNum || math.IsInf(f, 0) {
		return fb
	}
	if f == 0 {
		return fb
	}
	return f
}

type pageRange struct {
	start, end int
	pageNum    float64
	perPage    float64
	total      int
}

func paginate(total int, q url.Values) pageRange {
	perPage := math.Max(1, math.Min(100, queryNum(q, "per_page", 30)))
	pageNum := math.Max(1, queryNum(q, "page", 1))
	startF := (pageNum - 1) * perPage
	if startF >= float64(total) {
		return pageRange{start: total, end: total, pageNum: pageNum, perPage: perPage, total: total}
	}
	start := int(startF)
	end := int(startF + perPage)
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}
	return pageRange{start: start, end: end, pageNum: pageNum, perPage: perPage, total: total}
}

// parseQueryOrdered parses a raw query string preserving pair order (and
// duplicates), decoding '+' as space like URLSearchParams does. A malformed
// % escape is left as-is, matching the URL parser's leniency.
func parseQueryOrdered(raw string) [][2]string {
	if raw == "" {
		return nil
	}
	var out [][2]string
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if u, err := url.QueryUnescape(k); err == nil {
			k = u
		}
		if u, err := url.QueryUnescape(v); err == nil {
			v = u
		}
		out = append(out, [2]string{k, v})
	}
	return out
}

// setQueryParam mirrors URLSearchParams.set: replace the first pair with this
// key (dropping later duplicates), or append. Returns a fresh slice.
func setQueryParam(pairs [][2]string, key, value string) [][2]string {
	out := make([][2]string, 0, len(pairs)+1)
	done := false
	for _, p := range pairs {
		if p[0] == key {
			if done {
				continue
			}
			out = append(out, [2]string{key, value})
			done = true
			continue
		}
		out = append(out, p)
	}
	if !done {
		out = append(out, [2]string{key, value})
	}
	return out
}

// formEncode mirrors the application/x-www-form-urlencoded serializer used by
// URLSearchParams.toString: alphanumerics and *-._ stay, space becomes '+',
// everything else is %XX (uppercase hex).
func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

func serializeQuery(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, formEncode(p[0])+"="+formEncode(p[1]))
	}
	return strings.Join(parts, "&")
}

// linkHeader mirrors TS linkHeader: null (here "") when on/after the last
// page; otherwise next+last links with per_page then page set, in insertion
// order — built by hand, never url.Values.Encode (it sorts keys).
func linkHeader(req *Request, basePath string, pg pageRange) string {
	lastPage := math.Max(1, math.Ceil(float64(pg.total)/pg.perPage))
	if pg.pageNum >= lastPage {
		return ""
	}
	host := req.Header("host")
	if host == "" {
		host = "localhost"
	}
	base := setQueryParam(parseQueryOrdered(req.RawQuery), "per_page", jsNumStr(pg.perPage))
	urlFor := func(page float64) string {
		q := setQueryParam(base, "page", jsNumStr(page))
		return "http://" + host + basePath + "?" + serializeQuery(q)
	}
	return "<" + urlFor(pg.pageNum+1) + ">; rel=\"next\", <" + urlFor(lastPage) + ">; rel=\"last\""
}

// --- response shapes (field order = TS object literal order) ---

type rootResp struct {
	CurrentUserURL string `json:"current_user_url"`
	RateLimitURL   string `json:"rate_limit_url"`
}

type metaResp struct {
	InstalledVersion string `json:"installed_version"`
}

type userResp struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type branchResp struct {
	Name   string `json:"name"`
	Commit struct {
		Sha string `json:"sha"`
	} `json:"commit"`
}

type contentFileResp struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	Sha      string `json:"sha"`
}

type contentInner struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Sha      string `json:"sha"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding"`
}

type commitInner struct {
	Sha string `json:"sha"`
}

type putContentResp struct {
	Content contentInner `json:"content"`
	Commit  commitInner  `json:"commit"`
}

type deleteContentResp struct {
	Commit commitInner `json:"commit"`
}

type refObject struct {
	Sha  string `json:"sha"`
	Type string `json:"type"`
}

type refResp struct {
	Ref    string    `json:"ref"`
	Object refObject `json:"object"`
}

type labelResp struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type commentResp struct {
	ID        int    `json:"id"`
	Body      string `json:"body"`
	User      User   `json:"user"`
	CreatedAt string `json:"created_at"`
}

type mergeResp struct {
	Sha     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// asLabels mirrors TS asLabels: strings stay; objects contribute .name when
// it is a string; anything else is skipped; non-arrays -> [].
func asLabels(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, item := range arr {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if name, ok := t["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func needRepo(s *Store, owner, name, reqID string) (*RepoRec, *Response) {
	repo, ok := s.FindRepo(owner, name)
	if !ok {
		return nil, ErrJSON(404, "Repository "+owner+"/"+name+" not found", reqID)
	}
	return repo, nil
}

// apiErr converts a store *ApiError into its response (nil -> nil).
func apiErr(err *ApiError, reqID string) *Response {
	if err == nil {
		return nil
	}
	return ErrJSON(err.Status, err.Message, reqID)
}

// nonNil returns xs or an empty slice so lists marshal as [] not null.
func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// HandleRest handles one /api/v3 request. req.Path is the pathname after
// /api/v3 (” or '/' = root). Mirrors TS handleRest.
func HandleRest(req *Request, s *Store, token string) (resp *Response) {
	defer func() {
		if r := recover(); r != nil {
			if ae, ok := r.(*ApiError); ok {
				resp = ErrJSON(ae.Status, ae.Message, req.ID)
				return
			}
			resp = ErrJSON(500, "Internal Server Error", req.ID)
		}
	}()
	return handleRestInner(req, s, token)
}

func handleRestInner(req *Request, s *Store, token string) *Response {
	reqID := req.ID
	method := req.Method

	// Auth: every /api/v3 route needs the exact bearer token (when configured).
	if token != "" {
		if req.Header("authorization") != "Bearer "+token {
			return ErrJSON(401, "Bad credentials", reqID)
		}
	}

	// GET/HEAD skip the body entirely; other methods parse it.
	var jsonBody Body
	if method != "GET" && method != "HEAD" {
		body, aerr := parseBody(req.Body)
		if aerr != nil {
			return ErrJSON(aerr.Status, aerr.Message, reqID)
		}
		jsonBody = body
	}
	if jsonBody == nil {
		jsonBody = Body{}
	}

	segs := []string{}
	for _, seg := range strings.Split(req.Path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}

	// GET /api/v3/ — root
	if len(segs) == 0 {
		if method != "GET" {
			return ErrJSON(404, "Not Found", reqID)
		}
		return SendJSON(200, rootResp{CurrentUserURL: "/api/v3/user", RateLimitURL: "/api/v3/rate_limit"}, reqID, [2]string{"x-oauth-scopes", "repo, read:org"})
	}

	// GET /api/v3/meta
	if len(segs) == 1 && segs[0] == "meta" {
		if method != "GET" {
			return ErrJSON(404, "Not Found", reqID)
		}
		return SendJSON(200, metaResp{InstalledVersion: "1.0.0"}, reqID)
	}

	// GET /api/v3/user — the token's login (learned); 404 while unknown.
	if len(segs) == 1 && segs[0] == "user" {
		if method != "GET" {
			return ErrJSON(404, "Not Found", reqID)
		}
		if s.ViewerLogin == nil {
			return ErrJSON(404, "Not Found", reqID)
		}
		return SendJSON(200, userResp{Login: *s.ViewerLogin, ID: 1000, Type: "User"}, reqID)
	}

	// /api/v3/user/repos
	if len(segs) == 2 && segs[0] == "user" && segs[1] == "repos" {
		if method == "GET" {
			repos := s.ListRepos()
			pg := paginate(len(repos), req.Query)
			page := make([]RepoJSON, 0, pg.end-pg.start)
			for _, r := range repos[pg.start:pg.end] {
				page = append(page, s.RepoJSON(r))
			}
			var extra [][2]string
			if link := linkHeader(req, "/api/v3/user/repos", pg); link != "" {
				extra = append(extra, [2]string{"link", link})
			}
			return SendJSON(200, page, reqID, extra...)
		}
		if method == "POST" {
			repo, aerr := s.CreateRepo(strVal(jsonBody["name"]), boolVal(jsonBody["private"], false), strVal(jsonBody["description"]))
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(201, s.RepoJSON(repo), reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}

	// /api/v3/repos/{owner}/{repo}[...]
	if len(segs) >= 3 && segs[0] == "repos" {
		owner := dec(segs[1])
		name := dec(segs[2])
		rest := segs[3:]
		return handleRepoRoute(req, s, method, owner, name, rest, jsonBody, reqID)
	}

	return ErrJSON(404, "Not Found", reqID)
}

// handleRepoRoute mirrors TS handleRepoRoute: /repos/{owner}/{repo}[...].
func handleRepoRoute(req *Request, s *Store, method, owner, name string, rest []string, jsonBody Body, reqID string) *Response {
	// /repos/{o}/{r} itself
	if len(rest) == 0 {
		if method == "GET" {
			repo, resp := needRepo(s, owner, name, reqID)
			if resp != nil {
				return resp
			}
			return SendJSON(200, s.RepoJSON(repo), reqID)
		}
		if method == "PATCH" {
			repo, resp := needRepo(s, owner, name, reqID)
			if resp != nil {
				return resp
			}
			if _, present := jsonBody["description"]; present {
				repo.Description = strVal(jsonBody["description"])
			}
			repo.UpdatedAt = s.TrialNow
			return SendJSON(200, s.RepoJSON(repo), reqID)
		}
		if method == "DELETE" {
			repo, resp := needRepo(s, owner, name, reqID)
			if resp != nil {
				return resp
			}
			s.DeleteRepo(repo)
			return SendJSON(204, nil, reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}

	sub := rest[0]
	repo, resp := needRepo(s, owner, name, reqID)
	if resp != nil {
		return resp
	}

	// GET /branches, GET /branches/{branch}
	if sub == "branches" {
		if method == "GET" && len(rest) == 1 {
			return SendJSON(200, nonNil(s.ListBranches(repo)), reqID)
		}
		if method == "GET" && len(rest) == 2 {
			branch := dec(rest[1])
			tip, ok := repo.Branches[branch]
			if !ok {
				return ErrJSON(404, "No such branch "+branch, reqID)
			}
			var out branchResp
			out.Name = branch
			out.Commit.Sha = tip
			return SendJSON(200, out, reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}

	// GET /commits[?sha=]
	if sub == "commits" {
		if method == "GET" && len(rest) == 1 {
			commits, aerr := s.ListCommits(repo, req.Query.Get("sha"))
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(200, nonNil(commits), reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}

	// /contents/{path...}
	if sub == "contents" {
		return handleContents(req, s, repo, method, rest[1:], jsonBody, reqID)
	}

	// POST /git/refs, GET /git/refs/heads/{branch}
	if sub == "git" {
		return handleGitRefs(s, repo, method, rest, jsonBody, reqID)
	}

	// POST /labels, GET /labels
	if sub == "labels" {
		return handleLabels(s, repo, method, rest, jsonBody, reqID)
	}

	// /issues...
	if sub == "issues" {
		return handleIssues(s, repo, method, rest, req, jsonBody, reqID)
	}

	// /pulls...
	if sub == "pulls" {
		return handlePulls(s, repo, method, rest, req, jsonBody, reqID)
	}

	return ErrJSON(404, "Not Found", reqID)
}

// b64Len mirrors TS Buffer.from(str, 'base64').length for the contents PUT
// size field: strip the same [\r\n\t ] chars the store strips, then decode.
// (When the store accepted the content this decode succeeds.)
func b64Len(s string) int {
	clean := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, s)
	if b, err := base64.StdEncoding.DecodeString(clean); err == nil {
		return len(b)
	}
	if b, err := base64.RawStdEncoding.DecodeString(clean); err == nil {
		return len(b)
	}
	return 0
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func handleContents(req *Request, s *Store, repo *RepoRec, method string, rest []string, jsonBody Body, reqID string) *Response {
	decoded := make([]string, 0, len(rest))
	for _, seg := range rest {
		decoded = append(decoded, dec(seg))
	}
	filePath := strings.Join(decoded, "/")
	if method == "GET" {
		ref := req.Query.Get("ref")
		if filePath == "" {
			listing, aerr := s.ListDirectory(repo, "", ref)
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(200, nonNil(listing), reqID)
		}
		// A directory lists its entries; a file returns the file object.
		// A named path that is neither is a 404 (GET-after-DELETE relies on this).
		tree, aerr := s.ResolveTree(repo, ref)
		if aerr != nil {
			return apiErr(aerr, reqID)
		}
		if _, ok := tree[filePath]; !ok {
			listing, aerr := s.ListDirectory(repo, filePath, ref)
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			if len(listing) == 0 {
				return ErrJSON(404, "No such file or directory "+filePath, reqID)
			}
			return SendJSON(200, listing, reqID)
		}
		entry, aerr := s.GetContentEntry(repo, filePath, ref)
		if aerr != nil {
			return apiErr(aerr, reqID)
		}
		return SendJSON(200, contentFileResp{
			Type:     "file",
			Encoding: "base64",
			Size:     len(entry.Content),
			Name:     baseName(filePath),
			Path:     filePath,
			Content:  base64.StdEncoding.EncodeToString(entry.Content),
			Sha:      entry.BlobSha,
		}, reqID)
	}
	if method == "PUT" {
		if filePath == "" {
			return ErrJSON(404, "Not Found", reqID)
		}
		branch := strVal(jsonBody["branch"])
		sha := strVal(jsonBody["sha"])
		contentSha, commitSha, created, aerr := s.PutContent(repo, filePath, strVal(jsonBody["content"]), strVal(jsonBody["message"]), branch, sha)
		if aerr != nil {
			return apiErr(aerr, reqID)
		}
		status := 200
		if created {
			status = 201
		}
		return SendJSON(status, putContentResp{
			Content: contentInner{
				Name:     baseName(filePath),
				Path:     filePath,
				Sha:      contentSha,
				Size:     b64Len(strVal(jsonBody["content"])),
				Encoding: "base64",
			},
			Commit: commitInner{Sha: commitSha},
		}, reqID)
	}
	if method == "DELETE" {
		if filePath == "" {
			return ErrJSON(404, "Not Found", reqID)
		}
		branch := strVal(jsonBody["branch"])
		sha := strVal(jsonBody["sha"])
		commitSha, aerr := s.DeleteContent(repo, filePath, strVal(jsonBody["message"]), branch, sha)
		if aerr != nil {
			return apiErr(aerr, reqID)
		}
		return SendJSON(200, deleteContentResp{Commit: commitInner{Sha: commitSha}}, reqID)
	}
	return ErrJSON(404, "Not Found", reqID)
}

var gitRefsRe = regexp.MustCompile(`^refs/heads/(.+)$`)

func handleGitRefs(s *Store, repo *RepoRec, method string, rest []string, jsonBody Body, reqID string) *Response {
	if len(rest) == 2 && rest[1] == "refs" && method == "POST" {
		ref := strVal(jsonBody["ref"])
		sha := strVal(jsonBody["sha"])
		m := gitRefsRe.FindStringSubmatch(ref)
		if m == nil {
			return ErrJSON(422, "Invalid ref "+ref, reqID)
		}
		tip, aerr := s.CreateBranch(repo, m[1], sha)
		if aerr != nil {
			return apiErr(aerr, reqID)
		}
		return SendJSON(201, refResp{Ref: ref, Object: refObject{Sha: tip, Type: "commit"}}, reqID)
	}
	if len(rest) >= 4 && rest[1] == "refs" && rest[2] == "heads" && method == "GET" {
		decoded := make([]string, 0, len(rest)-3)
		for _, seg := range rest[3:] {
			decoded = append(decoded, dec(seg))
		}
		branch := strings.Join(decoded, "/")
		tip, ok := repo.Branches[branch]
		if !ok {
			return ErrJSON(404, "No such ref refs/heads/"+branch, reqID)
		}
		return SendJSON(200, refResp{Ref: "refs/heads/" + branch, Object: refObject{Sha: tip, Type: "commit"}}, reqID)
	}
	return ErrJSON(404, "Not Found", reqID)
}

func handleLabels(s *Store, repo *RepoRec, method string, rest []string, jsonBody Body, reqID string) *Response {
	if method == "POST" && len(rest) == 1 {
		label := s.CreateLabel(repo, strVal(jsonBody["name"]), strVal(jsonBody["color"], "ededed"))
		return SendJSON(201, labelResp{Name: label.Name, Color: label.Color}, reqID)
	}
	if method == "GET" && len(rest) == 1 {
		// TS iterates the labels Map in insertion order; Go maps have no
		// order, so sort by name for deterministic output.
		names := make([]string, 0, len(repo.Labels))
		for name := range repo.Labels {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]labelResp, 0, len(names))
		for _, name := range names {
			l := repo.Labels[name]
			out = append(out, labelResp{Name: l.Name, Color: l.Color})
		}
		return SendJSON(200, out, reqID)
	}
	return ErrJSON(404, "Not Found", reqID)
}

// jsIssueNumber mirrors TS Number(rest[1]) + Number.isInteger + >= 1 for
// issue/pull numbers: "0x10" -> 16, " 12 " -> 12, "1.5"/"abc" -> 404.
func jsIssueNumber(seg string) (int, bool) {
	f, ok := jsNumber(seg)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f != math.Trunc(f) || f < 1 || f > float64(math.MaxInt32) {
		return 0, false
	}
	return int(f), true
}

func optStr(jsonBody Body, key string) *string {
	if s, ok := jsonBody[key].(string); ok {
		return &s
	}
	return nil
}

// handleIssues mirrors TS /issues...
func handleIssues(s *Store, repo *RepoRec, method string, rest []string, req *Request, jsonBody Body, reqID string) *Response {
	if len(rest) == 1 {
		if method == "GET" {
			// GitHub's default list state is open; an explicit state=all
			// overrides it. Absent -> "open"; present-but-empty stays "".
			state := "open"
			if _, ok := req.Query["state"]; ok {
				state = req.Query.Get("state")
			}
			list := s.ListIssues(repo, state, req.Query.Get("sort"), req.Query.Get("direction"))
			pg := paginate(len(list), req.Query)
			page := make([]IssueJSON, 0, pg.end-pg.start)
			for _, iss := range list[pg.start:pg.end] {
				page = append(page, s.IssueJSON(repo, iss))
			}
			return SendJSON(200, page, reqID)
		}
		if method == "POST" {
			issue, aerr := s.CreateIssue(repo, strVal(jsonBody["title"]), strVal(jsonBody["body"]), asLabels(jsonBody["labels"]))
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(201, s.IssueJSON(repo, issue), reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}
	number, ok := jsIssueNumber(rest[1])
	if !ok {
		return ErrJSON(404, "Not Found", reqID)
	}
	if len(rest) == 2 {
		if method == "GET" {
			// Hot path: serve the cached serialization, no re-serialize.
			issue, aerr := s.GetIssue(repo, number)
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendRaw(200, s.IssueJSONCached(repo, issue), reqID)
		}
		if method == "PATCH" {
			patched, aerr := s.PatchIssue(repo, number, optStr(jsonBody, "state"), optStr(jsonBody, "title"), optStr(jsonBody, "body"))
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(200, s.IssueJSON(repo, patched), reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}
	if len(rest) == 3 && rest[2] == "comments" {
		if method == "GET" {
			issue, aerr := s.GetIssue(repo, number)
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			out := make([]commentResp, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				out = append(out, commentResp{ID: c.ID, Body: c.Body, User: c.User, CreatedAt: c.CreatedAt})
			}
			return SendJSON(200, out, reqID)
		}
		if method == "POST" {
			comment, aerr := s.CreateComment(repo, number, strVal(jsonBody["body"]))
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(201, commentResp{ID: comment.ID, Body: comment.Body, User: comment.User, CreatedAt: comment.CreatedAt}, reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}
	return ErrJSON(404, "Not Found", reqID)
}

// handlePulls mirrors TS /pulls...
func handlePulls(s *Store, repo *RepoRec, method string, rest []string, req *Request, jsonBody Body, reqID string) *Response {
	if len(rest) == 1 {
		if method == "GET" {
			list := s.ListPulls(repo, req.Query.Get("state"))
			pg := paginate(len(list), req.Query)
			page := make([]PullJSON, 0, pg.end-pg.start)
			for _, p := range list[pg.start:pg.end] {
				page = append(page, s.PullJSON(repo, p))
			}
			return SendJSON(200, page, reqID)
		}
		if method == "POST" {
			pull, aerr := s.CreatePull(repo, strVal(jsonBody["title"]), strVal(jsonBody["body"]), strVal(jsonBody["head"]), strVal(jsonBody["base"]))
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(201, s.PullJSON(repo, pull), reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}
	number, ok := jsIssueNumber(rest[1])
	if !ok {
		return ErrJSON(404, "Not Found", reqID)
	}
	if len(rest) == 2 {
		if method == "GET" {
			pull, aerr := s.GetPull(repo, number)
			if aerr != nil {
				return apiErr(aerr, reqID)
			}
			return SendJSON(200, s.PullJSON(repo, pull), reqID)
		}
		return ErrJSON(404, "Not Found", reqID)
	}
	if len(rest) == 3 && rest[2] == "files" && method == "GET" {
		pull, aerr := s.GetPull(repo, number)
		if aerr != nil {
			return apiErr(aerr, reqID)
		}
		return SendJSON(200, nonNil(pull.Files), reqID)
	}
	if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
		sha, merged, message, aerr := s.MergePull(repo, number)
		if aerr != nil {
			return apiErr(aerr, reqID)
		}
		return SendJSON(200, mergeResp{Sha: sha, Merged: merged, Message: message}, reqID)
	}
	return ErrJSON(404, "Not Found", reqID)
}
