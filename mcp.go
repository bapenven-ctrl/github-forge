package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// MCP over streamable HTTP (JSON-RPC)
// ---------------------------------------------------------------------------

var MCP_SUPPORTED_VERSIONS = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const MCP_TOOLS_JSON = `[
  {"name": "create_repo", "description": "Create a new repository owned by the authenticated user.", "inputSchema": {"type": "object", "properties": {"name": {"type": "string", "description": "Repository name"}, "description": {"type": "string", "description": "Repository description"}, "private": {"type": "boolean", "description": "Whether the repository is private"}, "owner": {"type": "string", "description": "Owner login (defaults to the viewer)"}}, "required": ["name"], "additionalProperties": true}},
  {"name": "get_repo", "description": "Read one repository by owner and name.", "inputSchema": {"type": "object", "properties": {"owner": {"type": "string"}, "repo": {"type": "string"}}, "required": ["owner", "repo"], "additionalProperties": true}},
  {"name": "list_repos", "description": "List the repositories owned by the authenticated user.", "inputSchema": {"type": "object", "properties": {"owner": {"type": "string"}}, "required": [], "additionalProperties": true}},
  {"name": "create_issue", "description": "Create an issue in a repository.", "inputSchema": {"type": "object", "properties": {"owner": {"type": "string"}, "repo": {"type": "string"}, "title": {"type": "string"}, "body": {"type": "string"}}, "required": ["owner", "repo", "title"], "additionalProperties": true}},
  {"name": "get_issue", "description": "Read one issue by number.", "inputSchema": {"type": "object", "properties": {"owner": {"type": "string"}, "repo": {"type": "string"}, "issue_number": {"type": "integer"}}, "required": ["owner", "repo", "issue_number"], "additionalProperties": true}},
  {"name": "list_issues", "description": "List issues in a repository, optionally filtered by state.", "inputSchema": {"type": "object", "properties": {"owner": {"type": "string"}, "repo": {"type": "string"}, "state": {"type": "string"}}, "required": ["owner", "repo"], "additionalProperties": true}},
  {"name": "create_pull_request", "description": "Open a pull request from a head branch into a base branch.", "inputSchema": {"type": "object", "properties": {"owner": {"type": "string"}, "repo": {"type": "string"}, "title": {"type": "string"}, "body": {"type": "string"}, "head": {"type": "string"}, "base": {"type": "string"}}, "required": ["owner", "repo", "title", "head", "base"], "additionalProperties": true}},
  {"name": "merge_pull_request", "description": "Merge an open pull request.", "inputSchema": {"type": "object", "properties": {"owner": {"type": "string"}, "repo": {"type": "string"}, "pull_number": {"type": "integer"}}, "required": ["owner", "repo", "pull_number"], "additionalProperties": true}}
]`

var mcpTools []map[string]interface{}
var mcpToolsByName map[string]map[string]interface{}

func init() {
	mcpToolsByName = map[string]map[string]interface{}{}
	_ = json.Unmarshal([]byte(MCP_TOOLS_JSON), &mcpTools)
	for _, t := range mcpTools {
		mcpToolsByName[t["name"].(string)] = t
	}
}

func mcpToolResult(obj interface{}) map[string]interface{} {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(obj)
	text := strings.TrimRight(buf.String(), "\n")
	return map[string]interface{}{
		"content":           []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"structuredContent": obj,
		"isError":           false,
	}
}

func mcpToolError(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": message}},
		"isError": true,
	}
}

type mcpRPCError struct {
	code    int
	message string
}

func mcpArgStr(args map[string]interface{}, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func mcpArgInt(args map[string]interface{}, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func mcpCallTool(name string, arguments map[string]interface{}) (interface{}, *mcpRPCError) {
	tool, ok := mcpToolsByName[name]
	if !ok {
		return nil, &mcpRPCError{-32602, "unknown tool: " + name}
	}
	schema, _ := tool["inputSchema"].(map[string]interface{})
	var required []string
	if req, ok := schema["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	var missing []string
	for _, key := range required {
		if v, present := arguments[key]; !present || v == nil {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &mcpRPCError{-32602, "missing required arguments: " + strings.Join(missing, ", ")}
	}
	if name == "create_repo" {
		repoName := mcpArgStr(arguments, "name")
		if _, exists := FORGE.Repos[repoName]; exists {
			return mcpToolError("repository already exists"), nil
		}
		private := false
		if b, ok := arguments["private"].(bool); ok {
			private = b
		}
		repo := FORGE.CreateRepo(repoName, private, mcpArgStr(arguments, "description"))
		return mcpToolResult(repoJSON(repo)), nil
	}
	if name == "list_repos" {
		out := make([]interface{}, 0)
		for _, r := range FORGE.OrderedRepos() {
			out = append(out, repoJSON(r))
		}
		return mcpToolResult(out), nil
	}
	repo := FORGE.GetRepo(mcpArgStr(arguments, "owner"), mcpArgStr(arguments, "repo"))
	if repo == nil {
		return mcpToolError("repository not found"), nil
	}
	switch name {
	case "get_repo":
		return mcpToolResult(repoJSON(repo)), nil
	case "create_issue":
		issue := createIssue(repo, mcpArgStr(arguments, "title"), mcpArgStr(arguments, "body"), nil)
		return mcpToolResult(issueJSON(repo, issue)), nil
	case "get_issue":
		issue := repo.Issues[mcpArgInt(arguments, "issue_number")]
		if issue == nil {
			return mcpToolError("issue not found"), nil
		}
		return mcpToolResult(issueJSON(repo, issue)), nil
	case "list_issues":
		state := mcpArgStr(arguments, "state")
		if state == "" {
			state = "open"
		}
		var items []*Issue
		for _, i := range repo.Issues {
			if state == "all" || i.State == state {
				items = append(items, i)
			}
		}
		sort.Slice(items, func(a, b int) bool { return items[a].Number < items[b].Number })
		out := make([]interface{}, 0, len(items))
		for _, i := range items {
			out = append(out, issueJSON(repo, i))
		}
		return mcpToolResult(out), nil
	case "create_pull_request":
		head := mcpArgStr(arguments, "head")
		if idx := strings.LastIndex(head, ":"); idx >= 0 {
			head = head[idx+1:]
		}
		base := mcpArgStr(arguments, "base")
		if idx := strings.LastIndex(base, ":"); idx >= 0 {
			base = base[idx+1:]
		}
		if branchTip(repo, head) == "" || branchTip(repo, base) == "" {
			return mcpToolError("head or base branch does not exist"), nil
		}
		pull := createPull(repo, mcpArgStr(arguments, "title"), mcpArgStr(arguments, "body"), head, base)
		return mcpToolResult(pullJSON(repo, pull)), nil
	case "merge_pull_request":
		pull := repo.Pulls[mcpArgInt(arguments, "pull_number")]
		if pull == nil {
			return mcpToolError("pull request not found"), nil
		}
		if mergePull(repo, pull, "merge") == "" {
			return mcpToolError("pull request is not mergeable"), nil
		}
		return mcpToolResult(pullJSON(repo, pull)), nil
	}
	return nil, &mcpRPCError{-32602, "unknown tool: " + name}
}

func mcpReply(body []byte) Reply {
	var request interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if len(body) == 0 || dec.Decode(&request) != nil {
		return jsonReply(400, map[string]interface{}{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]interface{}{"code": -32700, "message": "Parse error"},
		}, nil)
	}
	if list, ok := request.([]interface{}); ok {
		var responses []interface{}
		for _, item := range list {
			if reply := mcpHandleOne(item); reply != nil {
				responses = append(responses, reply)
			}
		}
		if responses == nil {
			responses = []interface{}{}
		}
		return jsonReply(200, responses, nil)
	}
	reply := mcpHandleOne(request)
	if reply == nil {
		return Reply{Status: 202, Body: []byte{}, ContentType: "application/json; charset=utf-8"}
	}
	return jsonReply(200, reply, nil)
}

func mcpHandleOne(request interface{}) map[string]interface{} {
	reqMap, ok := request.(map[string]interface{})
	if !ok {
		return map[string]interface{}{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]interface{}{"code": -32600, "message": "Invalid Request"},
		}
	}
	method, _ := reqMap["method"].(string)
	requestID, hasID := reqMap["id"]
	params, _ := reqMap["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
	}
	if !hasID && strings.HasPrefix(method, "notifications/") {
		return nil
	}
	result := func(value interface{}) map[string]interface{} {
		return map[string]interface{}{"jsonrpc": "2.0", "id": requestID, "result": value}
	}
	errorReply := func(code int, message string) map[string]interface{} {
		return map[string]interface{}{
			"jsonrpc": "2.0", "id": requestID,
			"error": map[string]interface{}{"code": code, "message": message},
		}
	}
	switch method {
	case "server/discover":
		return result(map[string]interface{}{
			"serverInfo":        map[string]interface{}{"name": "forge-mcp", "version": FORGE_VERSION},
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"supportedVersions": MCP_SUPPORTED_VERSIONS,
		})
	case "initialize":
		offered, _ := params["protocolVersion"].(string)
		chosen := "2025-11-25"
		for _, v := range MCP_SUPPORTED_VERSIONS {
			if v == offered {
				chosen = offered
			}
		}
		return result(map[string]interface{}{
			"protocolVersion": chosen,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "forge-mcp", "version": FORGE_VERSION},
		})
	case "ping":
		return result(map[string]interface{}{})
	case "tools/list":
		tools := make([]interface{}, len(mcpTools))
		for i, t := range mcpTools {
			tools[i] = t
		}
		return result(map[string]interface{}{"tools": tools})
	case "tools/call":
		toolName, _ := params["name"].(string)
		arguments, _ := params["arguments"].(map[string]interface{})
		if arguments == nil {
			arguments = map[string]interface{}{}
		}
		value, rpcErr := mcpCallTool(toolName, arguments)
		if rpcErr != nil {
			return errorReply(rpcErr.code, rpcErr.message)
		}
		return result(value)
	}
	return errorReply(-32601, fmt.Sprintf("Method not found: %s", method))
}
