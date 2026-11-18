// Package main implements a GitHub-core clone: git smart HTTP, REST v3,
// GraphQL v4 and MCP over a single in-memory store. Stdlib only.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Object model
// ---------------------------------------------------------------------------

// Obj is one git object.
type Obj struct {
	Type string // commit | tree | blob | tag
	Data []byte

	treeOnce sync.Once
	tree     []TreeEntry
	commit   *CommitInfo
}

type TreeEntry struct {
	Mode  string // "100644" | "100755" | "120000" | "40000"
	Name  string
	OID   string // hex
	IsDir bool
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string // "Name <email> ts +0000"
	Committer string
	Message   string
}

// hashObj returns the sha1 id of an object with the given type and payload.
func hashObj(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (o *Obj) Tree() []TreeEntry {
	o.treeOnce.Do(func() {
		if o.Type != "tree" {
			return
		}
		d := o.Data
		for len(d) > 0 {
			sp := bytes.IndexByte(d, ' ')
			if sp < 0 {
				break
			}
			mode := string(d[:sp])
			nul := bytes.IndexByte(d[sp+1:], 0)
			if nul < 0 || sp+1+nul+21 > len(d) {
				break
			}
			name := string(d[sp+1 : sp+1+nul])
			oid := hex.EncodeToString(d[sp+1+nul+1 : sp+1+nul+21])
			o.tree = append(o.tree, TreeEntry{Mode: mode, Name: name, OID: oid, IsDir: mode == "40000"})
			d = d[sp+1+nul+21:]
		}
	})
	return o.tree
}

func (o *Obj) Commit() *CommitInfo {
	o.treeOnce.Do(func() {
		if o.Type != "commit" {
			return
		}
		c := &CommitInfo{}
		body := string(o.Data)
		head, msg, _ := strings.Cut(body, "\n\n")
		c.Message = msg
		for _, line := range strings.Split(head, "\n") {
			k, v, ok := strings.Cut(line, " ")
			if !ok {
				continue
			}
			switch k {
			case "tree":
				c.Tree = v
			case "parent":
				c.Parents = append(c.Parents, v)
			case "author":
				c.Author = v
			case "committer":
				c.Committer = v
			}
		}
		o.commit = c
	})
	return o.commit
}

// buildTree serialises entries in git tree order (directories sort as name+"/").
func buildTree(entries []TreeEntry) []byte {
	cp := make([]TreeEntry, len(entries))
	copy(cp, entries)
	sort.Slice(cp, func(i, j int) bool {
		ni, nj := cp[i].Name, cp[j].Name
		if cp[i].IsDir {
			ni += "/"
		}
		if cp[j].IsDir {
			nj += "/"
		}
		return ni < nj
	})
	var b bytes.Buffer
	for _, e := range cp {
		b.WriteString(e.Mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		b.Write(raw)
	}
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID   int
	Seq  int64
	Body string
}

type Issue struct {
	ID       int    // REST numeric id
	GID      string // GraphQL global id
	Number   int
	Title    string
	Body     string
	State    string // open | closed
	Labels   []string
	Seq      int64
	Comments []*Comment
}

type PR struct {
	ID         int
	GID        string
	Number     int
	Title      string
	Body       string
	Head       string // branch name
	Base       string // branch name
	HeadSHA    string
	BaseSHA    string
	State      string // open | closed
	Merged     bool
	MergedAt   string // ISO, "" if not merged
	MergeSHA   string
	Seq        int64
	commentSeq int
	Comments   []*Comment
}

type Repo struct {
	ID            int // databaseId
	GID           string
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int64
	Refs          map[string]string // full ref -> oid
	Objects       map[string]*Obj
	Issues        []*Issue
	IssueByNum    map[int]*Issue
	Pulls         []*PR
	PRByNum       map[int]*PR
	Labels        map[string]*Label
	NumSeq        int // shared issue/PR number counter
	CommentSeq    int
	shallowSet    map[string]bool // commits the client holds as shallow
}

type Store struct {
	mu       sync.RWMutex
	seq      int64            // global logical clock (ids, timestamps)
	repos    map[string]*Repo // owner/name
	repoList []*Repo          // creation order
	nextRepo int
	reqID    int64
}

func NewStore() *Store {
	return &Store{repos: map[string]*Repo{}, nextRepo: 1000}
}

func (s *Store) nextSeq() int64 { return atomic.AddInt64(&s.seq, 1) }

// ts returns a deterministic ISO timestamp for the given seq.
func tsOf(seq int64) string {
	return time.Unix(1700000000+seq, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func globalID(kind string, n int) string {
	return "R_" + kind + "_" + strconv.Itoa(n)
}

// CreateRepo creates a repo owned by viewer. Returns nil if name taken.
func (s *Store) CreateRepo(owner, name, desc string, priv bool) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil
	}
	s.nextRepo++
	id := s.nextRepo
	r := &Repo{
		ID: id, GID: globalID("Repository", id),
		Owner: owner, Name: name, Description: desc, Private: priv,
		DefaultBranch: "main",
		CreatedSeq:    s.nextSeq(),
		Refs:          map[string]string{},
		Objects:       map[string]*Obj{},
		IssueByNum:    map[int]*Issue{},
		PRByNum:       map[int]*PR{},
		Labels:        map[string]*Label{},
	}
	s.repos[key] = r
	s.repoList = append(s.repoList, r)
	return r
}

func (s *Store) GetRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[owner+"/"+name]
}

func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "/" + name
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	for i, r := range s.repoList {
		if r.Owner == owner && r.Name == name {
			s.repoList = append(s.repoList[:i], s.repoList[i+1:]...)
			break
		}
	}
	return true
}

// ReposByOwner lists repos of an owner in creation order.
func (s *Store) ReposByOwner(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Repo
	for _, r := range s.repoList {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Git object helpers (operate on a repo; caller holds no lock — repos are
// mutated under the store write lock in handlers, reads under read lock)
// ---------------------------------------------------------------------------

func (r *Repo) putObj(typ string, data []byte) string {
	oid := hashObj(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Obj{Type: typ, Data: data}
	}
	return oid
}

func (r *Repo) getObj(oid string) *Obj { return r.Objects[oid] }

// makeCommit creates a commit object with deterministic author/committer.
func (r *Repo) makeCommit(tree string, parents []string, message string, seq int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	ident := fmt.Sprintf("ada iversen <adaiversen@example.invalid> %d +0000", 1700000000+seq)
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s", ident, ident, message)
	return r.putObj("commit", []byte(b.String()))
}

// getFile returns the blob oid of path in the given tree ("" tree = missing).
func (r *Repo) getFile(treeOID, path string) (string, bool) {
	if treeOID == "" {
		return "", false
	}
	parts := strings.Split(path, "/")
	cur := treeOID
	for i, part := range parts {
		o := r.getObj(cur)
		if o == nil || o.Type != "tree" {
			return "", false
		}
		var found *TreeEntry
		for _, e := range o.Tree() {
			if e.Name == part {
				ee := e
				found = &ee
				break
			}
		}
		if found == nil {
			return "", false
		}
		if i == len(parts)-1 {
			if found.IsDir {
				return "", false
			}
			return found.OID, true
		}
		if !found.IsDir {
			return "", false
		}
		cur = found.OID
	}
	return "", false
}

// setFile returns a new tree oid with path set to blobOID (or deleted if "").
func (r *Repo) setFile(treeOID, path, blobOID string) string {
	parts := strings.Split(path, "/")
	return r.setFileRec(treeOID, parts, blobOID)
}

func (r *Repo) setFileRec(treeOID string, parts []string, blobOID string) string {
	var entries []TreeEntry
	if treeOID != "" {
		if o := r.getObj(treeOID); o != nil {
			entries = append(entries, o.Tree()...)
		}
	}
	name := parts[0]
	idx := -1
	for i, e := range entries {
		if e.Name == name {
			idx = i
			break
		}
	}
	if len(parts) == 1 {
		if blobOID == "" {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx] = TreeEntry{Mode: "100644", Name: name, OID: blobOID}
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
			}
		}
	} else {
		var sub string
		if idx >= 0 {
			sub = entries[idx].OID
		}
		sub = r.setFileRec(sub, parts[1:], blobOID)
		if sub == "" {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx >= 0 {
				entries[idx] = TreeEntry{Mode: "40000", Name: name, OID: sub, IsDir: true}
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: sub, IsDir: true})
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return r.putObj("tree", buildTree(entries))
}

// flattenTree returns path -> blob oid for every file under treeOID.
func (r *Repo) flattenTree(treeOID string) map[string]string {
	out := map[string]string{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		o := r.getObj(oid)
		if o == nil || o.Type != "tree" {
			return
		}
		for _, e := range o.Tree() {
			p := prefix + e.Name
			if e.IsDir {
				walk(e.OID, p+"/")
			} else {
				out[p] = e.OID
			}
		}
	}
	if treeOID != "" {
		walk(treeOID, "")
	}
	return out
}

// commitTree returns the root tree oid of a commit ("" if commit missing).
func (r *Repo) commitTree(oid string) string {
	o := r.getObj(oid)
	if o == nil || o.Type != "commit" {
		return ""
	}
	return o.Commit().Tree
}

// diffTrees returns file changes from base tree to head tree.
type FileChange struct {
	Path      string
	Status    string // added | modified | deleted
	OldOID    string
	NewOID    string
	Additions int
	Deletions int
	Changes   int
}

func (r *Repo) diffTrees(baseTree, headTree string) []FileChange {
	b := r.flattenTree(baseTree)
	h := r.flattenTree(headTree)
	var paths []string
	seen := map[string]bool{}
	for p := range b {
		seen[p] = true
		paths = append(paths, p)
	}
	for p := range h {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []FileChange
	for _, p := range paths {
		bo, bok := b[p]
		ho, hok := h[p]
		switch {
		case bok && !hok:
			out = append(out, FileChange{Path: p, Status: "deleted", OldOID: bo})
		case !bok && hok:
			out = append(out, FileChange{Path: p, Status: "added", NewOID: ho})
		case bo != ho:
			out = append(out, FileChange{Path: p, Status: "modified", OldOID: bo, NewOID: ho})
		}
	}
	// line counts
	for i := range out {
		fc := &out[i]
		var oldB, newB []byte
		if fc.OldOID != "" {
			if o := r.getObj(fc.OldOID); o != nil {
				oldB = o.Data
			}
		}
		if fc.NewOID != "" {
			if o := r.getObj(fc.NewOID); o != nil {
				newB = o.Data
			}
		}
		fc.Additions, fc.Deletions = lineDiffCount(oldB, newB)
		fc.Changes = fc.Additions + fc.Deletions
	}
	return out
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// lineDiffCount uses a simple LCS over lines; inputs are small.
func lineDiffCount(a, b []byte) (add, del int) {
	if len(a) == 0 && len(b) == 0 {
		return 0, 0
	}
	al := splitLines(a)
	bl := splitLines(b)
	// LCS via DP over the shorter dimension
	if len(al) == 0 {
		return len(bl), 0
	}
	if len(bl) == 0 {
		return 0, len(al)
	}
	// Cap: fall back to whole-file counts for huge files (not seen in checks).
	if len(al)*len(bl) > 4000000 {
		return len(bl), len(al)
	}
	prev := make([]int, len(bl)+1)
	cur := make([]int, len(bl)+1)
	for i := 1; i <= len(al); i++ {
		for j := 1; j <= len(bl); j++ {
			if al[i-1] == bl[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[len(bl)]
	return len(bl) - lcs, len(al) - lcs
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.SplitAfter(string(b), "\n")
	if s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

// ancestors returns commit oids reachable from tips (BFS, children first).
func (r *Repo) ancestors(tips []string) map[string]bool {
	seen := map[string]bool{}
	queue := append([]string{}, tips...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o := r.getObj(oid)
		if o == nil {
			continue
		}
		switch o.Type {
		case "commit":
			queue = append(queue, o.Commit().Parents...)
		case "tag":
			if tgt := tagTarget(o); tgt != "" {
				queue = append(queue, tgt)
			}
		}
	}
	return seen
}

// reachableObjectsStop is reachableObjects, except commits in stop are
// treated as shallow on the client: the commit itself and its tree are held,
// but its parents are not (deepen negotiation).
func (r *Repo) reachableObjectsStop(tips []string, stop map[string]bool) map[string]bool {
	seen := map[string]bool{}
	var visitTree func(oid string)
	visitTree = func(oid string) {
		if seen[oid] {
			return
		}
		o := r.getObj(oid)
		if o == nil {
			return
		}
		seen[oid] = true
		switch o.Type {
		case "tree":
			for _, e := range o.Tree() {
				visitTree(e.OID)
			}
		case "tag":
			if tgt := tagTarget(o); tgt != "" {
				visitTree(tgt)
			}
		}
	}
	var visit func(oid string)
	visit = func(oid string) {
		if seen[oid] {
			return
		}
		o := r.getObj(oid)
		if o == nil {
			return
		}
		seen[oid] = true
		switch o.Type {
		case "commit":
			c := o.Commit()
			visitTree(c.Tree)
			if !stop[oid] {
				for _, p := range c.Parents {
					visit(p)
				}
			}
		case "tree":
			visitTree(oid)
		case "tag":
			if tgt := tagTarget(o); tgt != "" {
				visit(tgt)
			}
		}
	}
	for _, t := range tips {
		visit(t)
	}
	return seen
}

// reachableObjects returns all object oids reachable from tips.
func (r *Repo) reachableObjects(tips []string) map[string]bool {
	seen := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		if seen[oid] {
			return
		}
		o := r.getObj(oid)
		if o == nil {
			return
		}
		seen[oid] = true
		switch o.Type {
		case "commit":
			c := o.Commit()
			visit(c.Tree)
			for _, p := range c.Parents {
				visit(p)
			}
		case "tree":
			for _, e := range o.Tree() {
				visit(e.OID)
			}
		case "tag":
			if tgt := tagTarget(o); tgt != "" {
				visit(tgt)
			}
		}
	}
	for _, t := range tips {
		visit(t)
	}
	return seen
}

func tagTarget(o *Obj) string {
	for _, line := range strings.Split(string(o.Data), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == "object" {
			return v
		}
	}
	return ""
}

// mergeBase finds a common ancestor of a and b (BFS from a first).
func (r *Repo) mergeBase(a, b string) string {
	bAnc := r.ancestors([]string{b})
	queue := []string{a}
	seen := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if bAnc[oid] {
			return oid
		}
		o := r.getObj(oid)
		if o == nil || o.Type != "commit" {
			continue
		}
		queue = append(queue, o.Commit().Parents...)
	}
	return ""
}

// listCommits returns commit oids newest-first from tip (DFS, parents in order).
func (r *Repo) listCommits(tip string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(oid string)
	walk = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		o := r.getObj(oid)
		if o == nil || o.Type != "commit" {
			return
		}
		out = append(out, oid)
		for _, p := range o.Commit().Parents {
			walk(p)
		}
	}
	walk(tip)
	return out
}

// ---------------------------------------------------------------------------
// Pack read/write (no deltas written; deltas read for client packs)
// ---------------------------------------------------------------------------

func writePack(objs []*Obj) []byte {
	var buf bytes.Buffer
	buf.Write([]byte("PACK"))
	buf.Write([]byte{0, 0, 0, 2})
	n := make([]byte, 4)
	n[0] = byte(len(objs) >> 24)
	n[1] = byte(len(objs) >> 16)
	n[2] = byte(len(objs) >> 8)
	n[3] = byte(len(objs))
	buf.Write(n)
	typeNum := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	for _, o := range objs {
		// type+size varint
		size := len(o.Data)
		t := typeNum[o.Type]
		first := byte((t << 4) | (size & 0x0f))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		// Pooled zlib writer: a fresh writer costs ~440us on this host,
		// Reset costs ~6us. Same stream per object, as pack format needs.
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&buf)
		zw.Write(o.Data)
		zw.Close()
		zlibWriterPool.Put(zw)
	}
	h := sha1.Sum(buf.Bytes())
	buf.Write(h[:])
	return buf.Bytes()
}

// zlibWriterPool reuses compress/zlib writers across pack builds; writer
// construction is the dominant cost of writePack on this host.
var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

// zlibReaderPool reuses compress/zlib readers across pack reads.
var zlibReaderPool = sync.Pool{}

// readPack parses a packfile; lookup resolves REF_DELTA bases that live in
// the repository (may be nil). Returns objects (type+data).
func readPack(data []byte, lookup func(oid string) *Obj) ([]*Obj, error) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	version := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(data[8])<<24 | int(data[9])<<16 | int(data[10])<<8 | int(data[11])
	pos := 12
	typeNum := map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
	type packEntry struct {
		typ    int
		offset int
		oid    string
		data   []byte
	}
	entries := make([]*packEntry, 0, count)
	byOffset := map[int]*packEntry{}
	for i := 0; i < count; i++ {
		if pos >= len(data) {
			return nil, fmt.Errorf("pack truncated")
		}
		start := pos
		c := data[pos]
		pos++
		typ := int((c >> 4) & 0x7)
		size := int64(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= len(data) {
				return nil, fmt.Errorf("pack truncated (size)")
			}
			c = data[pos]
			pos++
			size |= int64(c&0x7f) << shift
			shift += 7
		}
		var baseOID string
		var baseOff int
		if typ == 6 {
			// OFS_DELTA: negative offset varint follows
			if pos >= len(data) {
				return nil, fmt.Errorf("pack truncated (ofs)")
			}
			c = data[pos]
			pos++
			off := int64(c & 0x7f)
			for c&0x80 != 0 {
				c = data[pos]
				pos++
				off = ((off + 1) << 7) | int64(c&0x7f)
			}
			baseOff = start - int(off)
		} else if typ == 7 {
			if pos+20 > len(data) {
				return nil, fmt.Errorf("pack truncated (ref)")
			}
			baseOID = hex.EncodeToString(data[pos : pos+20])
			pos += 20
		}
		raw, consumed, err := inflateAt(data[pos:])
		if err != nil {
			return nil, err
		}
		pos += consumed
		e := &packEntry{typ: typ, offset: start, data: raw}
		if typ == 6 {
			e.typ = -6 // marker: ofs delta, base offset in baseOff
			_ = baseOff
			e.data = raw
			// store base offset in a side map via offset field reuse
			e.offset = start
			byOffset[start] = e
			// stash base offset in oid field as decimal string (hack, resolved below)
			e.oid = fmt.Sprintf("ofs:%d", baseOff)
		} else if typ == 7 {
			e.typ = -7
			e.oid = baseOID
			byOffset[start] = e
		} else {
			e.oid = hashObj(typeNum[typ], raw)
			byOffset[start] = e
		}
		entries = append(entries, e)
		_ = size
	}
	// Resolve deltas.
	objs := make([]*Obj, 0, count)
	resolved := map[*packEntry]*Obj{}
	var resolve func(e *packEntry) (*Obj, error)
	resolve = func(e *packEntry) (*Obj, error) {
		if o, ok := resolved[e]; ok {
			return o, nil
		}
		if e.typ > 0 {
			o := &Obj{Type: typeNum[e.typ], Data: e.data}
			resolved[e] = o
			return o, nil
		}
		var base *Obj
		if e.typ == -6 {
			var off int
			fmt.Sscanf(e.oid, "ofs:%d", &off)
			be, ok := byOffset[off]
			if !ok {
				return nil, fmt.Errorf("ofs delta base not found")
			}
			b, err := resolve(be)
			if err != nil {
				return nil, err
			}
			base = b
		} else {
			// REF_DELTA: base may be in repo or earlier in pack
			for _, pe := range entries {
				if pe.typ > 0 && pe.oid == e.oid {
					b, err := resolve(pe)
					if err != nil {
						return nil, err
					}
					base = b
					break
				}
			}
			if base == nil && lookup != nil {
				if o := lookup(e.oid); o != nil {
					base = o
				}
			}
			if base == nil {
				return nil, fmt.Errorf("ref delta base %s not found", e.oid)
			}
		}
		data, err := applyDelta(base.Data, e.data)
		if err != nil {
			return nil, err
		}
		o := &Obj{Type: base.Type, Data: data}
		resolved[e] = o
		return o, nil
	}
	for _, e := range entries {
		o, err := resolve(e)
		if err != nil {
			return nil, err
		}
		objs = append(objs, o)
	}
	return objs, nil
}

// inflateAt inflates one zlib stream at the start of b; returns payload and
// bytes consumed. Uses a bytes.Reader so flate reads byte-at-a-time and the
// consumed length is exact. The zlib reader is pooled (construction is
// expensive); Reset rebinds it to the new input.
func inflateAt(b []byte) ([]byte, int, error) {
	br := bytes.NewReader(b)
	var zr *zlibReader
	if v := zlibReaderPool.Get(); v != nil {
		zr = v.(*zlibReader)
		if err := zr.Reset(br); err != nil {
			zlibReaderPool.Put(zr)
			return nil, 0, err
		}
	} else {
		r, err := zlib.NewReader(br)
		if err != nil {
			return nil, 0, err
		}
		zr = &zlibReader{r}
	}
	out, err := io.ReadAll(zr.r)
	if err != nil {
		zlibReaderPool.Put(zr)
		return nil, 0, err
	}
	consumed := len(b) - br.Len()
	zlibReaderPool.Put(zr)
	return out, consumed, nil
}

type zlibReader struct {
	r io.ReadCloser
}

func (z *zlibReader) Reset(r io.Reader) error {
	// compress/zlib's reader has Reset(r io.Reader, dict []byte) error.
	if rr, ok := z.r.(interface{ Reset(io.Reader, []byte) error }); ok {
		return rr.Reset(r, nil)
	}
	return io.EOF
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("delta truncated")
			}
			c := delta[pos]
			pos++
			v |= int64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if int(srcSize) != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			var off, size int64
			if op&0x01 != 0 {
				off = int64(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				off |= int64(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				off |= int64(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				off |= int64(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				size = int64(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				size |= int64(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				size |= int64(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out.Write(base[off : off+size])
		} else if op != 0 {
			n := int(op)
			if pos+n > len(delta) {
				return nil, fmt.Errorf("delta insert out of range")
			}
			out.Write(delta[pos : pos+n])
			pos += n
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if int64(out.Len()) != dstSize {
		return nil, fmt.Errorf("delta result size mismatch")
	}
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// pkt-line helpers
// ---------------------------------------------------------------------------

func pktLine(s string) []byte {
	n := len(s) + 4
	return []byte(fmt.Sprintf("%04x%s", n, s))
}

func pktFlush() []byte { return []byte("0000") }

// parsePktLines splits a buffer into pkt-line payloads (flush = nil marker).
// Returns payloads and the offset where the pack (or non-pkt data) starts.
func parsePktLines(b []byte) ([][]byte, int) {
	var out [][]byte
	pos := 0
	for pos+4 <= len(b) {
		n, err := strconv.ParseInt(string(b[pos:pos+4]), 16, 32)
		if err != nil || n < 0 {
			break
		}
		if n == 0 {
			out = append(out, nil)
			pos += 4
			continue
		}
		if n == 1 || n == 2 {
			out = append(out, []byte{byte(n)})
			pos += 4
			continue
		}
		if pos+int(n) > len(b) {
			break
		}
		out = append(out, b[pos+4:pos+int(n)])
		pos += int(n)
	}
	return out, pos
}
