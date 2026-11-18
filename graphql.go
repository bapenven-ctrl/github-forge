package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// GraphQL v4: a small hand-written parser + executor covering exactly the
// schema surface the suite introspects and calls.
// ---------------------------------------------------------------------------

var GQL_TYPE_FIELDS = map[string][]string{
	"Query":     {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation":  {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":      {"id", "login", "name", "email", "url"},
	"Repository": {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
		"viewerPermission", "hasIssuesEnabled", "hasPullRequestsEnabled", "isPrivate",
		"issue", "issues", "pullRequest", "pullRequests", "url", "createdAt", "updatedAt"},
	"Ref": {"name", "target"},
	"Issue": {"id", "number", "title", "state", "body", "author", "labels", "comments",
		"createdAt", "updatedAt", "url"},
	"IssueConnection":       {"nodes", "pageInfo", "totalCount"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "mergeable",
		"headRefName", "baseRefName", "files", "author", "createdAt", "updatedAt"},
	"PullRequestConnection":       {"nodes", "pageInfo", "totalCount"},
	"PullRequestChangedFile":      {"path"},
	"PullRequestChangedFileConnection": {"nodes", "pageInfo", "totalCount"},
	"PageInfo":              {"hasNextPage", "endCursor", "startCursor", "hasPreviousPage"},
	"IssueComment":          {"id", "body", "author", "createdAt"},
	"CreateIssuePayload":    {"issue", "clientMutationId"},
	"CloseIssuePayload":     {"issue", "clientMutationId"},
	"CreatePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":   {"pullRequest", "clientMutationId"},
	"AddCommentPayload":         {"commentEdge", "clientMutationId"},
	"IssueCommentEdge":          {"node", "cursor"},
	"__Type":     {"name", "kind", "fields", "enumValues", "description"},
	"__Field":    {"name", "description", "isDeprecated", "deprecationReason"},
	"__EnumValue": {"name", "description", "isDeprecated", "deprecationReason"},
	"__Schema":   {"types", "queryType", "mutationType"},
}

var GQL_ENUM_VALUES = map[string][]string{
	"IssueState":            {"OPEN", "CLOSED"},
	"PullRequestState":      {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":        {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":  {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"SearchType":            {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE"},
	"IssueOrderField":       {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":        {"ASC", "DESC"},
}

type GqlError struct{ msg string }

func (e *GqlError) Error() string { return e.msg }

func gqlErr(format string, args ...interface{}) *GqlError {
	return &GqlError{msg: fmt.Sprintf(format, args...)}
}

// --- tokenizer ---

type gqlToken struct {
	kind  string // name, int, float, string, punct, spread, eof
	value string
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func gqlTokenize(text string) ([]gqlToken, error) {
	var tokens []gqlToken
	pos := 0
	for pos < len(text) {
		c := text[pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			pos++
			continue
		}
		if c == '#' {
			for pos < len(text) && text[pos] != '\n' {
				pos++
			}
			continue
		}
		if c == '.' && pos+2 < len(text)+1 && strings.HasPrefix(text[pos:], "...") {
			tokens = append(tokens, gqlToken{"spread", "..."})
			pos += 3
			continue
		}
		if isNameStart(c) {
			start := pos
			for pos < len(text) && isNameChar(text[pos]) {
				pos++
			}
			tokens = append(tokens, gqlToken{"name", text[start:pos]})
			continue
		}
		if c == '"' {
			start := pos
			pos++
			for pos < len(text) {
				if text[pos] == '\\' {
					pos += 2
					continue
				}
				if text[pos] == '"' {
					pos++
					break
				}
				pos++
			}
			tokens = append(tokens, gqlToken{"string", text[start:pos]})
			continue
		}
		if c == '-' || (c >= '0' && c <= '9') {
			start := pos
			if c == '-' {
				pos++
			}
			isFloat := false
			for pos < len(text) && ((text[pos] >= '0' && text[pos] <= '9') || text[pos] == '.' || text[pos] == 'e' || text[pos] == 'E' || text[pos] == '+' || text[pos] == '-') {
				if text[pos] == '.' || text[pos] == 'e' || text[pos] == 'E' {
					isFloat = true
				}
				pos++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind, text[start:pos]})
			continue
		}
		if strings.IndexByte("!$&():=@[]{}|=", c) >= 0 {
			tokens = append(tokens, gqlToken{"punct", string(c)})
			pos++
			continue
		}
		return nil, gqlErr("unexpected character %q", string(c))
	}
	tokens = append(tokens, gqlToken{"eof", ""})
	return tokens, nil
}

// --- values ---

type GVal struct {
	Kind  string // var, enum, string, int, float, bool, null, list, object
	Str   string
	Int   int64
	Float float64
	Bool  bool
	List  []*GVal
	Obj   map[string]*GVal
	Keys  []string
}

func gqlResolveValue(v *GVal, variables map[string]interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch v.Kind {
	case "var":
		return variables[v.Str]
	case "enum":
		return v.Str
	case "string":
		return v.Str
	case "int":
		return v.Int
	case "float":
		return v.Float
	case "bool":
		return v.Bool
	case "null":
		return nil
	case "list":
		out := make([]interface{}, len(v.List))
		for i, item := range v.List {
			out[i] = gqlResolveValue(item, variables)
		}
		return out
	case "object":
		out := map[string]interface{}{}
		for _, k := range v.Keys {
			out[k] = gqlResolveValue(v.Obj[k], variables)
		}
		return out
	}
	return nil
}

// --- parser ---

type GSelection struct {
	Kind       string // field, spread, inline
	Name       string
	Alias      string
	TypeCond   string
	Args       map[string]*GVal
	Selections []*GSelection
}

type GOperation struct {
	Type       string
	Name       string
	VarDefs    map[string]*GVal
	Selections []*GSelection
}

type GFragment struct {
	Type       string
	Selections []*GSelection
}

type GqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *GqlParser) peek() gqlToken  { return p.tokens[p.pos] }
func (p *GqlParser) next() gqlToken  { t := p.tokens[p.pos]; p.pos++; return t }

func (p *GqlParser) expectPunct(value string) error {
	t := p.next()
	if t.kind != "punct" || t.value != value {
		return gqlErr("expected %q, got %q", value, t.value)
	}
	return nil
}

func (p *GqlParser) skipPunct(value string) bool {
	t := p.peek()
	if t.kind == "punct" && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *GqlParser) isPunct(value string) bool {
	t := p.peek()
	return t.kind == "punct" && t.value == value
}

func (p *GqlParser) parseDocument() ([]*GOperation, map[string]*GFragment, error) {
	var operations []*GOperation
	fragments := map[string]*GFragment{}
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "punct" && t.value == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, &GOperation{Type: "query", VarDefs: map[string]*GVal{}, Selections: sels})
		} else if t.kind == "name" && (t.value == "query" || t.value == "mutation" || t.value == "subscription") {
			p.next()
			op := &GOperation{Type: t.value, VarDefs: map[string]*GVal{}}
			if p.peek().kind == "name" {
				op.Name = p.next().value
			}
			if p.isPunct("(") {
				defs, err := p.parseVariableDefs()
				if err != nil {
					return nil, nil, err
				}
				op.VarDefs = defs
			}
			if err := p.skipDirectives(); err != nil {
				return nil, nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			op.Selections = sels
			operations = append(operations, op)
		} else if t.kind == "name" && t.value == "fragment" {
			p.next()
			fragName := p.next().value
			p.next() // 'on'
			typeCond := p.next().value
			if err := p.skipDirectives(); err != nil {
				return nil, nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[fragName] = &GFragment{Type: typeCond, Selections: sels}
		} else {
			return nil, nil, gqlErr("unexpected token %q", t.value)
		}
	}
	return operations, fragments, nil
}

func (p *GqlParser) skipDirectives() error {
	for p.isPunct("@") {
		p.next()
		p.next()
		if p.isPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *GqlParser) parseVariableDefs() (map[string]*GVal, error) {
	defs := map[string]*GVal{}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for !p.isPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name := p.next().value
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		var def *GVal
		if p.skipPunct("=") {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			def = v
		}
		defs[name] = def
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return defs, nil
}

func (p *GqlParser) parseType() error {
	if p.skipPunct("[") {
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		p.next()
	}
	p.skipPunct("!")
	return nil
}

func (p *GqlParser) parseSelectionSet() ([]*GSelection, error) {
	var selections []*GSelection
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	for !p.isPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *GqlParser) parseSelection() (*GSelection, error) {
	t := p.peek()
	if t.kind == "spread" {
		p.next()
		if p.peek().kind == "name" && p.peek().value == "on" {
			p.next()
			typeCond := p.next().value
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &GSelection{Kind: "inline", TypeCond: typeCond, Selections: sels}, nil
		}
		fragName := p.next().value
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		return &GSelection{Kind: "spread", Name: fragName}, nil
	}
	name := p.next().value
	alias := ""
	if p.skipPunct(":") {
		alias = name
		name = p.next().value
	}
	args := map[string]*GVal{}
	if p.isPunct("(") {
		a, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		args = a
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	var selections []*GSelection
	if p.isPunct("{") {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		selections = sels
	}
	return &GSelection{Kind: "field", Name: name, Alias: alias, Args: args, Selections: selections}, nil
}

func (p *GqlParser) parseArguments() (map[string]*GVal, error) {
	args := map[string]*GVal{}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for !p.isPunct(")") {
		argName := p.next().value
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[argName] = v
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *GqlParser) parseValue() (*GVal, error) {
	t := p.next()
	switch {
	case t.kind == "punct" && t.value == "$":
		return &GVal{Kind: "var", Str: p.next().value}, nil
	case t.kind == "punct" && t.value == "[":
		var items []*GVal
		for !p.isPunct("]") {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return &GVal{Kind: "list", List: items}, nil
	case t.kind == "punct" && t.value == "{":
		obj := &GVal{Kind: "object", Obj: map[string]*GVal{}}
		for !p.isPunct("}") {
			key := p.next().value
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj.Obj[key] = v
			obj.Keys = append(obj.Keys, key)
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return obj, nil
	case t.kind == "string":
		var s string
		if err := json.Unmarshal([]byte(t.value), &s); err != nil {
			return nil, err
		}
		return &GVal{Kind: "string", Str: s}, nil
	case t.kind == "int":
		n, _ := strconv.ParseInt(t.value, 10, 64)
		return &GVal{Kind: "int", Int: n}, nil
	case t.kind == "float":
		f, _ := strconv.ParseFloat(t.value, 64)
		return &GVal{Kind: "float", Float: f}, nil
	case t.kind == "name":
		switch t.value {
		case "true":
			return &GVal{Kind: "bool", Bool: true}, nil
		case "false":
			return &GVal{Kind: "bool", Bool: false}, nil
		case "null":
			return &GVal{Kind: "null"}, nil
		}
		return &GVal{Kind: "enum", Str: t.value}, nil
	}
	return nil, gqlErr("unexpected value token %q", t.value)
}

// --- validation ---

func gqlChildType(typeName, field string) string {
	mapping := map[[2]string]string{
		{"Query", "viewer"}: "User", {"Query", "repository"}: "Repository",
		{"Query", "node"}: "Repository", {"Query", "__type"}: "__Type",
		{"Repository", "owner"}: "User", {"Repository", "defaultBranchRef"}: "Ref",
		{"Repository", "issue"}: "Issue", {"Repository", "issues"}: "IssueConnection",
		{"Repository", "pullRequest"}: "PullRequest", {"Repository", "pullRequests"}: "PullRequestConnection",
		{"IssueConnection", "nodes"}: "Issue", {"IssueConnection", "pageInfo"}: "PageInfo",
		{"PullRequestConnection", "nodes"}: "PullRequest", {"PullRequestConnection", "pageInfo"}: "PageInfo",
		{"PullRequest", "files"}: "PullRequestChangedFileConnection", {"PullRequest", "author"}: "User",
		{"Issue", "author"}: "User",
		{"PullRequestChangedFileConnection", "nodes"}: "PullRequestChangedFile",
		{"PullRequestChangedFileConnection", "pageInfo"}: "PageInfo",
		{"CreateIssuePayload", "issue"}: "Issue", {"CloseIssuePayload", "issue"}: "Issue",
		{"CreatePullRequestPayload", "pullRequest"}: "PullRequest",
		{"MergePullRequestPayload", "pullRequest"}: "PullRequest",
		{"AddCommentPayload", "commentEdge"}: "IssueCommentEdge",
		{"IssueCommentEdge", "node"}: "IssueComment", {"IssueComment", "author"}: "User",
		{"__Type", "fields"}: "__Field", {"__Type", "enumValues"}: "__EnumValue",
	}
	return mapping[[2]string{typeName, field}]
}

func gqlValidate(selections []*GSelection, typeName string, fragments map[string]*GFragment, errors *[]string) {
	fields, ok := GQL_TYPE_FIELDS[typeName]
	if !ok {
		return
	}
	fieldSet := map[string]bool{}
	for _, f := range fields {
		fieldSet[f] = true
	}
	for _, selection := range selections {
		switch selection.Kind {
		case "spread":
			if frag, ok := fragments[selection.Name]; ok {
				gqlValidate(frag.Selections, frag.Type, fragments, errors)
			}
		case "inline":
			gqlValidate(selection.Selections, selection.TypeCond, fragments, errors)
		default:
			name := selection.Name
			if name == "__typename" {
				continue
			}
			if !fieldSet[name] {
				*errors = append(*errors, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", name, typeName))
				continue
			}
			if child := gqlChildType(typeName, name); child != "" && len(selection.Selections) > 0 {
				gqlValidate(selection.Selections, child, fragments, errors)
			}
		}
	}
}

// --- tagged runtime values ---

type GTag struct {
	Type    string
	Payload interface{}
}

func gqlTag(typeName string, payload interface{}) *GTag {
	return &GTag{Type: typeName, Payload: payload}
}

type repoIssue struct {
	R *Repo
	I *Issue
}

type repoPull struct {
	R *Repo
	P *Pull
}

type repoIssueComment struct {
	R *Repo
	I *Issue
	C *Comment
}

type gqlConn struct {
	Items []*GTag
	Total int
	Page  map[string]interface{}
}

func gqlConnection(items []*GTag, args map[string]interface{}) *gqlConn {
	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
			parts := strings.Split(string(raw), ":")
			if len(parts) == 2 {
				if n, err := strconv.Atoi(parts[1]); err == nil {
					start = n + 1
				}
			}
		}
	}
	sliced := items
	if start > len(items) {
		sliced = []*GTag{}
	} else {
		sliced = items[start:]
	}
	if first, ok := toIntOK(args["first"]); ok {
		if first < len(sliced) {
			sliced = sliced[:first]
		}
	}
	endIndex := start + len(sliced) - 1
	var endCursor, startCursor interface{}
	if len(sliced) > 0 {
		endCursor = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", endIndex)))
		startCursor = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", start)))
	}
	page := map[string]interface{}{
		"hasNextPage":     (start + len(sliced)) < len(items),
		"endCursor":       endCursor,
		"startCursor":     startCursor,
		"hasPreviousPage": start > 0,
	}
	return &gqlConn{Items: sliced, Total: len(items), Page: page}
}

func toIntOK(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func toInt(v interface{}, def int) int {
	if n, ok := toIntOK(v); ok {
		return n
	}
	return def
}

func argStr(args map[string]interface{}, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func gqlRepoByDbid(dbid int) *Repo {
	for _, repo := range FORGE.OrderedRepos() {
		if repo.Dbid == dbid {
			return repo
		}
	}
	return nil
}

func gqlIssuesConnection(repo *Repo, args map[string]interface{}) *GTag {
	var items []*Issue
	for _, i := range repo.Issues {
		items = append(items, i)
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Number < items[b].Number })
	if statesRaw, ok := args["states"].([]interface{}); ok {
		wanted := map[string]bool{}
		for _, s := range statesRaw {
			if str, ok := s.(string); ok {
				wanted[strings.ToLower(str)] = true
			}
		}
		var filtered []*Issue
		for _, i := range items {
			if wanted[i.State] {
				filtered = append(filtered, i)
			}
		}
		items = filtered
	}
	direction := "DESC"
	field := "CREATED_AT"
	if order, ok := args["orderBy"].(map[string]interface{}); ok {
		if d := argStr(order, "direction"); d != "" {
			direction = d
		}
		if f := argStr(order, "field"); f != "" {
			field = f
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		var ka, kb int64
		switch field {
		case "COMMENTS":
			ka, kb = int64(len(items[a].Comments)), int64(len(items[b].Comments))
		case "UPDATED_AT":
			ka, kb = items[a].UpdatedTs, items[b].UpdatedTs
		default:
			ka, kb = int64(items[a].Number), int64(items[b].Number)
		}
		if direction == "DESC" {
			return ka > kb
		}
		return ka < kb
	})
	tagged := make([]*GTag, len(items))
	for i, issue := range items {
		tagged[i] = gqlTag("Issue", repoIssue{repo, issue})
	}
	return gqlTag("IssueConnection", gqlConnection(tagged, args))
}

func gqlPullsConnection(repo *Repo, args map[string]interface{}) *GTag {
	var items []*Pull
	for _, p := range repo.Pulls {
		items = append(items, p)
	}
	sort.Slice(items, func(a, b int) bool { return items[a].Number > items[b].Number })
	if statesRaw, ok := args["states"].([]interface{}); ok {
		wanted := map[string]bool{}
		for _, s := range statesRaw {
			if str, ok := s.(string); ok {
				wanted[str] = true
			}
		}
		var filtered []*Pull
		for _, p := range items {
			stateOf := "OPEN"
			if p.Merged {
				stateOf = "MERGED"
			} else if p.State == "closed" {
				stateOf = "CLOSED"
			}
			if wanted[stateOf] {
				filtered = append(filtered, p)
			}
		}
		items = filtered
	}
	tagged := make([]*GTag, len(items))
	for i, pull := range items {
		tagged[i] = gqlTag("PullRequest", repoPull{repo, pull})
	}
	return gqlTag("PullRequestConnection", gqlConnection(tagged, args))
}

func dbidFromGlobalID(value string) (int, bool) {
	_, key := parseGlobalID(value)
	if key == "" {
		return 0, false
	}
	n, err := strconv.Atoi(key)
	return n, err == nil
}

func repoIssueFromGlobalID(value string) (*Repo, int, bool) {
	_, key := parseGlobalID(value)
	parts := strings.Split(key, ":")
	if len(parts) < 2 {
		return nil, 0, false
	}
	dbid, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, 0, false
	}
	number, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, 0, false
	}
	repo := gqlRepoByDbid(dbid)
	if repo == nil {
		return nil, 0, false
	}
	return repo, number, true
}

// gqlResolveField returns either a plain value or a *GTag.
func gqlResolveField(typeName string, obj interface{}, field string, args map[string]interface{}) (interface{}, error) {
	switch typeName {
	case "Query":
		switch field {
		case "viewer":
			return gqlTag("User", nil), nil
		case "repository":
			repo := FORGE.GetRepo(argStr(args, "owner"), argStr(args, "name"))
			if repo == nil {
				return nil, nil
			}
			return gqlTag("Repository", repo), nil
		case "node":
			decodedType, key := parseGlobalID(argStr(args, "id"))
			if decodedType == "Repository" {
				if dbid, ok := dbidFromGlobalID(argStr(args, "id")); ok {
					if repo := gqlRepoByDbid(dbid); repo != nil {
						return gqlTag("Repository", repo), nil
					}
				}
				return nil, nil
			}
			if decodedType == "Issue" || decodedType == "PullRequest" {
				parts := strings.Split(key, ":")
				if len(parts) < 2 {
					return nil, nil
				}
				dbid, err := strconv.Atoi(parts[0])
				if err != nil {
					return nil, nil
				}
				repo := gqlRepoByDbid(dbid)
				if repo == nil {
					return nil, nil
				}
				number, err := strconv.Atoi(parts[1])
				if err != nil {
					return nil, nil
				}
				if decodedType == "Issue" {
					if issue, ok := repo.Issues[number]; ok {
						return gqlTag("Issue", repoIssue{repo, issue}), nil
					}
				} else {
					if pull, ok := repo.Pulls[number]; ok {
						return gqlTag("PullRequest", repoPull{repo, pull}), nil
					}
				}
			}
			return nil, nil
		case "__type":
			name := argStr(args, "name")
			if _, ok := GQL_TYPE_FIELDS[name]; ok {
				return gqlTag("__Type", name), nil
			}
			if _, ok := GQL_ENUM_VALUES[name]; ok {
				return gqlTag("__Type", name), nil
			}
			return nil, nil
		case "__schema":
			return gqlTag("__Schema", nil), nil
		}
	case "Mutation":
		input, _ := args["input"].(map[string]interface{})
		if input == nil {
			input = map[string]interface{}{}
		}
		switch field {
		case "createIssue":
			dbid, ok := dbidFromGlobalID(argStr(input, "repositoryId"))
			if !ok {
				return nil, gqlErr("repository not found")
			}
			repo := gqlRepoByDbid(dbid)
			if repo == nil {
				return nil, gqlErr("repository not found")
			}
			issue := createIssue(repo, argStr(input, "title"), argStr(input, "body"), nil)
			return gqlTag("CreateIssuePayload", repoIssue{repo, issue}), nil
		case "closeIssue":
			repo, number, ok := repoIssueFromGlobalID(argStr(input, "issueId"))
			if !ok {
				return nil, gqlErr("issue not found")
			}
			issue := repo.Issues[number]
			if issue == nil {
				return nil, gqlErr("issue not found")
			}
			issue.State = "closed"
			ts := CLOCK.Tick()
			issue.ClosedTs = &ts
			return gqlTag("CloseIssuePayload", repoIssue{repo, issue}), nil
		case "createPullRequest":
			dbid, ok := dbidFromGlobalID(argStr(input, "repositoryId"))
			if !ok {
				return nil, gqlErr("repository not found")
			}
			repo := gqlRepoByDbid(dbid)
			if repo == nil {
				return nil, gqlErr("repository not found")
			}
			pull := createPull(repo, argStr(input, "title"), argStr(input, "body"),
				argStr(input, "headRefName"), argStr(input, "baseRefName"))
			return gqlTag("CreatePullRequestPayload", repoPull{repo, pull}), nil
		case "mergePullRequest":
			repo, number, ok := repoIssueFromGlobalID(argStr(input, "pullRequestId"))
			if !ok {
				return nil, gqlErr("pull request not found")
			}
			pull := repo.Pulls[number]
			if pull == nil {
				return nil, gqlErr("pull request not found")
			}
			method := argStr(input, "mergeMethod")
			if method == "" {
				method = "MERGE"
			}
			if mergePull(repo, pull, strings.ToLower(method)) == "" {
				return nil, gqlErr("pull request is not mergeable")
			}
			return gqlTag("MergePullRequestPayload", repoPull{repo, pull}), nil
		case "addComment":
			repo, number, ok := repoIssueFromGlobalID(argStr(input, "subjectId"))
			if !ok {
				return nil, gqlErr("subject not found")
			}
			issue := repo.Issues[number]
			if issue == nil {
				return nil, gqlErr("subject not found")
			}
			comment := &Comment{ID: FORGE.AllocID(), Body: argStr(input, "body"), CreatedTs: CLOCK.Tick()}
			issue.Comments = append(issue.Comments, comment)
			return gqlTag("AddCommentPayload", repoIssueComment{repo, issue, comment}), nil
		}
	case "User":
		switch field {
		case "id":
			return globalID("User", VIEWER_ID), nil
		case "login":
			return VIEWER_LOGIN, nil
		case "name":
			return VIEWER_NAME, nil
		case "email":
			return VIEWER_EMAIL, nil
		case "url":
			return "", nil
		}
	case "Repository":
		repo := obj.(*Repo)
		switch field {
		case "id":
			return globalID("Repository", repo.Dbid), nil
		case "databaseId":
			return repo.Dbid, nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			return VIEWER_LOGIN + "/" + repo.Name, nil
		case "owner":
			return gqlTag("User", nil), nil
		case "defaultBranchRef":
			return gqlTag("Ref", "main"), nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "hasPullRequestsEnabled":
			return true, nil
		case "isPrivate":
			return repo.Private, nil
		case "url":
			return "", nil
		case "createdAt", "updatedAt":
			return isoOf(repo.CreatedTs), nil
		case "issue":
			issue := repo.Issues[toInt(args["number"], 0)]
			if issue == nil {
				return nil, nil
			}
			return gqlTag("Issue", repoIssue{repo, issue}), nil
		case "issues":
			return gqlIssuesConnection(repo, args), nil
		case "pullRequest":
			pull := repo.Pulls[toInt(args["number"], 0)]
			if pull == nil {
				return nil, nil
			}
			return gqlTag("PullRequest", repoPull{repo, pull}), nil
		case "pullRequests":
			return gqlPullsConnection(repo, args), nil
		}
	case "Ref":
		if field == "name" {
			return obj, nil
		}
		return nil, nil
	case "Issue":
		ri := obj.(repoIssue)
		issue := ri.I
		switch field {
		case "id":
			return globalID("Issue", fmt.Sprintf("%d:%d", ri.R.Dbid, issue.Number)), nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "body":
			return issue.Body, nil
		case "state":
			return strings.ToUpper(issue.State), nil
		case "author":
			return gqlTag("User", nil), nil
		case "createdAt":
			return isoOf(issue.CreatedTs), nil
		case "updatedAt":
			return isoOf(issue.UpdatedTs), nil
		case "url":
			return "", nil
		}
		return nil, nil
	case "PullRequest":
		rp := obj.(repoPull)
		pull := rp.P
		switch field {
		case "id":
			return globalID("PullRequest", fmt.Sprintf("%d:%d", rp.R.Dbid, pull.Number)), nil
		case "number":
			return pull.Number, nil
		case "title":
			return pull.Title, nil
		case "body":
			return pull.Body, nil
		case "state":
			if pull.Merged {
				return "MERGED", nil
			}
			if pull.State == "closed" {
				return "CLOSED", nil
			}
			return "OPEN", nil
		case "merged":
			return pull.Merged, nil
		case "mergeable":
			if len(mergeConflicts(rp.R, pull)) > 0 {
				return "CONFLICTING", nil
			}
			return "MERGEABLE", nil
		case "headRefName":
			return pull.Head, nil
		case "baseRefName":
			return pull.Base, nil
		case "author":
			return gqlTag("User", nil), nil
		case "createdAt":
			return isoOf(pull.CreatedTs), nil
		case "updatedAt":
			return isoOf(pull.UpdatedTs), nil
		case "files":
			files := pullFiles(rp.R, pull)
			tagged := make([]*GTag, len(files))
			for i, f := range files {
				tagged[i] = gqlTag("PullRequestChangedFile", f.Filename)
			}
			return gqlTag("PullRequestChangedFileConnection", gqlConnection(tagged, args)), nil
		}
		return nil, nil
	case "PullRequestChangedFile":
		if field == "path" {
			return obj, nil
		}
		return nil, nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		conn := obj.(*gqlConn)
		switch field {
		case "nodes":
			out := make([]interface{}, len(conn.Items))
			for i, item := range conn.Items {
				out[i] = item
			}
			return out, nil
		case "totalCount":
			return conn.Total, nil
		case "pageInfo":
			return gqlTag("PageInfo", conn.Page), nil
		}
		return nil, nil
	case "PageInfo":
		page := obj.(map[string]interface{})
		return page[field], nil
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			return gqlTag("Issue", obj), nil
		}
		return nil, nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			return gqlTag("PullRequest", obj), nil
		}
		return nil, nil
	case "AddCommentPayload":
		if field == "commentEdge" {
			return gqlTag("IssueCommentEdge", obj), nil
		}
		return nil, nil
	case "IssueCommentEdge":
		if field == "node" {
			return gqlTag("IssueComment", obj), nil
		}
		if field == "cursor" {
			return base64.StdEncoding.EncodeToString([]byte("cursor:0")), nil
		}
		return nil, nil
	case "IssueComment":
		ric := obj.(repoIssueComment)
		comment := ric.C
		switch field {
		case "id":
			return globalID("IssueComment", fmt.Sprintf("%d:%d:%d", ric.R.Dbid, ric.I.Number, comment.ID)), nil
		case "body":
			return comment.Body, nil
		case "author":
			return gqlTag("User", nil), nil
		case "createdAt":
			return isoOf(comment.CreatedTs), nil
		}
		return nil, nil
	case "__Type":
		name := obj.(string)
		switch field {
		case "name":
			return name, nil
		case "kind":
			if _, isEnum := GQL_ENUM_VALUES[name]; isEnum {
				return "ENUM", nil
			}
			return "OBJECT", nil
		case "description":
			return nil, nil
		case "fields":
			names, ok := GQL_TYPE_FIELDS[name]
			if !ok {
				return nil, nil
			}
			out := make([]interface{}, len(names))
			for i, n := range names {
				out[i] = gqlTag("__Field", n)
			}
			return out, nil
		case "enumValues":
			values, ok := GQL_ENUM_VALUES[name]
			if !ok {
				return nil, nil
			}
			out := make([]interface{}, len(values))
			for i, n := range values {
				out[i] = gqlTag("__EnumValue", n)
			}
			return out, nil
		}
		return nil, nil
	case "__Field", "__EnumValue":
		switch field {
		case "name":
			return obj, nil
		case "isDeprecated":
			return false, nil
		}
		return nil, nil
	}
	return nil, nil
}

func gqlExecuteSet(selections []*GSelection, typeName string, obj interface{}, variables map[string]interface{}, fragments map[string]*GFragment) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for _, selection := range selections {
		switch selection.Kind {
		case "spread":
			if frag, ok := fragments[selection.Name]; ok {
				inner, err := gqlExecuteSet(frag.Selections, frag.Type, obj, variables, fragments)
				if err != nil {
					return nil, err
				}
				for k, v := range inner {
					out[k] = v
				}
			}
		case "inline":
			runtime := typeName
			if tag, ok := obj.(*GTag); ok {
				runtime = tag.Type
			}
			if selection.TypeCond == runtime || selection.TypeCond == typeName {
				inner, err := gqlExecuteSet(selection.Selections, selection.TypeCond, obj, variables, fragments)
				if err != nil {
					return nil, err
				}
				for k, v := range inner {
					out[k] = v
				}
			}
		default:
			name := selection.Name
			key := selection.Alias
			if key == "" {
				key = name
			}
			if name == "__typename" {
				if tag, ok := obj.(*GTag); ok {
					out[key] = tag.Type
				} else {
					out[key] = typeName
				}
				continue
			}
			args := map[string]interface{}{}
			for argName, argValue := range selection.Args {
				args[argName] = gqlResolveValue(argValue, variables)
			}
			payload := obj
			if tag, ok := obj.(*GTag); ok {
				payload = tag.Payload
			}
			value, err := gqlResolveField(typeName, payload, name, args)
			if err != nil {
				return nil, err
			}
			completed, err := gqlComplete(value, selection, variables, fragments)
			if err != nil {
				return nil, err
			}
			out[key] = completed
		}
	}
	return out, nil
}

func gqlComplete(value interface{}, selection *GSelection, variables map[string]interface{}, fragments map[string]*GFragment) (interface{}, error) {
	if value == nil {
		return nil, nil
	}
	if tag, ok := value.(*GTag); ok {
		if len(selection.Selections) == 0 {
			return nil, nil
		}
		return gqlExecuteSet(selection.Selections, tag.Type, tag, variables, fragments)
	}
	if list, ok := value.([]interface{}); ok {
		out := make([]interface{}, len(list))
		for i, item := range list {
			completed, err := gqlComplete(item, selection, variables, fragments)
			if err != nil {
				return nil, err
			}
			out[i] = completed
		}
		return out, nil
	}
	return value, nil
}

func gqlRunOne(document map[string]interface{}) map[string]interface{} {
	queryRaw, ok := document["query"].(string)
	if !ok {
		return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "query is required"}}}
	}
	tokens, err := gqlTokenize(queryRaw)
	if err != nil {
		return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": err.Error()}}}
	}
	parser := &GqlParser{tokens: tokens}
	operations, fragments, err := parser.parseDocument()
	if err != nil {
		return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": err.Error()}}}
	}
	if len(operations) == 0 {
		return map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "no operation found"}}}
	}
	operation := operations[0]
	if name, ok := document["operationName"].(string); ok && name != "" {
		for _, candidate := range operations {
			if candidate.Name == name {
				operation = candidate
			}
		}
	}
	variables := map[string]interface{}{}
	for varName, def := range operation.VarDefs {
		if def != nil {
			variables[varName] = gqlResolveValue(def, map[string]interface{}{})
		}
	}
	if vars, ok := document["variables"].(map[string]interface{}); ok {
		for k, v := range vars {
			variables[k] = v
		}
	}
	rootType := "Query"
	if operation.Type == "mutation" {
		rootType = "Mutation"
	}
	var validationErrors []string
	gqlValidate(operation.Selections, rootType, fragments, &validationErrors)
	if len(validationErrors) > 0 {
		errs := make([]interface{}, len(validationErrors))
		for i, msg := range validationErrors {
			errs[i] = map[string]interface{}{"message": msg}
		}
		return map[string]interface{}{"errors": errs, "data": nil}
	}
	data, execErr := gqlExecuteSet(operation.Selections, rootType, nil, variables, fragments)
	if execErr != nil {
		return map[string]interface{}{
			"errors": []interface{}{map[string]interface{}{"message": execErr.Error()}},
			"data":   nil,
		}
	}
	return map[string]interface{}{"data": data}
}

func graphqlReply(body []byte) Reply {
	if len(body) == 0 {
		return jsonReply(400, map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "invalid request"}}}, nil)
	}
	var document interface{}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&document); err != nil {
		return jsonReply(400, map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "invalid JSON"}}}, nil)
	}
	if list, ok := document.([]interface{}); ok {
		out := make([]interface{}, len(list))
		for i, item := range list {
			if m, ok := item.(map[string]interface{}); ok {
				out[i] = gqlRunOne(m)
			} else {
				out[i] = gqlRunOne(map[string]interface{}{})
			}
		}
		return jsonReply(200, out, nil)
	}
	docMap, ok := document.(map[string]interface{})
	if !ok {
		return jsonReply(400, map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "invalid request"}}}, nil)
	}
	return jsonReply(200, gqlRunOne(docMap), nil)
}
