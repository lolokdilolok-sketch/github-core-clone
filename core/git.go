package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Git smart HTTP: /{owner}/{repo}.git/info/refs, git-upload-pack, git-receive-pack
// ---------------------------------------------------------------------------

func gitRepoFromPath(p string) (owner, name string, ok bool) {
	// /{owner}/{repo}.git/...
	p = strings.TrimPrefix(p, "/")
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	owner = parts[0]
	repo := parts[1]
	if !strings.HasSuffix(repo, ".git") {
		return "", "", false
	}
	return owner, strings.TrimSuffix(repo, ".git"), true
}

func gitAuth(w http.ResponseWriter, r *http.Request) bool { return auth(w, r) }

func gitInfoRefs(w http.ResponseWriter, r *http.Request) {
	if !gitAuth(w, r) {
		return
	}
	owner, name, ok := gitRepoFromPath(r.URL.Path)
	if !ok {
		w.WriteHeader(404)
		return
	}
	rp := resolveRepoGit(w, owner, name)
	if rp == nil {
		return
	}
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		w.WriteHeader(404)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	if r.Header.Get("Git-Protocol") == "version=2" && service == "git-upload-pack" {
		// v2 advertisement
		buf.Write(pktLine("version 2\n"))
		buf.Write(pktLine("ls-refs=unborn\n"))
		buf.Write(pktLine("fetch=shallow wait-for-done filter\n"))
		buf.Write(pktLine("object-format=sha1\n"))
		buf.Write(pktFlush())
	} else {
		buf.Write(pktLine("# service=" + service + "\n"))
		buf.Write(pktFlush())
		store.mu.RLock()
		if service == "git-upload-pack" {
			writeUploadPackV0Advertisement(&buf, rp)
		} else {
			writeReceivePackAdvertisement(&buf, rp)
		}
		store.mu.RUnlock()
	}
	w.Write(buf.Bytes())
}

func resolveRepoGit(w http.ResponseWriter, owner, name string) *Repo {
	if owner != viewerLogin {
		w.WriteHeader(404)
		return nil
	}
	rp := store.GetRepo(owner, name)
	if rp == nil {
		w.WriteHeader(404)
		return nil
	}
	return rp
}

func sortedRefs(rp *Repo) []string {
	var refs []string
	for ref := range rp.Refs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

func writeUploadPackV0Advertisement(buf *bytes.Buffer, rp *Repo) {
	refs := sortedRefs(rp)
	caps := "side-band-64k thin-pack ofs-delta multi_ack_detailed shallow symref=HEAD:refs/heads/" + rp.DefaultBranch
	if len(refs) == 0 {
		// capability line with zero-id HEAD
		buf.Write(pktLine("0000000000000000000000000000000000000000 capabilities^{}\x00" + caps + "\n"))
		buf.Write(pktFlush())
		return
	}
	for i, ref := range refs {
		line := rp.Refs[ref] + " " + ref + "\n"
		if i == 0 {
			line = rp.Refs[ref] + " " + ref + "\x00" + caps + "\n"
		}
		buf.Write(pktLine(line))
	}
	buf.Write(pktFlush())
}

func writeReceivePackAdvertisement(buf *bytes.Buffer, rp *Repo) {
	refs := sortedRefs(rp)
	caps := "report-status side-band-64k delete-refs"
	if len(refs) == 0 {
		buf.Write(pktLine("0000000000000000000000000000000000000000 capabilities^{}\x00" + caps + "\n"))
		buf.Write(pktFlush())
		return
	}
	for i, ref := range refs {
		line := rp.Refs[ref] + " " + ref + "\n"
		if i == 0 {
			line = rp.Refs[ref] + " " + ref + "\x00" + caps + "\n"
		}
		buf.Write(pktLine(line))
	}
	buf.Write(pktFlush())
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

func gitUploadPack(w http.ResponseWriter, r *http.Request) {
	if !gitAuth(w, r) {
		return
	}
	owner, name, ok := gitRepoFromPath(r.URL.Path)
	if !ok {
		w.WriteHeader(404)
		return
	}
	rp := resolveRepoGit(w, owner, name)
	if rp == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	if r.Header.Get("Git-Protocol") == "version=2" {
		handleUploadPackV2(w, rp, body)
		return
	}
	handleUploadPackV0(w, rp, body)
}

// fetchRequest is the parsed client fetch request (both versions).
type fetchRequest struct {
	wants     []string
	haves     []string
	deepen    int // 0 = none
	deepenNot bool
	unshallow bool
	filter    string
	// v2 ls-refs args
	lsRefs    bool
	peel      bool
	unborn    bool
	symrefs   bool
	refPrefix string
	shallow   []string // client-declared shallow boundaries
}

func handleUploadPackV0(w http.ResponseWriter, rp *Repo, body []byte) {
	lines, rest := parsePktLines(body)
	fr := &fetchRequest{}
	for _, l := range lines {
		if l == nil {
			// v0 splits wants and haves across two sections; keep scanning.
			continue
		}
		s := strings.TrimRight(string(l), "\n")
		switch {
		case strings.HasPrefix(s, "want "):
			// first field is the oid; capabilities may follow space-separated
			fr.wants = append(fr.wants, strings.Fields(s[5:])[0])
		case strings.HasPrefix(s, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(s[5:]))
		case strings.HasPrefix(s, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(s[7:])); err == nil {
				fr.deepen = n
			}
		case strings.HasPrefix(s, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(s[8:]))
		case s == "done":
		}
	}
	_ = rest
	// Build the object set (shared read lock: the clone bench fetches concurrently).
	store.mu.RLock()
	objs, shallow, unshallow := planFetch(rp, fr)
	var buf bytes.Buffer
	buf.Write(pktLine("NAK\n"))
	for _, s := range shallow {
		buf.Write(pktLine("shallow " + s + "\n"))
	}
	for _, s := range unshallow {
		buf.Write(pktLine("unshallow " + s + "\n"))
	}
	buf.Write(pktFlush())
	pack := writePack(objs)
	// sideband: band 1 chunks of up to 65515 bytes
	const maxChunk = 65515
	for len(pack) > 0 {
		n := len(pack)
		if n > maxChunk {
			n = maxChunk
		}
		chunk := pack[:n]
		pack = pack[n:]
		payload := append([]byte{1}, chunk...)
		buf.Write(pktLine(string(payload)))
	}
	buf.Write(pktFlush())
	store.mu.RUnlock()
	recordShallow(rp, fr, shallow, unshallow)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(buf.Bytes())
}

// recordShallow remembers the shallow boundary the server just served, so a
// later deepen can compute unshallow. Exclusive lock: mutates rp.shallowSet.
func recordShallow(rp *Repo, fr *fetchRequest, shallow, unshallow []string) {
	if fr.deepen <= 0 {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if rp.shallowSet == nil {
		rp.shallowSet = map[string]bool{}
	}
	for _, s := range shallow {
		rp.shallowSet[s] = true
	}
	for _, s := range unshallow {
		delete(rp.shallowSet, s)
	}
}

func handleUploadPackV2(w http.ResponseWriter, rp *Repo, body []byte) {
	lines, _ := parsePktLines(body)
	// Commands are separated by delim (0001) / flush.
	var cmds []string
	var args []string
	inArgs := false
	for _, l := range lines {
		if l == nil {
			break
		}
		if len(l) == 1 && l[0] == 1 {
			inArgs = true
			continue
		}
		s := strings.TrimRight(string(l), "\n")
		if !inArgs {
			cmds = append(cmds, s)
		} else {
			args = append(args, s)
		}
	}
	// Multiple commands may be present in sequence; handle ls-refs then fetch
	// by scanning the raw lines again in order.
	// Simpler: iterate commands sequentially using the args after each delim.
	// The kit client sends one command per request.
	cmd := ""
	if len(cmds) > 0 {
		cmd = strings.TrimPrefix(cmds[0], "command=")
	}
	fr := &fetchRequest{}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			fr.wants = append(fr.wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(a[7:])); err == nil {
				fr.deepen = n
			}
		case strings.HasPrefix(a, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(a[8:]))
		case a == "unshallow":
			fr.unshallow = true
		case strings.HasPrefix(a, "filter "):
			fr.filter = strings.TrimSpace(a[7:])
		case a == "peel":
			fr.peel = true
		case a == "unborn":
			fr.unborn = true
		case a == "symrefs":
			fr.symrefs = true
		case strings.HasPrefix(a, "ref-prefix "):
			fr.refPrefix = strings.TrimSpace(a[11:])
		}
	}
	var buf bytes.Buffer
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	switch cmd {
	case "ls-refs":
		fr.lsRefs = true
		store.mu.RLock()
		writeLsRefs(&buf, rp, fr)
		store.mu.RUnlock()
	case "fetch":
		store.mu.RLock()
		shallow, unshallow := writeFetchV2(&buf, rp, fr)
		store.mu.RUnlock()
		recordShallow(rp, fr, shallow, unshallow)
	default:
		// unknown command: empty response
	}
	w.Write(buf.Bytes())
}

func writeLsRefs(buf *bytes.Buffer, rp *Repo, fr *fetchRequest) {
	refs := sortedRefs(rp)
	if len(refs) == 0 {
		if fr.unborn {
			buf.Write(pktLine("unborn HEAD symref-target:refs/heads/" + rp.DefaultBranch + "\n"))
		}
		buf.Write(pktFlush())
		return
	}
	// HEAD symref line only when no ref-prefix filters it out.
	if fr.symrefs && (fr.refPrefix == "" || strings.HasPrefix("HEAD", fr.refPrefix)) {
		buf.Write(pktLine(rp.Refs["refs/heads/"+rp.DefaultBranch] + " HEAD symref-target:refs/heads/" + rp.DefaultBranch + "\n"))
	}
	for _, ref := range refs {
		if fr.refPrefix != "" && !strings.HasPrefix(ref, fr.refPrefix) {
			continue
		}
		line := rp.Refs[ref] + " " + ref
		if fr.peel {
			if o := rp.getObj(rp.Refs[ref]); o != nil && o.Type == "tag" {
				if tgt := tagTarget(o); tgt != "" {
					line += " peeled:" + tgt
				}
			}
		}
		buf.Write(pktLine(line + "\n"))
	}
	buf.Write(pktFlush())
}

// planFetch computes the objects to send, shallow boundary and unshallow list.
// shallowBoundary is read from a per-repo shallow set — but the server must
// track shallow per clone; the client tells us via deepens. We track the
// shallow set in the repo struct (not persisted across restarts — the harness
// runs one server per trial, so in-memory is fine).
func planFetch(rp *Repo, fr *fetchRequest) (objs []*Obj, shallow []string, unshallow []string) {
	// Resolve wants to commit oids (tags resolve to their target commit).
	var wantCommits []string
	var wantObjs []string
	for _, w := range fr.wants {
		o := rp.getObj(w)
		if o == nil {
			continue
		}
		wantObjs = append(wantObjs, w)
		if o.Type == "tag" {
			if tgt := tagTarget(o); tgt != "" {
				wantCommits = append(wantCommits, tgt)
			}
		} else if o.Type == "commit" {
			wantCommits = append(wantCommits, w)
		}
	}
	haveStop := map[string]bool{}
	for s := range rp.shallowSet {
		haveStop[s] = true
	}
	for _, s := range fr.shallow {
		haveStop[s] = true
	}
	haveSet := rp.reachableObjectsStop(fr.haves, haveStop)
	// Depth-limited commit set.
	included := map[string]bool{}
	var boundary []string
	if fr.deepen > 0 {
		// BFS by generation from wants.
		type node struct {
			oid   string
			depth int
		}
		queue := []node{}
		for _, w := range wantCommits {
			queue = append(queue, node{w, 0})
		}
		seen := map[string]bool{}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			if seen[n.oid] {
				continue
			}
			seen[n.oid] = true
			included[n.oid] = true
			o := rp.getObj(n.oid)
			if o == nil || o.Type != "commit" {
				continue
			}
			if n.depth+1 < fr.deepen {
				for _, p := range o.Commit().Parents {
					queue = append(queue, node{p, n.depth + 1})
				}
			}
		}
		// boundary = included commits with a parent outside included
		for oid := range included {
			o := rp.getObj(oid)
			if o == nil || o.Type != "commit" {
				continue
			}
			parents := o.Commit().Parents
			if len(parents) == 0 {
				boundary = append(boundary, oid)
				continue
			}
			for _, p := range parents {
				if !included[p] {
					boundary = append(boundary, oid)
					break
				}
			}
		}
		sort.Strings(boundary)
	} else {
		for _, w := range wantCommits {
			included[w] = true
		}
		// full closure: included = ancestors
		for oid := range rp.ancestors(wantCommits) {
			included[oid] = true
		}
	}
	// Object set: closure of included commits + wanted tag objects.
	send := map[string]bool{}
	for _, w := range wantObjs {
		if o := rp.getObj(w); o != nil && o.Type == "tag" {
			send[w] = true
		}
	}
	for oid := range included {
		o := rp.getObj(oid)
		if o == nil {
			continue
		}
		send[oid] = true
		if o.Type == "commit" {
			for t := range rp.reachableObjects([]string{o.Commit().Tree}) {
				send[t] = true
			}
		}
	}
	// Subtract haves.
	for oid := range haveSet {
		delete(send, oid)
	}
	// Deterministic order: sort by oid.
	var oids []string
	for oid := range send {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	for _, oid := range oids {
		if o := rp.getObj(oid); o != nil {
			objs = append(objs, o)
		}
	}
	if fr.deepen > 0 {
		shallow = boundary
		// unshallow: previously shallow commits now fully included
		oldShallow := map[string]bool{}
		for s := range rp.shallowSet {
			oldShallow[s] = true
		}
		for _, s := range fr.shallow {
			oldShallow[s] = true
		}
		for s := range oldShallow {
			if included[s] {
				isBoundary := false
				for _, b := range boundary {
					if b == s {
						isBoundary = true
						break
					}
				}
				if !isBoundary {
					unshallow = append(unshallow, s)
				}
			}
		}
		sort.Strings(unshallow)
	}
	return objs, shallow, unshallow
}

func writeFetchV2(buf *bytes.Buffer, rp *Repo, fr *fetchRequest) (shallow, unshallow []string) {
	objs, shallow, unshallow := planFetch(rp, fr)
	// acknowledgments section
	if len(fr.haves) > 0 {
		buf.Write(pktLine("acknowledgments\n"))
		buf.Write(pktLine("NAK\n"))
		buf.Write(pktLine("ready\n"))
		buf.Write([]byte("0001")) // delim
	}
	if len(shallow) > 0 || len(unshallow) > 0 {
		buf.Write(pktLine("shallow-info\n"))
		for _, s := range shallow {
			buf.Write(pktLine("shallow " + s + "\n"))
		}
		for _, s := range unshallow {
			buf.Write(pktLine("unshallow " + s + "\n"))
		}
		buf.Write([]byte("0001"))
	}
	buf.Write(pktLine("wanted-refs\n"))
	for _, w := range fr.wants {
		ref := ""
		for _, r := range sortedRefs(rp) {
			if rp.Refs[r] == w {
				ref = r
				break
			}
		}
		if ref == "" {
			ref = "refs/heads/" + rp.DefaultBranch
		}
		buf.Write(pktLine(w + " " + ref + "\n"))
	}
	buf.Write([]byte("0001"))
	buf.Write(pktLine("packfile\n"))
	pack := writePack(objs)
	// sideband: band 1 chunks of up to 65520 bytes
	const maxChunk = 65515
	for len(pack) > 0 {
		n := len(pack)
		if n > maxChunk {
			n = maxChunk
		}
		chunk := pack[:n]
		pack = pack[n:]
		payload := append([]byte{1}, chunk...)
		buf.Write(pktLine(string(payload)))
	}
	buf.Write(pktFlush())
	// shallow boundary recording is the caller's job (recordShallow, under
	// the exclusive lock); this function only reads.
	return shallow, unshallow
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

func gitReceivePack(w http.ResponseWriter, r *http.Request) {
	if !gitAuth(w, r) {
		return
	}
	owner, name, ok := gitRepoFromPath(r.URL.Path)
	if !ok {
		w.WriteHeader(404)
		return
	}
	rp := resolveRepoGit(w, owner, name)
	if rp == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	lines, packStart := parsePktLines(body)
	type cmd struct {
		oldOID, newOID, ref string
	}
	var cmds []cmd
	for _, l := range lines {
		if l == nil {
			break
		}
		s := strings.TrimRight(string(l), "\n")
		if idx := strings.Index(s, "\x00"); idx >= 0 {
			s = s[:idx]
		}
		parts := strings.Fields(s)
		if len(parts) == 3 {
			cmds = append(cmds, cmd{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		}
	}
	// Parse the pack (if any) — it starts at packStart. Take the exclusive
	// lock first: readPack's base lookup reads the repo's objects.
	store.mu.Lock()
	defer store.mu.Unlock()
	var packObjs []*Obj
	if packStart < len(body) && bytes.HasPrefix(body[packStart:], []byte("PACK")) {
		objs, err := readPack(body[packStart:], func(oid string) *Obj { return rp.getObj(oid) })
		if err == nil {
			packObjs = objs
		}
	}
	// Results per command.
	type result struct {
		ok  bool
		msg string
	}
	results := make([]result, len(cmds))
	// First store all pack objects (they may be needed for FF checks).
	for _, o := range packObjs {
		oid := hashObj(o.Type, o.Data)
		if _, exists := rp.Objects[oid]; !exists {
			rp.Objects[oid] = o
		}
	}
	zero := "0000000000000000000000000000000000000000"
	for i, c := range cmds {
		if c.newOID == zero {
			// delete
			if _, exists := rp.Refs[c.ref]; !exists {
				results[i] = result{false, "remote ref does not exist"}
				continue
			}
			delete(rp.Refs, c.ref)
			results[i] = result{true, ""}
			continue
		}
		// new ref or update: must fast-forward
		cur, exists := rp.Refs[c.ref]
		if exists {
			if cur != c.oldOID {
				results[i] = result{false, "non-fast-forward"}
				continue
			}
			// FF check: old must be an ancestor of new
			if !rp.ancestors([]string{c.newOID})[cur] {
				results[i] = result{false, "non-fast-forward"}
				continue
			}
		} else {
			if c.oldOID != zero {
				results[i] = result{false, "remote ref does not exist"}
				continue
			}
		}
		if rp.getObj(c.newOID) == nil {
			results[i] = result{false, "object not found"}
			continue
		}
		rp.Refs[c.ref] = c.newOID
		results[i] = result{true, ""}
	}
	// Build report.
	var report strings.Builder
	report.WriteString("unpack ok\n")
	for i, c := range cmds {
		if results[i].ok {
			fmt.Fprintf(&report, "ok %s\n", c.ref)
		} else {
			fmt.Fprintf(&report, "ng %s %s\n", c.ref, results[i].msg)
		}
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	// sideband band 1: report text in chunks
	rep := report.String()
	const maxChunk = 65515
	for len(rep) > 0 {
		n := len(rep)
		if n > maxChunk {
			n = maxChunk
		}
		chunk := rep[:n]
		rep = rep[n:]
		payload := append([]byte{1}, []byte(chunk)...)
		buf.Write(pktLine(string(payload)))
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}
