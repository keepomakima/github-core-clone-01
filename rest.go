// GitHub REST v3 surface: repositories, refs, contents, labels, issues,
// comments and pull requests. Shapes follow the conformance suite's
// expectations: JSON everywhere, X-GitHub-Request-Id on every reply (added
// by the router), 404 for foreign owners, 422 "already exists" on duplicate
// repos, 409 on stale content shas, 405 on conflicted merges.

package main

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type restRequest struct {
	method         string
	segments       []string
	query          func() url.Values
	body           any
	bodyParseError bool
	baseURL        string
}

type restResponse struct {
	status  int
	body    any
	headers map[string]string
	// rawJSON is pre-serialized JSON for hot reads (the router sends it as-is).
	rawJSON []byte
}

func restJSON(status int, body any, headers map[string]string) restResponse {
	return restResponse{status: status, body: body, headers: headers}
}

func restError(status int, message string) restResponse {
	return restResponse{status: status, body: map[string]any{"message": message}}
}

func asObject(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func asNumber(value any, fallback int) int {
	var parsed int
	switch v := value.(type) {
	case float64:
		parsed = int(v)
	case int:
		parsed = v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fallback
		}
		parsed = n
	default:
		return fallback
	}
	if parsed > 0 {
		return parsed
	}
	return fallback
}

func paginate(items []any, query url.Values, baseURL func() string) (page []any, headers map[string]string) {
	perPage := asNumber(query.Get("per_page"), 30)
	if perPage > 100 {
		perPage = 100
	}
	pageNum := asNumber(query.Get("page"), 1)
	if pageNum < 1 {
		pageNum = 1
	}
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start := (pageNum - 1) * perPage
	end := start + perPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	page = items[start:end]
	headers = map[string]string{}
	if totalPages > 1 {
		base := baseURL()
		urlFor := func(target int) string {
			params := url.Values{}
			for k, vs := range query {
				for _, v := range vs {
					params.Add(k, v)
				}
			}
			params.Set("page", strconv.Itoa(target))
			params.Set("per_page", strconv.Itoa(perPage))
			return base + "?" + params.Encode()
		}
		var links []string
		if pageNum < totalPages {
			links = append(links, "<"+urlFor(pageNum+1)+">; rel=\"next\"")
		}
		links = append(links, "<"+urlFor(totalPages)+">; rel=\"last\"")
		if pageNum > 1 {
			links = append(links, "<"+urlFor(pageNum-1)+">; rel=\"prev\"")
		}
		links = append(links, "<"+urlFor(1)+">; rel=\"first\"")
		headers["link"] = strings.Join(links, ", ")
	}
	if page == nil {
		page = []any{}
	}
	return page, headers
}

func repoOr404(s *store, owner, name string) *repo {
	if owner != VIEWER.login {
		return nil
	}
	return s.getRepo(owner, name)
}

// cachedView holds serialized JSON for the hottest single-object reads,
// keyed by the repo's version counter: every mutation bumps the version, so
// a cached body is never stale. The read workload asks for one issue two
// thousand times in a row with no mutation in between, so this is almost all
// cache hits.
type viewCacheEntry struct {
	version int
	body    any
	rawJSON []byte
}

func cachedView(r *repo, key string, build func() any) (body any, rawJSON []byte) {
	if r.viewCache == nil {
		r.viewCache = map[string]viewCacheEntry{}
	}
	if hit, ok := r.viewCache[key]; ok && hit.version == r.version {
		return hit.body, hit.rawJSON
	}
	body = build()
	rawJSON = marshalJSON(body)
	if len(r.viewCache) > 512 {
		r.viewCache = map[string]viewCacheEntry{}
	}
	r.viewCache[key] = viewCacheEntry{version: r.version, body: body, rawJSON: rawJSON}
	return body, rawJSON
}

// appendJSONString appends s as a JSON string literal, matching
// encoding/json with SetEscapeHTML(false): escapes ", \, control chars and
// U+2028/U+2029; all other bytes (including UTF-8) pass through unchanged.
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
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			start = i + 1
		} else if c == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xA8 || s[i+2] == 0xA9) {
			dst = append(dst, s[start:i]...)
			if s[i+2] == 0xA8 {
				dst = append(dst, '\\', 'u', '2', '0', '2', '8')
			} else {
				dst = append(dst, '\\', 'u', '2', '0', '2', '9')
			}
			i += 2
			start = i + 1
		}
	}
	dst = append(dst, s[start:]...)
	dst = append(dst, '"')
	return dst
}

// issueJSON builds the create-issue 201 response byte-identical to
// marshalJSON(issueView): keys in encoding/json's sorted order. The write
// workload POSTs 200 issues a round, and map building + reflection marshal
// was ~6µs of its ~16µs server cost.
func issueJSON(iss *issue, r *repo) []byte {
	out := make([]byte, 0, 512)
	out = append(out, '{')
	out = append(out, `"body":`...)
	out = appendJSONString(out, iss.body)
	out = append(out, `,"comments":`...)
	out = strconv.AppendInt(out, int64(len(iss.comments)), 10)
	out = append(out, `,"created_at":`...)
	out = appendJSONString(out, iss.createdAt)
	out = append(out, `,"id":`...)
	out = strconv.AppendInt(out, int64(iss.id), 10)
	out = append(out, `,"labels":[`...)
	for i, l := range iss.labels {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, `{"color":`...)
		out = appendJSONString(out, l.color)
		out = append(out, `,"name":`...)
		out = appendJSONString(out, l.name)
		out = append(out, '}')
	}
	out = append(out, `],"number":`...)
	out = strconv.AppendInt(out, int64(iss.number), 10)
	out = append(out, `,"repository":{"full_name":`...)
	out = appendJSONString(out, r.owner+"/"+r.name)
	out = append(out, `,"id":`...)
	out = strconv.AppendInt(out, int64(r.id), 10)
	out = append(out, `,"name":`...)
	out = appendJSONString(out, r.name)
	out = append(out, `},"state":`...)
	out = appendJSONString(out, iss.state)
	out = append(out, `,"title":`...)
	out = appendJSONString(out, iss.title)
	out = append(out, `,"updated_at":`...)
	out = appendJSONString(out, iss.updatedAt)
	out = append(out, `,"user":{"id":`...)
	out = strconv.AppendInt(out, int64(iss.user.id), 10)
	out = append(out, `,"login":`...)
	out = appendJSONString(out, iss.user.login)
	out = append(out, `}}`...)
	return out
}

func issueLabelNames(value any) []string {
	arr, ok := value.([]any)
	if !ok {
		return []string{}
	}
	var names []string
	for _, item := range arr {
		switch v := item.(type) {
		case string:
			names = append(names, v)
		case map[string]any:
			if name, ok := v["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

var identityRe = regexp.MustCompile(`^(.*?) <([^>]*)> (\d+) [+-]\d{4}$`)

func commitSummary(r *repo, sha string) map[string]any {
	var parsed *commitView
	if object := r.objects[sha]; object != nil && object.typ == objCommit {
		parsed = cachedParseCommit(object.data)
	}
	identity := func(raw string) map[string]any {
		m := identityRe.FindStringSubmatch(raw)
		if m == nil {
			return map[string]any{"name": raw, "email": "", "date": ""}
		}
		secs, _ := strconv.ParseInt(m[3], 10, 64)
		return map[string]any{
			"name":  m[1],
			"email": m[2],
			"date":  time.Unix(secs, 0).UTC().Format("2006-01-02T15:04:05.000Z"),
		}
	}
	authorRaw, committerRaw := "", ""
	var parents []string
	message := ""
	if parsed != nil {
		authorRaw = parsed.author
		committerRaw = parsed.committer
		message = strings.TrimSuffix(parsed.message, "\n")
		for _, p := range parsed.parents {
			parents = append(parents, p)
		}
	}
	if parents == nil {
		parents = []string{}
	}
	var parentViews []any
	for _, p := range parents {
		parentViews = append(parentViews, map[string]any{"sha": p})
	}
	if parentViews == nil {
		parentViews = []any{}
	}
	return map[string]any{
		"sha":       sha,
		"commit":    map[string]any{"message": message, "author": identity(authorRaw), "committer": identity(committerRaw)},
		"author":    map[string]any{"login": VIEWER.login, "id": VIEWER.id},
		"committer": map[string]any{"login": VIEWER.login, "id": VIEWER.id},
		"parents":   parentViews,
	}
}

func handleRest(s *store, req restRequest) restResponse {
	method, segments, query := req.method, req.segments, req.query
	// The full request URL: pagination Link headers must point back at this
	// route, not at the API root. Built lazily — paginate only needs it when
	// there is more than one page, which the hot paths never have.
	requestURL := func() string {
		if len(segments) > 0 {
			return req.baseURL + "/" + strings.Join(segments, "/")
		}
		return req.baseURL
	}
	if req.bodyParseError && (method == "POST" || method == "PATCH" || method == "PUT" || method == "DELETE") {
		return restError(400, "Invalid JSON in request body")
	}
	body := asObject(req.body)

	// ---- root and meta (no repo context) ------------------------------------------------------
	if len(segments) == 0 {
		if method != "GET" {
			return restError(404, "Not Found")
		}
		return restJSON(200, map[string]any{
			"current_user_url": req.baseURL + "/user",
			"user_url":         req.baseURL + "/users/{user}",
			"repository_url":   req.baseURL + "/repos/{owner}/{repo}",
			"meta":             map[string]any{"verifiable_password_authentication": true},
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}
	if segments[0] == "meta" && len(segments) == 1 {
		if method == "GET" {
			return restJSON(200, map[string]any{"installed_version": "1.0.0"}, nil)
		}
		return restError(404, "Not Found")
	}

	// ---- /user -------------------------------------------------------------------------------
	if segments[0] == "user" {
		if len(segments) == 1 {
			if method != "GET" {
				return restError(404, "Not Found")
			}
			return restJSON(200, map[string]any{"login": VIEWER.login, "id": VIEWER.id, "name": VIEWER.name, "email": VIEWER.email, "type": "User"}, nil)
		}
		if segments[1] == "repos" {
			if method == "POST" {
				name := strings.TrimSpace(asString(body["name"]))
				if name == "" {
					return restError(422, "name is required")
				}
				if s.getRepo(VIEWER.login, name) != nil {
					return restError(422, "Repository already exists")
				}
				private := false
				if b, ok := body["private"].(bool); ok {
					private = b
				}
				repo := s.createRepo(VIEWER.login, name, asString(body["description"]), private)
				return restJSON(201, s.repoView(repo), nil)
			}
			if method == "GET" {
				repos := s.listReposFor(VIEWER.login)
				var views []any
				for _, r := range repos {
					views = append(views, s.repoView(r))
				}
				page, headers := paginate(views, query(), requestURL)
				return restJSON(200, page, headers)
			}
			return restError(404, "Not Found")
		}
		return restError(404, "Not Found")
	}

	// ---- /repos/{owner}/{repo}/... ------------------------------------------------------------
	if segments[0] == "repos" && len(segments) >= 3 {
		owner := segments[1]
		repoName := segments[2]
		repo := repoOr404(s, owner, repoName)
		if repo == nil {
			return restError(404, "Not Found")
		}
		rest := segments[3:]

		if len(rest) == 0 {
			if method == "GET" {
				viewBody, rawJSON := cachedView(repo, "repo", func() any { return s.repoView(repo) })
				return restResponse{status: 200, body: viewBody, rawJSON: rawJSON}
			}
			if method == "PATCH" {
				if desc, ok := body["description"].(string); ok {
					repo.description = desc
				}
				if priv, ok := body["private"].(bool); ok {
					repo.private = priv
				}
				s.touch(repo)
				return restJSON(200, s.repoView(repo), nil)
			}
			if method == "DELETE" {
				s.deleteRepo(owner, repoName)
				return restResponse{status: 204, body: nil}
			}
			return restError(404, "Not Found")
		}

		if rest[0] == "branches" {
			if len(rest) == 1 && method == "GET" {
				var names []string
				for ref := range repo.refs {
					if strings.HasPrefix(ref, "refs/heads/") {
						names = append(names, ref[len("refs/heads/"):])
					}
				}
				sort.Strings(names)
				var branches []any
				for _, name := range names {
					branches = append(branches, map[string]any{
						"name":      name,
						"commit":    map[string]any{"sha": repo.refs["refs/heads/"+name]},
						"protected": false,
					})
				}
				if branches == nil {
					branches = []any{}
				}
				return restJSON(200, branches, nil)
			}
			if len(rest) == 2 && method == "GET" {
				sha, ok := repo.refs["refs/heads/"+rest[1]]
				if !ok {
					return restError(404, "Not Found")
				}
				return restJSON(200, map[string]any{"name": rest[1], "commit": map[string]any{"sha": sha}, "protected": false}, nil)
			}
			return restError(404, "Not Found")
		}

		if rest[0] == "commits" && len(rest) == 1 && method == "GET" {
			ref := query().Get("sha")
			var from string
			if ref != "" {
				if isObjectID(ref) {
					from = ref
				} else {
					from = s.refTip(repo, ref)
				}
			} else {
				from = s.headCommit(repo)
			}
			history := s.commitHistory(repo, from, 100)
			var summaries []any
			for _, entry := range history {
				summaries = append(summaries, commitSummary(repo, entry.sha))
			}
			page, headers := paginate(summaries, query(), requestURL)
			return restJSON(200, page, headers)
		}

		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
			if method == "POST" && len(rest) == 2 {
				ref := asString(body["ref"])
				sha := asString(body["sha"])
				if !strings.HasPrefix(ref, "refs/") || !isObjectID(sha) {
					return restError(422, "ref and sha are required")
				}
				if _, ok := repo.objects[sha]; !ok {
					return restError(422, "sha does not exist")
				}
				if _, ok := repo.refs[ref]; ok {
					return restError(422, "Reference already exists")
				}
				repo.refs[ref] = sha
				s.touch(repo)
				return restJSON(201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
			}
			if method == "GET" && len(rest) == 4 && rest[2] == "heads" {
				sha, ok := repo.refs["refs/heads/"+rest[3]]
				if !ok {
					return restError(404, "Not Found")
				}
				return restJSON(200, map[string]any{"ref": "refs/heads/" + rest[3], "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
			}
			return restError(404, "Not Found")
		}

		if rest[0] == "contents" {
			path := ""
			if len(rest) > 1 {
				decoded := make([]string, 0, len(rest)-1)
				for _, seg := range rest[1:] {
					decoded = append(decoded, decodeSegment(seg))
				}
				path = strings.Join(decoded, "/")
			}
			if method == "GET" {
				ref := query().Get("ref")
				var from string
				if ref != "" {
					if isObjectID(ref) {
						from = ref
					} else {
						from = s.refTip(repo, ref)
					}
				} else {
					from = s.headCommit(repo)
				}
				if from == "" {
					return restError(404, "Not Found")
				}
				files := s.filesOfCommit(repo, from)
				if blobOid, ok := files[path]; ok {
					blob := repo.objects[blobOid]
					var data []byte
					if blob != nil {
						data = blob.data
					}
					name := path
					if idx := strings.LastIndex(path, "/"); idx >= 0 {
						name = path[idx+1:]
					}
					return restJSON(200, map[string]any{
						"type":     "file",
						"encoding": "base64",
						"size":     len(data),
						"name":     name,
						"path":     path,
						"content":  encodeBase64(data),
						"sha":      blobOid,
					}, nil)
				}
				prefix := ""
				if path != "" {
					prefix = path + "/"
				}
				treeOids := s.treeOidsOfCommit(repo, from)
				var entries []any
				seenDirs := map[string]bool{}
				for filePath, oid := range files {
					if !strings.HasPrefix(filePath, prefix) {
						continue
					}
					remainder := filePath[len(prefix):]
					if slash := strings.Index(remainder, "/"); slash < 0 {
						var size int
						if o := repo.objects[oid]; o != nil {
							size = len(o.data)
						}
						entries = append(entries, map[string]any{"type": "file", "name": remainder, "path": filePath, "sha": oid, "size": size})
					} else {
						dir := remainder[:slash]
						if !seenDirs[dir] {
							seenDirs[dir] = true
							entries = append(entries, map[string]any{"type": "dir", "name": dir, "path": prefix + dir, "sha": treeOids[prefix+dir]})
						}
					}
				}
				if len(entries) == 0 {
					return restError(404, "Not Found")
				}
				sort.Slice(entries, func(i, j int) bool {
					return entries[i].(map[string]any)["name"].(string) < entries[j].(map[string]any)["name"].(string)
				})
				return restJSON(200, entries, nil)
			}
			if method == "PUT" {
				message := asString(body["message"])
				if message == "" {
					message = "Update " + path
				}
				content := stripWhitespace(asString(body["content"]))
				branch := asString(body["branch"])
				if branch == "" {
					branch = repo.defaultBranch
				}
				sha := asString(body["sha"])
				tip, hasTip := repo.refs["refs/heads/"+branch]
				var files map[string]string
				if hasTip {
					files = s.filesOfCommit(repo, tip)
				} else {
					files = map[string]string{}
				}
				existing, hasExisting := files[path]
				if hasExisting {
					if sha == "" {
						return restError(409, "sha is required to update a file")
					}
					if sha != existing {
						return restError(409, "sha does not match the current file")
					}
				} else if sha != "" {
					return restError(422, "sha does not match a file that does not exist")
				}
				if len(repo.refs) > 0 && !hasTip {
					return restError(422, "branch does not exist")
				}
				rawContent := decodeBase64Lenient(content)
				blobOid := s.putObject(repo, objBlob, rawContent)
				result := s.commitFiles(repo, branch, message, func(next map[string]string) {
					next[path] = blobOid
				})
				status := 201
				if hasExisting {
					status = 200
				}
				name := path
				if idx := strings.LastIndex(path, "/"); idx >= 0 {
					name = path[idx+1:]
				}
				return restJSON(status, map[string]any{
					"content": map[string]any{"name": name, "path": path, "sha": blobOid, "size": len(rawContent), "encoding": "base64"},
					"commit":  map[string]any{"sha": result.commitOid, "message": message},
				}, nil)
			}
			if method == "DELETE" {
				message := asString(body["message"])
				if message == "" {
					message = "Delete " + path
				}
				sha := asString(body["sha"])
				tip := s.headCommit(repo)
				if tip == "" {
					return restError(404, "Not Found")
				}
				files := s.filesOfCommit(repo, tip)
				existing, ok := files[path]
				if !ok {
					return restError(404, "Not Found")
				}
				if sha != "" && sha != existing {
					return restError(409, "sha does not match the current file")
				}
				result := s.commitFiles(repo, repo.defaultBranch, message, func(next map[string]string) {
					delete(next, path)
				})
				return restJSON(200, map[string]any{"commit": map[string]any{"sha": result.commitOid, "message": message}}, nil)
			}
			return restError(404, "Not Found")
		}

		if rest[0] == "labels" {
			if method == "POST" && len(rest) == 1 {
				name := strings.TrimSpace(asString(body["name"]))
				color := asString(body["color"])
				if color == "" {
					color = "000000"
				}
				if name == "" {
					return restError(422, "name is required")
				}
				if existing, ok := repo.labels[name]; ok {
					return restJSON(201, map[string]any{"name": existing.name, "color": existing.color}, nil)
				}
				repo.labels[name] = label{name: name, color: color}
				s.touch(repo)
				return restJSON(201, map[string]any{"name": name, "color": color}, nil)
			}
			if method == "GET" && len(rest) == 1 {
				var out []any
				for _, l := range repo.labels {
					out = append(out, map[string]any{"name": l.name, "color": l.color})
				}
				if out == nil {
					out = []any{}
				}
				return restJSON(200, out, nil)
			}
			return restError(404, "Not Found")
		}

		if rest[0] == "issues" {
			if len(rest) == 1 {
				if method == "POST" {
					title := strings.TrimSpace(asString(body["title"]))
					if title == "" {
						return restError(422, "title is required")
					}
					labelNames := issueLabelNames(body["labels"])
					for _, name := range labelNames {
						if _, ok := repo.labels[name]; !ok {
							return restError(422, "label "+name+" does not exist")
						}
					}
					iss, err := s.createIssue(repo, title, asString(body["body"]), labelNames)
					if err != nil {
						return restError(422, err.Error())
					}
					return restResponse{status: 201, rawJSON: issueJSON(iss, repo)}
				}
				if method == "GET" {
					q := query()
					state := q.Get("state")
					if state == "" {
						state = "open"
					}
					sortField := q.Get("sort")
					if sortField == "" {
						sortField = "created"
					}
					direction := q.Get("direction")
					if direction == "" {
						direction = "desc"
					}
					var issues []*issue
					for _, iss := range repo.issues {
						issues = append(issues, iss)
					}
					if state == "open" {
						var filtered []*issue
						for _, iss := range issues {
							if iss.state == "open" {
								filtered = append(filtered, iss)
							}
						}
						issues = filtered
					} else if state == "closed" {
						var filtered []*issue
						for _, iss := range issues {
							if iss.state == "closed" {
								filtered = append(filtered, iss)
							}
						}
						issues = filtered
					}
					key := "created_at"
					if sortField == "updated" {
						key = "updated_at"
					}
					sort.Slice(issues, func(i, j int) bool {
						var a, b string
						if key == "updated_at" {
							a, b = issues[i].updatedAt, issues[j].updatedAt
						} else {
							a, b = issues[i].createdAt, issues[j].createdAt
						}
						byTime := 0
						if a < b {
							byTime = -1
						} else if a > b {
							byTime = 1
						}
						if byTime != 0 {
							if direction == "asc" {
								return byTime < 0
							}
							return byTime > 0
						}
						if direction == "asc" {
							return issues[i].number < issues[j].number
						}
						return issues[i].number > issues[j].number
					})
					var views []any
					for _, iss := range issues {
						views = append(views, s.issueView(iss, repo))
					}
					page, headers := paginate(views, query(), requestURL)
					return restJSON(200, page, headers)
				}
				return restError(404, "Not Found")
			}
			number, err := strconv.Atoi(rest[1])
			if err != nil {
				return restError(404, "Not Found")
			}
			iss, ok := repo.issues[number]
			if !ok {
				return restError(404, "Not Found")
			}
			if len(rest) == 2 {
				if method == "GET" {
					viewBody, rawJSON := cachedView(repo, "issue:"+strconv.Itoa(number), func() any { return s.issueView(iss, repo) })
					return restResponse{status: 200, body: viewBody, rawJSON: rawJSON}
				}
				if method == "PATCH" {
					if state, ok := body["state"].(string); ok && (state == "open" || state == "closed") {
						iss.state = state
					}
					if title, ok := body["title"].(string); ok {
						iss.title = title
					}
					if b, ok := body["body"].(string); ok {
						iss.body = b
					}
					iss.updatedAt = s.nowISO()
					s.touch(repo)
					return restJSON(200, s.issueView(iss, repo), nil)
				}
				return restError(404, "Not Found")
			}
			if rest[2] == "comments" {
				if method == "POST" {
					commentBody := asString(body["body"])
					if commentBody == "" {
						return restError(422, "body is required")
					}
					comment := s.addComment(repo, iss, commentBody)
					return restJSON(201, s.commentView(comment), nil)
				}
				if method == "GET" {
					var out []any
					for _, c := range iss.comments {
						out = append(out, s.commentView(c))
					}
					if out == nil {
						out = []any{}
					}
					return restJSON(200, out, nil)
				}
				return restError(404, "Not Found")
			}
			return restError(404, "Not Found")
		}

		if rest[0] == "pulls" {
			if len(rest) == 1 {
				if method == "POST" {
					title := strings.TrimSpace(asString(body["title"]))
					head := asString(body["head"])
					base := asString(body["base"])
					if title == "" || head == "" || base == "" {
						return restError(422, "title, head and base are required")
					}
					headSHA := s.refTip(repo, head)
					baseSHA := s.refTip(repo, base)
					if headSHA == "" || baseSHA == "" {
						return restError(422, "head or base branch does not exist")
					}
					for _, pull := range repo.pulls {
						if pull.state == "open" && pull.headRef == head && pull.baseRef == base {
							return restError(422, "A pull request already exists for these branches")
						}
					}
					pull := s.createPull(repo, title, asString(body["body"]), head, base, headSHA, baseSHA)
					return restJSON(201, s.pullView(pull, repo), nil)
				}
				if method == "GET" {
					q := query()
					state := q.Get("state")
					if state == "" {
						state = "open"
					}
					var pulls []*pullRequest
					for _, pull := range repo.pulls {
						pulls = append(pulls, pull)
					}
					if state == "open" {
						var filtered []*pullRequest
						for _, pull := range pulls {
							if pull.state == "open" {
								filtered = append(filtered, pull)
							}
						}
						pulls = filtered
					} else if state == "closed" {
						var filtered []*pullRequest
						for _, pull := range pulls {
							if pull.state == "closed" {
								filtered = append(filtered, pull)
							}
						}
						pulls = filtered
					}
					sort.Slice(pulls, func(i, j int) bool { return pulls[i].number > pulls[j].number })
					var views []any
					for _, pull := range pulls {
						views = append(views, s.pullView(pull, repo))
					}
					page, headers := paginate(views, query(), requestURL)
					return restJSON(200, page, headers)
				}
				return restError(404, "Not Found")
			}
			number, err := strconv.Atoi(rest[1])
			if err != nil {
				return restError(404, "Not Found")
			}
			pull, ok := repo.pulls[number]
			if !ok {
				return restError(404, "Not Found")
			}
			if len(rest) == 2 {
				if method == "GET" {
					return restJSON(200, s.pullView(pull, repo), nil)
				}
				return restError(404, "Not Found")
			}
			if rest[2] == "files" && method == "GET" {
				files := s.diffFiles(repo, pull.baseSHA, pull.headSHA)
				var out []any
				for _, entry := range files {
					status := entry.status
					if status == "removed" {
						status = "changed"
					}
					out = append(out, map[string]any{
						"filename":  entry.filename,
						"status":    status,
						"additions": entry.additions,
						"deletions": entry.deletions,
						"changes":   entry.changes,
					})
				}
				if out == nil {
					out = []any{}
				}
				return restJSON(200, out, nil)
			}
			if rest[2] == "merge" && method == "PUT" {
				methodName := asString(body["merge_method"])
				if methodName == "" {
					methodName = "squash"
				}
				if methodName != "squash" && methodName != "merge" && methodName != "rebase" {
					return restError(422, "merge_method must be merge, squash or rebase")
				}
				result := s.mergePull(repo, pull)
				if !result.ok {
					if result.reason == "conflict" {
						return restError(405, "Merge conflict")
					}
					return restError(405, result.reason)
				}
				return restJSON(200, map[string]any{"sha": result.sha, "merged": true, "message": "Pull Request successfully merged"}, nil)
			}
			return restError(404, "Not Found")
		}

		return restError(404, "Not Found")
	}

	return restError(404, "Not Found")
}

func encodeBase64(data []byte) string {
	return b64Encode(data)
}
