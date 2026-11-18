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
// GraphQL v4 — small hand-rolled parser + executor for the query subset the
// conformance suite uses (fields, aliases, variables, inline fragments,
// mutations, two-document batches, __type introspection).
// ---------------------------------------------------------------------------

func graphqlHandler(w http.ResponseWriter, r *http.Request) {
	if !auth(w, r) {
		return
	}
	if r.Method != "POST" {
		errJSON(w, 404, "Not Found")
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "cannot read body"}}})
		return
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		writeJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "empty body"}}})
		return
	}
	if trimmed[0] == '[' {
		var docs []json.RawMessage
		if err := json.Unmarshal(trimmed, &docs); err != nil {
			writeJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "invalid JSON"}}})
			return
		}
		var out []any
		for _, d := range docs {
			out = append(out, execGraphQLDoc(d))
		}
		if out == nil {
			out = []any{}
		}
		writeJSON(w, 200, out)
		return
	}
	writeJSON(w, 200, execGraphQLDoc(trimmed))
}

type gqlDoc struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
	Operation string         `json:"operationName"`
}

func execGraphQLDoc(raw json.RawMessage) map[string]any {
	var doc gqlDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": "invalid JSON"}}}
	}
	p := &gqlParser{s: doc.Query, vars: doc.Variables}
	op, err := p.parseOperation()
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	ex := &gqlExecutor{vars: doc.Variables, errors: []map[string]any{}}
	data := ex.execute(op)
	res := map[string]any{"data": data}
	if len(ex.errors) > 0 {
		var errs []any
		for _, e := range ex.errors {
			errs = append(errs, e)
		}
		res["errors"] = errs
	}
	return res
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type gqlField struct {
	Alias string
	Name  string
	Args  map[string]any
	Sub   []*gqlField
}

type gqlOperation struct {
	Kind   string // query | mutation
	Fields []*gqlField
}

type gqlParser struct {
	s    string
	pos  int
	vars map[string]any
}

func (p *gqlParser) skipWS() {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		if c == '#' {
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *gqlParser) peek() byte {
	p.skipWS()
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *gqlParser) ident() string {
	p.skipWS()
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
		} else {
			break
		}
	}
	return p.s[start:p.pos]
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	p.skipWS()
	kind := "query"
	if w := p.ident(); w == "query" || w == "mutation" {
		kind = w
		// optional operation name
		p.ident()
		// optional variable definitions
		if p.peek() == '(' {
			p.parseVarDefs()
		}
	} else {
		// anonymous shorthand: rewind (the ident was a field name)
		p.pos = 0
	}
	fields, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{Kind: kind, Fields: fields}, nil
}

func (p *gqlParser) parseVarDefs() {
	// skip (...) balancing parens
	if p.peek() != '(' {
		return
	}
	depth := 0
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		p.pos++
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if p.peek() != '{' {
		return nil, &gqlErr{"expected {"}
	}
	p.pos++ // {
	var fields []*gqlField
	for {
		c := p.peek()
		if c == 0 {
			return nil, &gqlErr{"unexpected end of document"}
		}
		if c == '}' {
			p.pos++
			return fields, nil
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		if f != nil {
			fields = append(fields, f)
		}
	}
}

func (p *gqlParser) parseField() (*gqlField, error) {
	p.skipWS()
	// fragment spread or inline fragment
	if p.pos+2 < len(p.s) && p.s[p.pos] == '.' && p.s[p.pos+1] == '.' && p.s[p.pos+2] == '.' {
		p.pos += 3
		p.skipWS()
		if p.peek() == '{' {
			// ... { ... } — treat as anonymous fragment, inline its fields
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlField{Name: "__inline__", Sub: sub}, nil
		}
		// ... on Type { ... } or ...FragName
		w := p.ident()
		if w == "on" {
			p.ident() // type name
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlField{Name: "__inline__", Sub: sub}, nil
		}
		// named fragment spread — unsupported in this subset; skip
		return nil, nil
	}
	name := p.ident()
	if name == "" {
		return nil, &gqlErr{"expected field name"}
	}
	f := &gqlField{Name: name, Args: map[string]any{}}
	// alias
	if p.peek() == ':' {
		p.pos++
		f.Alias = name
		f.Name = p.ident()
	}
	// arguments
	if p.peek() == '(' {
		p.pos++
		for {
			p.skipWS()
			if p.peek() == ')' {
				p.pos++
				break
			}
			argName := p.ident()
			p.skipWS()
			if p.peek() == ':' {
				p.pos++
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f.Args[argName] = v
		}
	}
	// sub-selection
	if p.peek() == '{' {
		sub, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.Sub = sub
	}
	return f, nil
}

func (p *gqlParser) parseValue() (any, error) {
	p.skipWS()
	if p.pos >= len(p.s) {
		return nil, &gqlErr{"unexpected end in value"}
	}
	c := p.s[p.pos]
	switch {
	case c == '$':
		p.pos++
		name := p.ident()
		if p.vars != nil {
			if v, ok := p.vars[name]; ok {
				return v, nil
			}
		}
		return nil, nil
	case c == '"':
		return p.parseString()
	case c == '[':
		p.pos++
		var arr []any
		for {
			p.skipWS()
			if p.peek() == ']' {
				p.pos++
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case c == '{':
		p.pos++
		m := map[string]any{}
		for {
			p.skipWS()
			if p.peek() == '}' {
				p.pos++
				break
			}
			k := p.ident()
			p.skipWS()
			if p.peek() == ':' {
				p.pos++
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, nil
	case c == 't' || c == 'f':
		w := p.ident()
		return w == "true", nil
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		for p.pos < len(p.s) && (p.s[p.pos] == '-' || p.s[p.pos] == '.' || (p.s[p.pos] >= '0' && p.s[p.pos] <= '9')) {
			p.pos++
		}
		n, _ := strconv.ParseFloat(p.s[start:p.pos], 64)
		return n, nil
	default:
		// enum identifier
		return p.ident(), nil
	}
}

func (p *gqlParser) parseString() (string, error) {
	// p.s[p.pos] == '"'
	p.pos++
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		p.pos++
		if c == '"' {
			return b.String(), nil
		}
		if c == '\\' && p.pos < len(p.s) {
			esc := p.s[p.pos]
			p.pos++
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'u':
				if p.pos+4 <= len(p.s) {
					n, _ := strconv.ParseInt(p.s[p.pos:p.pos+4], 16, 32)
					b.WriteRune(rune(n))
					p.pos += 4
				}
			default:
				b.WriteByte(esc)
			}
			continue
		}
		b.WriteByte(c)
	}
	return "", &gqlErr{"unterminated string"}
}

type gqlErr struct{ msg string }

func (e *gqlErr) Error() string { return e.msg }

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlExecutor struct {
	vars   map[string]any
	errors []map[string]any
}

func (ex *gqlExecutor) addError(msg string) {
	ex.errors = append(ex.errors, map[string]any{"message": msg})
}

func (ex *gqlExecutor) execute(op *gqlOperation) map[string]any {
	if op.Kind == "mutation" {
		return ex.execFields(op.Fields, "Mutation", nil)
	}
	return ex.execFields(op.Fields, "Query", nil)
}

// ctx values: nil (root), *Repo, *Issue, *PR, userLogin string, "connection" wrapper
func (ex *gqlExecutor) execFields(fields []*gqlField, typ string, ctx any) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.Name == "__inline__" {
			sub := ex.execFields(f.Sub, typ, ctx)
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		key := f.Name
		if f.Alias != "" {
			key = f.Alias
		}
		out[key] = ex.resolveField(f, typ, ctx)
	}
	return out
}

func (ex *gqlExecutor) resolveField(f *gqlField, typ string, ctx any) any {
	switch typ {
	case "Query":
		return ex.resolveQuery(f)
	case "Mutation":
		return ex.resolveMutation(f)
	case "Repository":
		return ex.resolveRepo(f, ctx.(*Repo))
	case "Issue":
		return ex.resolveIssue(f, ctx.(*Issue))
	case "PullRequest":
		return ex.resolvePR(f, ctx.(*PR))
	case "User":
		return ex.resolveUser(f, ctx.(string))
	case "Ref":
		return ex.resolveRef(f, ctx.(string))
	case "IssueConnection":
		return ex.resolveIssueConn(f, ctx.(issueConnCtx))
	case "PullRequestConnection":
		return ex.resolvePRConn(f, ctx.(prConnCtx))
	case "PullRequestChangedFileConnection":
		return ex.resolveFileConn(f, ctx.(fileConnCtx))
	case "PullRequestChangedFile":
		return ex.resolveFile(f, ctx.(FileChange))
	case "CreateIssuePayload", "CloseIssuePayload":
		if f.Name == "issue" {
			return ex.execFields(f.Sub, "Issue", ctx.(*Issue))
		}
		return ex.unknownField(f, typ)
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.Name == "pullRequest" {
			return ex.execFields(f.Sub, "PullRequest", ctx.(*PR))
		}
		return ex.unknownField(f, typ)
	case "AddCommentPayload":
		if f.Name == "commentEdge" {
			return ex.execFields(f.Sub, "IssueCommentEdge", ctx.(*Comment))
		}
		return ex.unknownField(f, typ)
	case "IssueCommentEdge":
		if f.Name == "node" {
			return ex.execFields(f.Sub, "IssueComment", ctx.(*Comment))
		}
		return ex.unknownField(f, typ)
	case "IssueComment":
		c := ctx.(*Comment)
		switch f.Name {
		case "id":
			return globalID("IssueComment", c.ID)
		case "body":
			return c.Body
		}
		return ex.unknownField(f, typ)
	case "PageInfo":
		pi := ctx.(pageInfo)
		switch f.Name {
		case "hasNextPage":
			return pi.hasNext
		case "endCursor":
			return pi.endCursor
		case "hasPreviousPage":
			return false
		case "startCursor":
			return ""
		}
		return ex.unknownField(f, "PageInfo")
	case "__Type":
		return ex.resolveType(f, ctx.(string))
	}
	ex.addError("Cannot query field \"" + f.Name + "\" on type \"" + typ + "\".")
	return nil
}

func (ex *gqlExecutor) unknownField(f *gqlField, typ string) any {
	ex.addError("Cannot query field \"" + f.Name + "\" on type \"" + typ + "\".")
	return nil
}

func argStr(f *gqlField, name string) string {
	if v, ok := f.Args[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(f *gqlField, name string) (int, bool) {
	if v, ok := f.Args[name]; ok {
		if n, ok := v.(float64); ok {
			return int(n), true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

func (ex *gqlExecutor) resolveQuery(f *gqlField) any {
	switch f.Name {
	case "viewer":
		return ex.execFields(f.Sub, "User", viewerLogin)
	case "repository":
		owner := argStr(f, "owner")
		name := argStr(f, "name")
		if owner != viewerLogin {
			return nil
		}
		rp := store.GetRepo(owner, name)
		if rp == nil {
			return nil
		}
		return ex.execFields(f.Sub, "Repository", rp)
	case "node":
		id := argStr(f, "id")
		kind, n := parseGlobalID(id)
		switch kind {
		case "Repository":
			for _, rp := range store.ReposByOwner(viewerLogin) {
				if rp.ID == n {
					return ex.execFields(f.Sub, "Repository", rp)
				}
			}
		case "Issue":
			for _, rp := range store.ReposByOwner(viewerLogin) {
				for _, iss := range rp.Issues {
					if iss.ID == n {
						return ex.execFields(f.Sub, "Issue", iss)
					}
				}
			}
		case "PullRequest":
			for _, rp := range store.ReposByOwner(viewerLogin) {
				for _, pr := range rp.Pulls {
					if pr.ID == n {
						return ex.execFields(f.Sub, "PullRequest", pr)
					}
				}
			}
		}
		return nil
	case "__type":
		return ex.execFields(f.Sub, "__Type", argStr(f, "name"))
	case "__schema":
		return map[string]any{}
	}
	return ex.unknownField(f, "Query")
}

func parseGlobalID(id string) (string, int) {
	// "R_<Kind>_<n>"
	if !strings.HasPrefix(id, "R_") {
		return "", 0
	}
	rest := strings.TrimPrefix(id, "R_")
	i := strings.LastIndex(rest, "_")
	if i < 0 {
		return "", 0
	}
	n, err := strconv.Atoi(rest[i+1:])
	if err != nil {
		return "", 0
	}
	return rest[:i], n
}

// ---------------------------------------------------------------------------
// Mutation
// ---------------------------------------------------------------------------

func (ex *gqlExecutor) resolveMutation(f *gqlField) any {
	switch f.Name {
	case "createIssue":
		input, _ := f.Args["input"].(map[string]any)
		repoID := ""
		if v, ok := input["repositoryId"]; ok {
			repoID, _ = v.(string)
		}
		kind, n := parseGlobalID(repoID)
		if kind != "Repository" {
			return map[string]any{"issue": nil}
		}
		var rp *Repo
		for _, r := range store.ReposByOwner(viewerLogin) {
			if r.ID == n {
				rp = r
				break
			}
		}
		if rp == nil {
			return map[string]any{"issue": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		store.mu.Lock()
		rp.NumSeq++
		num := rp.NumSeq
		seq := store.nextSeq()
		id := 2000000 + int(seq)
		iss := &Issue{ID: id, GID: globalID("Issue", id), Number: num, Title: title, Body: body, State: "open", Seq: seq, TS: tsOf(seq)}
		rp.Issues = append(rp.Issues, iss)
		rp.IssueByNum[num] = iss
		store.mu.Unlock()
		return ex.execFields(f.Sub, "CreateIssuePayload", iss)
	case "closeIssue":
		input, _ := f.Args["input"].(map[string]any)
		issueID, _ := input["issueId"].(string)
		kind, n := parseGlobalID(issueID)
		var iss *Issue
		if kind == "Issue" {
			for _, rp := range store.ReposByOwner(viewerLogin) {
				for _, i := range rp.Issues {
					if i.ID == n {
						iss = i
					}
				}
			}
		}
		if iss == nil {
			return map[string]any{"issue": nil}
		}
		store.mu.Lock()
		iss.State = "closed"
		store.mu.Unlock()
		return ex.execFields(f.Sub, "CloseIssuePayload", iss)
	case "createPullRequest":
		input, _ := f.Args["input"].(map[string]any)
		repoID, _ := input["repositoryId"].(string)
		kind, n := parseGlobalID(repoID)
		if kind != "Repository" {
			return map[string]any{"pullRequest": nil}
		}
		var rp *Repo
		for _, r := range store.ReposByOwner(viewerLogin) {
			if r.ID == n {
				rp = r
				break
			}
		}
		if rp == nil {
			return map[string]any{"pullRequest": nil}
		}
		head, _ := input["headRefName"].(string)
		base, _ := input["baseRefName"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		headSHA := rp.Refs["refs/heads/"+head]
		baseSHA := rp.Refs["refs/heads/"+base]
		if headSHA == "" || baseSHA == "" {
			return map[string]any{"pullRequest": nil}
		}
		store.mu.Lock()
		rp.NumSeq++
		num := rp.NumSeq
		seq := store.nextSeq()
		id := 4000000 + int(seq)
		pr := &PR{ID: id, GID: globalID("PullRequest", id), Number: num, Title: title, Body: body,
			Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA, State: "open", Seq: seq}
		rp.Pulls = append(rp.Pulls, pr)
		rp.PRByNum[num] = pr
		store.mu.Unlock()
		return ex.execFields(f.Sub, "CreatePullRequestPayload", pr)
	case "mergePullRequest":
		input, _ := f.Args["input"].(map[string]any)
		prID, _ := input["pullRequestId"].(string)
		kind, n := parseGlobalID(prID)
		var pr *PR
		var rp *Repo
		if kind == "PullRequest" {
			for _, r := range store.ReposByOwner(viewerLogin) {
				for _, p := range r.Pulls {
					if p.ID == n {
						pr = p
						rp = r
					}
				}
			}
		}
		if pr == nil || pr.State == "closed" {
			return map[string]any{"pullRequest": nil}
		}
		if prConflict(rp, pr) {
			ex.addError("Merge conflict")
			return map[string]any{"pullRequest": nil}
		}
		store.mu.Lock()
		headSHA := prHeadSHA(rp, pr)
		baseSHA := prBaseSHA(rp, pr)
		base := rp.mergeBase(headSHA, baseSHA)
		newTree := rp.commitTree(baseSHA)
		for _, fc := range rp.diffTrees(rp.commitTree(base), rp.commitTree(headSHA)) {
			if fc.Status == "deleted" {
				newTree = rp.setFile(newTree, fc.Path, "")
			} else {
				newTree = rp.setFile(newTree, fc.Path, fc.NewOID)
			}
		}
		seq := store.nextSeq()
		mergeSHA := rp.makeCommit(newTree, []string{baseSHA}, "Merge pull request #"+strconv.Itoa(pr.Number)+" from "+pr.Head+"\n\n"+pr.Title, seq)
		rp.Refs["refs/heads/"+pr.Base] = mergeSHA
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = tsOf(seq)
		pr.MergeSHA = mergeSHA
		store.mu.Unlock()
		return ex.execFields(f.Sub, "MergePullRequestPayload", pr)
	case "addComment":
		input, _ := f.Args["input"].(map[string]any)
		subjectID, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)
		kind, n := parseGlobalID(subjectID)
		if kind == "Issue" {
			for _, rp := range store.ReposByOwner(viewerLogin) {
				for _, iss := range rp.Issues {
					if iss.ID == n {
						store.mu.Lock()
						rp.CommentSeq++
						c := &Comment{ID: 3000000 + rp.CommentSeq, Seq: store.nextSeq(), Body: body}
						iss.Comments = append(iss.Comments, c)
						store.mu.Unlock()
						return ex.execFields(f.Sub, "AddCommentPayload", c)
					}
				}
			}
		}
		return map[string]any{"comment": nil}
	}
	return ex.unknownField(f, "Mutation")
}

// ---------------------------------------------------------------------------
// Type resolvers
// ---------------------------------------------------------------------------

func (ex *gqlExecutor) resolveRepo(f *gqlField, rp *Repo) any {
	switch f.Name {
	case "id":
		return rp.GID
	case "databaseId":
		return rp.ID
	case "name":
		return rp.Name
	case "nameWithOwner":
		return rp.Owner + "/" + rp.Name
	case "owner":
		return ex.execFields(f.Sub, "User", rp.Owner)
	case "description":
		return rp.Description
	case "private":
		return rp.Private
	case "url":
		return "https://example.invalid/" + rp.Owner + "/" + rp.Name
	case "defaultBranchRef":
		if rp.Refs["refs/heads/"+rp.DefaultBranch] == "" {
			return nil
		}
		return ex.execFields(f.Sub, "Ref", rp.DefaultBranch)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		num, ok := argInt(f, "number")
		if !ok {
			return nil
		}
		iss := rp.IssueByNum[num]
		if iss == nil {
			return nil
		}
		return ex.execFields(f.Sub, "Issue", iss)
	case "issues":
		issues := filterIssues(rp, f)
		first, hasFirst := argInt(f, "first")
		return ex.execFields(f.Sub, "IssueConnection", issueConnCtx{items: issues, first: first, hasFirst: hasFirst, after: argStr(f, "after")})
	case "pullRequests":
		prs := filterPRs(rp, f)
		first, hasFirst := argInt(f, "first")
		return ex.execFields(f.Sub, "PullRequestConnection", prConnCtx{items: prs, first: first, hasFirst: hasFirst, after: argStr(f, "after")})
	case "pullRequest":
		num, ok := argInt(f, "number")
		if !ok {
			return nil
		}
		pr := rp.PRByNum[num]
		if pr == nil {
			return nil
		}
		return ex.execFields(f.Sub, "PullRequest", pr)
	}
	return ex.unknownField(f, "Repository")
}

func filterIssues(rp *Repo, f *gqlField) []*Issue {
	// states filter
	states := map[string]bool{}
	if v, ok := f.Args["states"]; ok {
		if arr, ok := v.([]any); ok {
			for _, s := range arr {
				if str, ok := s.(string); ok {
					states[str] = true
				}
			}
		}
	}
	var list []*Issue
	for _, iss := range rp.Issues {
		if len(states) > 0 && !states[strings.ToUpper(iss.State)] {
			continue
		}
		list = append(list, iss)
	}
	// orderBy CREATED_AT DESC
	if v, ok := f.Args["orderBy"]; ok {
		if m, ok := v.(map[string]any); ok {
			if dir, ok := m["direction"].(string); ok && dir == "DESC" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
	}
	return list
}

func filterPRs(rp *Repo, f *gqlField) []*PR {
	states := map[string]bool{}
	if v, ok := f.Args["states"]; ok {
		if arr, ok := v.([]any); ok {
			for _, s := range arr {
				if str, ok := s.(string); ok {
					states[str] = true
				}
			}
		}
	}
	var list []*PR
	for _, pr := range rp.Pulls {
		if len(states) > 0 && !states[strings.ToUpper(pr.State)] {
			continue
		}
		list = append(list, pr)
	}
	return list
}

func (ex *gqlExecutor) resolveIssue(f *gqlField, iss *Issue) any {
	switch f.Name {
	case "id":
		return iss.GID
	case "databaseId":
		return iss.ID
	case "number":
		return iss.Number
	case "title":
		return iss.Title
	case "body":
		return iss.Body
	case "state":
		return strings.ToUpper(iss.State)
	case "url":
		return "https://example.invalid/issues/" + strconv.Itoa(iss.Number)
	case "author":
		return ex.execFields(f.Sub, "User", viewerLogin)
	}
	return ex.unknownField(f, "Issue")
}

func (ex *gqlExecutor) resolvePR(f *gqlField, pr *PR) any {
	switch f.Name {
	case "id":
		return pr.GID
	case "databaseId":
		return pr.ID
	case "number":
		return pr.Number
	case "title":
		return pr.Title
	case "body":
		return pr.Body
	case "state":
		if pr.Merged {
			return "MERGED"
		}
		return strings.ToUpper(pr.State)
	case "merged":
		return pr.Merged
	case "mergeable":
		return "MERGEABLE"
	case "headRefName":
		return pr.Head
	case "baseRefName":
		return pr.Base
	case "headRefOid":
		// resolve live via the owning repo if possible
		for _, r := range store.ReposByOwner(viewerLogin) {
			for _, p := range r.Pulls {
				if p == pr {
					return prHeadSHA(r, pr)
				}
			}
		}
		return pr.HeadSHA
	case "baseRefOid":
		for _, r := range store.ReposByOwner(viewerLogin) {
			for _, p := range r.Pulls {
				if p == pr {
					return prBaseSHA(r, pr)
				}
			}
		}
		return pr.BaseSHA
	case "files":
		// find repo for diff — search all repos for this PR
		var rp *Repo
		for _, r := range store.ReposByOwner(viewerLogin) {
			for _, p := range r.Pulls {
				if p == pr {
					rp = r
				}
			}
		}
		var changes []FileChange
		if rp != nil {
			base := rp.mergeBase(prHeadSHA(rp, pr), prBaseSHA(rp, pr))
			changes = rp.diffTrees(rp.commitTree(base), rp.commitTree(prHeadSHA(rp, pr)))
		}
		first, hasFirst := argInt(f, "first")
		return ex.execFields(f.Sub, "PullRequestChangedFileConnection", fileConnCtx{items: changes, first: first, hasFirst: hasFirst, after: argStr(f, "after")})
	case "author":
		return ex.execFields(f.Sub, "User", viewerLogin)
	}
	return ex.unknownField(f, "PullRequest")
}

func (ex *gqlExecutor) resolveUser(f *gqlField, login string) any {
	switch f.Name {
	case "login":
		return login
	case "id":
		if login == viewerLogin {
			return globalID("User", viewerID)
		}
		return globalID("User", 1002)
	case "databaseId":
		if login == viewerLogin {
			return viewerID
		}
		return 1002
	case "name":
		return "Ada Iversen"
	}
	return ex.unknownField(f, "User")
}

func (ex *gqlExecutor) resolveRef(f *gqlField, name string) any {
	switch f.Name {
	case "name":
		return name
	}
	return ex.unknownField(f, "Ref")
}

// Connection contexts carry the parent field's first/after arguments, since
// the suite puts pagination on the connection field, not on nodes/pageInfo.
type issueConnCtx struct {
	items    []*Issue
	first    int
	hasFirst bool
	after    string
}

type prConnCtx struct {
	items    []*PR
	first    int
	hasFirst bool
	after    string
}

type fileConnCtx struct {
	items    []FileChange
	first    int
	hasFirst bool
	after    string
}

func connStart(after string) int {
	if after == "" {
		return 0
	}
	if n, err := strconv.Atoi(after); err == nil {
		return n
	}
	return 0
}

func connEnd(total, start, first int, hasFirst bool) int {
	end := total
	if hasFirst && start+first < end {
		end = start + first
	}
	if end < start {
		end = start
	}
	return end
}

func (ex *gqlExecutor) resolveIssueConn(f *gqlField, c issueConnCtx) any {
	switch f.Name {
	case "nodes":
		start := connStart(c.after)
		end := connEnd(len(c.items), start, c.first, c.hasFirst)
		var nodes []any
		for _, iss := range c.items[start:end] {
			nodes = append(nodes, ex.execFields(f.Sub, "Issue", iss))
		}
		if nodes == nil {
			nodes = []any{}
		}
		return nodes
	case "pageInfo":
		start := connStart(c.after)
		end := connEnd(len(c.items), start, c.first, c.hasFirst)
		hasNext := c.hasFirst && start+c.first < len(c.items)
		endCursor := ""
		if end > start {
			endCursor = strconv.Itoa(end)
		}
		return ex.execFields(f.Sub, "PageInfo", pageInfo{hasNext, endCursor})
	case "totalCount":
		return len(c.items)
	}
	return ex.unknownField(f, "IssueConnection")
}

type pageInfo struct {
	hasNext   bool
	endCursor string
}

func (ex *gqlExecutor) resolvePRConn(f *gqlField, c prConnCtx) any {
	switch f.Name {
	case "nodes":
		start := connStart(c.after)
		end := connEnd(len(c.items), start, c.first, c.hasFirst)
		var nodes []any
		for _, pr := range c.items[start:end] {
			nodes = append(nodes, ex.execFields(f.Sub, "PullRequest", pr))
		}
		if nodes == nil {
			nodes = []any{}
		}
		return nodes
	case "pageInfo":
		start := connStart(c.after)
		end := connEnd(len(c.items), start, c.first, c.hasFirst)
		hasNext := c.hasFirst && start+c.first < len(c.items)
		endCursor := ""
		if end > start {
			endCursor = strconv.Itoa(end)
		}
		return ex.execFields(f.Sub, "PageInfo", pageInfo{hasNext, endCursor})
	case "totalCount":
		return len(c.items)
	}
	return ex.unknownField(f, "PullRequestConnection")
}

func (ex *gqlExecutor) resolveFileConn(f *gqlField, c fileConnCtx) any {
	switch f.Name {
	case "nodes":
		start := connStart(c.after)
		end := connEnd(len(c.items), start, c.first, c.hasFirst)
		var nodes []any
		for _, fc := range c.items[start:end] {
			nodes = append(nodes, ex.execFields(f.Sub, "PullRequestChangedFile", fc))
		}
		if nodes == nil {
			nodes = []any{}
		}
		return nodes
	case "totalCount":
		return len(c.items)
	}
	return ex.unknownField(f, "PullRequestChangedFileConnection")
}

func (ex *gqlExecutor) resolveFile(f *gqlField, fc FileChange) any {
	switch f.Name {
	case "path":
		return fc.Path
	case "additions":
		return fc.Additions
	case "deletions":
		return fc.Deletions
	case "changeType":
		return strings.ToUpper(fc.Status)
	}
	return ex.unknownField(f, "PullRequestChangedFile")
}

// PageInfo is resolved via a synthetic type; handled in resolveField.

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "databaseId", "number", "title", "state", "body", "url", "author", "createdAt", "updatedAt", "labels", "comments", "closedAt"},
	"PullRequest": {"id", "databaseId", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "author", "createdAt", "updatedAt", "mergedAt"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt", "updatedAt"},
	"User":        {"id", "databaseId", "login", "name", "email", "avatarUrl"},
	"Ref":         {"name", "target"},
}

func (ex *gqlExecutor) resolveType(f *gqlField, name string) any {
	switch f.Name {
	case "name":
		return name
	case "fields":
		fields, ok := gqlTypeFields[name]
		if !ok {
			return nil
		}
		var out []any
		for _, fn := range fields {
			out = append(out, map[string]any{"name": fn})
		}
		return out
	case "enumValues":
		if name == "SearchType" {
			var out []any
			for _, v := range []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"} {
				out = append(out, map[string]any{"name": v})
			}
			return out
		}
		return nil
	case "kind":
		return "OBJECT"
	}
	return ex.unknownField(f, "__Type")
}
