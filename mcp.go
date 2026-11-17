// MCP over Streamable HTTP (JSON-RPC 2.0, plain JSON answers): the six
// tools the suite requires plus two convenience tools, all acting on the
// same in-memory store as REST and GraphQL. The tools array is built once
// so two tools/list calls answer byte-identical JSON.

package main

type mcpTool struct {
	name        string
	description string
	inputSchema map[string]any
}

var mcpTools = []mcpTool{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "Repository name"},
				"description": map[string]any{"type": "string", "description": "Repository description"},
				"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private"},
			},
			"required":           []any{"name"},
			"additionalProperties": false,
		},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
			},
			"required":           []any{"owner", "repo"},
			"additionalProperties": false,
		},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
				"title": map[string]any{"type": "string", "description": "Issue title"},
				"body":  map[string]any{"type": "string", "description": "Issue body"},
			},
			"required":           []any{"owner", "repo", "title"},
			"additionalProperties": false,
		},
	},
	{
		name:        "list_issues",
		description: "List issues in a repository, optionally filtered by state.",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
				"state": map[string]any{"type": "string", "description": "Issue state: open, closed or all", "enum": []any{"open", "closed", "all"}},
			},
			"required":           []any{"owner", "repo"},
			"additionalProperties": false,
		},
	},
	{
		name:        "create_pull_request",
		description: "Create a pull request from a head branch onto a base branch.",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
				"title": map[string]any{"type": "string", "description": "Pull request title"},
				"head":  map[string]any{"type": "string", "description": "Head branch name"},
				"base":  map[string]any{"type": "string", "description": "Base branch name"},
			},
			"required":           []any{"owner", "repo", "title", "head", "base"},
			"additionalProperties": false,
		},
	},
	{
		name:        "merge_pull_request",
		description: "Merge a pull request with the squash method.",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string", "description": "Repository owner"},
				"repo":        map[string]any{"type": "string", "description": "Repository name"},
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number"},
			},
			"required":           []any{"owner", "repo", "pull_number"},
			"additionalProperties": false,
		},
	},
	{
		name:        "list_repos",
		description: "List repositories owned by a user (defaults to the authenticated user).",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner"},
			},
			"required":           []any{},
			"additionalProperties": false,
		},
	},
	{
		name:        "get_issue",
		description: "Get one issue by repository and number.",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":        map[string]any{"type": "string", "description": "Repository owner"},
				"repo":         map[string]any{"type": "string", "description": "Repository name"},
				"issue_number": map[string]any{"type": "integer", "description": "Issue number"},
			},
			"required":           []any{"owner", "repo", "issue_number"},
			"additionalProperties": false,
		},
	},
}

func mcpToolsJSON() []any {
	var out []any
	for _, t := range mcpTools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.inputSchema,
		})
	}
	return out
}

var mcpSupportedVersions = []any{"2026-07-28", "2025-11-25"}
var mcpServerInfo = map[string]any{"name": "forge-mcp", "version": "1.0.0"}

type mcpResponse struct {
	status int
	body   any
}

func mcpEnvelope(id any, result any) mcpResponse {
	return mcpResponse{status: 200, body: map[string]any{"jsonrpc": "2.0", "id": id, "result": result}}
}

func mcpErrorEnvelope(id any, code int, message string) mcpResponse {
	return mcpResponse{status: 200, body: map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}}
}

func mcpToolResult(data any) any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSON(data))}},
		"structuredContent": data,
	}
}

func mcpToolError(message string) any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(marshalJSON(map[string]any{"error": message}))}},
		"isError": true,
	}
}

func mcpMissingArgs(args map[string]any, required []string) string {
	for _, key := range required {
		v, ok := args[key]
		if !ok || v == nil || v == "" {
			return "Missing required argument: " + key
		}
	}
	return ""
}

func mcpCallTool(s *store, name string, args map[string]any) any {
	owner := asStr(args["owner"])
	if owner == "" {
		owner = VIEWER.login
	}
	repoName := asStr(args["repo"])
	switch name {
	case "create_repo":
		if missing := mcpMissingArgs(args, []string{"name"}); missing != "" {
			return mcpToolError(missing)
		}
		repoNameArg := asStr(args["name"])
		if s.getRepo(VIEWER.login, repoNameArg) != nil {
			return mcpToolError("Repository already exists")
		}
		private := false
		if b, ok := args["private"].(bool); ok {
			private = b
		}
		r := s.createRepo(VIEWER.login, repoNameArg, asStr(args["description"]), private)
		return mcpToolResult(s.repoView(r))
	case "get_repo":
		if missing := mcpMissingArgs(args, []string{"owner", "repo"}); missing != "" {
			return mcpToolError(missing)
		}
		if owner != VIEWER.login {
			return mcpToolError("Not Found")
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError("Not Found")
		}
		return mcpToolResult(s.repoView(r))
	case "create_issue":
		if missing := mcpMissingArgs(args, []string{"owner", "repo", "title"}); missing != "" {
			return mcpToolError(missing)
		}
		if owner != VIEWER.login {
			return mcpToolError("Not Found")
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError("Not Found")
		}
		iss, _ := s.createIssue(r, asStr(args["title"]), asStr(args["body"]), nil)
		return mcpToolResult(s.issueView(iss, r))
	case "list_issues":
		if missing := mcpMissingArgs(args, []string{"owner", "repo"}); missing != "" {
			return mcpToolError(missing)
		}
		if owner != VIEWER.login {
			return mcpToolError("Not Found")
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError("Not Found")
		}
		state := asStr(args["state"])
		if state == "" {
			state = "open"
		}
		var views []any
		for _, iss := range r.issues {
			if state == "all" || iss.state == state {
				views = append(views, s.issueView(iss, r))
			}
		}
		if views == nil {
			views = []any{}
		}
		return mcpToolResult(map[string]any{"issues": views})
	case "create_pull_request":
		if missing := mcpMissingArgs(args, []string{"owner", "repo", "title", "head", "base"}); missing != "" {
			return mcpToolError(missing)
		}
		if owner != VIEWER.login {
			return mcpToolError("Not Found")
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError("Not Found")
		}
		head := asStr(args["head"])
		base := asStr(args["base"])
		headSHA := s.refTip(r, head)
		baseSHA := s.refTip(r, base)
		if headSHA == "" || baseSHA == "" {
			return mcpToolError("head or base branch does not exist")
		}
		for _, pull := range r.pulls {
			if pull.state == "open" && pull.headRef == head && pull.baseRef == base {
				return mcpToolError("A pull request already exists for these branches")
			}
		}
		pull := s.createPull(r, asStr(args["title"]), "", head, base, headSHA, baseSHA)
		return mcpToolResult(s.pullView(pull, r))
	case "merge_pull_request":
		if missing := mcpMissingArgs(args, []string{"owner", "repo", "pull_number"}); missing != "" {
			return mcpToolError(missing)
		}
		if owner != VIEWER.login {
			return mcpToolError("Not Found")
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError("Not Found")
		}
		pull := r.pulls[asInt(args["pull_number"])]
		if pull == nil {
			return mcpToolError("Not Found")
		}
		result := s.mergePull(r, pull)
		if !result.ok {
			return mcpToolError(result.reason)
		}
		return mcpToolResult(map[string]any{"merged": true, "number": pull.number, "sha": result.sha})
	case "list_repos":
		repos := s.listReposFor(owner)
		var views []any
		for _, r := range repos {
			views = append(views, s.repoView(r))
		}
		if views == nil {
			views = []any{}
		}
		return mcpToolResult(map[string]any{"repositories": views})
	case "get_issue":
		if missing := mcpMissingArgs(args, []string{"owner", "repo", "issue_number"}); missing != "" {
			return mcpToolError(missing)
		}
		if owner != VIEWER.login {
			return mcpToolError("Not Found")
		}
		r := s.getRepo(owner, repoName)
		var iss *issue
		if r != nil {
			iss = r.issues[asInt(args["issue_number"])]
		}
		if r == nil || iss == nil {
			return mcpToolError("Not Found")
		}
		return mcpToolResult(s.issueView(iss, r))
	default:
		return nil
	}
}

func handleMcp(s *store, body any, bodyParseError bool) mcpResponse {
	if bodyParseError {
		return mcpResponse{status: 400, body: map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}}}
	}
	m, ok := body.(map[string]any)
	if !ok {
		return mcpErrorEnvelope(nil, -32600, "Invalid Request")
	}
	method, ok := m["method"].(string)
	if !ok {
		return mcpErrorEnvelope(nil, -32600, "Invalid Request")
	}
	var id any
	if v, exists := m["id"]; exists {
		id = v
	} else {
		id = nil
	}
	_, hasID := m["id"]
	isNotification := !hasID
	params := asMap(m["params"])

	if method == "notifications/initialized" {
		return mcpResponse{status: 202, body: nil}
	}
	if method == "ping" {
		return mcpEnvelope(id, map[string]any{})
	}
	if method == "server/discover" {
		return mcpEnvelope(id, map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        mcpServerInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		})
	}
	if method == "initialize" {
		requested := asStr(params["protocolVersion"])
		protocolVersion := "2026-07-28"
		for _, v := range mcpSupportedVersions {
			if v == requested {
				protocolVersion = requested
				break
			}
		}
		return mcpEnvelope(id, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo,
		})
	}
	if method == "tools/list" {
		return mcpEnvelope(id, map[string]any{"tools": mcpToolsJSON()})
	}
	if method == "tools/call" {
		name := asStr(params["name"])
		args := asMap(params["arguments"])
		if name == "" {
			return mcpErrorEnvelope(id, -32602, "Tool name is required")
		}
		known := false
		for _, t := range mcpTools {
			if t.name == name {
				known = true
				break
			}
		}
		if !known {
			return mcpErrorEnvelope(id, -32601, "Unknown tool: "+name)
		}
		result := mcpCallTool(s, name, args)
		if result == nil {
			return mcpErrorEnvelope(id, -32601, "Unknown tool: "+name)
		}
		return mcpEnvelope(id, result)
	}
	if isNotification {
		return mcpResponse{status: 202, body: nil}
	}
	return mcpErrorEnvelope(id, -32601, "Method not found: "+method)
}
