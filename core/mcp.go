package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// MCP — JSON-RPC 2.0 Streamable HTTP at /mcp.
// Modern revision 2026-07-28 via server/discover; legacy initialize handshake
// (2025-11-25 and earlier) also supported.
// ---------------------------------------------------------------------------

type mcpToolDef struct {
	Name        string
	Description string
	Required    []string
	Props       map[string]any // name -> schema fragment
}

var mcpTools = []mcpToolDef{
	{
		Name: "create_repo", Description: "Create a repository for the authenticated user.",
		Required: []string{"name"},
		Props: map[string]any{
			"owner":       map[string]any{"type": "string", "description": "Repository owner login."},
			"name":        map[string]any{"type": "string", "description": "Repository name."},
			"description": map[string]any{"type": "string", "description": "Repository description."},
			"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
		},
	},
	{
		Name: "get_repo", Description: "Get a repository by owner and name.",
		Required: []string{"owner", "repo"},
		Props: map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner login."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
		},
	},
	{
		Name: "create_issue", Description: "Create an issue in a repository.",
		Required: []string{"owner", "repo", "title"},
		Props: map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner login."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
			"title": map[string]any{"type": "string", "description": "Issue title."},
			"body":  map[string]any{"type": "string", "description": "Issue body."},
		},
	},
	{
		Name: "list_issues", Description: "List issues in a repository.",
		Required: []string{"owner", "repo"},
		Props: map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner login."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
			"state": map[string]any{"type": "string", "description": "Issue state: open, closed, or all."},
		},
	},
	{
		Name: "create_pull_request", Description: "Create a pull request in a repository.",
		Required: []string{"owner", "repo", "title", "head", "base"},
		Props: map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner login."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
			"title": map[string]any{"type": "string", "description": "Pull request title."},
			"body":  map[string]any{"type": "string", "description": "Pull request body."},
			"head":  map[string]any{"type": "string", "description": "Head branch name."},
			"base":  map[string]any{"type": "string", "description": "Base branch name."},
		},
	},
	{
		Name: "merge_pull_request", Description: "Merge a pull request in a repository.",
		Required: []string{"owner", "repo", "pull_number"},
		Props: map[string]any{
			"owner":       map[string]any{"type": "string", "description": "Repository owner login."},
			"repo":        map[string]any{"type": "string", "description": "Repository name."},
			"pull_number": map[string]any{"type": "integer", "description": "Pull request number."},
		},
	},
}

// toolsListJSON is built once so two tools/list answers are byte-identical.
var toolsListJSON []byte

func init() {
	var tools []any
	for _, t := range mcpTools {
		props := map[string]any{}
		var order []string
		for k := range t.Props {
			order = append(order, k)
		}
		// deterministic property order: sorted
		for i := 0; i < len(order); i++ {
			for j := i + 1; j < len(order); j++ {
				if order[j] < order[i] {
					order[i], order[j] = order[j], order[i]
				}
			}
		}
		for _, k := range order {
			props[k] = t.Props[k]
		}
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   t.Required,
			},
		})
	}
	b, _ := json.Marshal(map[string]any{"tools": tools})
	toolsListJSON = b
}

func mcpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	// Auth before anything else: no Bearer -> 401 (even for tools/list).
	if !auth(w, r) {
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		mcpParseError(w)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(b, &req); err != nil {
		mcpParseError(w)
		return
	}
	method, _ := req["method"].(string)
	id := req["id"] // may be nil (notification)
	params, _ := req["params"].(map[string]any)
	_ = params

	// Notifications (no id) -> 202, no body.
	if id == nil {
		w.WriteHeader(202)
		return
	}
	// Session header echo (Streamable HTTP): return one if the client sent one.
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	switch method {
	case "server/discover":
		mcpResult(w, id, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"protocolVersion":   "2026-07-28",
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "github-forge", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "github-forge", "version": "1.0.0"},
			},
		})
	case "initialize":
		ver := "2025-11-25"
		if p, ok := req["params"].(map[string]any); ok {
			if v, ok := p["protocolVersion"].(string); ok && v != "" {
				ver = v
			}
		}
		mcpResult(w, id, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "github-forge", "version": "1.0.0"},
		})
	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// byte-identical every time: same tools JSON, same id echo
		var buf bytes.Buffer
		buf.WriteString(`{"jsonrpc":"2.0","id":`)
		ib, _ := json.Marshal(id)
		buf.Write(ib)
		buf.WriteString(`,"result":`)
		buf.Write(toolsListJSON)
		buf.WriteString(`}`)
		w.Write(buf.Bytes())
	case "tools/call":
		mcpCallTool(w, id, params)
	default:
		mcpError(w, id, -32601, "Method not found")
	}
}

func mcpParseError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
}

func mcpResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	w.Write(b)
}

func mcpError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	w.Write(b)
}

func mcpToolResult(w http.ResponseWriter, id any, structured any, isError bool) {
	textBytes, _ := json.Marshal(structured)
	content := []any{map[string]any{"type": "text", "text": string(textBytes)}}
	res := map[string]any{"content": content, "structuredContent": structured}
	if isError {
		res["isError"] = true
	}
	mcpResult(w, id, res)
}

func mcpCallTool(w http.ResponseWriter, id any, params map[string]any) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// find tool def (suffix, case-insensitive)
	var def *mcpToolDef
	for i := range mcpTools {
		if strings.EqualFold(mcpTools[i].Name, name) || strings.HasSuffix(strings.ToLower(name), strings.ToLower(mcpTools[i].Name)) {
			def = &mcpTools[i]
			break
		}
	}
	if def == nil {
		mcpError(w, id, -32601, "Unknown tool: "+name)
		return
	}
	// required arguments
	for _, req := range def.Required {
		if _, ok := args[req]; !ok {
			mcpToolResult(w, id, map[string]any{"error": "Missing required argument: " + req}, true)
			return
		}
	}
	owner := argStrMCP(args, "owner")
	if owner == "" {
		owner = viewerLogin
	}
	repoName := argStrMCP(args, "repo")
	if repoName == "" {
		repoName = argStrMCP(args, "name")
	}

	switch def.Name {
	case "create_repo":
		rpName := argStrMCP(args, "name")
		if rpName == "" {
			mcpToolResult(w, id, map[string]any{"error": "Missing required argument: name"}, true)
			return
		}
		rp := store.CreateRepo(owner, rpName, argStrMCP(args, "description"), argBoolMCP(args, "private"))
		if rp == nil {
			mcpToolResult(w, id, map[string]any{"error": "Repository already exists"}, true)
			return
		}
		mcpToolResult(w, id, repoJSON(rp), false)
	case "get_repo":
		rp := store.GetRepo(owner, repoName)
		if rp == nil || owner != viewerLogin {
			mcpToolResult(w, id, map[string]any{"error": "Repository not found"}, true)
			return
		}
		mcpToolResult(w, id, repoJSON(rp), false)
	case "create_issue":
		rp := store.GetRepo(owner, repoName)
		if rp == nil || owner != viewerLogin {
			mcpToolResult(w, id, map[string]any{"error": "Repository not found"}, true)
			return
		}
		title := argStrMCP(args, "title")
		store.mu.Lock()
		rp.NumSeq++
		num := rp.NumSeq
		seq := store.nextSeq()
		id2 := 2000000 + int(seq)
		iss := &Issue{ID: id2, GID: globalID("Issue", id2), Number: num, Title: title, Body: argStrMCP(args, "body"), State: "open", Seq: seq, TS: tsOf(seq)}
		rp.Issues = append(rp.Issues, iss)
		rp.IssueByNum[num] = iss
		store.mu.Unlock()
		mcpToolResult(w, id, issueJSON(rp, iss), false)
	case "list_issues":
		rp := store.GetRepo(owner, repoName)
		if rp == nil || owner != viewerLogin {
			mcpToolResult(w, id, map[string]any{"error": "Repository not found"}, true)
			return
		}
		state := argStrMCP(args, "state")
		if state == "" {
			state = "open"
		}
		var items []any
		for _, iss := range rp.Issues {
			if state == "all" || iss.State == state {
				items = append(items, issueJSON(rp, iss))
			}
		}
		if items == nil {
			items = []any{}
		}
		mcpToolResult(w, id, map[string]any{"issues": items}, false)
	case "create_pull_request":
		rp := store.GetRepo(owner, repoName)
		if rp == nil || owner != viewerLogin {
			mcpToolResult(w, id, map[string]any{"error": "Repository not found"}, true)
			return
		}
		head := argStrMCP(args, "head")
		base := argStrMCP(args, "base")
		headSHA := rp.Refs["refs/heads/"+head]
		baseSHA := rp.Refs["refs/heads/"+base]
		if headSHA == "" || baseSHA == "" {
			mcpToolResult(w, id, map[string]any{"error": "head or base branch does not exist"}, true)
			return
		}
		store.mu.Lock()
		rp.NumSeq++
		num := rp.NumSeq
		seq := store.nextSeq()
		id2 := 4000000 + int(seq)
		pr := &PR{ID: id2, GID: globalID("PullRequest", id2), Number: num, Title: argStrMCP(args, "title"),
			Body: argStrMCP(args, "body"), Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA, State: "open", Seq: seq}
		rp.Pulls = append(rp.Pulls, pr)
		rp.PRByNum[num] = pr
		store.mu.Unlock()
		mcpToolResult(w, id, prJSON(rp, pr), false)
	case "merge_pull_request":
		rp := store.GetRepo(owner, repoName)
		if rp == nil || owner != viewerLogin {
			mcpToolResult(w, id, map[string]any{"error": "Repository not found"}, true)
			return
		}
		num := argIntMCP(args, "pull_number")
		pr := rp.PRByNum[num]
		if pr == nil {
			mcpToolResult(w, id, map[string]any{"error": "Pull request not found"}, true)
			return
		}
		if pr.State == "closed" {
			mcpToolResult(w, id, map[string]any{"error": "Pull request already merged or closed"}, true)
			return
		}
		if prConflict(rp, pr) {
			mcpToolResult(w, id, map[string]any{"error": "Merge conflict"}, true)
			return
		}
		store.mu.Lock()
		base := rp.mergeBase(pr.HeadSHA, pr.BaseSHA)
		newTree := rp.commitTree(pr.BaseSHA)
		for _, fc := range rp.diffTrees(rp.commitTree(base), rp.commitTree(pr.HeadSHA)) {
			if fc.Status == "deleted" {
				newTree = rp.setFile(newTree, fc.Path, "")
			} else {
				newTree = rp.setFile(newTree, fc.Path, fc.NewOID)
			}
		}
		seq := store.nextSeq()
		mergeSHA := rp.makeCommit(newTree, []string{pr.BaseSHA}, "Merge pull request #"+strconv.Itoa(pr.Number)+" from "+pr.Head+"\n\n"+pr.Title, seq)
		rp.Refs["refs/heads/"+pr.Base] = mergeSHA
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = tsOf(seq)
		pr.MergeSHA = mergeSHA
		store.mu.Unlock()
		mcpToolResult(w, id, prJSON(rp, pr), false)
	}
}

func argStrMCP(args map[string]any, k string) string {
	if v, ok := args[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argBoolMCP(args map[string]any, k string) bool {
	if v, ok := args[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func argIntMCP(args map[string]any, k string) int {
	if v, ok := args[k]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return 0
}
