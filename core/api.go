package main

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// REST v3 — path is the part after /api/v3/
// ---------------------------------------------------------------------------

func restHandler(w http.ResponseWriter, r *http.Request, path string) {
	if !auth(w, r) {
		return
	}
	seg := splitPath(path)
	if len(seg) == 0 {
		writeJSON(w, 200, map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"})
		return
	}
	switch seg[0] {
	case "user":
		restUser(w, r, seg[1:])
	case "repos":
		restRepos(w, r, seg[1:])
	default:
		errJSON(w, 404, "Not Found")
	}
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if u, err := url.PathUnescape(s); err == nil {
			parts[i] = u
		}
	}
	return parts
}

func restUser(w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) == 0 {
		if r.Method != "GET" {
			errJSON(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, userJSON())
		return
	}
	if seg[0] == "repos" {
		if r.Method == "GET" {
			repos := store.ReposByOwner(viewerLogin)
			var items []any
			for _, rp := range repos {
				items = append(items, repoJSON(rp))
			}
			page, hasNext := paginate(items, r)
			if hasNext {
				pageNum := 1
				if v := r.URL.Query().Get("page"); v != "" {
					pageNum, _ = strconv.Atoi(v)
				}
				linkHeader(w, r, pageNum+1)
			}
			writeJSON(w, 200, page)
			return
		}
		if r.Method == "POST" {
			m, err := readJSONBody(w, r)
			if err != nil {
				return
			}
			name := strField(m, "name")
			if name == "" {
				errJSON(w, 422, "Validation Failed: name is required")
				return
			}
			rp := store.CreateRepo(viewerLogin, name, strField(m, "description"), boolField(m, "private"))
			if rp == nil {
				errJSON(w, 422, "Validation Failed: name already exists on this account")
				return
			}
			writeJSON(w, 201, repoJSON(rp))
			return
		}
	}
	errJSON(w, 404, "Not Found")
}

func userJSON() map[string]any {
	return map[string]any{
		"id": viewerID, "login": viewerLogin, "node_id": globalID("User", viewerID),
		"name": "Ada Iversen", "email": "adaiversen@example.invalid", "type": "User",
		"avatar_url": "https://example.invalid/avatar", "html_url": "https://example.invalid/" + viewerLogin,
		"url": "/api/v3/users/" + viewerLogin, "site_admin": false,
	}
}

func repoJSON(rp *Repo) map[string]any {
	return map[string]any{
		"id": rp.ID, "node_id": rp.GID, "name": rp.Name, "full_name": rp.Owner + "/" + rp.Name,
		"owner":            map[string]any{"login": rp.Owner, "id": viewerID, "type": "User"},
		"private":          rp.Private,
		"description":      rp.Description,
		"fork":             false,
		"created_at":       tsOf(rp.CreatedSeq),
		"updated_at":       tsOf(rp.CreatedSeq),
		"pushed_at":        tsOf(rp.CreatedSeq),
		"homepage":         "",
		"size":             0,
		"stargazers_count": 0, "watchers_count": 0, "language": "", "forks_count": 0,
		"open_issues_count": len(rp.Issues),
		"license":           nil,
		"default_branch":    rp.DefaultBranch,
		"has_issues":        true,
		"html_url":          "https://example.invalid/" + rp.Owner + "/" + rp.Name,
		"url":               "/api/v3/repos/" + rp.Owner + "/" + rp.Name,
	}
}

func issueJSON(rp *Repo, iss *Issue) map[string]any {
	var labels []any
	for _, l := range iss.Labels {
		labels = append(labels, map[string]any{"name": l, "color": "ededed"})
	}
	if labels == nil {
		labels = []any{}
	}
	return map[string]any{
		"id": iss.ID, "node_id": iss.GID, "number": iss.Number,
		"title": iss.Title, "body": iss.Body, "state": iss.State,
		"labels": labels, "comments": len(iss.Comments),
		"user":       map[string]any{"login": viewerLogin, "id": viewerID},
		"created_at": tsOf(iss.Seq), "updated_at": tsOf(iss.Seq),
		"closed_at":      nil,
		"repository_url": "/api/v3/repos/" + rp.Owner + "/" + rp.Name,
		"url":            "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/issues/" + strconv.Itoa(iss.Number),
		"html_url":       "https://example.invalid/" + rp.Owner + "/" + rp.Name + "/issues/" + strconv.Itoa(iss.Number),
	}
}

// appendJSONString appends s as a JSON string literal. Fast path when no
// byte needs escaping (the common case for titles, bodies and labels);
// the slow path escapes exactly what JSON requires (", \, control chars).
// Unlike encoding/json it does not \u-escape <, > or & — JSON parsers read
// both forms identically, and every consumer here parses the JSON.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			dst = append(dst, s[start:i]...)
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				const hexd = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hexd[c>>4], hexd[c&0xf])
			}
			start = i + 1
		}
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// issueJSONBytes renders one issue as JSON without reflection or map
// allocation. Field set and values are identical to issueJSON; only the
// encoder differs. Used on the hot REST paths (GET/POST/PATCH one issue).
// iss.TS is the precomputed created_at/updated_at (both are tsOf(Seq)).
func issueJSONBytes(rp *Repo, iss *Issue) []byte {
	ts := iss.TS
	if ts == "" {
		ts = tsOf(iss.Seq)
	}
	b := make([]byte, 0, 768)
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, int64(iss.ID), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, iss.GID)
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(iss.Number), 10)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, iss.Title)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, iss.Body)
	b = append(b, `,"state":`...)
	b = appendJSONString(b, iss.State)
	b = append(b, `,"labels":[`...)
	for i, l := range iss.Labels {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = appendJSONString(b, l)
		b = append(b, `,"color":"ededed"}`...)
	}
	b = append(b, `],"comments":`...)
	b = strconv.AppendInt(b, int64(len(iss.Comments)), 10)
	b = append(b, `,"user":{"login":"adaiversen","id":1000}`...)
	b = append(b, `,"created_at":`...)
	b = appendJSONString(b, ts)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, ts)
	b = append(b, `,"closed_at":null,"repository_url":`...)
	repoURL := "/api/v3/repos/" + rp.Owner + "/" + rp.Name
	b = appendJSONString(b, repoURL)
	b = append(b, `,"url":`...)
	b = appendJSONString(b, repoURL+"/issues/"+strconv.Itoa(iss.Number))
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://example.invalid/"+rp.Owner+"/"+rp.Name+"/issues/"+strconv.Itoa(iss.Number))
	b = append(b, '}')
	return b
}

func prJSON(rp *Repo, pr *PR) map[string]any {
	mergedAt := any(nil)
	if pr.Merged {
		mergedAt = pr.MergedAt
	}
	headSHA := prHeadSHA(rp, pr)
	baseSHA := prBaseSHA(rp, pr)
	return map[string]any{
		"id": pr.ID, "node_id": pr.GID, "number": pr.Number,
		"title": pr.Title, "body": pr.Body, "state": pr.State,
		"merged": pr.Merged, "mergeable": prMergeable(rp, pr),
		"merge_commit_sha": pr.MergeSHA,
		"merged_at":        mergedAt,
		"created_at":       tsOf(pr.Seq), "updated_at": tsOf(pr.Seq),
		"user": map[string]any{"login": viewerLogin, "id": viewerID},
		"head": map[string]any{"ref": pr.Head, "sha": headSHA, "label": rp.Owner + ":" + pr.Head,
			"repo": map[string]any{"full_name": rp.Owner + "/" + rp.Name}},
		"base": map[string]any{"ref": pr.Base, "sha": baseSHA, "label": rp.Owner + ":" + pr.Base,
			"repo": map[string]any{"full_name": rp.Owner + "/" + rp.Name}},
		"html_url": "https://example.invalid/" + rp.Owner + "/" + rp.Name + "/pull/" + strconv.Itoa(pr.Number),
		"url":      "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/pulls/" + strconv.Itoa(pr.Number),
	}
}

// prHeadSHA / prBaseSHA resolve the pull request's branches live, so pushes
// made after the PR was created are reflected in files/merge/conflict
// checks. Falls back to the SHA recorded at creation if a branch is gone.
func prHeadSHA(rp *Repo, pr *PR) string {
	if sha := rp.Refs["refs/heads/"+pr.Head]; sha != "" {
		return sha
	}
	return pr.HeadSHA
}

func prBaseSHA(rp *Repo, pr *PR) string {
	if sha := rp.Refs["refs/heads/"+pr.Base]; sha != "" {
		return sha
	}
	return pr.BaseSHA
}

func prMergeable(rp *Repo, pr *PR) string {
	if pr.Merged {
		return "MERGEABLE"
	}
	if prConflict(rp, pr) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// prConflict: both sides changed the same path differently since merge-base.
func prConflict(rp *Repo, pr *PR) bool {
	headSHA := prHeadSHA(rp, pr)
	baseSHA := prBaseSHA(rp, pr)
	base := rp.mergeBase(headSHA, baseSHA)
	if base == "" {
		return false
	}
	b2h := map[string]FileChange{}
	for _, fc := range rp.diffTrees(rp.commitTree(base), rp.commitTree(headSHA)) {
		b2h[fc.Path] = fc
	}
	for _, fc := range rp.diffTrees(rp.commitTree(base), rp.commitTree(baseSHA)) {
		if other, ok := b2h[fc.Path]; ok {
			if other.NewOID != fc.NewOID || other.Status != fc.Status {
				return true
			}
		}
	}
	return false
}

// resolveRepo loads a repo; the owner must be the viewer, else 404.
func resolveRepo(w http.ResponseWriter, owner, name string) *Repo {
	if owner != viewerLogin {
		errJSON(w, 404, "Not Found")
		return nil
	}
	rp := store.GetRepo(owner, name)
	if rp == nil {
		errJSON(w, 404, "Not Found")
		return nil
	}
	return rp
}

// resolveRef returns the commit oid for a branch name or oid; "" if unknown.
func resolveRef(rp *Repo, ref string) string {
	if ref == "" {
		return rp.Refs["refs/heads/"+rp.DefaultBranch]
	}
	if oid := rp.Refs["refs/heads/"+ref]; oid != "" {
		return oid
	}
	if rp.getObj(ref) != nil {
		return ref
	}
	return ""
}

func restRepos(w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) < 2 {
		errJSON(w, 404, "Not Found")
		return
	}
	owner, name := seg[0], seg[1]
	rp := resolveRepo(w, owner, name)
	if rp == nil {
		return
	}
	rest := seg[2:]

	// /repos/{o}/{r}
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			writeJSON(w, 200, repoJSON(rp))
		case "PATCH":
			m, err := readJSONBody(w, r)
			if err != nil {
				return
			}
			store.mu.Lock()
			if d, ok := m["description"]; ok {
				if s, ok := d.(string); ok {
					rp.Description = s
				}
			}
			if v, ok := m["default_branch"]; ok {
				if s, ok := v.(string); ok && s != "" {
					rp.DefaultBranch = s
				}
			}
			store.mu.Unlock()
			writeJSON(w, 200, repoJSON(rp))
		case "DELETE":
			store.DeleteRepo(owner, name)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(204)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}

	switch rest[0] {
	case "branches":
		restBranches(w, r, rp, rest[1:])
	case "commits":
		if r.Method == "GET" {
			sha := r.URL.Query().Get("sha")
			tip := resolveRef(rp, sha)
			if tip == "" {
				writeJSON(w, 200, []any{})
				return
			}
			var items []any
			for _, oid := range rp.listCommits(tip) {
				items = append(items, commitJSON(rp, oid))
			}
			page, _ := paginate(items, r)
			writeJSON(w, 200, page)
			return
		}
		errJSON(w, 404, "Not Found")
	case "git":
		restGit(w, r, rp, rest[1:])
	case "contents":
		restContents(w, r, rp, rest[1:])
	case "labels":
		restLabels(w, r, rp)
	case "issues":
		restIssues(w, r, rp, rest[1:])
	case "pulls":
		restPulls(w, r, rp, rest[1:])
	default:
		errJSON(w, 404, "Not Found")
	}
}

func commitJSON(rp *Repo, oid string) map[string]any {
	o := rp.getObj(oid)
	if o == nil {
		return map[string]any{"sha": oid}
	}
	c := o.Commit()
	author, committer := parseIdent(c.Author), parseIdent(c.Committer)
	var parents []any
	for _, p := range c.Parents {
		parents = append(parents, map[string]any{"sha": p})
	}
	if parents == nil {
		parents = []any{}
	}
	return map[string]any{
		"sha": oid,
		"commit": map[string]any{
			"message": c.Message,
			"author":  author, "committer": committer,
		},
		"author":    map[string]any{"login": viewerLogin, "id": viewerID},
		"committer": map[string]any{"login": viewerLogin, "id": viewerID},
		"parents":   parents,
		"url":       "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/commits/" + oid,
	}
}

func parseIdent(s string) map[string]any {
	// "Name <email> ts +0000"
	name, email, date := s, "", ""
	if i := strings.Index(s, "<"); i >= 0 {
		name = strings.TrimSpace(s[:i])
		rest := s[i+1:]
		if j := strings.Index(rest, ">"); j >= 0 {
			email = rest[:j]
			tsPart := strings.Fields(strings.TrimSpace(rest[j+1:]))
			if len(tsPart) > 0 {
				if n, err := strconv.ParseInt(tsPart[0], 10, 64); err == nil {
					date = tsOf(n - 1700000000)
				}
			}
		}
	}
	return map[string]any{"name": name, "email": email, "date": date}
}

func restBranches(w http.ResponseWriter, r *http.Request, rp *Repo, rest []string) {
	if len(rest) == 0 {
		if r.Method != "GET" {
			errJSON(w, 404, "Not Found")
			return
		}
		var names []string
		for ref := range rp.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
			}
		}
		sort.Strings(names)
		var items []any
		for _, n := range names {
			items = append(items, branchJSON(rp, n))
		}
		if items == nil {
			items = []any{}
		}
		writeJSON(w, 200, items)
		return
	}
	if r.Method != "GET" {
		errJSON(w, 404, "Not Found")
		return
	}
	name := strings.Join(rest, "/")
	oid := rp.Refs["refs/heads/"+name]
	if oid == "" {
		errJSON(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, branchJSON(rp, name))
}

func branchJSON(rp *Repo, name string) map[string]any {
	return map[string]any{
		"name":      name,
		"commit":    map[string]any{"sha": rp.Refs["refs/heads/"+name]},
		"protected": false,
	}
}

func restGit(w http.ResponseWriter, r *http.Request, rp *Repo, rest []string) {
	if len(rest) == 0 || rest[0] != "refs" {
		errJSON(w, 404, "Not Found")
		return
	}
	rest = rest[1:]
	switch r.Method {
	case "GET":
		// /git/refs or /git/refs/heads/{branch}
		if len(rest) == 0 {
			var items []any
			var refs []string
			for ref := range rp.Refs {
				refs = append(refs, ref)
			}
			sort.Strings(refs)
			for _, ref := range refs {
				items = append(items, map[string]any{"ref": ref, "object": map[string]any{"sha": rp.Refs[ref], "type": "commit"}})
			}
			if items == nil {
				items = []any{}
			}
			writeJSON(w, 200, items)
			return
		}
		ref := "refs/" + strings.Join(rest, "/")
		oid, ok := rp.Refs[ref]
		if !ok {
			errJSON(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"ref": ref, "object": map[string]any{"sha": oid, "type": "commit"}})
	case "POST":
		m, err := readJSONBody(w, r)
		if err != nil {
			return
		}
		ref := strField(m, "ref")
		sha := strField(m, "sha")
		if ref == "" || sha == "" {
			errJSON(w, 422, "Validation Failed")
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, exists := rp.Refs[ref]; exists {
			errJSON(w, 422, "Validation Failed: Reference already exists")
			return
		}
		if rp.getObj(sha) == nil {
			errJSON(w, 422, "Validation Failed: Invalid SHA")
			return
		}
		rp.Refs[ref] = sha
		writeJSON(w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}})
	default:
		errJSON(w, 404, "Not Found")
	}
}

func restContents(w http.ResponseWriter, r *http.Request, rp *Repo, rest []string) {
	path := strings.Join(rest, "/")
	ref := r.URL.Query().Get("ref")
	switch r.Method {
	case "GET":
		tip := resolveRef(rp, ref)
		if tip == "" {
			errJSON(w, 404, "Not Found")
			return
		}
		tree := rp.commitTree(tip)
		if path == "" {
			writeJSON(w, 200, dirListing(rp, tree, ""))
			return
		}
		if blob, ok := rp.getFile(tree, path); ok {
			o := rp.getObj(blob)
			writeJSON(w, 200, map[string]any{
				"type": "file", "encoding": "base64", "size": len(o.Data),
				"name": baseName(path), "path": path, "sha": blob,
				"content": base64.StdEncoding.EncodeToString(o.Data),
			})
			return
		}
		// directory?
		flat := rp.flattenTree(tree)
		prefix := path + "/"
		found := false
		for p := range flat {
			if strings.HasPrefix(p, prefix) {
				found = true
				break
			}
		}
		if !found {
			errJSON(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, dirListingAt(rp, tree, path))
	case "PUT", "DELETE":
		m, err := readJSONBody(w, r)
		if err != nil {
			return
		}
		branch := strField(m, "branch")
		if branch == "" {
			branch = rp.DefaultBranch
		}
		tip := rp.Refs["refs/heads/"+branch]
		var tree string
		if tip != "" {
			tree = rp.commitTree(tip)
		}
		curBlob, exists := rp.getFile(tree, path)
		if r.Method == "DELETE" {
			if !exists {
				errJSON(w, 404, "Not Found")
				return
			}
			store.mu.Lock()
			newTree := rp.setFile(tree, path, "")
			seq := store.nextSeq()
			parents := []string{}
			if tip != "" {
				parents = []string{tip}
			}
			sha := rp.makeCommit(newTree, parents, strField(m, "message"), seq)
			rp.Refs["refs/heads/"+branch] = sha
			store.mu.Unlock()
			writeJSON(w, 200, map[string]any{"commit": map[string]any{"sha": sha}})
			return
		}
		// PUT
		sha := strField(m, "sha")
		if exists && sha != "" && sha != curBlob {
			errJSON(w, 409, "Conflict: sha does not match")
			return
		}
		contentB64 := strField(m, "content")
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(contentB64, "\n", ""))
		if err != nil {
			errJSON(w, 422, "Validation Failed: invalid base64 content")
			return
		}
		msg := strField(m, "message")
		if msg == "" {
			msg = "Update " + path
		}
		store.mu.Lock()
		blobOID := rp.putObj("blob", raw)
		newTree := rp.setFile(tree, path, blobOID)
		seq := store.nextSeq()
		parents := []string{}
		if tip != "" {
			parents = []string{tip}
		}
		commitSHA := rp.makeCommit(newTree, parents, msg, seq)
		rp.Refs["refs/heads/"+branch] = commitSHA
		store.mu.Unlock()
		status := 201
		if exists {
			status = 200
		}
		writeJSON(w, status, map[string]any{
			"content": map[string]any{
				"type": "file", "encoding": "base64", "size": len(raw),
				"name": baseName(path), "path": path, "sha": blobOID,
				"content": contentB64,
			},
			"commit": map[string]any{"sha": commitSHA, "message": msg},
		})
	default:
		errJSON(w, 404, "Not Found")
	}
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// dirListing lists the root (or a subtree) as GitHub content entries.
func dirListing(rp *Repo, treeOID, prefix string) []any {
	return dirListingAt(rp, treeOID, prefix)
}

func dirListingAt(rp *Repo, treeOID, path string) []any {
	o := rp.getObj(treeOID)
	if o == nil {
		return []any{}
	}
	cur := o
	if path != "" {
		parts := strings.Split(path, "/")
		for _, part := range parts {
			var next string
			for _, e := range cur.Tree() {
				if e.Name == part && e.IsDir {
					next = e.OID
					break
				}
			}
			if next == "" {
				return []any{}
			}
			cur = rp.getObj(next)
			if cur == nil {
				return []any{}
			}
		}
	}
	var items []any
	for _, e := range cur.Tree() {
		typ := "file"
		if e.IsDir {
			typ = "dir"
		}
		size := 0
		if !e.IsDir {
			if bo := rp.getObj(e.OID); bo != nil {
				size = len(bo.Data)
			}
		}
		p := e.Name
		if path != "" {
			p = path + "/" + e.Name
		}
		items = append(items, map[string]any{
			"type": typ, "name": e.Name, "path": p, "sha": e.OID, "size": size,
		})
	}
	if items == nil {
		items = []any{}
	}
	return items
}

func restLabels(w http.ResponseWriter, r *http.Request, rp *Repo) {
	switch r.Method {
	case "GET":
		var items []any
		for _, l := range rp.Labels {
			items = append(items, map[string]any{"name": l.Name, "color": l.Color})
		}
		if items == nil {
			items = []any{}
		}
		writeJSON(w, 200, items)
	case "POST":
		m, err := readJSONBody(w, r)
		if err != nil {
			return
		}
		name := strField(m, "name")
		if name == "" {
			errJSON(w, 422, "Validation Failed: name is required")
			return
		}
		color := strField(m, "color")
		if color == "" {
			color = "ededed"
		}
		store.mu.Lock()
		rp.Labels[name] = &Label{Name: name, Color: color}
		store.mu.Unlock()
		writeJSON(w, 201, map[string]any{"name": name, "color": color})
	default:
		errJSON(w, 404, "Not Found")
	}
}

func restIssues(w http.ResponseWriter, r *http.Request, rp *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			sortParam := r.URL.Query().Get("sort")
			dirParam := r.URL.Query().Get("direction")
			var list []*Issue
			for _, iss := range rp.Issues {
				if state == "all" || iss.State == state {
					list = append(list, iss)
				}
			}
			if sortParam == "created" && dirParam == "asc" {
				// already in creation order
			} else if sortParam == "created" || sortParam == "" {
				// default: newest first
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			var items []any
			for _, iss := range list {
				items = append(items, issueJSON(rp, iss))
			}
			if items == nil {
				items = []any{}
			}
			page, hasNext := paginate(items, r)
			if hasNext {
				pageNum := 1
				if v := r.URL.Query().Get("page"); v != "" {
					pageNum, _ = strconv.Atoi(v)
				}
				linkHeader(w, r, pageNum+1)
			}
			writeJSON(w, 200, page)
		case "POST":
			m, err := readJSONBody(w, r)
			if err != nil {
				return
			}
			title := strField(m, "title")
			if title == "" {
				errJSON(w, 422, "Validation Failed: title is required")
				return
			}
			var labels []string
			if raw, ok := m["labels"]; ok {
				if arr, ok := raw.([]any); ok {
					for _, v := range arr {
						if s, ok := v.(string); ok {
							labels = append(labels, s)
						}
					}
				}
			}
			for _, l := range labels {
				if _, ok := rp.Labels[l]; !ok {
					errJSON(w, 422, "Validation Failed: label does not exist")
					return
				}
			}
			store.mu.Lock()
			rp.NumSeq++
			num := rp.NumSeq
			seq := store.nextSeq()
			id := 2000000 + int(seq)
			iss := &Issue{ID: id, GID: globalID("Issue", id), Number: num, Title: title,
				Body: strField(m, "body"), State: "open", Labels: labels, Seq: seq, TS: tsOf(seq)}
			rp.Issues = append(rp.Issues, iss)
			rp.IssueByNum[num] = iss
			store.mu.Unlock()
			writeJSONBytes(w, 201, issueJSONBytes(rp, iss))
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	iss := rp.IssueByNum[num]
	if iss == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case "GET":
			writeJSONBytes(w, 200, issueJSONBytes(rp, iss))
		case "PATCH":
			m, err := readJSONBody(w, r)
			if err != nil {
				return
			}
			store.mu.Lock()
			if v, ok := m["title"]; ok {
				if s, ok := v.(string); ok {
					iss.Title = s
				}
			}
			if v, ok := m["body"]; ok {
				if s, ok := v.(string); ok {
					iss.Body = s
				}
			}
			if v, ok := m["state"]; ok {
				if s, ok := v.(string); ok && (s == "open" || s == "closed") {
					iss.State = s
				}
			}
			store.mu.Unlock()
			writeJSONBytes(w, 200, issueJSONBytes(rp, iss))
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	if rest[1] == "comments" {
		switch r.Method {
		case "GET":
			var items []any
			for _, c := range iss.Comments {
				items = append(items, map[string]any{"id": c.ID, "body": c.Body, "user": map[string]any{"login": viewerLogin, "id": viewerID}})
			}
			if items == nil {
				items = []any{}
			}
			writeJSON(w, 200, items)
		case "POST":
			m, err := readJSONBody(w, r)
			if err != nil {
				return
			}
			store.mu.Lock()
			rp.CommentSeq++
			c := &Comment{ID: 3000000 + rp.CommentSeq, Seq: store.nextSeq(), Body: strField(m, "body")}
			iss.Comments = append(iss.Comments, c)
			store.mu.Unlock()
			writeJSON(w, 201, map[string]any{"id": c.ID, "body": c.Body, "user": map[string]any{"login": viewerLogin, "id": viewerID}})
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	errJSON(w, 404, "Not Found")
}

func restPulls(w http.ResponseWriter, r *http.Request, rp *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			var list []*PR
			for _, pr := range rp.Pulls {
				if state == "all" || pr.State == state {
					list = append(list, pr)
				}
			}
			var items []any
			for _, pr := range list {
				items = append(items, prJSON(rp, pr))
			}
			if items == nil {
				items = []any{}
			}
			page, hasNext := paginate(items, r)
			if hasNext {
				pageNum := 1
				if v := r.URL.Query().Get("page"); v != "" {
					pageNum, _ = strconv.Atoi(v)
				}
				linkHeader(w, r, pageNum+1)
			}
			writeJSON(w, 200, page)
		case "POST":
			m, err := readJSONBody(w, r)
			if err != nil {
				return
			}
			title := strField(m, "title")
			head := strField(m, "head")
			base := strField(m, "base")
			if title == "" || head == "" || base == "" {
				errJSON(w, 422, "Validation Failed")
				return
			}
			// head may be "owner:branch"
			if i := strings.Index(head, ":"); i >= 0 {
				head = head[i+1:]
			}
			if i := strings.Index(base, ":"); i >= 0 {
				base = base[i+1:]
			}
			headSHA := rp.Refs["refs/heads/"+head]
			baseSHA := rp.Refs["refs/heads/"+base]
			if headSHA == "" || baseSHA == "" {
				errJSON(w, 422, "Validation Failed: head or base branch does not exist")
				return
			}
			for _, pr := range rp.Pulls {
				if pr.State == "open" && pr.Head == head && pr.Base == base {
					errJSON(w, 422, "Validation Failed: A pull request already exists")
					return
				}
			}
			store.mu.Lock()
			rp.NumSeq++
			num := rp.NumSeq
			seq := store.nextSeq()
			id := 4000000 + int(seq)
			pr := &PR{ID: id, GID: globalID("PullRequest", id), Number: num, Title: title,
				Body: strField(m, "body"), Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA,
				State: "open", Seq: seq}
			rp.Pulls = append(rp.Pulls, pr)
			rp.PRByNum[num] = pr
			store.mu.Unlock()
			writeJSON(w, 201, prJSON(rp, pr))
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	pr := rp.PRByNum[num]
	if pr == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		if r.Method == "GET" {
			writeJSON(w, 200, prJSON(rp, pr))
			return
		}
		errJSON(w, 404, "Not Found")
		return
	}
	switch rest[1] {
	case "files":
		if r.Method != "GET" {
			errJSON(w, 404, "Not Found")
			return
		}
		base := rp.mergeBase(prHeadSHA(rp, pr), prBaseSHA(rp, pr))
		var items []any
		for _, fc := range rp.diffTrees(rp.commitTree(base), rp.commitTree(prHeadSHA(rp, pr))) {
			items = append(items, map[string]any{
				"filename": fc.Path, "status": fc.Status,
				"additions": fc.Additions, "deletions": fc.Deletions, "changes": fc.Changes,
			})
		}
		if items == nil {
			items = []any{}
		}
		writeJSON(w, 200, items)
	case "merge":
		if r.Method != "PUT" {
			errJSON(w, 404, "Not Found")
			return
		}
		if pr.State == "closed" {
			if pr.Merged {
				errJSON(w, 405, "Pull request already merged")
			} else {
				errJSON(w, 405, "Pull request is closed")
			}
			return
		}
		if prConflict(rp, pr) {
			errJSON(w, 405, "Merge conflict")
			return
		}
		store.mu.Lock()
		// Squash: apply head diff onto base tip.
		headSHA := prHeadSHA(rp, pr)
		baseSHA := prBaseSHA(rp, pr)
		base := rp.mergeBase(headSHA, baseSHA)
		baseTree := rp.commitTree(baseSHA)
		newTree := baseTree
		// Apply the diff base->head onto base tip tree.
		for _, fc := range rp.diffTrees(rp.commitTree(base), rp.commitTree(headSHA)) {
			if fc.Status == "deleted" {
				newTree = rp.setFile(newTree, fc.Path, "")
			} else {
				newTree = rp.setFile(newTree, fc.Path, fc.NewOID)
			}
		}
		seq := store.nextSeq()
		mergeMsg := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.Head + "\n\n" + pr.Title
		mergeSHA := rp.makeCommit(newTree, []string{baseSHA}, mergeMsg, seq)
		rp.Refs["refs/heads/"+pr.Base] = mergeSHA
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = tsOf(seq)
		pr.MergeSHA = mergeSHA
		store.mu.Unlock()
		writeJSON(w, 200, map[string]any{"sha": mergeSHA, "merged": true, "message": "Pull request successfully merged"})
	default:
		errJSON(w, 404, "Not Found")
	}
}
