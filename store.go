// Store types + method signatures (ported from store.ts; signatures exactly as in
// the shared contract stub).
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	sortpkg "sort"
	"strings"
	"time"
)

const defaultBranch = "main"

const authorLine = "Forge <forge@example.invalid>"

type GitObject struct {
	Type string
	Data []byte
}

type FileEntry struct {
	Content []byte
	BlobSha string
}

type Tree map[string]*FileEntry

type User struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type CommitRec struct {
	Sha     string
	TreeSha string
	Tree    Tree
	Parents []string
	Message string
}

type LabelRec struct {
	Name  string
	Color string
}

type CommentRec struct {
	ID        int
	Body      string
	User      User
	CreatedAt string
}

type IssueRec struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []*LabelRec
	User      User
	CreatedAt string
	UpdatedAt string
	Seq       int
	Comments  []*CommentRec
}

type PullFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

type RefInfo struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
}

type PullRec struct {
	ID                   int
	Number               int
	Title                string
	Body                 string
	State                string
	Merged               bool
	MergedAt             *string
	MergeCommitSha       *string
	Head                 RefInfo
	Base                 RefInfo
	User                 User
	CreatedAt            string
	UpdatedAt            string
	BaseTreeAtCreation   Tree
	BaseCommitAtCreation string
	Files                []PullFile
}

type RepoRec struct {
	ID            int
	Name          string
	Owner         *string // nil = unbound (see FindRepo)
	Private       bool
	Description   string
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Seq           int
	Branches      map[string]string
	Tags          map[string]string
	Commits       map[string]*CommitRec
	Labels        map[string]*LabelRec
	Issues        map[int]*IssueRec
	Pulls         map[int]*PullRec
	NextNumber    int
}

type Store struct {
	Repos       map[string]*RepoRec
	Objects     map[string]*GitObject
	ViewerLogin *string
	TrialNow    string
	TrialEpoch  int64

	nextID         int
	nextRepoID     int
	repoSeq        int
	issueTextCache map[string]string
	issueBufCache  map[string][]byte
}

type Ref struct {
	Name string
	Oid  string
}

type CommitParsed struct {
	Tree      string
	Parents   []string
	Message   string
	Author    string
	Committer string
}

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

type TagParsed struct {
	Object  string
	Type    string
	Tag     string
	Message string
}

type BranchJSON struct {
	Name   string `json:"name"`
	Commit struct {
		Sha string `json:"sha"`
	} `json:"commit"`
}

type CommitJSON struct {
	Sha    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

type DirEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	Sha  string `json:"sha"`
}

type RepoOwnerJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type RepoJSON struct {
	ID            int           `json:"id"`
	Name          string        `json:"name"`
	FullName      string        `json:"full_name"`
	Owner         RepoOwnerJSON `json:"owner"`
	Private       bool          `json:"private"`
	Description   string        `json:"description"`
	DefaultBranch string        `json:"default_branch"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
}

type LabelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type IssueJSON struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []LabelJSON `json:"labels"`
	User      User        `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	Comments  int         `json:"comments"`
}

type PullJSON struct {
	ID             int     `json:"id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	Head           RefInfo `json:"head"`
	Base           RefInfo `json:"base"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSha *string `json:"merge_commit_sha"`
	User           User    `json:"user"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// --- package-level helpers ---

func Sha1(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func BlobSha(content []byte) string { return ObjHash("blob", content) }

func ObjHash(kind string, body []byte) string {
	head := []byte(kind + " " + itoa(len(body)) + "\x00")
	h := make([]byte, 0, len(head)+len(body))
	h = append(h, head...)
	h = append(h, body...)
	return Sha1(h)
}

func ParseCommit(data []byte) CommitParsed {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	var head, message string
	if blank == -1 {
		head = text
		message = ""
	} else {
		head = text[:blank]
		message = text[blank+2:]
	}
	out := CommitParsed{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.Parents = append(out.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			out.Committer = line[10:]
		}
	}
	out.Message = message
	return out
}

func ParseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space == -1 {
			break
		}
		space += offset
		mode := string(data[offset:space])
		nul := bytes.IndexByte(data[space:], 0x00)
		if nul == -1 {
			break
		}
		nul += space
		name := string(data[space+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, Oid: oid})
		offset = nul + 21
	}
	return out
}

func ParseTag(data []byte) TagParsed {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	var head, message string
	if blank == -1 {
		head = text
		message = ""
	} else {
		head = text[:blank]
		message = text[blank+2:]
	}
	out := TagParsed{Message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			out.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			out.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			out.Tag = strings.TrimSpace(line[4:])
		}
	}
	return out
}

// treeNode is the shared nested-tree builder used by BuildTreeObjects/TreeShaFor.
type treeNode struct {
	files map[string]string
	dirs  map[string]*treeNode
}

func newTreeNode() *treeNode {
	return &treeNode{files: map[string]string{}, dirs: map[string]*treeNode{}}
}

func buildNestedTree(files Tree) *treeNode {
	root := newTreeNode()
	for path, entry := range files {
		parts := strings.Split(path, "/")
		node := root
		for i := 0; i < len(parts)-1; i++ {
			next, ok := node.dirs[parts[i]]
			if !ok {
				next = newTreeNode()
				node.dirs[parts[i]] = next
			}
			node = next
		}
		node.files[parts[len(parts)-1]] = entry.BlobSha
	}
	return root
}

// gitSortKey: directories sort as name+"/".
func gitSortKey(mode, name string) string {
	if mode == "40000" {
		return name + "/"
	}
	return name
}

func sortedTreeEntries(node *treeNode, childSha func(*treeNode) string) []TreeEntry {
	var entries []TreeEntry
	for name, sha := range node.files {
		entries = append(entries, TreeEntry{Mode: "100644", Name: name, Oid: sha})
	}
	for name, child := range node.dirs {
		entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: childSha(child)})
	}
	sortpkg.Slice(entries, func(i, j int) bool {
		return gitSortKey(entries[i].Mode, entries[i].Name) < gitSortKey(entries[j].Mode, entries[j].Name)
	})
	return entries
}

func encodeTree(entries []TreeEntry) []byte {
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.Oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func BuildTreeObjects(files Tree) (string, map[string]*GitObject) {
	objects := map[string]*GitObject{}
	root := buildNestedTree(files)
	for _, entry := range files {
		objects[entry.BlobSha] = &GitObject{Type: "blob", Data: entry.Content}
	}
	var build func(node *treeNode) string
	build = func(node *treeNode) string {
		entries := sortedTreeEntries(node, build)
		body := encodeTree(entries)
		sha := ObjHash("tree", body)
		objects[sha] = &GitObject{Type: "tree", Data: body}
		return sha
	}
	return build(root), objects
}

func TreeShaFor(files Tree) string {
	root := buildNestedTree(files)
	var build func(node *treeNode) string
	build = func(node *treeNode) string {
		entries := sortedTreeEntries(node, build)
		return ObjHash("tree", encodeTree(entries))
	}
	return build(root)
}

func SubtreeShaFor(files Tree, prefix string) string {
	if prefix == "" {
		return TreeShaFor(files)
	}
	sub := Tree{}
	base := prefix + "/"
	for path, entry := range files {
		if strings.HasPrefix(path, base) {
			sub[path[len(base):]] = entry
		}
	}
	return TreeShaFor(sub)
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return len(strings.Split(text, "\n"))
}

func DiffTrees(base, head Tree) []PullFile {
	pathSet := map[string]struct{}{}
	for p := range base {
		pathSet[p] = struct{}{}
	}
	for p := range head {
		pathSet[p] = struct{}{}
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sortpkg.Strings(paths)
	var out []PullFile
	for _, path := range paths {
		b, bOk := base[path]
		h, hOk := head[path]
		if hOk && (!bOk || b.BlobSha != h.BlobSha) {
			newText := string(h.Content)
			oldText := ""
			if bOk {
				oldText = string(b.Content)
			}
			additions := lineCount(newText)
			if additions < 1 {
				additions = 1
			}
			deletions := 0
			if bOk {
				deletions = lineCount(oldText)
			}
			out = append(out, PullFile{
				Filename: path, Status: "added", Additions: additions, Deletions: deletions, Changes: additions + deletions,
			})
			if bOk {
				out[len(out)-1].Status = "modified"
			}
		} else if bOk && !hOk {
			deletions := lineCount(string(b.Content))
			if deletions < 1 {
				deletions = 1
			}
			out = append(out, PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: deletions, Changes: deletions})
		}
	}
	return out
}

// --- Store methods (stubs) ---

// --- Store methods ---

func NewStore(trialNow string) *Store {
	s := &Store{
		Repos:          map[string]*RepoRec{},
		Objects:        map[string]*GitObject{},
		TrialNow:       trialNow,
		nextID:         1,
		nextRepoID:     1,
		issueTextCache: map[string]string{},
		issueBufCache:  map[string][]byte{},
	}
	// TS uses Date.parse; accept RFC3339 plus plain-date / datetime fallbacks.
	parsed := false
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, trialNow); err == nil {
			s.TrialEpoch = t.Unix()
			parsed = true
			break
		}
	}
	if !parsed {
		s.TrialEpoch = 0
	}
	return s
}

func (s *Store) newID() int {
	id := s.nextID
	s.nextID++
	return id
}

func (s *Store) user(login string) User { return User{Login: login, ID: 1000} }

func (s *Store) RegisterObject(typ string, data []byte) string {
	oid := ObjHash(typ, data)
	s.Objects[oid] = &GitObject{Type: typ, Data: data}
	return oid
}

func (s *Store) GetObject(oid string) (*GitObject, bool) {
	obj, ok := s.Objects[oid]
	return obj, ok
}

func (s *Store) AllRefs(repo *RepoRec) []Ref {
	var out []Ref
	for name, oid := range repo.Branches {
		out = append(out, Ref{Name: "refs/heads/" + name, Oid: oid})
	}
	for name, oid := range repo.Tags {
		out = append(out, Ref{Name: "refs/tags/" + name, Oid: oid})
	}
	sortpkg.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) PeelToCommit(oid string) string {
	cur := oid
	for i := 0; i < 8; i++ {
		obj, ok := s.Objects[cur]
		if !ok || obj.Type != "tag" {
			return cur
		}
		cur = ParseTag(obj.Data).Object
	}
	return cur
}

func (s *Store) TreeToFlat(treeOid string) (Tree, bool) {
	out := Tree{}
	var walk func(oid, prefix string) bool
	walk = func(oid, prefix string) bool {
		obj, ok := s.Objects[oid]
		if !ok || obj.Type != "tree" {
			return false
		}
		for _, e := range ParseTree(obj.Data) {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				if !walk(e.Oid, path) {
					return false
				}
			} else {
				blob, ok := s.Objects[e.Oid]
				if !ok || blob.Type != "blob" {
					return false
				}
				out[path] = &FileEntry{Content: blob.Data, BlobSha: e.Oid}
			}
		}
		return true
	}
	if !walk(treeOid, "") {
		return nil, false
	}
	return out, true
}

func (s *Store) MaterializeCommit(repo *RepoRec, oid string) (*CommitRec, bool) {
	if existing, ok := repo.Commits[oid]; ok {
		return existing, true
	}
	obj, ok := s.Objects[oid]
	if !ok || obj.Type != "commit" {
		return nil, false
	}
	parsed := ParseCommit(obj.Data)
	tree, ok := s.TreeToFlat(parsed.Tree)
	if !ok {
		return nil, false
	}
	rec := &CommitRec{Sha: oid, TreeSha: parsed.Tree, Tree: tree, Parents: parsed.Parents, Message: parsed.Message}
	repo.Commits[oid] = rec
	return rec, true
}

func (s *Store) MaterializeAncestry(repo *RepoRec, tip string) {
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		// Early-stop guard (v6/v5): a commit already in the map has its whole
		// ancestry in the map too, so stop the walk there.
		if _, ok := repo.Commits[oid]; ok {
			continue
		}
		rec, ok := s.MaterializeCommit(repo, oid)
		if !ok {
			continue
		}
		for _, p := range rec.Parents {
			stack = append(stack, p)
		}
	}
}

func (s *Store) IsAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := s.Objects[oid]
		if !ok || obj.Type != "commit" {
			continue
		}
		parents := ParseCommit(obj.Data).Parents
		for _, p := range parents {
			if p == ancestor {
				return true
			}
		}
		for _, p := range parents {
			stack = append(stack, p)
		}
	}
	return false
}

func (s *Store) setRef(repo *RepoRec, ref, oid string, present bool) {
	if strings.HasPrefix(ref, "refs/heads/") {
		name := ref[len("refs/heads/"):]
		if !present {
			delete(repo.Branches, name)
		} else {
			repo.Branches[name] = oid
		}
	} else if strings.HasPrefix(ref, "refs/tags/") {
		name := ref[len("refs/tags/"):]
		if !present {
			delete(repo.Tags, name)
		} else {
			repo.Tags[name] = oid
		}
	}
}

func (s *Store) ApplyRefUpdate(repo *RepoRec, ref, oldOid, newOid string) (bool, string) {
	const zero = "0000000000000000000000000000000000000000"
	current, hasCurrent := s.RefTarget(repo, ref)
	if newOid == zero {
		if !hasCurrent || current != oldOid {
			return false, "non-fast-forward"
		}
		s.setRef(repo, ref, "", false)
		return true, ""
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		if hasCurrent || oldOid != zero {
			return false, "non-fast-forward"
		}
		s.setRef(repo, ref, newOid, true)
		s.MaterializeAncestry(repo, s.PeelToCommit(newOid))
		return true, ""
	}
	if oldOid == zero {
		if hasCurrent {
			return false, "non-fast-forward"
		}
	} else {
		if !hasCurrent || current != oldOid {
			return false, "non-fast-forward"
		}
		if !s.IsAncestor(oldOid, newOid) {
			return false, "non-fast-forward"
		}
	}
	s.setRef(repo, ref, newOid, true)
	s.MaterializeAncestry(repo, newOid)
	return true, ""
}

func (s *Store) RefTarget(repo *RepoRec, ref string) (string, bool) {
	if strings.HasPrefix(ref, "refs/heads/") {
		oid, ok := repo.Branches[ref[len("refs/heads/"):]]
		return oid, ok
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		oid, ok := repo.Tags[ref[len("refs/tags/"):]]
		return oid, ok
	}
	return "", false
}

func (s *Store) OwnerName(repo *RepoRec) string {
	if repo.Owner != nil {
		return *repo.Owner
	}
	if s.ViewerLogin != nil {
		return *s.ViewerLogin
	}
	return ""
}

func (s *Store) RepoJSON(repo *RepoRec) RepoJSON {
	owner := s.OwnerName(repo)
	fullName := repo.Name
	if owner != "" {
		fullName = owner + "/" + repo.Name
	}
	return RepoJSON{
		ID: repo.ID, Name: repo.Name, FullName: fullName,
		Owner:         RepoOwnerJSON{Login: owner, ID: 1000, Type: "User"},
		Private:       repo.Private,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
		CreatedAt:     repo.CreatedAt,
		UpdatedAt:     repo.UpdatedAt,
	}
}

func (s *Store) IssueJSON(repo *RepoRec, issue *IssueRec) IssueJSON {
	labels := make([]LabelJSON, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, LabelJSON{Name: l.Name, Color: l.Color})
	}
	return IssueJSON{
		ID: issue.ID, Number: issue.Number, Title: issue.Title, Body: issue.Body,
		State: issue.State, Labels: labels, User: issue.User,
		CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
		Comments: len(issue.Comments),
	}
}

func (s *Store) PullJSON(repo *RepoRec, pull *PullRec) PullJSON {
	return PullJSON{
		ID: pull.ID, Number: pull.Number, Title: pull.Title, Body: pull.Body,
		State: pull.State, Head: pull.Head, Base: pull.Base, Merged: pull.Merged,
		MergedAt: pull.MergedAt, MergeCommitSha: pull.MergeCommitSha,
		User: pull.User, CreatedAt: pull.CreatedAt, UpdatedAt: pull.UpdatedAt,
	}
}

func (s *Store) IssueJSONCached(repo *RepoRec, issue *IssueRec) []byte {
	key := itoa(repo.ID) + ":" + itoa(issue.Number)
	if hit, ok := s.issueBufCache[key]; ok {
		return hit
	}
	text := string(marshalJSON(s.IssueJSON(repo, issue))) + "\n"
	s.issueTextCache[key] = text
	buf := []byte(text)
	s.issueBufCache[key] = buf
	return buf
}

func (s *Store) makeCommit(tree Tree, parents []string, message string) *CommitRec {
	treeSha := TreeShaFor(tree)
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	var sb strings.Builder
	sb.WriteString("tree " + treeSha + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + authorLine + " " + itoa64(s.TrialEpoch) + " +0000\n")
	sb.WriteString("committer " + authorLine + " " + itoa64(s.TrialEpoch) + " +0000\n\n")
	sb.WriteString(msg)
	body := []byte(sb.String())
	sha := ObjHash("commit", body)
	s.RegisterObject("commit", body)
	_, treeObjs := BuildTreeObjects(tree)
	for oid, obj := range treeObjs {
		s.Objects[oid] = obj
	}
	return &CommitRec{Sha: sha, TreeSha: treeSha, Tree: tree, Parents: parents, Message: message}
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (s *Store) safeTree(repo *RepoRec, branch string) Tree {
	tip, ok := repo.Branches[branch]
	if !ok {
		return Tree{}
	}
	if c, ok := repo.Commits[tip]; ok {
		return c.Tree
	}
	return Tree{}
}
func (s *Store) CreateRepo(name string, isPrivate bool, description string) (*RepoRec, *ApiError) {
	if name == "" {
		return nil, NewApiError(422, "Repository name is required")
	}
	if _, ok := s.Repos[name]; ok {
		return nil, NewApiError(422, "Repository "+name+" already exists")
	}
	repo := &RepoRec{
		ID: s.nextRepoID, Name: name, Owner: s.ViewerLogin, Private: isPrivate,
		Description: description, DefaultBranch: defaultBranch,
		CreatedAt: s.TrialNow, UpdatedAt: s.TrialNow, Seq: s.repoSeq,
		Branches: map[string]string{}, Tags: map[string]string{},
		Commits: map[string]*CommitRec{}, Labels: map[string]*LabelRec{},
		Issues: map[int]*IssueRec{}, Pulls: map[int]*PullRec{}, NextNumber: 1,
	}
	s.nextRepoID++
	s.repoSeq++
	s.Repos[name] = repo
	return repo, nil
}

func (s *Store) FindRepo(owner, name string) (*RepoRec, bool) {
	repo, ok := s.Repos[name]
	if !ok {
		return nil, false
	}
	if repo.Owner == nil {
		repo.Owner = &owner
		if s.ViewerLogin == nil {
			v := owner
			s.ViewerLogin = &v
		}
		return repo, true
	}
	if *repo.Owner == owner {
		return repo, true
	}
	return nil, false
}

func (s *Store) ListRepos() []*RepoRec {
	out := make([]*RepoRec, 0, len(s.Repos))
	for _, r := range s.Repos {
		out = append(out, r)
	}
	sortpkg.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (s *Store) DeleteRepo(repo *RepoRec) { delete(s.Repos, repo.Name) }

func (s *Store) CommitOnBranch(repo *RepoRec, branch, message string, mutate func(Tree)) *CommitRec {
	tipSha, ok := repo.Branches[branch]
	if !ok {
		if defTip, ok2 := repo.Branches[repo.DefaultBranch]; ok2 && branch != repo.DefaultBranch {
			repo.Branches[branch] = defTip
			tipSha = defTip
			ok = true
		}
	}
	var parent *CommitRec
	if ok {
		parent = repo.Commits[tipSha]
	}
	tree := Tree{}
	if parent != nil {
		for k, v := range parent.Tree {
			tree[k] = v
		}
	}
	mutate(tree)
	var parents []string
	if parent != nil {
		parents = []string{parent.Sha}
	}
	commit := s.makeCommit(tree, parents, message)
	repo.Commits[commit.Sha] = commit
	repo.Branches[branch] = commit.Sha
	return commit
}

func (s *Store) CreateBranch(repo *RepoRec, branch, sha string) (string, *ApiError) {
	if existing, ok := repo.Branches[branch]; ok {
		return existing, nil
	}
	if _, ok := repo.Commits[sha]; !ok {
		return "", NewApiError(422, "Unknown commit "+sha)
	}
	repo.Branches[branch] = sha
	return sha, nil
}

func (s *Store) ListBranches(repo *RepoRec) []BranchJSON {
	names := make([]string, 0, len(repo.Branches))
	for n := range repo.Branches {
		names = append(names, n)
	}
	sortpkg.Strings(names)
	if len(names) == 0 {
		return []BranchJSON{}
	}
	out := make([]BranchJSON, 0, len(names))
	for _, name := range names {
		var b BranchJSON
		b.Name = name
		b.Commit.Sha = repo.Branches[name]
		out = append(out, b)
	}
	return out
}

func (s *Store) ListCommits(repo *RepoRec, ref string) ([]CommitJSON, *ApiError) {
	var tip string
	var hasTip bool
	if ref == "" {
		tip, hasTip = repo.Branches[repo.DefaultBranch]
	} else if v, ok := repo.Branches[ref]; ok {
		tip, hasTip = v, true
	} else if _, ok := repo.Commits[ref]; ok {
		tip, hasTip = ref, true
	}
	if !hasTip {
		want := ref
		if want == "" {
			want = repo.DefaultBranch
		}
		return nil, NewApiError(404, "No such ref "+want)
	}
	var out []CommitJSON
	seen := map[string]bool{}
	cur := tip
	for {
		if seen[cur] {
			break
		}
		seen[cur] = true
		commit, ok := repo.Commits[cur]
		if !ok {
			break
		}
		var cj CommitJSON
		cj.Sha = commit.Sha
		cj.Commit.Message = commit.Message
		out = append(out, cj)
		if len(commit.Parents) == 0 {
			break
		}
		cur = commit.Parents[0]
	}
	return out, nil
}

func (s *Store) ResolveTree(repo *RepoRec, ref string) (Tree, *ApiError) {
	var tip string
	var hasTip bool
	if ref == "" {
		tip, hasTip = repo.Branches[repo.DefaultBranch]
	} else if v, ok := repo.Branches[ref]; ok {
		tip, hasTip = v, true
	} else if _, ok := repo.Commits[ref]; ok {
		tip, hasTip = ref, true
	}
	if !hasTip {
		want := ref
		if want == "" {
			want = repo.DefaultBranch
		}
		return nil, NewApiError(404, "No such ref "+want)
	}
	commit, ok := repo.Commits[tip]
	if !ok {
		want := ref
		if want == "" {
			want = repo.DefaultBranch
		}
		return nil, NewApiError(404, "No such ref "+want)
	}
	return commit.Tree, nil
}

func (s *Store) PutContent(repo *RepoRec, path, contentB64, message, branch, sha string) (string, string, bool, *ApiError) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, contentB64)
	// Note: TS strips [\r\n\t ]+ via replace; Go's base64 decoder also rejects
	// invalid chars, so decode errors below map to the same 400.
	content, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		// Node's Buffer.from(base64) is padding-lenient; accept unpadded too.
		content, err = base64.RawStdEncoding.DecodeString(cleaned)
	}
	if err != nil {
		return "", "", false, NewApiError(400, "content is not valid base64")
	}
	targetBranch := branch
	if targetBranch == "" {
		targetBranch = repo.DefaultBranch
	}
	tree := s.safeTree(repo, targetBranch)
	existing, hasExisting := tree[path]
	if hasExisting {
		// TS: sha === undefined triggers 409; Go "" means absent.
		if sha == "" || !strings.EqualFold(sha, existing.BlobSha) {
			return "", "", false, NewApiError(409, "sha does not match the current blob (stale sha)")
		}
	}
	newBlobSha := BlobSha(content)
	commit := s.CommitOnBranch(repo, targetBranch, message, func(t Tree) {
		t[path] = &FileEntry{Content: content, BlobSha: newBlobSha}
	})
	return newBlobSha, commit.Sha, !hasExisting, nil
}

func (s *Store) GetContentEntry(repo *RepoRec, path, ref string) (*FileEntry, *ApiError) {
	tree, err := s.ResolveTree(repo, ref)
	if err != nil {
		return nil, err
	}
	entry, ok := tree[path]
	if !ok {
		return nil, NewApiError(404, "No such file "+path)
	}
	return entry, nil
}

func (s *Store) ListDirectory(repo *RepoRec, dirPath, ref string) ([]DirEntry, *ApiError) {
	tree, err := s.ResolveTree(repo, ref)
	if err != nil {
		return nil, err
	}
	base := ""
	if dirPath != "" {
		base = dirPath + "/"
	}
	type info struct {
		typ string
		sha string
	}
	seen := map[string]info{}
	for path := range tree {
		if !strings.HasPrefix(path, base) {
			continue
		}
		rest := path[len(base):]
		if rest == "" {
			continue
		}
		if slash := strings.Index(rest, "/"); slash == -1 {
			seen[rest] = info{typ: "file", sha: tree[path].BlobSha}
		} else {
			dir := rest[:slash]
			if _, ok := seen[dir]; !ok {
				seen[dir] = info{typ: "dir", sha: SubtreeShaFor(tree, base+dir)}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sortpkg.Strings(names)
	out := make([]DirEntry, 0, len(names))
	for _, name := range names {
		out = append(out, DirEntry{Type: seen[name].typ, Name: name, Path: base + name, Sha: seen[name].sha})
	}
	return out, nil
}

func (s *Store) DeleteContent(repo *RepoRec, path, message, branch, sha string) (string, *ApiError) {
	targetBranch := branch
	if targetBranch == "" {
		targetBranch = repo.DefaultBranch
	}
	tree := s.safeTree(repo, targetBranch)
	entry, ok := tree[path]
	if !ok {
		return "", NewApiError(404, "No such file "+path)
	}
	if sha != "" && !strings.EqualFold(sha, entry.BlobSha) {
		return "", NewApiError(409, "File sha "+sha+" is stale")
	}
	commit := s.CommitOnBranch(repo, targetBranch, message, func(t Tree) {
		delete(t, path)
	})
	return commit.Sha, nil
}
func (s *Store) CreateLabel(repo *RepoRec, name, color string) *LabelRec {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	if color == "" {
		color = "ededed"
	}
	label := &LabelRec{Name: name, Color: color}
	repo.Labels[name] = label
	return label
}

func (s *Store) CreateIssue(repo *RepoRec, title, body string, labelNames []string) (*IssueRec, *ApiError) {
	labels := make([]*LabelRec, 0, len(labelNames))
	for _, name := range labelNames {
		label, ok := repo.Labels[name]
		if !ok {
			return nil, NewApiError(422, "Label "+name+" does not exist")
		}
		labels = append(labels, label)
	}
	number := repo.NextNumber
	repo.NextNumber++
	owner := s.OwnerName(repo)
	issue := &IssueRec{
		ID: s.newID(), Number: number, Title: title, Body: body, State: "open",
		Labels: labels, User: s.user(owner),
		CreatedAt: s.TrialNow, UpdatedAt: s.TrialNow,
		Seq:      repo.NextNumber,
		Comments: []*CommentRec{},
	}
	repo.Issues[number] = issue
	return issue, nil
}

func (s *Store) ListIssues(repo *RepoRec, state, sort, direction string) []*IssueRec {
	list := make([]*IssueRec, 0, len(repo.Issues))
	for _, i := range repo.Issues {
		list = append(list, i)
	}
	if state != "" && state != "all" {
		filtered := list[:0]
		for _, i := range list {
			if i.State == state {
				filtered = append(filtered, i)
			}
		}
		list = filtered
	}
	dir := -1
	if direction == "asc" {
		dir = 1
	}
	if sort == "" || sort == "created" {
		sortpkg.Slice(list, func(a, b int) bool {
			if dir == 1 {
				return list[a].Seq < list[b].Seq
			}
			return list[a].Seq > list[b].Seq
		})
	} else {
		// TS leaves non-'created' sorts in insertion (= seq ascending) order.
		sortpkg.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
	}
	return list
}

func (s *Store) GetIssue(repo *RepoRec, number int) (*IssueRec, *ApiError) {
	issue, ok := repo.Issues[number]
	if !ok {
		return nil, NewApiError(404, "No such issue "+itoa(number))
	}
	return issue, nil
}

func (s *Store) PatchIssue(repo *RepoRec, number int, state, title, body *string) (*IssueRec, *ApiError) {
	issue, err := s.GetIssue(repo, number)
	if err != nil {
		return nil, err
	}
	if state != nil {
		if *state != "open" && *state != "closed" {
			return nil, NewApiError(422, "Invalid state "+*state)
		}
		issue.State = *state
	}
	if title != nil {
		issue.Title = *title
	}
	if body != nil {
		issue.Body = *body
	}
	issue.UpdatedAt = s.TrialNow
	key := itoa(repo.ID) + ":" + itoa(issue.Number)
	delete(s.issueTextCache, key)
	delete(s.issueBufCache, key)
	return issue, nil
}

func (s *Store) CreateComment(repo *RepoRec, number int, body string) (*CommentRec, *ApiError) {
	issue, err := s.GetIssue(repo, number)
	if err != nil {
		return nil, err
	}
	owner := s.OwnerName(repo)
	comment := &CommentRec{ID: s.newID(), Body: body, User: s.user(owner), CreatedAt: s.TrialNow}
	issue.Comments = append(issue.Comments, comment)
	issue.UpdatedAt = s.TrialNow
	return comment, nil
}

func (s *Store) CreatePull(repo *RepoRec, title, body, headRef, baseRef string) (*PullRec, *ApiError) {
	headSha, ok := repo.Branches[headRef]
	if !ok {
		return nil, NewApiError(422, "Unknown head branch "+headRef)
	}
	baseSha, ok := repo.Branches[baseRef]
	if !ok {
		return nil, NewApiError(422, "Unknown base branch "+baseRef)
	}
	headCommit, ok1 := repo.Commits[headSha]
	baseCommit, ok2 := repo.Commits[baseSha]
	if !ok1 || !ok2 {
		return nil, NewApiError(422, "Branch tip is not a commit")
	}
	number := repo.NextNumber
	repo.NextNumber++
	owner := s.OwnerName(repo)
	baseTree := Tree{}
	for k, v := range baseCommit.Tree {
		baseTree[k] = v
	}
	pull := &PullRec{
		ID: s.newID(), Number: number, Title: title, Body: body, State: "open",
		Merged: false, MergedAt: nil, MergeCommitSha: nil,
		Head: RefInfo{Ref: headRef, Sha: headSha, Label: owner + ":" + headRef},
		Base: RefInfo{Ref: baseRef, Sha: baseSha, Label: owner + ":" + baseRef},
		User: s.user(owner), CreatedAt: s.TrialNow, UpdatedAt: s.TrialNow,
		BaseTreeAtCreation: baseTree, BaseCommitAtCreation: baseSha,
		Files: DiffTrees(baseCommit.Tree, headCommit.Tree),
	}
	repo.Pulls[number] = pull
	return pull, nil
}

func (s *Store) ListPulls(repo *RepoRec, state string) []*PullRec {
	list := make([]*PullRec, 0, len(repo.Pulls))
	for _, p := range repo.Pulls {
		list = append(list, p)
	}
	sortpkg.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	if state == "" || state == "open" {
		filtered := list[:0]
		for _, p := range list {
			if p.State == "open" {
				filtered = append(filtered, p)
			}
		}
		list = filtered
	} else if state == "closed" {
		filtered := list[:0]
		for _, p := range list {
			if p.State == "closed" {
				filtered = append(filtered, p)
			}
		}
		list = filtered
	}
	return list
}

func (s *Store) GetPull(repo *RepoRec, number int) (*PullRec, *ApiError) {
	pull, ok := repo.Pulls[number]
	if !ok {
		return nil, NewApiError(404, "No such pull request "+itoa(number))
	}
	return pull, nil
}

func (s *Store) MergePull(repo *RepoRec, number int) (string, bool, string, *ApiError) {
	pull, err := s.GetPull(repo, number)
	if err != nil {
		return "", false, "", err
	}
	if pull.State != "open" || pull.Merged {
		return "", false, "", NewApiError(405, "Pull request "+itoa(number)+" cannot be merged")
	}
	baseTip, ok1 := repo.Branches[pull.Base.Ref]
	headTip, ok2 := repo.Branches[pull.Head.Ref]
	if !ok1 || !ok2 {
		return "", false, "", NewApiError(405, "Pull request "+itoa(number)+" cannot be merged")
	}
	baseCommit, ok1 := repo.Commits[baseTip]
	headCommit, ok2 := repo.Commits[headTip]
	if !ok1 || !ok2 {
		return "", false, "", NewApiError(405, "Pull request "+itoa(number)+" cannot be merged")
	}
	baseSince := DiffTrees(pull.BaseTreeAtCreation, baseCommit.Tree)
	prChanges := DiffTrees(pull.BaseTreeAtCreation, headCommit.Tree)
	changedOnBase := map[string]string{}
	for _, f := range baseSince {
		entry, ok := baseCommit.Tree[f.Filename]
		if !ok {
			changedOnBase[f.Filename] = ""
		} else {
			changedOnBase[f.Filename] = entry.BlobSha
		}
	}
	for _, f := range prChanges {
		if onBase, ok := changedOnBase[f.Filename]; ok {
			headEntry, ok := headCommit.Tree[f.Filename]
			headSha := ""
			if ok {
				headSha = headEntry.BlobSha
			}
			if headSha != onBase {
				return "", false, "", NewApiError(405, "Pull request "+itoa(number)+" has a merge conflict on "+f.Filename)
			}
		}
	}
	mergedTree := Tree{}
	for k, v := range baseCommit.Tree {
		mergedTree[k] = v
	}
	for _, f := range prChanges {
		headEntry, ok := headCommit.Tree[f.Filename]
		if !ok {
			delete(mergedTree, f.Filename)
		} else {
			mergedTree[f.Filename] = headEntry
		}
	}
	message := "Merge pull request #" + itoa(number) + " from " + pull.Head.Ref
	commit := s.CommitOnBranch(repo, pull.Base.Ref, message, func(t Tree) {
		for k := range t {
			delete(t, k)
		}
		for k, v := range mergedTree {
			t[k] = v
		}
	})
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = &s.TrialNow
	pull.MergeCommitSha = &commit.Sha
	pull.UpdatedAt = s.TrialNow
	return commit.Sha, true, message, nil
}
