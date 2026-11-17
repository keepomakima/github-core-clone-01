// GraphQL v4: a small parser and executor for the exact documents the
// conformance suite sends (viewer, repository, issues/pulls connections,
// mutations, node lookup, __type introspection, batch operations). Unknown
// fields answer HTTP 200 with an errors array naming the field; unknown
// repositories answer data.repository null. No model calls, no network.

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// lexer / parser
// ---------------------------------------------------------------------------------------------

type tokenKind string

const (
	tokName   tokenKind = "name"
	tokInt    tokenKind = "int"
	tokFloat  tokenKind = "float"
	tokString tokenKind = "string"
	tokPunct  tokenKind = "punct"
	tokSpread tokenKind = "spread"
)

type token struct {
	kind  tokenKind
	value string
	num   float64
}

func lex(source string) ([]token, error) {
	runes := []rune(source)
	var tokens []token
	i := 0
	isDigit := func(c rune) bool { return c >= '0' && c <= '9' }
	isNameStart := func(c rune) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' }
	isNameChar := func(c rune) bool { return isNameStart(c) || isDigit(c) }
	for i < len(runes) {
		ch := runes[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+2 < len(runes) && runes[i+1] == '.' && runes[i+2] == '.' {
			tokens = append(tokens, token{kind: tokSpread})
			i += 3
			continue
		}
		if strings.ContainsRune("{}():!@=|&$[]", ch) {
			tokens = append(tokens, token{kind: tokPunct, value: string(ch)})
			i++
			continue
		}
		if ch == '"' {
			var value []rune
			i++
			for i < len(runes) && runes[i] != '"' {
				if runes[i] == '\\' && i+1 < len(runes) {
					escaped := runes[i+1]
					switch escaped {
					case 'n':
						value = append(value, '\n')
					case 't':
						value = append(value, '\t')
					case 'r':
						value = append(value, '\r')
					default:
						value = append(value, escaped)
					}
					i += 2
				} else {
					value = append(value, runes[i])
					i++
				}
			}
			i++
			tokens = append(tokens, token{kind: tokString, value: string(value)})
			continue
		}
		if isDigit(ch) || ch == '-' {
			var text []rune
			for i < len(runes) {
				c := runes[i]
				if isDigit(c) || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
					text = append(text, c)
					i++
				} else {
					break
				}
			}
			s := string(text)
			num, _ := strconv.ParseFloat(s, 64)
			if strings.ContainsAny(s, ".eE") {
				tokens = append(tokens, token{kind: tokFloat, value: s, num: num})
			} else {
				tokens = append(tokens, token{kind: tokInt, value: s, num: num})
			}
			continue
		}
		if isNameStart(ch) {
			var text []rune
			for i < len(runes) && isNameChar(runes[i]) {
				text = append(text, runes[i])
				i++
			}
			tokens = append(tokens, token{kind: tokName, value: string(text)})
			continue
		}
		return nil, fmt.Errorf("unexpected character %c in GraphQL query", ch)
	}
	return tokens, nil
}

type valueNode struct {
	kind   string // variable, int, float, string, boolean, null, enum, list, object
	name   string
	num    float64
	str    string
	boolean bool
	values []valueNode
	fields []valueField
}

type valueField struct {
	name  string
	value valueNode
}

type fieldNode struct {
	alias       string
	hasAlias    bool
	name        string
	arguments   []argNode
	selectionSet []selectionNode
	hasSelection bool
}

type argNode struct {
	name  string
	value valueNode
}

type selectionNode struct {
	kind          string // field, inlineFragment, fragmentSpread
	field         *fieldNode
	typeCondition string
	hasCondition  bool
	selectionSet  []selectionNode
	name          string
}

type operationNode struct {
	kind                string // query, mutation
	name                string
	hasName             bool
	variableDefinitions []varDef
	selectionSet        []selectionNode
}

type varDef struct {
	name         string
	defaultValue *valueNode
}

type fragmentNode struct {
	name          string
	typeCondition string
	selectionSet  []selectionNode
}

type parser struct {
	pos    int
	tokens []token
}

func (p *parser) peek() *token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *parser) next() (token, error) {
	if p.pos >= len(p.tokens) {
		return token{}, errors.New("unexpected end of GraphQL query")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *parser) expectPunct(value string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tokPunct || t.value != value {
		return fmt.Errorf("expected %s", value)
	}
	return nil
}

func (p *parser) acceptPunct(value string) bool {
	t := p.peek()
	if t != nil && t.kind == tokPunct && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectName() (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	if t.kind != tokName {
		return "", errors.New("expected a name")
	}
	return t.value, nil
}

func (p *parser) acceptName(value string) bool {
	t := p.peek()
	if t != nil && t.kind == tokName && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *parser) parseDocument() ([]operationNode, map[string]*fragmentNode, error) {
	var operations []operationNode
	fragments := map[string]*fragmentNode{}
	for p.pos < len(p.tokens) {
		t := p.peek()
		if t.kind == tokPunct && t.value == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, operationNode{kind: "query", selectionSet: sel})
			continue
		}
		if t.kind == tokName && t.value == "fragment" {
			if _, err := p.next(); err != nil {
				return nil, nil, err
			}
			name, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			if _, err := p.expectName(); err != nil { // 'on'
				return nil, nil, err
			}
			typeCondition, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[name] = &fragmentNode{name: name, typeCondition: typeCondition, selectionSet: sel}
			continue
		}
		if t.kind == tokName && (t.value == "query" || t.value == "mutation") {
			kindTok, err := p.next()
			if err != nil {
				return nil, nil, err
			}
			kind := kindTok.value
			op := operationNode{kind: kind}
			if next := p.peek(); next != nil && next.kind == tokName {
				name, err := p.expectName()
				if err != nil {
					return nil, nil, err
				}
				op.name = name
				op.hasName = true
			}
			defs, err := p.parseVariableDefinitions()
			if err != nil {
				return nil, nil, err
			}
			op.variableDefinitions = defs
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			op.selectionSet = sel
			operations = append(operations, op)
			continue
		}
		return nil, nil, errors.New("unexpected token at top level of GraphQL document")
	}
	return operations, fragments, nil
}

func (p *parser) parseVariableDefinitions() ([]varDef, error) {
	var defs []varDef
	if !p.acceptPunct("(") {
		return defs, nil
	}
	for !p.acceptPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		// Skip the type (name, brackets, !) — types are not needed to execute.
		depth := 0
		for {
			t, err := p.next()
			if err != nil {
				return nil, err
			}
			if t.kind == tokPunct && t.value == "[" {
				depth++
			} else if t.kind == tokPunct && t.value == "]" {
				depth--
			} else if t.kind == tokPunct && (t.value == "," || t.value == ")") {
				if depth == 0 {
					if t.value == ")" {
						p.pos--
					}
					break
				}
			}
		}
		var defaultValue *valueNode
		if p.acceptPunct("=") {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			defaultValue = &v
		}
		defs = append(defs, varDef{name: name, defaultValue: defaultValue})
	}
	return defs, nil
}

func (p *parser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []selectionNode
	for !p.acceptPunct("}") {
		t := p.peek()
		if t != nil && t.kind == tokSpread {
			if _, err := p.next(); err != nil {
				return nil, err
			}
			next := p.peek()
			if next != nil && next.kind == tokName && next.value == "on" {
				if _, err := p.next(); err != nil {
					return nil, err
				}
				typeCondition, err := p.expectName()
				if err != nil {
					return nil, err
				}
				sel, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, selectionNode{kind: "inlineFragment", typeCondition: typeCondition, hasCondition: true, selectionSet: sel})
			} else if next != nil && next.kind == tokName {
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				selections = append(selections, selectionNode{kind: "fragmentSpread", name: name})
			} else {
				sel, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				selections = append(selections, selectionNode{kind: "inlineFragment", selectionSet: sel})
			}
			continue
		}
		field, err := p.parseField()
		if err != nil {
			return nil, err
		}
		selections = append(selections, selectionNode{kind: "field", field: field})
	}
	return selections, nil
}

func (p *parser) parseField() (*fieldNode, error) {
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	field := &fieldNode{name: name}
	if p.acceptPunct(":") {
		field.alias = name
		field.hasAlias = true
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
		field.name = name
	}
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			argName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			field.arguments = append(field.arguments, argNode{name: argName, value: v})
		}
	}
	for p.acceptPunct("@") {
		if _, err := p.expectName(); err != nil {
			return nil, err
		}
		if p.acceptPunct("(") {
			for !p.acceptPunct(")") {
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
		}
	}
	if next := p.peek(); next != nil && next.kind == tokPunct && next.value == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		field.selectionSet = sel
		field.hasSelection = true
	}
	return field, nil
}

func (p *parser) parseValue() (valueNode, error) {
	t, err := p.next()
	if err != nil {
		return valueNode{}, err
	}
	if t.kind == tokPunct && t.value == "$" {
		name, err := p.expectName()
		if err != nil {
			return valueNode{}, err
		}
		return valueNode{kind: "variable", name: name}, nil
	}
	if t.kind == tokPunct && t.value == "[" {
		var values []valueNode
		for !p.acceptPunct("]") {
			v, err := p.parseValue()
			if err != nil {
				return valueNode{}, err
			}
			values = append(values, v)
		}
		return valueNode{kind: "list", values: values}, nil
	}
	if t.kind == tokPunct && t.value == "{" {
		var fields []valueField
		for !p.acceptPunct("}") {
			name, err := p.expectName()
			if err != nil {
				return valueNode{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return valueNode{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return valueNode{}, err
			}
			fields = append(fields, valueField{name: name, value: v})
		}
		return valueNode{kind: "object", fields: fields}, nil
	}
	if t.kind == tokString {
		return valueNode{kind: "string", str: t.value}, nil
	}
	if t.kind == tokInt {
		return valueNode{kind: "int", num: t.num}, nil
	}
	if t.kind == tokFloat {
		return valueNode{kind: "float", num: t.num}, nil
	}
	if t.kind == tokName {
		switch t.value {
		case "true":
			return valueNode{kind: "boolean", boolean: true}, nil
		case "false":
			return valueNode{kind: "boolean", boolean: false}, nil
		case "null":
			return valueNode{kind: "null"}, nil
		default:
			return valueNode{kind: "enum", str: t.value}, nil
		}
	}
	return valueNode{}, errors.New("unexpected value in GraphQL query")
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

type gqlContext struct {
	store     *store
	variables map[string]any
	fragments map[string]*fragmentNode
	errors    []map[string]any
}

// gqlValue is a typed GraphQL value; fields are resolved lazily by
// resolveField, mirroring the TS discriminated union.
type gqlValue struct {
	typename string
	// User
	login string
	id    int
	name  string
	// Repository / Issue / PullRequest
	repo  *repo
	issue *issue
	pull  *pullRequest
	// Ref
	refName string
	// Connections
	nodes    []any
	pageInfo *gqlValue
	// PullRequestFile
	path string
	// Comment
	commentID   string
	commentBody string
	node        *gqlValue
	// PageInfo
	hasNextPage bool
	endCursor   *string
	// Payloads
	issueVal     *gqlValue
	pullRequestVal *gqlValue
	commentEdge  *gqlValue
	// Type
	typeName      string
	typeKind      string
	typeFields    []any
	typeEnumValues []any
}

func resolveValue(node valueNode, variables map[string]any) any {
	switch node.kind {
	case "variable":
		return variables[node.name]
	case "int", "float":
		return node.num
	case "string":
		return node.str
	case "boolean":
		return node.boolean
	case "null":
		return nil
	case "enum":
		return node.str
	case "list":
		var out []any
		for _, v := range node.values {
			out = append(out, resolveValue(v, variables))
		}
		if out == nil {
			out = []any{}
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = resolveValue(f.value, variables)
		}
		return out
	}
	return nil
}

func argsOf(field *fieldNode, ctx *gqlContext) map[string]any {
	out := map[string]any{}
	for _, arg := range field.arguments {
		out[arg.name] = resolveValue(arg.value, ctx.variables)
	}
	return out
}

func cursorFor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

func cursorNumber(cursor *string) *int {
	if cursor == nil || *cursor == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil {
		return nil
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return nil
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return nil
	}
	return &n
}

type pageResult struct {
	slice       []any
	hasNextPage bool
	endCursor   *string
}

func pageItems(items []numbered, first any, after any, descending bool) pageResult {
	var afterNumber *int
	if s, ok := after.(string); ok {
		afterNumber = cursorNumber(&s)
	}
	rest := items
	if afterNumber != nil {
		var filtered []numbered
		for _, item := range rest {
			if descending {
				if item.number() < *afterNumber {
					filtered = append(filtered, item)
				}
			} else if item.number() > *afterNumber {
				filtered = append(filtered, item)
			}
		}
		rest = filtered
	}
	limit := len(rest)
	if f, ok := first.(float64); ok && f > 0 {
		limit = int(f)
	}
	if limit > len(rest) {
		limit = len(rest)
	}
	slice := rest[:limit]
	hasNextPage := len(rest) > len(slice)
	var endCursor *string
	if len(slice) > 0 {
		c := cursorFor(slice[len(slice)-1].number())
		endCursor = &c
	}
	var out []any
	for _, item := range slice {
		out = append(out, item)
	}
	if out == nil {
		out = []any{}
	}
	return pageResult{slice: out, hasNextPage: hasNextPage, endCursor: endCursor}
}

type numbered interface{ number() int }

type numberedIssue struct{ v *issue }

func (n numberedIssue) number() int { return n.v.number }

type numberedPull struct{ v *pullRequest }

func (n numberedPull) number() int { return n.v.number }

type introspectionSpec struct {
	kind       string
	fields     []string
	enumValues []string
}

var introspection = map[string]introspectionSpec{
	"Issue": {kind: "OBJECT", fields: []string{"id", "number", "title", "state", "body"}},
	"PullRequest": {kind: "OBJECT", fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}},
	"Repository": {kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"SearchType": {kind: "ENUM", enumValues: []string{"ISSUE", "USER", "REPOSITORY"}},
}

func repoValue(r *repo) *gqlValue  { return &gqlValue{typename: "Repository", repo: r} }
func issueValue(r *repo, i *issue) *gqlValue {
	return &gqlValue{typename: "Issue", repo: r, issue: i}
}
func pullValue(r *repo, p *pullRequest) *gqlValue {
	return &gqlValue{typename: "PullRequest", repo: r, pull: p}
}

func findRepoByDatabaseID(s *store, databaseID int) *repo {
	for _, r := range s.repos {
		if r.databaseID == databaseID {
			return r
		}
	}
	return nil
}

func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func resolveField(value *gqlValue, field *fieldNode, ctx *gqlContext) any {
	s := ctx.store
	args := argsOf(field, ctx)
	switch value.typename {
	case "Query":
		switch field.name {
		case "viewer":
			return &gqlValue{typename: "User", login: VIEWER.login, id: VIEWER.id, name: VIEWER.name}
		case "repository":
			owner := asStr(args["owner"])
			name := asStr(args["name"])
			if owner != VIEWER.login {
				return nil
			}
			r := s.getRepo(owner, name)
			if r == nil {
				return nil
			}
			return repoValue(r)
		case "node":
			kind, parts, ok := decodeGlobalID(asStr(args["id"]))
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
				if r == nil {
					return nil
				}
				return repoValue(r)
			case "Issue":
				r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
				if r == nil {
					return nil
				}
				iss := r.issues[asInt(parsePart(parts, 1))]
				if iss == nil {
					return nil
				}
				return issueValue(r, iss)
			case "PullRequest":
				r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
				if r == nil {
					return nil
				}
				pull := r.pulls[asInt(parsePart(parts, 1))]
				if pull == nil {
					return nil
				}
				return pullValue(r, pull)
			}
			return nil
		case "__type":
			spec, ok := introspection[asStr(args["name"])]
			if !ok {
				return nil
			}
			var fields []any
			for _, f := range spec.fields {
				fields = append(fields, map[string]any{"name": f})
			}
			var enumValues []any
			for _, e := range spec.enumValues {
				enumValues = append(enumValues, map[string]any{"name": e})
			}
			return &gqlValue{typename: "Type", typeName: asStr(args["name"]), typeKind: spec.kind, typeFields: fields, typeEnumValues: enumValues}
		}
	case "Mutation":
		switch field.name {
		case "createIssue":
			input := asMap(args["input"])
			kind, parts, ok := decodeGlobalID(asStr(input["repositoryId"]))
			if !ok || kind != "Repository" {
				return &gqlValue{typename: "IssuePayload"}
			}
			r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
			if r == nil {
				return &gqlValue{typename: "IssuePayload"}
			}
			iss, _ := s.createIssue(r, asStr(input["title"]), asStr(input["body"]), nil)
			return &gqlValue{typename: "IssuePayload", issueVal: issueValue(r, iss)}
		case "closeIssue":
			input := asMap(args["input"])
			kind, parts, ok := decodeGlobalID(asStr(input["issueId"]))
			if !ok || kind != "Issue" {
				return &gqlValue{typename: "IssuePayload"}
			}
			r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
			var iss *issue
			if r != nil {
				iss = r.issues[asInt(parsePart(parts, 1))]
			}
			if r == nil || iss == nil {
				return &gqlValue{typename: "IssuePayload"}
			}
			iss.state = "closed"
			iss.updatedAt = s.nowISO()
			s.touch(r)
			return &gqlValue{typename: "IssuePayload", issueVal: issueValue(r, iss)}
		case "createPullRequest":
			input := asMap(args["input"])
			kind, parts, ok := decodeGlobalID(asStr(input["repositoryId"]))
			if !ok || kind != "Repository" {
				return &gqlValue{typename: "PullRequestPayload"}
			}
			r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
			if r == nil {
				return &gqlValue{typename: "PullRequestPayload"}
			}
			headRef := asStr(input["headRefName"])
			baseRef := asStr(input["baseRefName"])
			headSHA := s.refTip(r, headRef)
			baseSHA := s.refTip(r, baseRef)
			if headSHA == "" || baseSHA == "" {
				return &gqlValue{typename: "PullRequestPayload"}
			}
			pull := s.createPull(r, asStr(input["title"]), "", headRef, baseRef, headSHA, baseSHA)
			return &gqlValue{typename: "PullRequestPayload", pullRequestVal: pullValue(r, pull)}
		case "mergePullRequest":
			input := asMap(args["input"])
			kind, parts, ok := decodeGlobalID(asStr(input["pullRequestId"]))
			if !ok || kind != "PullRequest" {
				return &gqlValue{typename: "PullRequestPayload"}
			}
			r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
			var pull *pullRequest
			if r != nil {
				pull = r.pulls[asInt(parsePart(parts, 1))]
			}
			if r == nil || pull == nil {
				return &gqlValue{typename: "PullRequestPayload"}
			}
			s.mergePull(r, pull)
			return &gqlValue{typename: "PullRequestPayload", pullRequestVal: pullValue(r, pull)}
		case "addComment":
			input := asMap(args["input"])
			kind, parts, ok := decodeGlobalID(asStr(input["subjectId"]))
			if !ok || kind != "Issue" {
				return &gqlValue{typename: "CommentPayload"}
			}
			r := findRepoByDatabaseID(s, asInt(parsePart(parts, 0)))
			var iss *issue
			if r != nil {
				iss = r.issues[asInt(parsePart(parts, 1))]
			}
			if r == nil || iss == nil {
				return &gqlValue{typename: "CommentPayload"}
			}
			comment := s.addComment(r, iss, asStr(input["body"]))
			node := &gqlValue{
				typename:    "Comment",
				commentID:   globalID("IssueComment", []string{strconv.Itoa(r.databaseID), strconv.Itoa(iss.number), strconv.Itoa(comment.id)}),
				commentBody: comment.body,
			}
			return &gqlValue{typename: "CommentPayload", commentEdge: &gqlValue{typename: "CommentEdge", node: node}}
		}
	case "User":
		switch field.name {
		case "login":
			return value.login
		case "id":
			return value.id
		case "name":
			return value.name
		}
	case "Repository":
		r := value.repo
		switch field.name {
		case "id":
			return globalID("Repository", []string{strconv.Itoa(r.databaseID)})
		case "databaseId":
			return r.databaseID
		case "name":
			return r.name
		case "nameWithOwner":
			return r.owner + "/" + r.name
		case "owner":
			ownerID := 1001
			if r.owner == VIEWER.login {
				ownerID = VIEWER.id
			}
			return &gqlValue{typename: "User", login: r.owner, id: ownerID, name: r.owner}
		case "defaultBranchRef":
			if s.headCommit(r) == "" {
				return nil
			}
			return &gqlValue{typename: "Ref", refName: r.defaultBranch}
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "issue":
			iss := r.issues[asInt(args["number"])]
			if iss == nil {
				return nil
			}
			return issueValue(r, iss)
		case "issues":
			var states []string
			if arr, ok := args["states"].([]any); ok {
				for _, st := range arr {
					states = append(states, strings.ToLower(asStr(st)))
				}
			}
			orderBy := asMap(args["orderBy"])
			var issues []*issue
			for _, iss := range r.issues {
				issues = append(issues, iss)
			}
			if states != nil {
				wanted := map[string]bool{}
				for _, st := range states {
					wanted[st] = true
				}
				var filtered []*issue
				for _, iss := range issues {
					if wanted[iss.state] {
						filtered = append(filtered, iss)
					}
				}
				issues = filtered
			}
			direction := asStr(orderBy["direction"])
			if direction == "" {
				direction = "DESC"
			}
			sort.Slice(issues, func(i, j int) bool {
				if direction == "ASC" {
					return issues[i].number < issues[j].number
				}
				return issues[i].number > issues[j].number
			})
			var numbered []numbered
			for _, iss := range issues {
				numbered = append(numbered, numberedIssue{v: iss})
			}
			result := pageItems(numbered, args["first"], args["after"], direction != "ASC")
			var nodes []any
			for _, item := range result.slice {
				nodes = append(nodes, issueValue(r, item.(numberedIssue).v))
			}
			return &gqlValue{
				typename: "IssueConnection",
				nodes:    nodes,
				pageInfo: &gqlValue{typename: "PageInfo", hasNextPage: result.hasNextPage, endCursor: result.endCursor},
			}
		case "pullRequests":
			var states []string
			if arr, ok := args["states"].([]any); ok {
				for _, st := range arr {
					states = append(states, strings.ToLower(asStr(st)))
				}
			}
			var pulls []*pullRequest
			for _, pull := range r.pulls {
				pulls = append(pulls, pull)
			}
			if states != nil {
				wanted := map[string]bool{}
				for _, st := range states {
					wanted[st] = true
				}
				var filtered []*pullRequest
				for _, pull := range pulls {
					if wanted[pull.state] {
						filtered = append(filtered, pull)
					}
				}
				pulls = filtered
			} else {
				var filtered []*pullRequest
				for _, pull := range pulls {
					if pull.state == "open" {
						filtered = append(filtered, pull)
					}
				}
				pulls = filtered
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].number > pulls[j].number })
			var numbered []numbered
			for _, pull := range pulls {
				numbered = append(numbered, numberedPull{v: pull})
			}
			result := pageItems(numbered, args["first"], args["after"], true)
			var nodes []any
			for _, item := range result.slice {
				nodes = append(nodes, pullValue(r, item.(numberedPull).v))
			}
			return &gqlValue{
				typename: "PullRequestConnection",
				nodes:    nodes,
				pageInfo: &gqlValue{typename: "PageInfo", hasNextPage: result.hasNextPage, endCursor: result.endCursor},
			}
		case "pullRequest":
			pull := r.pulls[asInt(args["number"])]
			if pull == nil {
				return nil
			}
			return pullValue(r, pull)
		}
	case "Issue":
		iss := value.issue
		switch field.name {
		case "id":
			return globalID("Issue", []string{strconv.Itoa(value.repo.databaseID), strconv.Itoa(iss.number)})
		case "number":
			return iss.number
		case "title":
			return iss.title
		case "state":
			if iss.state == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "body":
			return iss.body
		}
	case "PullRequest":
		pull := value.pull
		r := value.repo
		switch field.name {
		case "id":
			return globalID("PullRequest", []string{strconv.Itoa(r.databaseID), strconv.Itoa(pull.number)})
		case "number":
			return pull.number
		case "headRefName":
			return pull.headRef
		case "baseRefName":
			return pull.baseRef
		case "mergeable":
			baseTip := s.refTip(r, pull.baseRef)
			if baseTip == "" {
				return "UNKNOWN"
			}
			if s.mergeable(r, baseTip, pull.headSHA) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "state":
			if pull.state == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "merged":
			return pull.merged
		case "files":
			files := s.diffFiles(r, pull.baseSHA, pull.headSHA)
			first := len(files)
			if f, ok := args["first"].(float64); ok && f > 0 {
				first = int(f)
			}
			if first > len(files) {
				first = len(files)
			}
			var nodes []any
			for _, entry := range files[:first] {
				nodes = append(nodes, &gqlValue{typename: "PullRequestFile", path: entry.filename})
			}
			if nodes == nil {
				nodes = []any{}
			}
			return &gqlValue{typename: "PullRequestFileConnection", nodes: nodes}
		}
	case "Ref":
		if field.name == "name" {
			return value.refName
		}
	case "IssueConnection", "PullRequestConnection":
		if field.name == "nodes" {
			return value.nodes
		}
		if field.name == "pageInfo" {
			return value.pageInfo
		}
	case "PullRequestFileConnection":
		if field.name == "nodes" {
			return value.nodes
		}
	case "PullRequestFile":
		if field.name == "path" {
			return value.path
		}
	case "Comment":
		if field.name == "id" {
			return value.commentID
		}
		if field.name == "body" {
			return value.commentBody
		}
	case "CommentEdge":
		if field.name == "node" {
			return value.node
		}
	case "PageInfo":
		switch field.name {
		case "hasNextPage":
			return value.hasNextPage
		case "endCursor":
			if value.endCursor == nil {
				return nil
			}
			return *value.endCursor
		case "hasPreviousPage":
			return false
		case "startCursor":
			return nil
		}
	case "IssuePayload":
		if field.name == "issue" {
			return value.issueVal
		}
	case "PullRequestPayload":
		if field.name == "pullRequest" {
			return value.pullRequestVal
		}
	case "CommentPayload":
		if field.name == "commentEdge" {
			return value.commentEdge
		}
	case "Type":
		switch field.name {
		case "name":
			return value.typeName
		case "kind":
			return value.typeKind
		case "fields":
			return value.typeFields
		case "enumValues":
			return value.typeEnumValues
		}
	}
	if field.name == "__typename" {
		return value.typename
	}
	ctx.errors = append(ctx.errors, map[string]any{"message": fmt.Sprintf("Cannot query field %q on type %q.", field.name, value.typename)})
	return nil
}

func parsePart(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return ""
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func executeSelection(value any, selections []selectionNode, ctx *gqlContext) any {
	if arr, ok := value.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, executeSelection(item, selections, ctx))
		}
		return out
	}
	if value == nil {
		return nil
	}
	if _, isScalar := value.(string); isScalar {
		return value
	}
	if _, isScalar := value.(float64); isScalar {
		return value
	}
	if _, isScalar := value.(bool); isScalar {
		return value
	}
	if _, isScalar := value.(int); isScalar {
		return value
	}
	out := map[string]any{}
	for _, selection := range selections {
		switch selection.kind {
		case "field":
			field := selection.field
			key := field.name
			if field.hasAlias {
				key = field.alias
			}
			var resolved any
			if gv, ok := value.(*gqlValue); ok {
				resolved = resolveField(gv, field, ctx)
			} else if m, ok := value.(map[string]any); ok {
				resolved = m[field.name]
			} else {
				resolved = nil
			}
			if field.hasSelection && resolved != nil {
				resolved = executeSelection(resolved, field.selectionSet, ctx)
			}
			out[key] = resolved
		case "inlineFragment":
			if selection.hasCondition {
				if gv, ok := value.(*gqlValue); ok && selection.typeCondition != gv.typename {
					continue
				}
			}
			if sub, ok := executeSelection(value, selection.selectionSet, ctx).(map[string]any); ok {
				for k, v := range sub {
					out[k] = v
				}
			}
		case "fragmentSpread":
			fragment, ok := ctx.fragments[selection.name]
			if !ok {
				ctx.errors = append(ctx.errors, map[string]any{"message": fmt.Sprintf("Unknown fragment %q.", selection.name)})
				continue
			}
			if gv, ok := value.(*gqlValue); ok && fragment.typeCondition != "" && fragment.typeCondition != gv.typename {
				continue
			}
			if sub, ok := executeSelection(value, fragment.selectionSet, ctx).(map[string]any); ok {
				for k, v := range sub {
					out[k] = v
				}
			}
		}
	}
	return out
}

func executeOperation(s *store, operation operationNode, variables map[string]any, fragments map[string]*fragmentNode) map[string]any {
	ctx := &gqlContext{store: s, variables: variables, fragments: fragments}
	var root *gqlValue
	if operation.kind == "mutation" {
		root = &gqlValue{typename: "Mutation"}
	} else {
		root = &gqlValue{typename: "Query"}
	}
	data := executeSelection(root, operation.selectionSet, ctx)
	envelope := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		var errs []any
		for _, e := range ctx.errors {
			errs = append(errs, e)
		}
		envelope["errors"] = errs
	}
	return envelope
}

// handleGraphQL handles one GraphQL request body (single operation or a batch
// array). It always answers HTTP 200 with an envelope, except a non-object
// body.
func handleGraphQL(s *store, body any) (status int, result any) {
	run := func(item any) map[string]any {
		m, ok := item.(map[string]any)
		if !ok {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Request must contain a query string."}}}
		}
		query, ok := m["query"].(string)
		if !ok {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Request must contain a query string."}}}
		}
		variables := map[string]any{}
		if v, ok := m["variables"].(map[string]any); ok {
			variables = v
		}
		tokens, err := lex(query)
		if err != nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
		}
		p := &parser{tokens: tokens}
		operations, fragments, err := p.parseDocument()
		if err != nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
		}
		var operation *operationNode
		operationName, _ := m["operationName"].(string)
		for i := range operations {
			if operationName == "" || operations[i].name == operationName {
				operation = &operations[i]
				break
			}
		}
		if operation == nil && len(operations) > 0 {
			operation = &operations[0]
		}
		if operation == nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "No operation found in document."}}}
		}
		return executeOperation(s, *operation, variables, fragments)
	}
	if arr, ok := body.([]any); ok {
		var out []any
		for _, item := range arr {
			out = append(out, run(item))
		}
		if out == nil {
			out = []any{}
		}
		return 200, out
	}
	if _, ok := body.(map[string]any); !ok {
		return 400, map[string]any{"errors": []any{map[string]any{"message": "Request body must be a JSON object."}}}
	}
	return 200, run(body)
}
