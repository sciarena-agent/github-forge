// graphql.go — GraphQL executor for the GitHub-flavoured surface the suite
// exercises: repository/issues/pullRequests queries, node(id:) global IDs,
// Relay-style pagination, mutations, and introspection for __type.
// Faithful port of graphql.ts (v6). Only the documents the suite sends are
// supported; unknown fields produce a GraphQL errors array (HTTP 200),
// unknown repos produce data.repository=null.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

/* ------------------------------------------------------------------ */
/* ordered JSON object (TS object literals keep insertion order)       */
/* ------------------------------------------------------------------ */

// gqlObj is a JSON object whose keys marshal in insertion order, like a
// JS object literal. Never marshal a Go map for a fixed-shape object.
type gqlObj struct {
	keys []string
	vals map[string]any
}

func newGqlObj() *gqlObj { return &gqlObj{vals: map[string]any{}} }

func (o *gqlObj) set(k string, v any) *gqlObj {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *gqlObj) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(marshalJSON(k))
		b.WriteByte(':')
		b.Write(marshalJSON(o.vals[k]))
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

/* ------------------------------------------------------------------ */
/* global IDs (opaque, stable): base64url of "Kind:..."                */
/* ------------------------------------------------------------------ */

func encodeGlobalID(kind string, parts ...any) string {
	strs := make([]string, 0, len(parts)+1)
	strs = append(strs, kind)
	for _, p := range parts {
		strs = append(strs, fmt.Sprint(p))
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(strs, ":")))
}

type decodedGlobalID struct {
	kind  string
	parts []string
}

// decodeB64URL mirrors Node Buffer.from(s, 'base64url'): lenient about
// padding and stray characters, unlike Go's strict decoders.
func decodeB64URL(s string) ([]byte, error) {
	var clean strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			clean.WriteByte(c)
		}
	}
	t := clean.String()
	if m := len(t) % 4; m != 0 {
		t += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(t)
}

func decodeGlobalID(id string) *decodedGlobalID {
	raw, err := decodeB64URL(id)
	if err != nil {
		return nil
	}
	text := string(raw)
	parts := strings.Split(text, ":")
	if len(parts) == 0 {
		return nil
	}
	kind := parts[0]
	rest := parts[1:]
	if len(rest) == 0 {
		return nil
	}
	return &decodedGlobalID{kind: kind, parts: rest}
}

func repoGlobalID(repo *RepoRec) string { return encodeGlobalID("Repository", repo.ID) }
func issueGlobalID(repo *RepoRec, issue *IssueRec) string {
	return encodeGlobalID("Issue", repo.ID, issue.Number)
}
func pullGlobalID(repo *RepoRec, pull *PullRec) string {
	return encodeGlobalID("PullRequest", repo.ID, pull.Number)
}
func commentGlobalID(commentID int) string { return encodeGlobalID("IssueComment", commentID) }

/* ------------------------------------------------------------------ */
/* cursor pagination                                                   */
/* ------------------------------------------------------------------ */

func encodeCursor(index int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", index)))
}

var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func decodeCursor(cursor any) int {
	s, ok := cursor.(string)
	if !ok {
		return -1
	}
	raw, err := decodeB64URL(s)
	if err != nil {
		return -1
	}
	m := cursorRe.FindStringSubmatch(string(raw))
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

/* ------------------------------------------------------------------ */
/* JS value helpers (mirror String()/Number()/parseInt() semantics)    */
/* ------------------------------------------------------------------ */

// jsString mirrors JS String(v): undefined -> "undefined", null -> "null".
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	case json.Number:
		return string(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return fmt.Sprint(v)
	}
}

// jsNumberString formats a float the way JS String(number) does for the
// magnitudes this surface sees (small integers and short decimals).
func jsNumberString(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// strOrEmpty mirrors JS String(v ?? ”): null/undefined -> "".
// jsStringArg mirrors JS String(v) for a value taken from a JSON object:
// absent (undefined) -> "undefined", present-null -> "null".
func jsStringArg(v any, present bool) string {
	if !present {
		return "undefined"
	}
	if v == nil {
		return "null"
	}
	return jsString(v)
}

func strOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

// jsNumber mirrors JS Number(v): null/undefined -> NaN.
func gqlNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return math.NaN()
	case float64:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return math.NaN()
	}
}

// jsParseInt mirrors JS parseInt(s, 10): leading whitespace/sign/digits.
func jsParseInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

/* ------------------------------------------------------------------ */
/* lexer + parser (subset: operations, fragments, aliases, variables) */
/* ------------------------------------------------------------------ */

type gqlTokenKind string

const (
	tokName   gqlTokenKind = "name"
	tokString gqlTokenKind = "string"
	tokInt    gqlTokenKind = "int"
	tokPunct  gqlTokenKind = "punct"
	tokSpread gqlTokenKind = "spread"
	tokVar    gqlTokenKind = "var"
)

type gqlToken struct {
	kind  gqlTokenKind
	value any // string for name/string/punct/spread/var, int for int
}

func (t gqlToken) marshalJSON() []byte {
	o := newGqlObj().set("kind", string(t.kind)).set("value", t.value)
	return marshalJSON(o)
}

var (
	varRe  = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z0-9_]*`)
	intRe  = regexp.MustCompile(`^-?\d+`)
	nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
)

func isNameStart(ch byte) bool {
	return ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
}
func isNameChar(ch byte) bool {
	return isNameStart(ch) || (ch >= '0' && ch <= '9')
}

func tokenize(src string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && strings.HasPrefix(src[i:], "...") {
			tokens = append(tokens, gqlToken{kind: tokSpread, value: "..."})
			i += 3
			continue
		}
		if ch == '$' {
			if m := varRe.FindString(src[i:]); m != "" {
				tokens = append(tokens, gqlToken{kind: tokVar, value: m[1:]})
				i += len(m)
				continue
			}
		}
		if strings.ContainsRune("{}()[]:!=$", rune(ch)) {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(ch)})
			i++
			continue
		}
		if ch == '"' {
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(esc)
					}
					j += 2
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: tokString, value: sb.String()})
			i = j + 1
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			if m := intRe.FindString(src[i:]); m != "" {
				n, _ := strconv.Atoi(m)
				tokens = append(tokens, gqlToken{kind: tokInt, value: n})
				i += len(m)
				continue
			}
		}
		if isNameStart(ch) {
			m := nameRe.FindString(src[i:])
			tokens = append(tokens, gqlToken{kind: tokName, value: m})
			i += len(m)
			continue
		}
		return nil, fmt.Errorf("unexpected character %s at %d", string(marshalJSON(string(ch))), i)
	}
	return tokens, nil
}

// FieldNode is one parsed field (with alias, args, selection set and an
// optional inline-fragment type condition).
type FieldNode struct {
	name         string
	alias        *string
	args         map[string]any
	selection    []*FieldNode
	fragmentType *string
}

// OperationNode is one parsed operation.
type OperationNode struct {
	kind      string // "query" | "mutation"
	name      *string
	variables map[string]any
	selection []*FieldNode
}

type gqlParser struct {
	pos    int
	tokens []gqlToken
}

func (p *gqlParser) peek() *gqlToken {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *gqlParser) next() (gqlToken, error) {
	if p.pos >= len(p.tokens) {
		return gqlToken{}, fmt.Errorf("unexpected end of query")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(value string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("expected %s, got %s", value, string(t.marshalJSON()))
	}
	return nil
}

func (p *gqlParser) peekPunct(value string) bool {
	t := p.peek()
	return t != nil && t.kind == tokPunct && t.value == value
}

func (p *gqlParser) parseDocument() ([]*OperationNode, error) {
	var ops []*OperationNode
	for p.pos < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*OperationNode, error) {
	kind := "query"
	var name *string
	if t := p.peek(); t != nil && t.kind == tokName && (t.value == "query" || t.value == "mutation") {
		kind = t.value.(string)
		p.next()
		if n := p.peek(); n != nil && n.kind == tokName {
			v := n.value.(string)
			name = &v
			p.next()
		}
	}
	// Variable definitions: ($name: Type! = default, ...)
	variables := map[string]any{}
	if p.peekPunct("(") {
		p.next()
		for !p.peekPunct(")") {
			v, err := p.next()
			if err != nil {
				return nil, err
			}
			if v.kind != tokVar {
				return nil, fmt.Errorf("expected variable in definition")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.skipType(); err != nil {
				return nil, err
			}
			if p.peekPunct("=") {
				p.next()
				def, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				variables[v.value.(string)] = def
			}
			if p.peekPunct(",") {
				p.next()
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	selection, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &OperationNode{kind: kind, name: name, variables: variables, selection: selection}, nil
}

func (p *gqlParser) skipType() error {
	// Type name, possibly wrapped in [ ] and suffixed with !.
	if p.peekPunct("[") {
		p.next()
		if err := p.skipType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		t, err := p.next()
		if err != nil {
			return err
		}
		if t.kind != tokName {
			return fmt.Errorf("expected type name")
		}
	}
	if p.peekPunct("!") {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*FieldNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var fields []*FieldNode
	for !p.peekPunct("}") {
		// An empty selection set ("{}") or EOF: parseField errors like TS
		// (next() throws 'unexpected end of query' at EOF).
		if p.peek() == nil {
			if _, err := p.next(); err != nil {
				return nil, err
			}
		}
		parsed, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, parsed...)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return fields, nil
}

func (p *gqlParser) parseField() ([]*FieldNode, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == tokSpread {
		// Inline fragment `... on Type { ... }` or named fragment (unsupported).
		n := p.peek()
		if n != nil && n.kind == tokName && n.value == "on" {
			p.next()
			typeTok, err := p.next()
			if err != nil {
				return nil, err
			}
			if typeTok.kind != tokName {
				return nil, fmt.Errorf("expected type in fragment")
			}
			selection, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ft := typeTok.value.(string)
			for _, f := range selection {
				f.fragmentType = &ft
			}
			return selection, nil
		}
		return nil, fmt.Errorf("named fragments are not supported")
	}
	if t.kind != tokName {
		return nil, fmt.Errorf("expected field name")
	}
	name := t.value.(string)
	var alias *string
	if p.peekPunct(":") {
		p.next()
		real, err := p.next()
		if err != nil {
			return nil, err
		}
		if real.kind != tokName {
			return nil, fmt.Errorf("expected field name after alias")
		}
		a := name
		alias = &a
		name = real.value.(string)
	}
	args := map[string]any{}
	if p.peekPunct("(") {
		p.next()
		for !p.peekPunct(")") {
			argName, err := p.next()
			if err != nil {
				return nil, err
			}
			if argName.kind != tokName {
				return nil, fmt.Errorf("expected argument name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argName.value.(string)] = v
			if p.peekPunct(",") {
				p.next()
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	var selection []*FieldNode
	if p.peekPunct("{") {
		selection, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return []*FieldNode{{name: name, alias: alias, args: args, selection: selection}}, nil
}

// gqlVar is a $variable reference; gqlEnum is a bare enum literal.
type gqlVar struct{ name string }
type gqlEnum struct{ name string }

func (p *gqlParser) parseValue() (any, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	switch t.kind {
	case tokVar:
		return gqlVar{name: t.value.(string)}, nil
	case tokString:
		return t.value.(string), nil
	case tokInt:
		return t.value.(int), nil
	case tokName:
		switch t.value.(string) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return gqlEnum{name: t.value.(string)}, nil
		}
	case tokPunct:
		if t.value == "[" {
			var list []any
			for !p.peekPunct("]") {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				list = append(list, v)
				if p.peekPunct(",") {
					p.next()
				}
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			return list, nil
		}
		if t.value == "{" {
			obj := map[string]any{}
			for !p.peekPunct("}") {
				key, err := p.next()
				if err != nil {
					return nil, err
				}
				if key.kind != tokName {
					return nil, fmt.Errorf("expected object key")
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[key.value.(string)] = v
				if p.peekPunct(",") {
					p.next()
				}
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return obj, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %s", string(t.marshalJSON()))
}

// resolveValue substitutes $variables and unwraps enums into plain values.
func resolveValue(value any, variables map[string]any) any {
	switch v := value.(type) {
	case gqlVar:
		return variables[v.name]
	case gqlEnum:
		return v.name
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = resolveValue(e, variables)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range v {
			out[k] = resolveValue(e, variables)
		}
		return out
	default:
		return value
	}
}

func resolveArgs(field *FieldNode, variables map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range field.args {
		out[k] = resolveValue(v, variables)
	}
	return out
}

/* ------------------------------------------------------------------ */
/* schema (fields the suite may ask for)                               */
/* ------------------------------------------------------------------ */

var gqlSchema = map[string]map[string]bool{
	"Query": {"viewer": true, "repository": true, "node": true, "__type": true},
	"User":  {"login": true, "id": true, "__typename": true},
	"Repository": {
		"id": true, "databaseId": true, "name": true, "nameWithOwner": true, "owner": true,
		"defaultBranchRef": true, "viewerPermission": true, "hasIssuesEnabled": true,
		"issue": true, "issues": true, "pullRequests": true, "pullRequest": true, "__typename": true,
	},
	"Ref": {"name": true, "__typename": true},
	"Issue": {
		"id": true, "number": true, "title": true, "state": true, "body": true,
		"createdAt": true, "updatedAt": true, "__typename": true,
	},
	"PullRequest": {
		"id": true, "number": true, "title": true, "state": true, "merged": true,
		"headRefName": true, "baseRefName": true, "mergeable": true, "files": true, "__typename": true,
	},
	"IssueConnection":                  {"nodes": true, "pageInfo": true, "totalCount": true, "__typename": true},
	"PullRequestConnection":            {"nodes": true, "pageInfo": true, "totalCount": true, "__typename": true},
	"PullRequestChangedFileConnection": {"nodes": true, "pageInfo": true, "__typename": true},
	"PullRequestChangedFile":           {"path": true, "__typename": true},
	"IssueComment":                     {"id": true, "body": true, "__typename": true},
	"IssueCommentConnection":           {"nodes": true, "pageInfo": true, "__typename": true},
	"PageInfo":                         {"hasNextPage": true, "endCursor": true, "__typename": true},
	"__Type":                           {"name": true, "fields": true, "enumValues": true, "__typename": true},
	"__Field":                          {"name": true, "__typename": true},
	"__EnumValue":                      {"name": true, "__typename": true},
	"CreateIssuePayload":               {"issue": true, "__typename": true},
	"CloseIssuePayload":                {"issue": true, "__typename": true},
	"CreatePullRequestPayload":         {"pullRequest": true, "__typename": true},
	"MergePullRequestPayload":          {"pullRequest": true, "__typename": true},
	"AddCommentPayload":                {"commentEdge": true, "__typename": true},
	"IssueCommentEdge":                 {"node": true, "__typename": true},
}

// Introspection data: type name -> field names / enum values.
type gqlTypeInfo struct {
	fields     []string
	enumValues []string
}

var gqlIntrospection = map[string]gqlTypeInfo{
	"Issue": {
		fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt"},
	},
	"PullRequest": {
		fields: []string{"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files"},
	},
	"Repository": {
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	},
	"SearchType":       {enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "CODE", "COMMIT"}},
	"IssueState":       {enumValues: []string{"OPEN", "CLOSED"}},
	"PullRequestState": {enumValues: []string{"OPEN", "CLOSED", "MERGED"}},
	"MergeableState":   {enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
}

/* ------------------------------------------------------------------ */
/* runtime values                                                      */
/* ------------------------------------------------------------------ */

// gqlValue is a node in the GraphQL value graph; TypeName mirrors __typename.
type gqlValue interface {
	TypeName() string
}

// undefinedVal is the sentinel for "field skipped by a fragment condition".
type undefinedVal struct{}

var gqlUndefined = undefinedVal{}

type gqlQuery struct{}

func (gqlQuery) TypeName() string { return "Query" }

type gqlUser struct{ login string }

func (gqlUser) TypeName() string { return "User" }

type gqlRepo struct{ repo *RepoRec }

func (gqlRepo) TypeName() string { return "Repository" }

type gqlRef struct{ name string }

func (gqlRef) TypeName() string { return "Ref" }

type gqlIssue struct {
	repo  *RepoRec
	issue *IssueRec
}

func (gqlIssue) TypeName() string { return "Issue" }

type gqlPull struct {
	repo *RepoRec
	pull *PullRec
}

func (gqlPull) TypeName() string { return "PullRequest" }

type gqlPageInfo struct {
	hasNextPage bool
	endCursor   *string
}

func (gqlPageInfo) TypeName() string { return "PageInfo" }

type gqlIssueConn struct {
	items      []any
	pageInfo   gqlPageInfo
	totalCount int
}

func (gqlIssueConn) TypeName() string { return "IssueConnection" }

type gqlPullConn struct {
	items      []any
	pageInfo   gqlPageInfo
	totalCount int
}

func (gqlPullConn) TypeName() string { return "PullRequestConnection" }

type gqlFileConn struct {
	items    []any
	pageInfo gqlPageInfo
}

func (gqlFileConn) TypeName() string { return "PullRequestChangedFileConnection" }

type gqlFile struct{ path string }

func (gqlFile) TypeName() string { return "PullRequestChangedFile" }

type gqlComment struct {
	id   string
	body string
}

func (gqlComment) TypeName() string { return "IssueComment" }

type gqlType struct{ name string }

func (gqlType) TypeName() string { return "__Type" }

type gqlField struct{ name string }

func (gqlField) TypeName() string { return "__Field" }

type gqlEnumValue struct{ name string }

func (gqlEnumValue) TypeName() string { return "__EnumValue" }

type gqlCreateIssuePayload struct{ issue gqlValue }

func (gqlCreateIssuePayload) TypeName() string { return "CreateIssuePayload" }

type gqlCloseIssuePayload struct{ issue gqlValue }

func (gqlCloseIssuePayload) TypeName() string { return "CloseIssuePayload" }

type gqlCreatePullPayload struct{ pullRequest gqlValue }

func (gqlCreatePullPayload) TypeName() string { return "CreatePullRequestPayload" }

type gqlMergePullPayload struct{ pullRequest gqlValue }

func (gqlMergePullPayload) TypeName() string { return "MergePullRequestPayload" }

type gqlAddCommentPayload struct{ commentEdge gqlValue }

func (gqlAddCommentPayload) TypeName() string { return "AddCommentPayload" }

type gqlCommentEdge struct{ node gqlValue }

func (gqlCommentEdge) TypeName() string { return "IssueCommentEdge" }

// MarshalJSON for a bare node value (only reachable when a field has no
// selection set, which the suite never sends for object fields): TS would
// stringify the raw object; emit just the __typename marker.
func (v gqlQuery) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "Query")), nil
}
func (v gqlUser) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "User")), nil
}
func (v gqlRepo) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "Repository")), nil
}
func (v gqlRef) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "Ref")), nil
}
func (v gqlIssue) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "Issue")), nil
}
func (v gqlPull) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "PullRequest")), nil
}
func (v gqlPageInfo) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "PageInfo")), nil
}
func (v gqlIssueConn) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "IssueConnection")), nil
}
func (v gqlPullConn) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "PullRequestConnection")), nil
}
func (v gqlFileConn) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "PullRequestChangedFileConnection")), nil
}
func (v gqlFile) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "PullRequestChangedFile")), nil
}
func (v gqlComment) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "IssueComment")), nil
}
func (v gqlType) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "__Type")), nil
}
func (v gqlField) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "__Field")), nil
}
func (v gqlEnumValue) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "__EnumValue")), nil
}
func (v gqlCreateIssuePayload) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "CreateIssuePayload")), nil
}
func (v gqlCloseIssuePayload) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "CloseIssuePayload")), nil
}
func (v gqlCreatePullPayload) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "CreatePullRequestPayload")), nil
}
func (v gqlMergePullPayload) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "MergePullRequestPayload")), nil
}
func (v gqlAddCommentPayload) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "AddCommentPayload")), nil
}
func (v gqlCommentEdge) MarshalJSON() ([]byte, error) {
	return marshalJSON(newGqlObj().set("__typename", "IssueCommentEdge")), nil
}

// gqlError is a GraphQL execution error with its response path.
type gqlError struct {
	message string
	path    []any
}

func (e *gqlError) Error() string { return e.message }

// gqlErrJSON is the wire shape of one entry in the errors array. Path is a
// pointer so an empty path still serializes as "path":[] (TS includes it;
// a nil pointer omits the key, as TS does for parse errors).
type gqlErrJSON struct {
	Message string `json:"message"`
	Path    *[]any `json:"path,omitempty"`
}

func gqlErrPath(e *gqlError) gqlErrJSON {
	p := e.path
	if p == nil {
		p = []any{}
	}
	return gqlErrJSON{Message: e.message, Path: &p}
}

func pageInfoFor(total, start, end int) gqlPageInfo {
	pi := gqlPageInfo{hasNextPage: end+1 < total}
	if end >= start {
		c := encodeCursor(end)
		pi.endCursor = &c
	}
	return pi
}

func paginateAny(items []any, args map[string]any) ([]any, gqlPageInfo) {
	first := len(items)
	if f, ok := args["first"].(int); ok {
		first = f
	} else if f, ok := args["first"].(float64); ok {
		first = int(f)
	}
	afterIndex := decodeCursor(args["after"])
	start := afterIndex + 1
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	endIdx := start + first
	if endIdx > len(items) {
		endIdx = len(items)
	}
	if endIdx < start {
		endIdx = start
	}
	page := items[start:endIdx]
	end := start - 1
	if len(page) > 0 {
		end = start + len(page) - 1
	}
	return page, pageInfoFor(len(items), start, end)
}

func pullMergeable(s *Store, repo *RepoRec, pull *PullRec) string {
	baseTip, ok := repo.Branches[pull.Base.Ref]
	if !ok {
		return "UNKNOWN"
	}
	headTip, ok := repo.Branches[pull.Head.Ref]
	if !ok {
		return "UNKNOWN"
	}
	baseCommit, ok := repo.Commits[baseTip]
	if !ok {
		return "UNKNOWN"
	}
	headCommit, ok := repo.Commits[headTip]
	if !ok {
		return "UNKNOWN"
	}
	baseSince := DiffTrees(pull.BaseTreeAtCreation, baseCommit.Tree)
	prChanges := DiffTrees(pull.BaseTreeAtCreation, headCommit.Tree)
	changedOnBase := map[string]string{}
	for _, f := range baseSince {
		entry, ok := baseCommit.Tree[f.Filename]
		if !ok || entry == nil {
			changedOnBase[f.Filename] = ""
		} else {
			changedOnBase[f.Filename] = entry.BlobSha
		}
	}
	for _, f := range prChanges {
		if onBase, ok := changedOnBase[f.Filename]; ok {
			headEntry, ok := headCommit.Tree[f.Filename]
			headSha := ""
			if ok && headEntry != nil {
				headSha = headEntry.BlobSha
			}
			if headSha != onBase {
				return "CONFLICTING"
			}
		}
	}
	return "MERGEABLE"
}

func findRepoByGlobalID(s *Store, globalID string) *RepoRec {
	decoded := decodeGlobalID(globalID)
	if decoded == nil || decoded.kind != "Repository" || len(decoded.parts) != 1 {
		return nil
	}
	id, ok := jsParseInt(decoded.parts[0])
	if !ok {
		return nil
	}
	for _, repo := range s.Repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* executor                                                            */
/* ------------------------------------------------------------------ */

type gqlExecutor struct {
	store *Store
}

func fieldKey(f *FieldNode) string {
	if f.alias != nil {
		return *f.alias
	}
	return f.name
}

func (e *gqlExecutor) executeOperation(op *OperationNode, variables map[string]any) *gqlObj {
	root := gqlQuery{}
	var errs []gqlErrJSON
	data := newGqlObj()
	for _, field := range op.selection {
		key := fieldKey(field)
		val, err := e.resolveField(root, field, variables, []any{})
		if err != nil {
			errs = append(errs, gqlErrPath(err))
			data.set(key, nil)
		} else if _, isUndef := val.(undefinedVal); !isUndef {
			// A top-level field skipped by a fragment condition is omitted
			// from data (TS: the key is undefined, so JSON drops it).
			data.set(key, val)
		}
	}
	if len(errs) > 0 && len(data.keys) == 0 {
		return envelope(nil, errs)
	}
	return envelope(data, errs)
}

func (e *gqlExecutor) resolveField(parent gqlValue, field *FieldNode, variables map[string]any, path []any) (any, *gqlError) {
	if parent == nil {
		return nil, nil
	}
	typeName := parent.TypeName()
	// Inline fragment type condition: skip when it does not match.
	if field.fragmentType != nil && *field.fragmentType != typeName {
		return gqlUndefined, nil
	}
	def, ok := gqlSchema[typeName]
	if !ok {
		return nil, &gqlError{message: fmt.Sprintf("Unknown type %q", typeName), path: path}
	}
	if field.name == "__typename" {
		return typeName, nil
	}
	if !def[field.name] {
		p := append(append([]any{}, path...), field.name)
		return nil, &gqlError{message: fmt.Sprintf("Cannot query field %q on type %q.", field.name, typeName), path: p}
	}
	args := resolveArgs(field, variables)
	childPath := append(append([]any{}, path...), fieldKey(field))
	value, err := e.fieldValue(parent, field.name, args, childPath)
	if err != nil {
		return nil, err
	}
	return e.resolveSelection(value, field.selection, variables, childPath)
}

func (e *gqlExecutor) resolveSelection(value any, selection []*FieldNode, variables map[string]any, path []any) (any, *gqlError) {
	if selection == nil {
		return value, nil
	}
	if value == nil {
		return nil, nil
	}
	if _, isUndef := value.(undefinedVal); isUndef {
		return nil, nil
	}
	if arr, ok := value.([]any); ok {
		out := make([]any, len(arr))
		for i, v := range arr {
			r, err := e.resolveSelection(v, selection, variables, append(append([]any{}, path...), i))
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	if node, ok := value.(gqlValue); ok {
		out := newGqlObj()
		for _, field := range selection {
			// Fragment gating happens in resolveField; a fragment field that
			// does not match returns undefined and is omitted.
			result, err := e.resolveField(node, field, variables, path)
			if err != nil {
				return nil, err
			}
			if _, isUndef := result.(undefinedVal); !isUndef {
				out.set(fieldKey(field), result)
			}
		}
		return out, nil
	}
	return value, nil
}

func (e *gqlExecutor) fieldValue(parent gqlValue, name string, args map[string]any, path []any) (any, *gqlError) {
	switch p := parent.(type) {
	case gqlQuery:
		switch name {
		case "viewer":
			login := ""
			if e.store.ViewerLogin != nil {
				login = *e.store.ViewerLogin
			}
			return gqlUser{login: login}, nil
		case "repository":
			repo, ok := e.store.FindRepo(strOrEmpty(args["owner"]), strOrEmpty(args["name"]))
			if !ok || repo == nil {
				return nil, nil
			}
			return gqlRepo{repo: repo}, nil
		case "node":
			decoded := decodeGlobalID(strOrEmpty(args["id"]))
			if decoded == nil {
				return nil, nil
			}
			switch decoded.kind {
			case "Repository":
				repo := findRepoByGlobalID(e.store, strOrEmpty(args["id"]))
				if repo == nil {
					return nil, nil
				}
				return gqlRepo{repo: repo}, nil
			case "Issue":
				if len(decoded.parts) != 2 {
					return nil, nil
				}
				repo := findRepoByGlobalID(e.store, encodeGlobalID("Repository", decoded.parts[0]))
				if repo == nil {
					return nil, nil
				}
				num, ok := jsParseInt(decoded.parts[1])
				if !ok {
					return nil, nil
				}
				issue, ok := repo.Issues[num]
				if !ok {
					return nil, nil
				}
				return gqlIssue{repo: repo, issue: issue}, nil
			case "PullRequest":
				if len(decoded.parts) != 2 {
					return nil, nil
				}
				repo := findRepoByGlobalID(e.store, encodeGlobalID("Repository", decoded.parts[0]))
				if repo == nil {
					return nil, nil
				}
				num, ok := jsParseInt(decoded.parts[1])
				if !ok {
					return nil, nil
				}
				pull, ok := repo.Pulls[num]
				if !ok {
					return nil, nil
				}
				return gqlPull{repo: repo, pull: pull}, nil
			}
			return nil, nil
		case "__type":
			typeName := strOrEmpty(args["name"])
			if _, ok := gqlIntrospection[typeName]; !ok {
				return nil, nil
			}
			return gqlType{name: typeName}, nil
		}
		return nil, nil
	case gqlUser:
		switch name {
		case "login":
			return p.login, nil
		case "id":
			return 1000, nil
		}
		return nil, nil
	case gqlRepo:
		repo := p.repo
		switch name {
		case "id":
			return repoGlobalID(repo), nil
		case "databaseId":
			return repo.ID, nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			owner := ""
			if repo.Owner != nil {
				owner = *repo.Owner
			}
			return owner + "/" + repo.Name, nil
		case "owner":
			owner := ""
			if repo.Owner != nil {
				owner = *repo.Owner
			}
			return gqlUser{login: owner}, nil
		case "defaultBranchRef":
			if _, ok := repo.Branches[repo.DefaultBranch]; !ok {
				return nil, nil
			}
			return gqlRef{name: repo.DefaultBranch}, nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "issue":
			num := gqlNumber(args["number"])
			if math.IsNaN(num) || num != math.Trunc(num) {
				return nil, nil
			}
			issue, ok := repo.Issues[int(num)]
			if !ok {
				return nil, nil
			}
			return gqlIssue{repo: repo, issue: issue}, nil
		case "issues":
			issues := make([]*IssueRec, 0, len(repo.Issues))
			for _, iss := range repo.Issues {
				issues = append(issues, iss)
			}
			sort.Slice(issues, func(i, j int) bool { return issues[i].Seq < issues[j].Seq })
			if states, ok := args["states"].([]any); ok && len(states) > 0 {
				want := map[string]bool{}
				for _, s := range states {
					if str, ok := s.(string); ok {
						want[strings.ToLower(str)] = true
					}
				}
				filtered := issues[:0]
				for _, iss := range issues {
					if want[strings.ToLower(iss.State)] {
						filtered = append(filtered, iss)
					}
				}
				issues = filtered
			}
			if orderBy, ok := args["orderBy"].(map[string]any); ok {
				if field, _ := orderBy["field"].(string); field == "CREATED_AT" {
					desc := orderBy["direction"] == "DESC"
					sort.Slice(issues, func(i, j int) bool {
						if desc {
							return issues[i].Seq > issues[j].Seq
						}
						return issues[i].Seq < issues[j].Seq
					})
				}
			}
			items := make([]any, len(issues))
			for i, iss := range issues {
				items[i] = gqlIssue{repo: repo, issue: iss}
			}
			page, pageInfo := paginateAny(items, args)
			return gqlIssueConn{items: page, pageInfo: pageInfo, totalCount: len(issues)}, nil
		case "pullRequests":
			pulls := make([]*PullRec, 0, len(repo.Pulls))
			for _, pr := range repo.Pulls {
				pulls = append(pulls, pr)
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
			items := make([]any, len(pulls))
			for i, pr := range pulls {
				items[i] = gqlPull{repo: repo, pull: pr}
			}
			page, pageInfo := paginateAny(items, args)
			return gqlPullConn{items: page, pageInfo: pageInfo, totalCount: len(pulls)}, nil
		case "pullRequest":
			num := gqlNumber(args["number"])
			if math.IsNaN(num) || num != math.Trunc(num) {
				return nil, nil
			}
			pull, ok := repo.Pulls[int(num)]
			if !ok {
				return nil, nil
			}
			return gqlPull{repo: repo, pull: pull}, nil
		}
		return nil, nil
	case gqlRef:
		if name == "name" {
			return p.name, nil
		}
		return nil, nil
	case gqlIssue:
		issue := p.issue
		switch name {
		case "id":
			return issueGlobalID(p.repo, issue), nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "state":
			return strings.ToUpper(issue.State), nil
		case "body":
			return issue.Body, nil
		case "createdAt":
			return issue.CreatedAt, nil
		case "updatedAt":
			return issue.UpdatedAt, nil
		}
		return nil, nil
	case gqlPull:
		pull := p.pull
		switch name {
		case "id":
			return pullGlobalID(p.repo, pull), nil
		case "number":
			return pull.Number, nil
		case "title":
			return pull.Title, nil
		case "state":
			if pull.Merged {
				return "MERGED", nil
			}
			return strings.ToUpper(pull.State), nil
		case "merged":
			return pull.Merged, nil
		case "headRefName":
			return pull.Head.Ref, nil
		case "baseRefName":
			return pull.Base.Ref, nil
		case "mergeable":
			return pullMergeable(e.store, p.repo, pull), nil
		case "files":
			items := make([]any, len(pull.Files))
			for i, f := range pull.Files {
				items[i] = gqlFile{path: f.Filename}
			}
			page, pageInfo := paginateAny(items, args)
			return gqlFileConn{items: page, pageInfo: pageInfo}, nil
		}
		return nil, nil
	case gqlIssueConn:
		switch name {
		case "nodes":
			return p.items, nil
		case "pageInfo":
			return p.pageInfo, nil
		case "totalCount":
			return p.totalCount, nil
		}
		return nil, nil
	case gqlPullConn:
		switch name {
		case "nodes":
			return p.items, nil
		case "pageInfo":
			return p.pageInfo, nil
		case "totalCount":
			return p.totalCount, nil
		}
		return nil, nil
	case gqlFileConn:
		switch name {
		case "nodes":
			return p.items, nil
		case "pageInfo":
			return p.pageInfo, nil
		}
		return nil, nil
	case gqlFile:
		if name == "path" {
			return p.path, nil
		}
		return nil, nil
	case gqlComment:
		switch name {
		case "id":
			return p.id, nil
		case "body":
			return p.body, nil
		}
		return nil, nil
	case gqlCreateIssuePayload, gqlCloseIssuePayload:
		if name == "issue" {
			switch v := parent.(type) {
			case gqlCreateIssuePayload:
				return v.issue, nil
			case gqlCloseIssuePayload:
				return v.issue, nil
			}
		}
		return nil, nil
	case gqlCreatePullPayload, gqlMergePullPayload:
		if name == "pullRequest" {
			switch v := parent.(type) {
			case gqlCreatePullPayload:
				return v.pullRequest, nil
			case gqlMergePullPayload:
				return v.pullRequest, nil
			}
		}
		return nil, nil
	case gqlAddCommentPayload:
		if name == "commentEdge" {
			return p.commentEdge, nil
		}
		return nil, nil
	case gqlCommentEdge:
		if name == "node" {
			return p.node, nil
		}
		return nil, nil
	case gqlPageInfo:
		switch name {
		case "hasNextPage":
			return p.hasNextPage, nil
		case "endCursor":
			if p.endCursor == nil {
				return nil, nil
			}
			return *p.endCursor, nil
		}
		return nil, nil
	case gqlType:
		info, ok := gqlIntrospection[p.name]
		if !ok {
			return nil, nil
		}
		switch name {
		case "name":
			return p.name, nil
		case "fields":
			out := make([]any, len(info.fields))
			for i, f := range info.fields {
				out[i] = gqlField{name: f}
			}
			return out, nil
		case "enumValues":
			out := make([]any, len(info.enumValues))
			for i, v := range info.enumValues {
				out[i] = gqlEnumValue{name: v}
			}
			return out, nil
		}
		return nil, nil
	case gqlField, gqlEnumValue:
		if name == "name" {
			switch v := parent.(type) {
			case gqlField:
				return v.name, nil
			case gqlEnumValue:
				return v.name, nil
			}
		}
		return nil, nil
	}
	return nil, nil
}

/* ------------------------------------------------------------------ */
/* mutations                                                           */
/* ------------------------------------------------------------------ */

func envelope(data any, errs []gqlErrJSON) *gqlObj {
	if errs == nil {
		errs = []gqlErrJSON{}
	}
	return newGqlObj().set("data", data).set("errors", errs)
}

func (e *gqlExecutor) executeMutation(op *OperationNode, variables map[string]any) *gqlObj {
	var errs []gqlErrJSON
	data := newGqlObj()
	for _, field := range op.selection {
		args := resolveArgs(field, variables)
		key := fieldKey(field)
		payload, gerr := e.mutationPayload(field, args, key)
		if gerr != nil {
			errs = append(errs, gqlErrPath(gerr))
			data.set(key, nil)
			continue
		}
		val, err := e.resolveSelection(payload, field.selection, variables, []any{key})
		if err != nil {
			errs = append(errs, gqlErrPath(err))
			data.set(key, nil)
			continue
		}
		data.set(key, val)
	}
	return envelope(data, errs)
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// mutationPayload resolves one root mutation field, mirroring the TS
// hand-rolled switch (store ApiErrors become GraphQL-level errors).
func (e *gqlExecutor) mutationPayload(field *FieldNode, args map[string]any, key string) (gqlValue, *gqlError) {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	errPath := []any{key}
	switch field.name {
	case "createIssue":
		repo := findRepoByGlobalID(e.store, jsStringArg(input["repositoryId"], hasKey(input, "repositoryId")))
		if repo == nil {
			return nil, &gqlError{message: "Could not resolve to a Repository with the global id.", path: errPath}
		}
		body := ""
		if bv, ok := input["body"]; ok {
			body = jsStringArg(bv, true)
		}
		issue, apiErr := e.store.CreateIssue(repo, jsStringArg(input["title"], hasKey(input, "title")), body, []string{})
		if apiErr != nil {
			return nil, &gqlError{message: apiErr.Message, path: errPath}
		}
		return gqlCreateIssuePayload{issue: gqlIssue{repo: repo, issue: issue}}, nil
	case "closeIssue":
		decoded := decodeGlobalID(jsStringArg(input["issueId"], hasKey(input, "issueId")))
		if decoded == nil || decoded.kind != "Issue" || len(decoded.parts) != 2 {
			return nil, &gqlError{message: "Could not resolve to an Issue with the global id.", path: errPath}
		}
		repo := findRepoByGlobalID(e.store, encodeGlobalID("Repository", decoded.parts[0]))
		num, ok := jsParseInt(decoded.parts[1])
		var issue *IssueRec
		if repo != nil && ok {
			issue, ok = repo.Issues[num]
		}
		if repo == nil || !ok || issue == nil {
			return nil, &gqlError{message: "Could not resolve to an Issue with the global id.", path: errPath}
		}
		closed := "closed"
		if _, apiErr := e.store.PatchIssue(repo, issue.Number, &closed, nil, nil); apiErr != nil {
			return nil, &gqlError{message: apiErr.Message, path: errPath}
		}
		return gqlCloseIssuePayload{issue: gqlIssue{repo: repo, issue: issue}}, nil
	case "createPullRequest":
		repo := findRepoByGlobalID(e.store, jsStringArg(input["repositoryId"], hasKey(input, "repositoryId")))
		if repo == nil {
			return nil, &gqlError{message: "Could not resolve to a Repository with the global id.", path: errPath}
		}
		body := ""
		if bv, ok := input["body"]; ok {
			body = jsStringArg(bv, true)
		}
		pull, apiErr := e.store.CreatePull(repo, jsStringArg(input["title"], hasKey(input, "title")), body, jsStringArg(input["headRefName"], hasKey(input, "headRefName")), jsStringArg(input["baseRefName"], hasKey(input, "baseRefName")))
		if apiErr != nil {
			return nil, &gqlError{message: apiErr.Message, path: errPath}
		}
		return gqlCreatePullPayload{pullRequest: gqlPull{repo: repo, pull: pull}}, nil
	case "mergePullRequest":
		decoded := decodeGlobalID(jsStringArg(input["pullRequestId"], hasKey(input, "pullRequestId")))
		if decoded == nil || decoded.kind != "PullRequest" || len(decoded.parts) != 2 {
			return nil, &gqlError{message: "Could not resolve to a PullRequest with the global id.", path: errPath}
		}
		repo := findRepoByGlobalID(e.store, encodeGlobalID("Repository", decoded.parts[0]))
		num, ok := jsParseInt(decoded.parts[1])
		var pull *PullRec
		if repo != nil && ok {
			pull, ok = repo.Pulls[num]
		}
		if repo == nil || !ok || pull == nil {
			return nil, &gqlError{message: "Could not resolve to a PullRequest with the global id.", path: errPath}
		}
		if _, _, _, apiErr := e.store.MergePull(repo, pull.Number); apiErr != nil {
			return nil, &gqlError{message: apiErr.Message, path: errPath}
		}
		return gqlMergePullPayload{pullRequest: gqlPull{repo: repo, pull: pull}}, nil
	case "addComment":
		decoded := decodeGlobalID(jsStringArg(input["subjectId"], hasKey(input, "subjectId")))
		if decoded == nil || decoded.kind != "Issue" || len(decoded.parts) != 2 {
			return nil, &gqlError{message: "Could not resolve to an Issue with the global id.", path: errPath}
		}
		repo := findRepoByGlobalID(e.store, encodeGlobalID("Repository", decoded.parts[0]))
		num, ok := jsParseInt(decoded.parts[1])
		var issue *IssueRec
		if repo != nil && ok {
			issue, ok = repo.Issues[num]
		}
		if repo == nil || !ok || issue == nil {
			return nil, &gqlError{message: "Could not resolve to an Issue with the global id.", path: errPath}
		}
		comment, apiErr := e.store.CreateComment(repo, issue.Number, jsStringArg(input["body"], hasKey(input, "body")))
		if apiErr != nil {
			return nil, &gqlError{message: apiErr.Message, path: errPath}
		}
		return gqlAddCommentPayload{commentEdge: gqlCommentEdge{node: gqlComment{id: commentGlobalID(comment.ID), body: comment.Body}}}, nil
	default:
		return nil, &gqlError{message: fmt.Sprintf("Cannot query field %q on type %q.", field.name, "Mutation"), path: errPath}
	}
}

/* ------------------------------------------------------------------ */
/* HTTP glue                                                           */
/* ------------------------------------------------------------------ */

// gqlRespond writes a GraphQL HTTP response. Unlike REST sendJson, the TS
// GraphQL handler writes JSON.stringify(body) with NO trailing newline.
func gqlRespond(status int, body []byte, reqID string) *Response {
	h := [][2]string{
		{"content-type", "application/json; charset=utf-8"},
		{"content-length", itoa(len(body))},
		{"x-github-request-id", reqID},
	}
	return &Response{Status: status, Header: h, Body: body}
}

// HandleGraphQL mirrors graphql.ts handleGraphQLHttp exactly.
func HandleGraphQL(req *Request, s *Store, token string) *Response {
	if req.Method != "POST" {
		return gqlRespond(405, marshalJSON(newGqlObj().set("message", "Method Not Allowed")), req.ID)
	}
	if token != "" && req.Header("authorization") != "Bearer "+token {
		return gqlRespond(401, marshalJSON(newGqlObj().set("message", "Bad credentials")), req.ID)
	}
	var parsed any
	if err := json.Unmarshal(req.Body, &parsed); err != nil {
		// TS 400 shape is {data:null, errors:[{message}]} — no "path" key.
		raw := newGqlObj().set("data", nil).set("errors", []gqlErrJSON{{Message: "Request body is not valid JSON"}})
		return gqlRespond(400, marshalJSON(raw), req.ID)
	}
	return gqlRespond(200, marshalJSON(executeGraphQL(s, parsed)), req.ID)
}

// Execute one GraphQL request body (single operation object or an array for batching).
func executeGraphQL(s *Store, body any) any {
	if arr, ok := body.([]any); ok {
		out := make([]any, len(arr))
		for i, entry := range arr {
			out[i] = executeOne(s, entry)
		}
		return out
	}
	return executeOne(s, body)
}

func executeOne(s *Store, entry any) *gqlObj {
	record, _ := entry.(map[string]any)
	if record == nil {
		record = map[string]any{}
	}
	query, _ := record["query"].(string)
	variables := map[string]any{}
	if v, ok := record["variables"].(map[string]any); ok {
		variables = v
	}
	var operationName *string
	if name, ok := record["operationName"].(string); ok {
		operationName = &name
	}
	tokens, err := tokenize(query)
	var operations []*OperationNode
	if err == nil {
		operations, err = (&gqlParser{tokens: tokens}).parseDocument()
	}
	if err != nil {
		return envelope(nil, []gqlErrJSON{{Message: err.Error()}})
	}
	if len(operations) == 0 {
		return envelope(nil, []gqlErrJSON{{Message: "no operation"}})
	}
	var op *OperationNode
	if operationName != nil {
		for _, o := range operations {
			if o.name != nil && *o.name == *operationName {
				op = o
				break
			}
		}
		if op == nil {
			return envelope(nil, []gqlErrJSON{{Message: "unknown operation " + *operationName}})
		}
	} else {
		op = operations[0]
	}
	mergedVariables := map[string]any{}
	for k, v := range op.variables {
		mergedVariables[k] = v
	}
	for k, v := range variables {
		mergedVariables[k] = v
	}
	if op.kind == "mutation" {
		return (&gqlExecutor{store: s}).executeMutation(op, mergedVariables)
	}
	return (&gqlExecutor{store: s}).executeOperation(op, mergedVariables)
}
