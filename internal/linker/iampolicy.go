package linker

import (
	"encoding/json"
	"fmt"
	"strings"
)

// statement is one IAM policy statement, with where it came from.
type statement struct {
	Sid         string
	Allow       bool
	Action      []string
	NotAction   []string
	Resource    []string
	NotResource []string
	// Conditional statements still grant: a condition narrows when, not what.
	// A conditional Deny, though, may not apply, so it cancels nothing.
	Conditional bool

	Source string // evidence source: "iam:GetRolePolicy orders-api-task orders-data"
	Label  string // how details name it: "inline policy orders-data"
}

type rawStatement struct {
	Sid         string
	Effect      string
	Action      json.RawMessage
	NotAction   json.RawMessage
	Resource    json.RawMessage
	NotResource json.RawMessage
	Condition   json.RawMessage
}

// parseStatements reads a policy document. IAM accepts Statement as one object
// or a list, and Action/Resource as a string or a list.
func parseStatements(doc json.RawMessage, source, label string) ([]statement, error) {
	var d struct{ Statement json.RawMessage }
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, err
	}
	var raws []rawStatement
	if err := json.Unmarshal(d.Statement, &raws); err != nil {
		var one rawStatement
		if err := json.Unmarshal(d.Statement, &one); err != nil {
			return nil, fmt.Errorf("statement is neither an object nor a list")
		}
		raws = []rawStatement{one}
	}
	out := make([]statement, 0, len(raws))
	for _, r := range raws {
		if r.Effect != "Allow" && r.Effect != "Deny" {
			return nil, fmt.Errorf("statement %q: effect %q", r.Sid, r.Effect)
		}
		out = append(out, statement{
			Sid: r.Sid, Allow: r.Effect == "Allow",
			Action: strOrList(r.Action), NotAction: strOrList(r.NotAction),
			Resource: strOrList(r.Resource), NotResource: strOrList(r.NotResource),
			Conditional: len(r.Condition) > 0 && string(r.Condition) != "{}" && string(r.Condition) != "null",
			Source:      source, Label: label,
		})
	}
	return out, nil
}

// action returns the entry that makes the statement cover an action, or "" if it
// does not. Action names are case-insensitive in IAM.
func (s statement) action(a string) (string, bool) {
	if len(s.NotAction) > 0 {
		for _, p := range s.NotAction {
			if wildMatch(p, a, true) {
				return "", false
			}
		}
		return "NotAction", true
	}
	for _, p := range s.Action {
		if wildMatch(p, a, true) {
			return p, true
		}
	}
	return "", false
}

// resource returns the entry that makes the statement cover an ARN. ARNs are
// case-sensitive.
func (s statement) resource(arn string) (string, bool) {
	if len(s.NotResource) > 0 {
		for _, p := range s.NotResource {
			if wildMatch(p, arn, false) {
				return "", false
			}
		}
		return "NotResource", true
	}
	for _, p := range s.Resource {
		if wildMatch(p, arn, false) {
			return p, true
		}
	}
	return "", false
}

// wildMatch matches IAM's wildcards: * is any run of characters, ? exactly one.
func wildMatch(pattern, s string, fold bool) bool {
	if fold {
		pattern, s = strings.ToLower(pattern), strings.ToLower(s)
	}
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// wideAction is an action entry that grants a whole service — "*", "sqs:*",
// or everything but a list. It permits every verb and states none.
func wideAction(entry string) bool {
	if entry == "*" || entry == "NotAction" {
		return true
	}
	_, verb, ok := strings.Cut(entry, ":")
	return ok && verb == "*"
}

// nameSegment is the part of a resource ARN pattern that names the resource:
// the queue name, the table name, the function name. "*" there, as much as
// Resource "*", grants every resource of the type.
func nameSegment(entry string) string {
	if entry == "*" || entry == "NotResource" {
		return "*"
	}
	p := strings.SplitN(entry, ":", 6)
	if len(p) < 6 {
		return "*"
	}
	res := p[5]
	switch p[2] {
	case "dynamodb":
		rest, ok := strings.CutPrefix(res, "table/")
		if !ok {
			return "*"
		}
		name, _, _ := strings.Cut(rest, "/")
		return name
	case "lambda":
		rest, ok := strings.CutPrefix(res, "function:")
		if !ok {
			return "*"
		}
		name, _, _ := strings.Cut(rest, ":")
		return name
	case "rds", "secretsmanager":
		// cluster:<id>, db:<id>, secret:<name>
		_, name, ok := strings.Cut(res, ":")
		if !ok {
			return "*"
		}
		return name
	}
	return res
}
