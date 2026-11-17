// The forge's store: every repo, issue, pull request, comment, label and
// ref lives in memory in maps. Nothing touches disk (the repo is mounted
// read-only in the trial) and no wall clock or unseeded randomness is used:
// every user-visible timestamp comes from a deterministic counter seeded by
// MAKEFUN_TRIAL_NOW, so a replayed trial leaves byte-identical state.

package main

import (
	"encoding/base64"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type label struct {
	name  string
	color string
}

type issueComment struct {
	id        int
	body      string
	createdAt string
	updatedAt string
	user      userRef
}

type userRef struct {
	login string
	id    int
}

type issue struct {
	id        int
	number    int
	title     string
	body      string
	state     string // "open" | "closed"
	labels    []label
	comments  []*issueComment
	createdAt string
	updatedAt string
	user      userRef
}

type pullRequest struct {
	id            int
	number        int
	title         string
	body          string
	state         string // "open" | "closed"
	merged        bool
	mergedAt      string // "" means null
	mergeCommitSHA string // "" means null
	headRef       string
	baseRef       string
	headSHA       string
	baseSHA       string
	createdAt     string
	updatedAt     string
	user          userRef
}

type repo struct {
	id                 int
	databaseID         int
	name               string
	owner              string
	description        string
	private            bool
	defaultBranch      string
	createdAt          string
	updatedAt          string
	objects            map[string]*gitObject
	refs               map[string]string
	labels             map[string]label
	issues             map[int]*issue
	nextIssueNumber    int
	nextCommentID      int
	pulls              map[int]*pullRequest
	nextPullNumber     int
	shallowBoundaries  map[string]bool
	version            int
	filesCache         map[string]string
	filesCacheCommit   string
	treeOidsCache      map[string]string
	treeOidsCacheCommit string
	// git HTTP caches (per repo, invalidated by version where noted)
	advCache      *cachedAdvertisements
	packCache     map[string][]byte
	responseCache map[string][]byte
	// REST cached views (key -> versioned pre-serialized body)
	viewCache map[string]viewCacheEntry
}

type store struct {
	repos       map[string]*repo
	nextRepoID  int
	clockSeconds int64
	idCounter   int
}

func newStore() *store {
	rawNow := os.Getenv("MAKEFUN_TRIAL_NOW")
	clockSeconds := int64(1700000000)
	if rawNow != "" {
		if t, err := time.Parse(time.RFC3339, rawNow); err == nil {
			clockSeconds = t.Unix()
		} else if ms, err2 := strconv.ParseInt(rawNow, 10, 64); err2 == nil {
			clockSeconds = ms / 1000
		}
	}
	return &store{repos: map[string]*repo{}, nextRepoID: 1, clockSeconds: clockSeconds, idCounter: 1}
}

// nowISO returns the next deterministic timestamp, one second later than the
// previous.
func (s *store) nowISO() string {
	s.clockSeconds++
	return time.Unix(s.clockSeconds, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// nextID is a strictly increasing number for synthetic ids that must never repeat.
func (s *store) nextID() int {
	s.idCounter++
	return s.idCounter
}

func (s *store) repoKey(owner, name string) string { return owner + "/" + name }

func (s *store) getRepo(owner, name string) *repo { return s.repos[s.repoKey(owner, name)] }

func (s *store) listReposFor(owner string) []*repo {
	var out []*repo
	for _, r := range s.repos {
		if r.owner == owner {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (s *store) createRepo(owner, name, description string, isPrivate bool) *repo {
	id := s.nextRepoID
	s.nextRepoID++
	stamp := s.nowISO()
	r := &repo{
		id:                id,
		databaseID:        id,
		name:              name,
		owner:             owner,
		description:       description,
		private:           isPrivate,
		defaultBranch:     "main",
		createdAt:         stamp,
		updatedAt:         stamp,
		objects:           map[string]*gitObject{},
		refs:              map[string]string{},
		labels:            map[string]label{},
		issues:            map[int]*issue{},
		nextIssueNumber:   1,
		nextCommentID:     1,
		pulls:             map[int]*pullRequest{},
		nextPullNumber:    1,
		shallowBoundaries: map[string]bool{},
		version:           1,
	}
	s.repos[s.repoKey(owner, name)] = r
	return r
}

func (s *store) deleteRepo(owner, name string) bool {
	key := s.repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

func (s *store) touch(r *repo) {
	r.version++
	r.updatedAt = s.nowISO()
}

// -------------------------------------------------------------------------------------------
// git objects and refs
// -------------------------------------------------------------------------------------------

func (s *store) putObject(r *repo, typ gitObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &gitObject{typ: typ, data: data}
	}
	return oid
}

func (s *store) lookup(r *repo) objectLookup {
	return func(oid string) *gitObject { return r.objects[oid] }
}

func (s *store) headRefName(r *repo) string { return "refs/heads/" + r.defaultBranch }

func (s *store) headCommit(r *repo) string {
	if v, ok := r.refs[s.headRefName(r)]; ok {
		return v
	}
	return ""
}

func (s *store) refTip(r *repo, ref string) string {
	if ref == "HEAD" {
		return s.headCommit(r)
	}
	if v, ok := r.refs[ref]; ok {
		return v
	}
	if v, ok := r.refs["refs/heads/"+ref]; ok {
		return v
	}
	return ""
}

// filesOfCommit returns the flat path -> blob oid map of the tree at
// commitOid (cached per tip).
func (s *store) filesOfCommit(r *repo, commitOid string) map[string]string {
	if r.filesCacheCommit == commitOid && r.filesCache != nil {
		return r.filesCache
	}
	out := map[string]string{}
	root := r.objects[commitOid]
	if root != nil && root.typ == objCommit {
		var walk func(treeOid, prefix string)
		walk = func(treeOid, prefix string) {
			tree := r.objects[treeOid]
			if tree == nil || tree.typ != objTree {
				return
			}
			for _, entry := range cachedParseTree(tree.data) {
				path := entry.name
				if prefix != "" {
					path = prefix + "/" + entry.name
				}
				if entry.mode == "40000" {
					walk(entry.oid, path)
				} else if entry.mode != "160000" {
					out[path] = entry.oid
				}
			}
		}
		walk(cachedParseCommit(root.data).tree, "")
	}
	r.filesCache = out
	r.filesCacheCommit = commitOid
	return out
}

// treeOidsOfCommit returns path -> tree oid of every directory in the tree
// at commitOid (cached per commit).
func (s *store) treeOidsOfCommit(r *repo, commitOid string) map[string]string {
	if r.treeOidsCacheCommit == commitOid && r.treeOidsCache != nil {
		return r.treeOidsCache
	}
	out := map[string]string{}
	root := r.objects[commitOid]
	if root != nil && root.typ == objCommit {
		var walk func(treeOid, prefix string)
		walk = func(treeOid, prefix string) {
			tree := r.objects[treeOid]
			if tree == nil || tree.typ != objTree {
				return
			}
			for _, entry := range cachedParseTree(tree.data) {
				if entry.mode == "40000" {
					path := entry.name
					if prefix != "" {
						path = prefix + "/" + entry.name
					}
					out[path] = entry.oid
					walk(entry.oid, path)
				}
			}
		}
		walk(cachedParseCommit(root.data).tree, "")
	}
	r.treeOidsCache = out
	r.treeOidsCacheCommit = commitOid
	return out
}

// buildTreeFromFiles builds (and stores) a tree from a flat path -> blob oid map.
func (s *store) buildTreeFromFiles(r *repo, files map[string]string) string {
	var buildLevel func(prefix string) string
	buildLevel = func(prefix string) string {
		var entries []treeEntry
		dirs := map[string]bool{}
		for path, oid := range files {
			if prefix != "" && !strings.HasPrefix(path, prefix+"/") {
				continue
			}
			rest := path
			if prefix != "" {
				rest = path[len(prefix)+1:]
			}
			if slash := strings.Index(rest, "/"); slash < 0 {
				entries = append(entries, treeEntry{mode: "100644", name: rest, oid: oid})
			} else {
				dirs[rest[:slash]] = true
			}
		}
		var dirNames []string
		for d := range dirs {
			dirNames = append(dirNames, d)
		}
		sort.Strings(dirNames)
		for _, dir := range dirNames {
			childPrefix := dir
			if prefix != "" {
				childPrefix = prefix + "/" + dir
			}
			entries = append(entries, treeEntry{mode: "40000", name: dir, oid: buildLevel(childPrefix)})
		}
		return s.putObject(r, objTree, buildTree(entries))
	}
	return buildLevel("")
}

type commitResult struct {
	commitOid string
	treeOid   string
}

// commitFiles creates a commit on a branch (or on the default branch when it
// is unborn).
func (s *store) commitFiles(r *repo, branch, message string, apply func(files map[string]string)) commitResult {
	ref := "refs/heads/" + branch
	tip, hasTip := r.refs[ref]
	files := map[string]string{}
	if hasTip {
		for k, v := range s.filesOfCommit(r, tip) {
			files[k] = v
		}
	}
	apply(files)
	treeOid := s.buildTreeFromFiles(r, files)
	author := VIEWER.name + " <" + VIEWER.email + ">"
	s.nowISO()
	stamp := s.clockSeconds
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	commitOid := s.putObject(r, objCommit, buildCommit(treeOid, parents, message, author, author, stamp))
	r.refs[ref] = commitOid
	s.touch(r)
	return commitResult{commitOid: commitOid, treeOid: treeOid}
}

// -------------------------------------------------------------------------------------------
// diffs and merges
// -------------------------------------------------------------------------------------------

func (s *store) lineCounts(oldContent, newContent []byte) (additions, deletions int) {
	var oldLines, newLines []string
	if oldContent != nil {
		oldLines = strings.Split(string(oldContent), "\n")
	}
	if newContent != nil {
		newLines = strings.Split(string(newContent), "\n")
	}
	oldCounts := map[string]int{}
	for _, line := range oldLines {
		oldCounts[line]++
	}
	newCounts := map[string]int{}
	for _, line := range newLines {
		newCounts[line]++
	}
	for line, count := range newCounts {
		if d := count - oldCounts[line]; d > 0 {
			additions += d
		}
	}
	for line, count := range oldCounts {
		if d := count - newCounts[line]; d > 0 {
			deletions += d
		}
	}
	return additions, deletions
}

type diffEntry struct {
	filename  string
	status    string // "added" | "removed" | "modified"
	additions int
	deletions int
	changes   int
}

func (s *store) diffFiles(r *repo, baseCommit, headCommit string) []diffEntry {
	base := map[string]string{}
	if baseCommit != "" {
		base = s.filesOfCommit(r, baseCommit)
	}
	head := map[string]string{}
	if headCommit != "" {
		head = s.filesOfCommit(r, headCommit)
	}
	pathSet := map[string]bool{}
	for k := range base {
		pathSet[k] = true
	}
	for k := range head {
		pathSet[k] = true
	}
	var paths []string
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []diffEntry
	for _, path := range paths {
		baseOid, hasBase := base[path]
		headOid, hasHead := head[path]
		if hasBase && hasHead && baseOid == headOid {
			continue
		}
		var baseBlob, headBlob []byte
		if hasBase {
			if o := r.objects[baseOid]; o != nil {
				baseBlob = o.data
			}
		}
		if hasHead {
			if o := r.objects[headOid]; o != nil {
				headBlob = o.data
			}
		}
		additions, deletions := s.lineCounts(baseBlob, headBlob)
		status := "modified"
		if !hasBase {
			status = "added"
		} else if !hasHead {
			status = "removed"
		}
		out = append(out, diffEntry{filename: path, status: status, additions: additions, deletions: deletions, changes: additions + deletions})
	}
	if out == nil {
		out = []diffEntry{}
	}
	return out
}

// mergeable is true when head can be squash-merged onto base without a conflict.
func (s *store) mergeable(r *repo, baseSha, headSha string) bool {
	if baseSha == headSha {
		return true
	}
	base := mergeBase(baseSha, headSha, s.lookup(r))
	if base == "" {
		return false
	}
	baseFiles := s.filesOfCommit(r, base)
	baseTip := s.filesOfCommit(r, baseSha)
	headTip := s.filesOfCommit(r, headSha)
	paths := map[string]bool{}
	for k := range baseFiles {
		paths[k] = true
	}
	for k := range baseTip {
		paths[k] = true
	}
	for k := range headTip {
		paths[k] = true
	}
	for path := range paths {
		original := baseFiles[path]
		onBase := baseTip[path]
		onHead := headTip[path]
		baseChanged := original != onBase
		headChanged := original != onHead
		if baseChanged && headChanged && onBase != onHead {
			return false
		}
	}
	return true
}

// -------------------------------------------------------------------------------------------
// issues, pulls, comments
// -------------------------------------------------------------------------------------------

func (s *store) createIssue(r *repo, title, body string, labelNames []string) (*issue, error) {
	var labels []label
	for _, name := range labelNames {
		l, ok := r.labels[name]
		if !ok {
			return nil, errLabelMissing(name)
		}
		labels = append(labels, l)
	}
	number := r.nextIssueNumber
	r.nextIssueNumber++
	stamp := s.nowISO()
	iss := &issue{
		id:        r.databaseID*100000 + number,
		number:    number,
		title:     title,
		body:      body,
		state:     "open",
		labels:    labels,
		comments:  []*issueComment{},
		createdAt: stamp,
		updatedAt: stamp,
		user:      userRef{login: VIEWER.login, id: VIEWER.id},
	}
	r.issues[number] = iss
	s.touch(r)
	return iss, nil
}

type labelError string

func errLabelMissing(name string) error { return labelError("label " + name + " does not exist") }
func (e labelError) Error() string      { return string(e) }

func (s *store) addComment(r *repo, iss *issue, body string) *issueComment {
	stamp := s.nowISO()
	comment := &issueComment{
		id:        r.nextCommentID,
		body:      body,
		createdAt: stamp,
		updatedAt: stamp,
		user:      userRef{login: VIEWER.login, id: VIEWER.id},
	}
	r.nextCommentID++
	iss.comments = append(iss.comments, comment)
	s.touch(r)
	return comment
}

func (s *store) createPull(r *repo, title, body, headRef, baseRef, headSHA, baseSHA string) *pullRequest {
	number := r.nextPullNumber
	r.nextPullNumber++
	stamp := s.nowISO()
	pull := &pullRequest{
		id:            r.databaseID*100000 + number,
		number:        number,
		title:         title,
		body:          body,
		state:         "open",
		merged:        false,
		mergedAt:      "",
		mergeCommitSHA: "",
		headRef:       headRef,
		baseRef:       baseRef,
		headSHA:       headSHA,
		baseSHA:       baseSHA,
		createdAt:     stamp,
		updatedAt:     stamp,
		user:          userRef{login: VIEWER.login, id: VIEWER.id},
	}
	r.pulls[number] = pull
	s.touch(r)
	return pull
}

type mergeResult struct {
	ok     bool
	sha    string
	reason string
}

// mergePull squash-merges an open pull request onto its base branch: the
// merge commit carries the head tree and the base tip as its only parent.
func (s *store) mergePull(r *repo, pull *pullRequest) mergeResult {
	if pull.state != "open" {
		return mergeResult{ok: false, reason: "already merged or closed"}
	}
	baseRef := "refs/heads/" + pull.baseRef
	baseTip, ok := r.refs[baseRef]
	if !ok {
		return mergeResult{ok: false, reason: "base branch is missing"}
	}
	if !s.mergeable(r, baseTip, pull.headSHA) {
		return mergeResult{ok: false, reason: "conflict"}
	}
	headFiles := s.filesOfCommit(r, pull.headSHA)
	treeOid := s.buildTreeFromFiles(r, headFiles)
	author := VIEWER.name + " <" + VIEWER.email + ">"
	s.nowISO()
	stamp := s.clockSeconds
	message := "Merge pull request #" + itoa(pull.number) + " from " + pull.headRef + "\n\n" + pull.title + "\n"
	sha := s.putObject(r, objCommit, buildCommit(treeOid, []string{baseTip}, message, author, author, stamp))
	r.refs[baseRef] = sha
	pull.state = "closed"
	pull.merged = true
	pull.mergedAt = s.nowISO()
	pull.mergeCommitSHA = sha
	pull.updatedAt = pull.mergedAt
	s.touch(r)
	return mergeResult{ok: true, sha: sha}
}

// -------------------------------------------------------------------------------------------
// global ids and reachability helpers used by several surfaces
// -------------------------------------------------------------------------------------------

func globalID(kind string, parts []string) string {
	joined := kind + ":" + strings.Join(parts, ":")
	return base64.RawURLEncoding.EncodeToString([]byte(joined))
}

func decodeGlobalID(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	text := string(raw)
	split := strings.Split(text, ":")
	if len(split) < 2 || split[0] == "" {
		return "", nil, false
	}
	return split[0], split[1:], true
}

type historyEntry struct {
	sha       string
	message   string
	author    string
	committer string
}

func (s *store) commitHistory(r *repo, fromSHA string, limit int) []historyEntry {
	var out []historyEntry
	if fromSHA == "" {
		return out
	}
	seen := map[string]bool{}
	type item struct{ sha string }
	queue := []item{{sha: fromSHA}}
	queueHead := 0
	for queueHead < len(queue) && len(out) < limit {
		sha := queue[queueHead].sha
		queueHead++
		if seen[sha] {
			continue
		}
		seen[sha] = true
		object := r.objects[sha]
		if object == nil || object.typ != objCommit {
			continue
		}
		parsed := cachedParseCommit(object.data)
		out = append(out, historyEntry{sha: sha, message: parsed.message, author: parsed.author, committer: parsed.committer})
		for _, parent := range parsed.parents {
			queue = append(queue, item{sha: parent})
		}
	}
	if out == nil {
		out = []historyEntry{}
	}
	return out
}

func (s *store) reachableObjects(r *repo, roots []string) (map[string]bool, error) {
	return collectReachable(roots, s.lookup(r))
}

func (s *store) isAncestorOf(r *repo, ancestor, tip string) bool {
	return isAncestor(ancestor, tip, s.lookup(r))
}

// -------------------------------------------------------------------------------------------
// JSON views
// -------------------------------------------------------------------------------------------

func userView(u userRef) map[string]any {
	return map[string]any{"login": u.login, "id": u.id}
}

func (s *store) repoView(r *repo) map[string]any {
	ownerID := 1001
	if r.owner == VIEWER.login {
		ownerID = VIEWER.id
	}
	openIssues := 0
	for _, iss := range r.issues {
		if iss.state == "open" {
			openIssues++
		}
	}
	return map[string]any{
		"id":                r.id,
		"databaseId":        r.databaseID,
		"name":              r.name,
		"full_name":         r.owner + "/" + r.name,
		"owner":             map[string]any{"login": r.owner, "id": ownerID},
		"private":           r.private,
		"description":       r.description,
		"default_branch":    r.defaultBranch,
		"created_at":        r.createdAt,
		"updated_at":        r.updatedAt,
		"has_issues":        true,
		"has_projects":      true,
		"has_wiki":          true,
		"has_pages":         false,
		"forks_count":       0,
		"open_issues_count": openIssues,
		"watchers_count":    0,
		"stargazers_count":  0,
	}
}

func (s *store) issueView(iss *issue, r *repo) map[string]any {
	var labels []any
	for _, l := range iss.labels {
		labels = append(labels, map[string]any{"name": l.name, "color": l.color})
	}
	if labels == nil {
		labels = []any{}
	}
	return map[string]any{
		"id":         iss.id,
		"number":     iss.number,
		"title":      iss.title,
		"body":       iss.body,
		"state":      iss.state,
		"labels":     labels,
		"comments":   len(iss.comments),
		"created_at": iss.createdAt,
		"updated_at": iss.updatedAt,
		"user":       userView(iss.user),
		"repository": map[string]any{"id": r.id, "name": r.name, "full_name": r.owner + "/" + r.name},
	}
}

func (s *store) pullView(pull *pullRequest, r *repo) map[string]any {
	mergedAt := any(nil)
	if pull.mergedAt != "" {
		mergedAt = pull.mergedAt
	}
	mergeSHA := any(nil)
	if pull.mergeCommitSHA != "" {
		mergeSHA = pull.mergeCommitSHA
	}
	return map[string]any{
		"id":               pull.id,
		"number":           pull.number,
		"title":            pull.title,
		"body":             pull.body,
		"state":            pull.state,
		"merged":           pull.merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"head":             map[string]any{"ref": pull.headRef, "sha": pull.headSHA},
		"base":             map[string]any{"ref": pull.baseRef, "sha": pull.baseSHA},
		"created_at":       pull.createdAt,
		"updated_at":       pull.updatedAt,
		"user":             userView(pull.user),
		"repository":       map[string]any{"id": r.id, "name": r.name, "full_name": r.owner + "/" + r.name},
	}
}

func (s *store) commentView(c *issueComment) map[string]any {
	return map[string]any{
		"id":         c.id,
		"body":       c.body,
		"created_at": c.createdAt,
		"updated_at": c.updatedAt,
		"user":       userView(c.user),
	}
}
