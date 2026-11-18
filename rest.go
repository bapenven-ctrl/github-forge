package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// HTTP reply plumbing
// ---------------------------------------------------------------------------

type Reply struct {
	Status      int
	Body        []byte
	ContentType string
	Headers     map[string]string
}

func jsonReply(status int, obj interface{}, headers map[string]string) Reply {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(obj)
	body := bytes.TrimRight(buf.Bytes(), "\n")
	return Reply{Status: status, Body: body, ContentType: "application/json; charset=utf-8", Headers: headers}
}

func notFound(message ...string) Reply {
	msg := "Not Found"
	if len(message) > 0 {
		msg = message[0]
	}
	return jsonReply(404, map[string]interface{}{"message": msg}, nil)
}

func validationFailed(message string) Reply {
	return jsonReply(422, map[string]interface{}{"message": message}, nil)
}

// parseJSONBody returns (data, errorReply). Empty body is an empty object; a
// non-object JSON value is also treated as an empty object, as in v1.
func parseJSONBody(body []byte) (map[string]interface{}, *Reply) {
	if len(body) == 0 {
		return map[string]interface{}{}, nil
	}
	var value interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&value); err != nil {
		r := jsonReply(400, map[string]interface{}{"message": "Problems parsing JSON"}, nil)
		return nil, &r
	}
	if m, ok := value.(map[string]interface{}); ok {
		return m, nil
	}
	return map[string]interface{}{}, nil
}

func strOf(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func hasKey(m map[string]interface{}, key string) bool {
	_, ok := m[key]
	return ok
}

func boolOf(m map[string]interface{}, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func paginate(items []interface{}, query url.Values, path, host string) ([]interface{}, map[string]string) {
	perPage := 30
	if v := query.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			perPage = n
		}
	}
	page := 1
	if v := query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	if page < 1 {
		page = 1
	}
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	sliced := items[start:end]
	linkURL := func(pageNo int) string {
		return fmt.Sprintf("http://%s%s?per_page=%d&page=%d", host, path, perPage, pageNo)
	}
	var links []string
	if page < totalPages {
		links = append(links, fmt.Sprintf(`<%s>; rel="next"`, linkURL(page+1)))
		links = append(links, fmt.Sprintf(`<%s>; rel="last"`, linkURL(totalPages)))
	}
	if page > 1 {
		links = append(links, fmt.Sprintf(`<%s>; rel="prev"`, linkURL(page-1)))
		links = append(links, fmt.Sprintf(`<%s>; rel="first"`, linkURL(1)))
	}
	headers := map[string]string{}
	if len(links) > 0 {
		headers["Link"] = strings.Join(links, ", ")
	}
	return sliced, headers
}

func toIface(items []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(items))
	for i, m := range items {
		out[i] = m
	}
	return out
}

// ---------------------------------------------------------------------------
// REST v3 routing
// ---------------------------------------------------------------------------

func restReply(method string, segments []string, query url.Values, body []byte, host, rawPath string) Reply {
	if len(segments) == 0 {
		if method == "GET" {
			root := map[string]interface{}{
				"current_user_url":  "/api/v3/user",
				"rate_limit_url":    "/api/v3/rate_limit",
				"repository_url":    "/api/v3/repos/{owner}/{repo}",
				"issue_search_url":  "/api/v3/search/issues",
			}
			return jsonReply(200, root, map[string]string{"X-Oauth-Scopes": "repo, read:org"})
		}
		return notFound()
	}
	head := segments[0]
	if head == "meta" && method == "GET" {
		return jsonReply(200, map[string]interface{}{"installed_version": FORGE_VERSION}, nil)
	}
	if head == "user" {
		if len(segments) == 1 && method == "GET" {
			return jsonReply(200, userJSON(), nil)
		}
		if len(segments) == 2 && segments[1] == "repos" {
			if method == "GET" {
				var items []map[string]interface{}
				for _, r := range FORGE.OrderedRepos() {
					items = append(items, repoJSON(r))
				}
				sliced, headers := paginate(toIface(items), query, rawPath, host)
				return jsonReply(200, sliced, headers)
			}
			if method == "POST" {
				data, errReply := parseJSONBody(body)
				if errReply != nil {
					return *errReply
				}
				name := strOf(data, "name")
				if name == "" {
					return validationFailed("Repository creation failed.")
				}
				if _, exists := FORGE.Repos[name]; exists {
					return jsonReply(422, map[string]interface{}{
						"message": "Repository creation failed.",
						"errors": []interface{}{map[string]interface{}{
							"resource": "Repository", "code": "custom", "field": "name",
							"message": "name already exists on this account",
						}},
					}, nil)
				}
				repo := FORGE.CreateRepo(name, boolOf(data, "private", false), strOf(data, "description"))
				return jsonReply(201, repoJSON(repo), nil)
			}
		}
		return notFound()
	}
	if head == "users" && len(segments) == 2 && method == "GET" {
		if segments[1] == VIEWER_LOGIN {
			return jsonReply(200, userJSON(), nil)
		}
		return notFound()
	}
	if head != "repos" || len(segments) < 3 {
		return notFound()
	}
	owner, name := segments[1], segments[2]
	repo := FORGE.GetRepo(owner, name)
	if repo == nil {
		return notFound()
	}
	rest := segments[3:]
	if len(rest) == 0 {
		switch method {
		case "GET":
			return jsonReply(200, repoJSON(repo), nil)
		case "PATCH":
			data, errReply := parseJSONBody(body)
			if errReply != nil {
				return *errReply
			}
			if hasKey(data, "description") {
				repo.Desc = strOf(data, "description")
			}
			if hasKey(data, "private") {
				repo.Private = boolOf(data, "private", repo.Private)
			}
			if hasKey(data, "name") && strOf(data, "name") != repo.Name && strOf(data, "name") != "" {
				FORGE.RenameRepo(repo, strOf(data, "name"))
			}
			return jsonReply(200, repoJSON(repo), nil)
		case "DELETE":
			FORGE.RemoveRepo(repo.Name)
			return Reply{Status: 204, Body: []byte{}, ContentType: "application/json; charset=utf-8"}
		}
		return notFound()
	}
	return restRepoReply(method, repo, rest, query, body, host, rawPath)
}

func restRepoReply(method string, repo *Repo, rest []string, query url.Values, body []byte, host, rawPath string) Reply {
	area := rest[0]
	switch area {
	case "branches":
		if len(rest) == 1 && method == "GET" {
			var items []map[string]interface{}
			var refs []string
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					refs = append(refs, ref)
				}
			}
			sort.Strings(refs)
			for _, ref := range refs {
				items = append(items, map[string]interface{}{
					"name":      ref[len("refs/heads/"):],
					"commit":    map[string]interface{}{"sha": repo.Refs[ref]},
					"protected": false,
				})
			}
			sliced, headers := paginate(toIface(items), query, rawPath, host)
			return jsonReply(200, sliced, headers)
		}
		if len(rest) >= 2 && method == "GET" {
			branch := strings.Join(rest[1:], "/")
			tip := branchTip(repo, branch)
			if tip == "" {
				return notFound()
			}
			return jsonReply(200, map[string]interface{}{
				"name": branch, "commit": commitJSON(tip), "protected": false,
			}, nil)
		}
		return notFound()
	case "commits":
		if len(rest) == 1 && method == "GET" {
			tip := resolveRef(repo, query.Get("sha"))
			if tip == "" {
				return jsonReply(200, []interface{}{}, nil)
			}
			var items []map[string]interface{}
			for _, oid := range ancestorsOf(tip) {
				items = append(items, commitJSON(oid))
			}
			sliced, headers := paginate(toIface(items), query, rawPath, host)
			return jsonReply(200, sliced, headers)
		}
		if len(rest) == 2 && method == "GET" {
			if obj, ok := STORE.Get(rest[1]); ok && obj.Type == "commit" {
				return jsonReply(200, commitJSON(rest[1]), nil)
			}
			return notFound()
		}
		return notFound()
	case "contents":
		return contentsReply(method, repo, rest[1:], query, body)
	case "git":
		return gitAPIReply(method, repo, rest[1:], body)
	case "labels":
		return labelsReply(method, repo, rest[1:], body)
	case "issues":
		return issuesReply(method, repo, rest[1:], query, body, host, rawPath)
	case "pulls":
		return pullsReply(method, repo, rest[1:], query, body, host, rawPath)
	}
	return notFound()
}

// encodebytesBase64 matches Python base64.encodebytes: 76-char lines, each
// newline terminated.
func encodebytesBase64(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteByte('\n')
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	b.WriteByte('\n')
	return b.String()
}

func contentsReply(method string, repo *Repo, pathParts []string, query url.Values, body []byte) Reply {
	path := strings.Join(pathParts, "/")
	if method == "GET" {
		tip := resolveRef(repo, query.Get("ref"))
		if tip == "" {
			if path == "" {
				return jsonReply(200, []interface{}{}, nil)
			}
			return notFound()
		}
		flat := commitFlat(tip)
		if path != "" {
			if entry, ok := flat[path]; ok {
				obj, _ := STORE.Get(entry.Oid)
				return jsonReply(200, map[string]interface{}{
					"type":     "file",
					"encoding": "base64",
					"content":  encodebytesBase64(obj.Data),
					"sha":      entry.Oid,
					"size":     len(obj.Data),
					"name":     path[strings.LastIndex(path, "/")+1:],
					"path":     path,
				}, nil)
			}
		}
		prefix := ""
		if path != "" {
			prefix = path + "/"
		}
		entries := map[string]map[string]interface{}{}
		for filePath, entry := range flat {
			if !strings.HasPrefix(filePath, prefix) {
				continue
			}
			remainder := filePath[len(prefix):]
			first := strings.SplitN(remainder, "/", 2)[0]
			if strings.Contains(remainder, "/") {
				if _, ok := entries[first]; !ok {
					entries[first] = map[string]interface{}{
						"type": "dir", "name": first, "path": prefix + first,
						"sha": findSubtreeOid(repo, tip, prefix+first), "size": 0,
					}
				}
			} else {
				obj, _ := STORE.Get(entry.Oid)
				entries[first] = map[string]interface{}{
					"type": "file", "name": first, "path": filePath,
					"sha": entry.Oid, "size": len(obj.Data),
				}
			}
		}
		if path != "" && len(entries) == 0 {
			return notFound()
		}
		names := make([]string, 0, len(entries))
		for n := range entries {
			names = append(names, n)
		}
		sort.Strings(names)
		out := make([]interface{}, 0, len(names))
		for _, n := range names {
			out = append(out, entries[n])
		}
		return jsonReply(200, out, nil)
	}
	data, errReply := parseJSONBody(body)
	if errReply != nil {
		return *errReply
	}
	branch := strOf(data, "branch")
	if branch == "" {
		branch = "main"
	}
	ref := "refs/heads/" + branch
	if method == "PUT" {
		if _, ok := repo.Refs[ref]; !ok && len(repo.Refs) > 0 {
			return notFound("Branch not found")
		}
		if path == "" {
			return validationFailed("path is required")
		}
		raw, err := base64.StdEncoding.DecodeString(strOf(data, "content"))
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(strOf(data, "content"))
			if err != nil {
				return jsonReply(400, map[string]interface{}{"message": "content is not valid base64"}, nil)
			}
		}
		tip := repo.Refs[ref]
		var flat map[string]FlatEntry
		if tip != "" {
			flat = commitFlat(tip)
		} else {
			flat = map[string]FlatEntry{}
		}
		existing, hasExisting := flat[path]
		providedSha, hasSha := data["sha"]
		providedShaStr := ""
		if s, ok := providedSha.(string); ok {
			providedShaStr = s
		}
		status := 201
		if hasExisting {
			if hasSha && providedShaStr != existing.Oid {
				return jsonReply(409, map[string]interface{}{"message": "SHA does not match"}, nil)
			}
			if !hasSha {
				return validationFailed("sha is required when updating a file")
			}
			status = 200
		}
		blobOid := STORE.Put("blob", raw)
		message := strOf(data, "message")
		if message == "" {
			message = "Update " + path
		}
		commitOid := commitFiles(repo, branch, map[string][]byte{path: raw}, nil, message)
		return jsonReply(status, map[string]interface{}{
			"content": map[string]interface{}{
				"name": path[strings.LastIndex(path, "/")+1:], "path": path, "sha": blobOid,
				"size": len(raw), "encoding": "base64", "type": "file",
			},
			"commit": commitJSON(commitOid),
		}, nil)
	}
	if method == "DELETE" {
		tip := repo.Refs[ref]
		if tip == "" {
			return notFound()
		}
		flat := commitFlat(tip)
		existing, hasExisting := flat[path]
		if !hasExisting {
			return notFound()
		}
		if providedSha, hasSha := data["sha"]; hasSha {
			if s, ok := providedSha.(string); ok && s != existing.Oid {
				return jsonReply(409, map[string]interface{}{"message": "SHA does not match"}, nil)
			}
		}
		message := strOf(data, "message")
		if message == "" {
			message = "Delete " + path
		}
		commitOid := commitFiles(repo, branch, nil, map[string]bool{path: true}, message)
		return jsonReply(200, map[string]interface{}{
			"content": nil, "commit": commitJSON(commitOid),
		}, nil)
	}
	return notFound()
}

func findSubtreeOid(repo *Repo, tip, dirPath string) string {
	treeOid := commitTreeOid(tip)
	for _, part := range strings.Split(strings.Trim(dirPath, "/"), "/") {
		if treeOid == "" {
			return ZERO_OID
		}
		obj, ok := STORE.Get(treeOid)
		if !ok {
			return ZERO_OID
		}
		found := ""
		for _, e := range parseTree(obj.Data) {
			if e.Name == part && e.Mode == "40000" {
				found = e.Oid
			}
		}
		treeOid = found
	}
	if treeOid == "" {
		return ZERO_OID
	}
	return treeOid
}

func gitAPIReply(method string, repo *Repo, rest []string, body []byte) Reply {
	if len(rest) > 0 && rest[0] == "refs" {
		if len(rest) == 1 {
			if method == "POST" {
				data, errReply := parseJSONBody(body)
				if errReply != nil {
					return *errReply
				}
				ref := strOf(data, "ref")
				sha := strOf(data, "sha")
				if !strings.HasPrefix(ref, "refs/") || !STORE.Has(sha) {
					return validationFailed("Invalid ref or sha")
				}
				if _, exists := repo.Refs[ref]; exists {
					return validationFailed("Reference already exists")
				}
				repo.Refs[ref] = sha
				FORGE.PackCache = map[string]*packCacheEntry{}
				return jsonReply(201, map[string]interface{}{
					"ref": ref, "node_id": globalID("Ref", ref),
					"object": map[string]interface{}{"sha": sha, "type": "commit"},
				}, nil)
			}
			if method == "GET" {
				var refs []string
				for ref := range repo.Refs {
					refs = append(refs, ref)
				}
				sort.Strings(refs)
				out := make([]interface{}, 0, len(refs))
				for _, ref := range refs {
					out = append(out, map[string]interface{}{
						"ref":    ref,
						"object": map[string]interface{}{"sha": repo.Refs[ref], "type": "commit"},
					})
				}
				return jsonReply(200, out, nil)
			}
		}
		if len(rest) >= 2 && method == "GET" {
			ref := "refs/" + strings.Join(rest[1:], "/")
			oid, ok := repo.Refs[ref]
			if !ok {
				return notFound()
			}
			return jsonReply(200, map[string]interface{}{
				"ref":    ref,
				"object": map[string]interface{}{"sha": oid, "type": "commit"},
			}, nil)
		}
	}
	return notFound()
}

func labelsReply(method string, repo *Repo, rest []string, body []byte) Reply {
	if len(rest) == 0 {
		if method == "POST" {
			data, errReply := parseJSONBody(body)
			if errReply != nil {
				return *errReply
			}
			name := strOf(data, "name")
			if name == "" {
				return validationFailed("name is required")
			}
			if _, exists := repo.Labels[name]; exists {
				return validationFailed("Label already exists")
			}
			color := strOf(data, "color")
			if color == "" {
				color = "ededed"
			}
			color = strings.TrimPrefix(color, "#")
			label := &Label{ID: FORGE.AllocID(), Name: name, Color: color}
			repo.Labels[name] = label
			repo.LabelOrder = append(repo.LabelOrder, name)
			return jsonReply(201, labelJSON(label), nil)
		}
		if method == "GET" {
			out := make([]interface{}, 0, len(repo.LabelOrder))
			for _, name := range repo.LabelOrder {
				out = append(out, labelJSON(repo.Labels[name]))
			}
			return jsonReply(200, out, nil)
		}
	}
	if len(rest) == 1 && method == "GET" {
		label, ok := repo.Labels[rest[0]]
		if !ok {
			return notFound()
		}
		return jsonReply(200, labelJSON(label), nil)
	}
	return notFound()
}

func labelNamesOf(data map[string]interface{}) []string {
	var names []string
	raw, ok := data["labels"]
	if !ok {
		return names
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return names
	}
	for _, entry := range arr {
		if m, ok := entry.(map[string]interface{}); ok {
			names = append(names, strOf(m, "name"))
		} else if s, ok := entry.(string); ok {
			names = append(names, s)
		}
	}
	return names
}

func issuesReply(method string, repo *Repo, rest []string, query url.Values, body []byte, host, rawPath string) Reply {
	if len(rest) == 0 {
		if method == "POST" {
			data, errReply := parseJSONBody(body)
			if errReply != nil {
				return *errReply
			}
			labelNames := labelNamesOf(data)
			for _, labelName := range labelNames {
				if _, ok := repo.Labels[labelName]; !ok {
					return validationFailed(fmt.Sprintf("Validation Failed: label '%s' does not exist", labelName))
				}
			}
			issue := createIssue(repo, strOf(data, "title"), strOf(data, "body"), labelNames)
			return jsonReply(201, issueJSON(repo, issue), nil)
		}
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			var items []*Issue
			for _, i := range repo.Issues {
				if state == "all" || i.State == state {
					items = append(items, i)
				}
			}
			direction := query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			sort.Slice(items, func(a, b int) bool {
				if direction != "asc" {
					return items[a].Number > items[b].Number
				}
				return items[a].Number < items[b].Number
			})
			var rendered []map[string]interface{}
			for _, i := range items {
				rendered = append(rendered, issueJSON(repo, i))
			}
			sliced, headers := paginate(toIface(rendered), query, rawPath, host)
			return jsonReply(200, sliced, headers)
		}
		return notFound()
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		return notFound()
	}
	issue := repo.Issues[number]
	if len(rest) == 1 {
		if issue == nil {
			return notFound()
		}
		if method == "GET" {
			return jsonReply(200, issueJSON(repo, issue), nil)
		}
		if method == "PATCH" {
			data, errReply := parseJSONBody(body)
			if errReply != nil {
				return *errReply
			}
			if hasKey(data, "title") {
				issue.Title = strOf(data, "title")
			}
			if hasKey(data, "body") {
				issue.Body = strOf(data, "body")
			}
			if hasKey(data, "labels") {
				names := labelNamesOf(data)
				for _, labelName := range names {
					if _, ok := repo.Labels[labelName]; !ok {
						return validationFailed("Validation Failed: unknown label")
					}
				}
				issue.Labels = names
			}
			if hasKey(data, "state") {
				issue.State = strOf(data, "state")
				if issue.State == "closed" {
					ts := CLOCK.Tick()
					issue.ClosedTs = &ts
				} else {
					issue.ClosedTs = nil
				}
			}
			issue.UpdatedTs = CLOCK.Tick()
			return jsonReply(200, issueJSON(repo, issue), nil)
		}
		return notFound()
	}
	if len(rest) == 2 && rest[1] == "comments" {
		if issue == nil {
			return notFound()
		}
		if method == "POST" {
			data, errReply := parseJSONBody(body)
			if errReply != nil {
				return *errReply
			}
			comment := &Comment{ID: FORGE.AllocID(), Body: strOf(data, "body"), CreatedTs: CLOCK.Tick()}
			issue.Comments = append(issue.Comments, comment)
			return jsonReply(201, commentJSON(repo, issue, comment), nil)
		}
		if method == "GET" {
			var items []map[string]interface{}
			for _, c := range issue.Comments {
				items = append(items, commentJSON(repo, issue, c))
			}
			sliced, headers := paginate(toIface(items), query, rawPath, host)
			return jsonReply(200, sliced, headers)
		}
	}
	return notFound()
}

func pullsReply(method string, repo *Repo, rest []string, query url.Values, body []byte, host, rawPath string) Reply {
	if len(rest) == 0 {
		if method == "POST" {
			data, errReply := parseJSONBody(body)
			if errReply != nil {
				return *errReply
			}
			head := strOf(data, "head")
			base := strOf(data, "base")
			if base == "" {
				base = "main"
			}
			if idx := strings.LastIndex(head, ":"); idx >= 0 {
				head = head[idx+1:]
			}
			if idx := strings.LastIndex(base, ":"); idx >= 0 {
				base = base[idx+1:]
			}
			if branchTip(repo, head) == "" || branchTip(repo, base) == "" {
				return validationFailed("Validation Failed: head or base branch does not exist")
			}
			pull := createPull(repo, strOf(data, "title"), strOf(data, "body"), head, base)
			return jsonReply(201, pullJSON(repo, pull), nil)
		}
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			var items []*Pull
			for _, pull := range repo.Pulls {
				rendered := "open"
				if pull.Merged || pull.State == "closed" {
					rendered = "closed"
				}
				if state == "all" || rendered == state {
					items = append(items, pull)
				}
			}
			sort.Slice(items, func(a, b int) bool { return items[a].Number < items[b].Number })
			var rendered []map[string]interface{}
			for _, p := range items {
				rendered = append(rendered, pullJSON(repo, p))
			}
			sliced, headers := paginate(toIface(rendered), query, rawPath, host)
			return jsonReply(200, sliced, headers)
		}
		return notFound()
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		return notFound()
	}
	pull := repo.Pulls[number]
	if len(rest) == 1 {
		if pull == nil {
			return notFound()
		}
		if method == "GET" {
			return jsonReply(200, pullJSON(repo, pull), nil)
		}
		if method == "PATCH" {
			data, errReply := parseJSONBody(body)
			if errReply != nil {
				return *errReply
			}
			if hasKey(data, "title") {
				pull.Title = strOf(data, "title")
			}
			if hasKey(data, "body") {
				pull.Body = strOf(data, "body")
			}
			if hasKey(data, "state") {
				pull.State = strOf(data, "state")
				if pull.State == "closed" {
					ts := CLOCK.Tick()
					pull.ClosedTs = &ts
				}
			}
			pull.UpdatedTs = CLOCK.Tick()
			return jsonReply(200, pullJSON(repo, pull), nil)
		}
		return notFound()
	}
	if pull == nil {
		return notFound()
	}
	if len(rest) == 2 && rest[1] == "files" && method == "GET" {
		files := pullFiles(repo, pull)
		var items []map[string]interface{}
		for _, f := range files {
			items = append(items, pullFileJSON(f))
		}
		sliced, headers := paginate(toIface(items), query, rawPath, host)
		return jsonReply(200, sliced, headers)
	}
	if len(rest) == 2 && rest[1] == "merge" && method == "PUT" {
		data, errReply := parseJSONBody(body)
		if errReply != nil {
			return *errReply
		}
		if pull.Merged || pull.State == "closed" {
			return jsonReply(405, map[string]interface{}{"message": "Pull Request is not mergeable"}, nil)
		}
		method2 := strOf(data, "merge_method")
		if method2 == "" {
			method2 = "merge"
		}
		mergedSha := mergePull(repo, pull, method2)
		if mergedSha == "" {
			return jsonReply(405, map[string]interface{}{"message": "Pull Request is not mergeable"}, nil)
		}
		return jsonReply(200, map[string]interface{}{
			"sha": mergedSha, "merged": true, "message": "Pull Request successfully merged",
		}, nil)
	}
	return notFound()
}
