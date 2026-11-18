package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	VIEWER_LOGIN   = "adaiversen"
	VIEWER_ID      = 1000
	VIEWER_NAME    = "ada iversen"
	VIEWER_EMAIL   = "adaiversen@example.invalid"
	STRANGER_LOGIN = "olgabauer"
	ZERO_OID       = "0000000000000000000000000000000000000000"
	FORGE_VERSION  = "1.0.0"
)

// stateMu guards every forge data structure below. Reads take RLock so the
// read benchmark and concurrent clones run in parallel; writes take Lock.
var stateMu sync.RWMutex

type Label struct {
	ID    int
	Name  string
	Color string
}

type Comment struct {
	ID        int
	Body      string
	CreatedTs int64
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []string
	Comments  []*Comment
	CreatedTs int64
	UpdatedTs int64
	ClosedTs  *int64
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string
	Merged         bool
	Head           string
	Base           string
	HeadSha        string
	BaseSha        string
	MergeCommitSha *string
	MergedTs       *int64
	ClosedTs       *int64
	CreatedTs      int64
	UpdatedTs      int64
}

type Repo struct {
	Name       string
	Private    bool
	Desc       string
	Dbid       int
	CreatedTs  int64
	Refs       map[string]string
	Labels     map[string]*Label
	LabelOrder []string
	Issues     map[int]*Issue
	Pulls      map[int]*Pull
	NextNumber int
}

type packCacheEntry struct {
	pack       []byte
	hasPack    bool
	newShallow map[string]bool
}

type Forge struct {
	Repos        map[string]*Repo
	RepoOrder    []string
	NextRepoDbid int
	NextObjectID int
	PackCache    map[string]*packCacheEntry
}

var FORGE = &Forge{
	Repos:        map[string]*Repo{},
	NextRepoDbid: 5000,
	NextObjectID: 1,
	PackCache:    map[string]*packCacheEntry{},
}

func (f *Forge) AllocID() int {
	v := f.NextObjectID
	f.NextObjectID++
	return v
}

func (f *Forge) CreateRepo(name string, private bool, description string) *Repo {
	repo := &Repo{
		Name:       name,
		Private:    private,
		Desc:       description,
		Dbid:       f.NextRepoDbid,
		CreatedTs:  CLOCK.Tick(),
		Refs:       map[string]string{},
		Labels:     map[string]*Label{},
		Issues:     map[int]*Issue{},
		Pulls:      map[int]*Pull{},
		NextNumber: 1,
	}
	f.NextRepoDbid++
	f.Repos[name] = repo
	f.RepoOrder = append(f.RepoOrder, name)
	return repo
}

func (f *Forge) GetRepo(owner, name string) *Repo {
	if owner != VIEWER_LOGIN {
		return nil
	}
	return f.Repos[name]
}

func (f *Forge) RemoveRepo(name string) {
	if _, ok := f.Repos[name]; !ok {
		return
	}
	delete(f.Repos, name)
	for i, n := range f.RepoOrder {
		if n == name {
			f.RepoOrder = append(f.RepoOrder[:i], f.RepoOrder[i+1:]...)
			break
		}
	}
}

func (f *Forge) RenameRepo(repo *Repo, newName string) {
	old := repo.Name
	f.RemoveRepo(old)
	repo.Name = newName
	f.Repos[newName] = repo
	f.RepoOrder = append(f.RepoOrder, newName)
}

func (f *Forge) OrderedRepos() []*Repo {
	out := make([]*Repo, 0, len(f.RepoOrder))
	for _, name := range f.RepoOrder {
		if r, ok := f.Repos[name]; ok {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Global (GraphQL node) ids
// ---------------------------------------------------------------------------

func globalID(typeName string, key interface{}) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%v", typeName, key)))
}

func parseGlobalID(value string) (string, string) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		// Python's b64decode tolerates missing padding.
		raw, err = base64.RawStdEncoding.DecodeString(value)
		if err != nil {
			return "", ""
		}
	}
	decoded := string(raw)
	idx := strings.Index(decoded, ":")
	if idx < 0 {
		return decoded, ""
	}
	return decoded[:idx], decoded[idx+1:]
}

// ---------------------------------------------------------------------------
// REST serializers
// ---------------------------------------------------------------------------

func userJSON() map[string]interface{} {
	return map[string]interface{}{
		"login":      VIEWER_LOGIN,
		"id":         VIEWER_ID,
		"node_id":    globalID("User", VIEWER_ID),
		"type":       "User",
		"site_admin": false,
		"name":       VIEWER_NAME,
		"email":      VIEWER_EMAIL,
	}
}

func repoJSON(repo *Repo) map[string]interface{} {
	ts := isoOf(repo.CreatedTs)
	openIssues := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			openIssues++
		}
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	return map[string]interface{}{
		"id":                repo.Dbid,
		"node_id":           globalID("Repository", repo.Dbid),
		"name":              repo.Name,
		"full_name":         VIEWER_LOGIN + "/" + repo.Name,
		"private":           repo.Private,
		"owner":             userJSON(),
		"description":       repo.Desc,
		"fork":              false,
		"default_branch":    "main",
		"created_at":        ts,
		"updated_at":        ts,
		"pushed_at":         ts,
		"size":              0,
		"stargazers_count":  0,
		"watchers_count":    0,
		"forks_count":       0,
		"open_issues_count": openIssues,
		"has_issues":        true,
		"has_pull_requests": true,
		"visibility":        visibility,
	}
}

func labelJSON(label *Label) map[string]interface{} {
	return map[string]interface{}{
		"id":          label.ID,
		"node_id":     globalID("Label", label.ID),
		"name":        label.Name,
		"color":       label.Color,
		"default":     false,
		"description": nil,
	}
}

func issueJSON(repo *Repo, issue *Issue) map[string]interface{} {
	labels := []interface{}{}
	for _, name := range issue.Labels {
		if l, ok := repo.Labels[name]; ok {
			labels = append(labels, labelJSON(l))
		}
	}
	var closedAt interface{}
	if issue.ClosedTs != nil {
		closedAt = isoOf(*issue.ClosedTs)
	}
	return map[string]interface{}{
		"id":         issue.ID,
		"node_id":    globalID("Issue", fmt.Sprintf("%d:%d", repo.Dbid, issue.Number)),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       userJSON(),
		"assignee":   nil,
		"assignees":  []interface{}{},
		"comments":   len(issue.Comments),
		"created_at": isoOf(issue.CreatedTs),
		"updated_at": isoOf(issue.UpdatedTs),
		"closed_at":  closedAt,
	}
}

func commentJSON(repo *Repo, issue *Issue, comment *Comment) map[string]interface{} {
	return map[string]interface{}{
		"id":         comment.ID,
		"node_id":    globalID("IssueComment", fmt.Sprintf("%d:%d:%d", repo.Dbid, issue.Number, comment.ID)),
		"body":       comment.Body,
		"user":       userJSON(),
		"created_at": isoOf(comment.CreatedTs),
		"updated_at": isoOf(comment.CreatedTs),
	}
}

func branchTip(repo *Repo, branch string) string {
	return repo.Refs["refs/heads/"+branch]
}

func pullJSON(repo *Repo, pull *Pull) map[string]interface{} {
	headSha := branchTip(repo, pull.Head)
	if headSha == "" {
		headSha = pull.HeadSha
	}
	baseSha := branchTip(repo, pull.Base)
	if baseSha == "" {
		baseSha = pull.BaseSha
	}
	state := "open"
	if pull.Merged || pull.State == "closed" {
		state = "closed"
	}
	var mergedAt, closedAt interface{}
	if pull.MergedTs != nil {
		mergedAt = isoOf(*pull.MergedTs)
	}
	if pull.ClosedTs != nil {
		closedAt = isoOf(*pull.ClosedTs)
	}
	var mergeSha interface{}
	if pull.MergeCommitSha != nil {
		mergeSha = *pull.MergeCommitSha
	}
	return map[string]interface{}{
		"id":               pull.ID,
		"node_id":          globalID("PullRequest", fmt.Sprintf("%d:%d", repo.Dbid, pull.Number)),
		"number":           pull.Number,
		"state":            state,
		"title":            pull.Title,
		"body":             pull.Body,
		"user":             userJSON(),
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSha,
		"head": map[string]interface{}{
			"ref":   pull.Head,
			"sha":   headSha,
			"label": VIEWER_LOGIN + ":" + pull.Head,
			"user":  userJSON(),
			"repo":  repoJSON(repo),
		},
		"base": map[string]interface{}{
			"ref":   pull.Base,
			"sha":   baseSha,
			"label": VIEWER_LOGIN + ":" + pull.Base,
			"user":  userJSON(),
			"repo":  repoJSON(repo),
		},
		"created_at": isoOf(pull.CreatedTs),
		"updated_at": isoOf(pull.UpdatedTs),
		"closed_at":  closedAt,
		"draft":      false,
	}
}

var personRe = regexp.MustCompile(`^(.*) <(.*)> (\d+) ([+-]\d{4})$`)

func commitJSON(oid string) map[string]interface{} {
	obj, _ := STORE.Get(oid)
	parsed := parseCommit(obj.Data)
	person := func(line string) map[string]interface{} {
		m := personRe.FindStringSubmatch(line)
		if m == nil {
			return map[string]interface{}{"name": "", "email": "", "date": isoOf(CLOCK.Value())}
		}
		ts, _ := parseInt64(m[3])
		return map[string]interface{}{"name": m[1], "email": m[2], "date": isoOf(ts)}
	}
	parents := []interface{}{}
	for _, p := range parsed.Parents {
		parents = append(parents, map[string]interface{}{"sha": p})
	}
	return map[string]interface{}{
		"sha":     oid,
		"node_id": globalID("Commit", oid),
		"commit": map[string]interface{}{
			"author":        person(parsed.Author),
			"committer":     person(parsed.Committer),
			"message":       parsed.Message,
			"tree":          map[string]interface{}{"sha": parsed.Tree},
			"comment_count": 0,
		},
		"author":    userJSON(),
		"committer": userJSON(),
		"parents":   parents,
	}
}

func parseInt64(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(s, "%d", &v)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Domain operations shared by REST, GraphQL and MCP
// ---------------------------------------------------------------------------

func commitFiles(repo *Repo, branch string, changes map[string][]byte, deletions map[string]bool, message string) string {
	ref := "refs/heads/" + branch
	tip := repo.Refs[ref]
	var flat map[string]FlatEntry
	if tip != "" {
		flat = commitFlat(tip)
	} else {
		flat = map[string]FlatEntry{}
	}
	paths := make([]string, 0, len(changes)+len(deletions))
	for p := range changes {
		paths = append(paths, p)
	}
	for p := range deletions {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if deletions[path] {
			delete(flat, path)
		} else {
			flat[path] = FlatEntry{Mode: "100644", Oid: STORE.Put("blob", changes[path])}
		}
	}
	treeOid := treeFromFlat(flat)
	ts := CLOCK.Tick()
	ident := viewerIdent(ts)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitOid := STORE.Put("commit", buildCommit(treeOid, parents, ident, ident, message))
	repo.Refs[ref] = commitOid
	FORGE.PackCache = map[string]*packCacheEntry{}
	return commitOid
}

var oidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func resolveRef(repo *Repo, refValue string) string {
	if refValue == "" || refValue == "HEAD" {
		refValue = "main"
	}
	if tip := repo.Refs["refs/heads/"+refValue]; tip != "" {
		return tip
	}
	if tip := repo.Refs[refValue]; tip != "" {
		return tip
	}
	if oidRe.MatchString(refValue) && STORE.Has(refValue) {
		return refValue
	}
	return ""
}

type PullFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
	Sha       string
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lcsLength is the longest-common-subsequence length of two line slices; it
// stands in for difflib counts (the suite only requires additions >= 1 on a
// changed file, which LCS reproduces faithfully).
func lcsLength(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		copy(prev, cur)
	}
	return prev[len(b)]
}

func pullFiles(repo *Repo, pull *Pull) []PullFile {
	headSha := branchTip(repo, pull.Head)
	if headSha == "" {
		headSha = pull.HeadSha
	}
	baseSha := branchTip(repo, pull.Base)
	if baseSha == "" {
		baseSha = pull.BaseSha
	}
	baseCommit := mergeBaseOf(baseSha, headSha)
	if baseCommit == "" {
		baseCommit = baseSha
	}
	baseFlat := commitFlat(baseCommit)
	headFlat := commitFlat(headSha)
	pathSet := map[string]bool{}
	for p := range baseFlat {
		pathSet[p] = true
	}
	for p := range headFlat {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var files []PullFile
	for _, path := range paths {
		before, hasBefore := baseFlat[path]
		after, hasAfter := headFlat[path]
		if hasBefore && hasAfter && before == after {
			continue
		}
		var beforeLines, afterLines []string
		if hasBefore {
			obj, _ := STORE.Get(before.Oid)
			beforeLines = splitLines(obj.Data)
		}
		if hasAfter {
			obj, _ := STORE.Get(after.Oid)
			afterLines = splitLines(obj.Data)
		}
		common := lcsLength(beforeLines, afterLines)
		additions := len(afterLines) - common
		deletions := len(beforeLines) - common
		status := "modified"
		if !hasBefore {
			status = "added"
		} else if !hasAfter {
			status = "removed"
		}
		sha := ""
		if hasAfter {
			sha = after.Oid
		} else {
			sha = before.Oid
		}
		files = append(files, PullFile{
			Filename: path, Status: status, Additions: additions,
			Deletions: deletions, Changes: additions + deletions, Sha: sha,
		})
	}
	return files
}

func pullFileJSON(f PullFile) map[string]interface{} {
	return map[string]interface{}{
		"filename":  f.Filename,
		"status":    f.Status,
		"additions": f.Additions,
		"deletions": f.Deletions,
		"changes":   f.Changes,
		"sha":       f.Sha,
	}
}

func mergeConflicts(repo *Repo, pull *Pull) []string {
	headSha := branchTip(repo, pull.Head)
	if headSha == "" {
		headSha = pull.HeadSha
	}
	baseSha := branchTip(repo, pull.Base)
	if baseSha == "" {
		baseSha = pull.BaseSha
	}
	if headSha == "" || baseSha == "" {
		return []string{"<missing-ref>"}
	}
	baseCommit := mergeBaseOf(baseSha, headSha)
	if baseCommit == "" {
		return []string{"<no-merge-base>"}
	}
	fb := commitFlat(baseCommit)
	fh := commitFlat(headSha)
	ft := commitFlat(baseSha)
	pathSet := map[string]bool{}
	for p := range fb {
		pathSet[p] = true
	}
	for p := range fh {
		pathSet[p] = true
	}
	var conflicts []string
	for path := range pathSet {
		baseV, hasBase := fb[path]
		headV, hasHead := fh[path]
		targetV, hasTarget := ft[path]
		sameBH := hasBase == hasHead && (!hasBase || baseV == headV)
		sameBT := hasBase == hasTarget && (!hasBase || baseV == targetV)
		sameHT := hasHead == hasTarget && (!hasHead || headV == targetV)
		if sameBH || sameBT || sameHT {
			continue
		}
		conflicts = append(conflicts, path)
	}
	return conflicts
}

func mergePull(repo *Repo, pull *Pull, method string) string {
	if len(mergeConflicts(repo, pull)) > 0 {
		return ""
	}
	headSha := branchTip(repo, pull.Head)
	if headSha == "" {
		headSha = pull.HeadSha
	}
	baseSha := branchTip(repo, pull.Base)
	if baseSha == "" {
		baseSha = pull.BaseSha
	}
	baseCommit := mergeBaseOf(baseSha, headSha)
	if baseCommit == "" {
		baseCommit = baseSha
	}
	fb := commitFlat(baseCommit)
	fh := commitFlat(headSha)
	merged := commitFlat(baseSha)
	pathSet := map[string]bool{}
	for p := range fb {
		pathSet[p] = true
	}
	for p := range fh {
		pathSet[p] = true
	}
	for path := range pathSet {
		baseV, hasBase := fb[path]
		headV, hasHead := fh[path]
		if hasBase == hasHead && (!hasBase || baseV == headV) {
			continue
		}
		if !hasHead {
			delete(merged, path)
		} else {
			merged[path] = headV
		}
	}
	treeOid := treeFromFlat(merged)
	ts := CLOCK.Tick()
	ident := viewerIdent(ts)
	var parents []string
	var message string
	if strings.ToLower(method) == "merge" {
		parents = []string{baseSha, headSha}
		message = fmt.Sprintf("Merge pull request #%d from %s", pull.Number, pull.Head)
	} else {
		parents = []string{baseSha}
		message = pull.Title
	}
	commitOid := STORE.Put("commit", buildCommit(treeOid, parents, ident, ident, message))
	repo.Refs["refs/heads/"+pull.Base] = commitOid
	pull.Merged = true
	pull.State = "closed"
	pull.MergedTs = &ts
	pull.ClosedTs = &ts
	pull.UpdatedTs = ts
	sha := commitOid
	pull.MergeCommitSha = &sha
	FORGE.PackCache = map[string]*packCacheEntry{}
	return commitOid
}

func createIssue(repo *Repo, title, body string, labelNames []string) *Issue {
	number := repo.NextNumber
	repo.NextNumber++
	ts := CLOCK.Tick()
	issue := &Issue{
		ID:        FORGE.AllocID(),
		Number:    number,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string{}, labelNames...),
		CreatedTs: ts,
		UpdatedTs: ts,
	}
	repo.Issues[number] = issue
	return issue
}

func createPull(repo *Repo, title, body, head, base string) *Pull {
	number := repo.NextNumber
	repo.NextNumber++
	ts := CLOCK.Tick()
	pull := &Pull{
		ID:        FORGE.AllocID(),
		Number:    number,
		Title:     title,
		Body:      body,
		State:     "open",
		Head:      head,
		Base:      base,
		HeadSha:   branchTip(repo, head),
		BaseSha:   branchTip(repo, base),
		CreatedTs: ts,
		UpdatedTs: ts,
	}
	repo.Pulls[number] = pull
	return pull
}
