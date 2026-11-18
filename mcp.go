// mcp.go — Model Context Protocol endpoint at POST /mcp (JSON-RPC 2.0,
// Streamable HTTP). Faithful port of mcp.ts (v6). Modern era first:
// server/discover (2026-07-28) with supportedVersions, serverInfo and a
// tools capability. Legacy initialize + notifications/initialized also
// answered. Every tool acts on the same in-memory store as REST, so objects
// created here read back over REST with the same ids. No auth -> HTTP 401.
// Bad JSON -> HTTP 400, JSON-RPC -32700. Unknown tool -> JSON-RPC -32601.
// Missing required argument -> JSON-RPC -32602.
package main

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const mcpServerName = "github-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18"}

// mcpToolDef is one entry of the tools/list constant, serialized the same
// way every time.
type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

type mcpInputSchema struct {
	Type       string   `json:"type"`
	Properties *gqlObj  `json:"properties"`
	Required   []string `json:"required"`
}

func mcpProp(typ, desc string) *gqlObj {
	return newGqlObj().set("type", typ).set("description", desc)
}

var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: newGqlObj().
				set("name", mcpProp("string", "Repository name")).
				set("description", mcpProp("string", "Repository description")).
				set("private", mcpProp("boolean", "Whether the repository is private")).
				set("owner", mcpProp("string", "Owner login")),
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: newGqlObj().
				set("owner", mcpProp("string", "Owner login")).
				set("repo", mcpProp("string", "Repository name")),
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: newGqlObj().
				set("owner", mcpProp("string", "Owner login")).
				set("repo", mcpProp("string", "Repository name")).
				set("title", mcpProp("string", "Issue title")).
				set("body", mcpProp("string", "Issue body")),
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: newGqlObj().
				set("owner", mcpProp("string", "Owner login")).
				set("repo", mcpProp("string", "Repository name")).
				set("state", mcpProp("string", "Issue state: open, closed, or all")),
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: newGqlObj().
				set("owner", mcpProp("string", "Owner login")).
				set("repo", mcpProp("string", "Repository name")).
				set("title", mcpProp("string", "Pull request title")).
				set("body", mcpProp("string", "Pull request body")).
				set("head", mcpProp("string", "Head branch name")).
				set("base", mcpProp("string", "Base branch name")),
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request by number.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: newGqlObj().
				set("owner", mcpProp("string", "Owner login")).
				set("repo", mcpProp("string", "Repository name")).
				set("pull_number", mcpProp("integer", "Pull request number")),
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

// rpcResult / rpcError wire shapes (field order: jsonrpc, id, result/error).
type mcpRpcResult struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result"`
}

type mcpRpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpRpcError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Error   mcpRpcErrorBody `json:"error"`
}

func mcpResult(id, result any) mcpRpcResult {
	return mcpRpcResult{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpError(id any, code int, message string) mcpRpcError {
	return mcpRpcError{JSONRPC: "2.0", ID: id, Error: mcpRpcErrorBody{Code: code, Message: message}}
}

// mcpToolResult is a tools/call result object. StructuredContent is omitted
// for tool errors (isError), present otherwise.
type mcpToolResult struct {
	Content           []mcpContentPart `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError"`
}

type mcpContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mcpToolOk(result any) mcpToolResult {
	return mcpToolResult{
		Content:           []mcpContentPart{{Type: "text", Text: string(marshalJSON(result))}},
		StructuredContent: result,
		IsError:           false,
	}
}

func mcpToolErr(message string) mcpToolResult {
	return mcpToolResult{
		Content: []mcpContentPart{{Type: "text", Text: message}},
		IsError: true,
	}
}

func mcpStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

var mcpDigitsRe = regexp.MustCompile(`^\d+$`)

func mcpAsNumber(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) {
			return int(t), true
		}
		return 0, false
	case int:
		return t, true
	case string:
		if mcpDigitsRe.MatchString(t) {
			n, err := strconv.Atoi(t)
			if err == nil {
				return n, true
			}
		}
		return 0, false
	default:
		return 0, false
	}
}

// splitFullName splits "owner/repo" into parts, or reports failure.
func mcpSplitFullName(v any) (string, string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", "", false
	}
	idx := strings.Index(s, "/")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// HandleMcp mirrors mcp.ts handleMcp exactly.
func HandleMcp(req *Request, s *Store, token string) *Response {
	reqID := req.ID
	if req.Method != "POST" {
		return SendJSON(405, map[string]string{"message": "Method Not Allowed"}, reqID)
	}
	// Raw body first: a body that is not JSON is HTTP 400 with JSON-RPC -32700.
	var parsed any
	if err := json.Unmarshal(req.Body, &parsed); err != nil {
		return SendJSON(400, mcpError(nil, -32700, "Parse error"), reqID)
	}
	msg, ok := parsed.(map[string]any)
	if !ok || msg == nil {
		return SendJSON(400, mcpError(nil, -32700, "Parse error"), reqID)
	}
	var id any
	if v, present := msg["id"]; present {
		id = v
	} else {
		id = nil
	}
	method, _ := msg["method"].(string)

	// Notifications carry no id and get an empty 202; they are answered
	// before auth because they are not requests and expect no response body.
	if strings.HasPrefix(method, "notifications/") {
		return &Response{Status: 202, Header: [][2]string{{"x-github-request-id", reqID}}}
	}

	// Auth (after the parse check, so a malformed body is always -32700).
	if token != "" {
		if req.Header("authorization") != "Bearer "+token {
			return SendJSON(401, map[string]string{"message": "Unauthorized"}, reqID)
		}
	}

	params := map[string]any{}
	if p, ok := msg["params"].(map[string]any); ok {
		params = p
	}

	// dispatch; ApiError from a tool surfaces as -32602, anything else -32603.
	// (callTool converts store ApiErrors to toolErr results itself, so the
	// -32602 arm below only fires for ApiErrors escaping dispatch.)
	switch method {
	case "server/discover":
		serverInfo := newGqlObj().set("name", mcpServerName).set("version", mcpServerVersion)
		result := newGqlObj().
			set("supportedVersions", mcpSupportedVersions).
			set("capabilities", newGqlObj().set("tools", newGqlObj())).
			set("serverInfo", serverInfo).
			set("_meta", newGqlObj().set("io.modelcontextprotocol/serverInfo", serverInfo))
		return SendJSON(200, mcpResult(id, result), reqID)
	case "initialize":
		offered, _ := params["protocolVersion"].(string)
		if offered == "" {
			offered = "2025-11-25"
		}
		negotiated := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == offered {
				negotiated = offered
				break
			}
		}
		result := newGqlObj().
			set("protocolVersion", negotiated).
			set("capabilities", newGqlObj().set("tools", newGqlObj())).
			set("serverInfo", newGqlObj().set("name", mcpServerName).set("version", mcpServerVersion))
		return SendJSON(200, mcpResult(id, result), reqID)
	case "ping":
		return SendJSON(200, mcpResult(id, newGqlObj()), reqID)
	case "tools/list":
		return SendJSON(200, mcpResult(id, newGqlObj().set("tools", mcpTools)), reqID)
	case "tools/call":
		toolResult, rpcErr := mcpCallTool(params, s)
		if rpcErr != nil {
			return SendJSON(200, mcpError(id, rpcErr.Code, rpcErr.Message), reqID)
		}
		return SendJSON(200, mcpResult(id, toolResult), reqID)
	default:
		return SendJSON(200, mcpError(id, -32601, "Method not found: "+method), reqID)
	}
}

type mcpRpcErr struct {
	Code    int
	Message string
}

func mcpFindTool(name string) *mcpToolDef {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	wanted := norm(name)
	for i := range mcpTools {
		tool := &mcpTools[i]
		if norm(tool.Name) == wanted {
			return tool
		}
		if len(wanted) > len(norm(tool.Name)) && strings.HasSuffix(wanted, norm(tool.Name)) {
			return tool
		}
	}
	return nil
}

// resolveRepoArgs resolves owner/repo from tool arguments, accepting
// full_name as a fallback.
func mcpResolveRepoArgs(args map[string]any) (string, string, bool) {
	owner := mcpStr(args["owner"])
	repo := mcpStr(args["repo"])
	if owner == "" || repo == "" {
		full := args["full_name"]
		if full == nil {
			full = args["fullName"]
		}
		if o, r, ok := mcpSplitFullName(full); ok {
			if owner == "" {
				owner = o
			}
			if repo == "" {
				repo = r
			}
		}
	}
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

func mcpCallTool(params map[string]any, s *Store) (mcpToolResult, *mcpRpcErr) {
	name := mcpStr(params["name"])
	args := map[string]any{}
	if a, ok := params["arguments"].(map[string]any); ok {
		args = a
	}
	tool := mcpFindTool(name)
	if tool == nil {
		return mcpToolResult{}, &mcpRpcErr{Code: -32601, Message: "Unknown tool: " + name}
	}

	// Required-argument check first: missing -> JSON-RPC -32602 (or isError, per the rules).
	var missing []string
	for _, key := range tool.InputSchema.Required {
		v, present := args[key]
		if !present || v == nil || v == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): " + strings.Join(missing, ", ")}
	}

	switch tool.Name {
	case "create_repo":
		repoName := mcpStr(args["name"])
		// The owner the caller names is the token's login; learn it when unknown.
		ownerArg := mcpStr(args["owner"])
		if ownerArg != "" && s.ViewerLogin == nil {
			s.ViewerLogin = &ownerArg
		}
		isPrivate := false
		if b, ok := args["private"].(bool); ok {
			isPrivate = b
		}
		repo, apiErr := s.CreateRepo(repoName, isPrivate, mcpStr(args["description"]))
		if apiErr != nil {
			return mcpToolErr(apiErr.Message), nil
		}
		if ownerArg != "" && repo.Owner == nil {
			repo.Owner = &ownerArg
		}
		return mcpToolOk(s.RepoJSON(repo)), nil
	case "get_repo":
		owner, repoName, ok := mcpResolveRepoArgs(args)
		if !ok {
			return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): owner, repo"}
		}
		repo, found := s.FindRepo(owner, repoName)
		if !found || repo == nil {
			return mcpToolErr("Repository " + owner + "/" + repoName + " not found"), nil
		}
		return mcpToolOk(s.RepoJSON(repo)), nil
	case "create_issue":
		owner, repoName, ok := mcpResolveRepoArgs(args)
		if !ok {
			return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): owner, repo"}
		}
		repo, found := s.FindRepo(owner, repoName)
		if !found || repo == nil {
			return mcpToolErr("Repository " + owner + "/" + repoName + " not found"), nil
		}
		title := mcpStr(args["title"])
		if title == "" {
			return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): title"}
		}
		var labels []string
		if arr, ok := args["labels"].([]any); ok {
			for _, l := range arr {
				switch v := l.(type) {
				case string:
					if v != "" {
						labels = append(labels, v)
					}
				case map[string]any:
					if n := mcpStr(v["name"]); n != "" {
						labels = append(labels, n)
					}
				}
			}
		}
		issue, apiErr := s.CreateIssue(repo, title, mcpStr(args["body"]), labels)
		if apiErr != nil {
			return mcpToolErr(apiErr.Message), nil
		}
		return mcpToolOk(s.IssueJSON(repo, issue)), nil
	case "list_issues":
		owner, repoName, ok := mcpResolveRepoArgs(args)
		if !ok {
			return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): owner, repo"}
		}
		repo, found := s.FindRepo(owner, repoName)
		if !found || repo == nil {
			return mcpToolErr("Repository " + owner + "/" + repoName + " not found"), nil
		}
		state := mcpStr(args["state"])
		if state == "" {
			state = "open"
		}
		list := s.ListIssues(repo, state, "", "")
		out := make([]any, len(list))
		for i, iss := range list {
			out[i] = s.IssueJSON(repo, iss)
		}
		return mcpToolOk(out), nil
	case "create_pull_request":
		owner, repoName, ok := mcpResolveRepoArgs(args)
		if !ok {
			return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): owner, repo"}
		}
		repo, found := s.FindRepo(owner, repoName)
		if !found || repo == nil {
			return mcpToolErr("Repository " + owner + "/" + repoName + " not found"), nil
		}
		pull, apiErr := s.CreatePull(repo, mcpStr(args["title"]), mcpStr(args["body"]), mcpStr(args["head"]), mcpStr(args["base"]))
		if apiErr != nil {
			return mcpToolErr(apiErr.Message), nil
		}
		return mcpToolOk(s.PullJSON(repo, pull)), nil
	case "merge_pull_request":
		owner, repoName, ok := mcpResolveRepoArgs(args)
		if !ok {
			return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): owner, repo"}
		}
		repo, found := s.FindRepo(owner, repoName)
		if !found || repo == nil {
			return mcpToolErr("Repository " + owner + "/" + repoName + " not found"), nil
		}
		numArg := args["pull_number"]
		if numArg == nil {
			numArg = args["number"]
		}
		number, ok := mcpAsNumber(numArg)
		if !ok {
			return mcpToolResult{}, &mcpRpcErr{Code: -32602, Message: "Missing required argument(s): pull_number"}
		}
		sha, merged, message, apiErr := s.MergePull(repo, number)
		if apiErr != nil {
			return mcpToolErr(apiErr.Message), nil
		}
		return mcpToolOk(newGqlObj().set("sha", sha).set("merged", merged).set("message", message)), nil
	default:
		return mcpToolResult{}, &mcpRpcErr{Code: -32601, Message: "Unknown tool: " + name}
	}
}
