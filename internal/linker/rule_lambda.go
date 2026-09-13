package linker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// eventSourceMappingRule: a queue or stream triggers its consumer.
//
// The mapping's state decides the edge's status. A disabled mapping is still an
// edge — the relationship exists and a rebuild may want it — but carries no flow.
// A mapping caught mid-update (enabled unknown) is kept and marked unsettled.
type eventSourceMappingRule struct{}

func (eventSourceMappingRule) Name() string { return "lambda.event-source-mapping" }
func (eventSourceMappingRule) Tier() int    { return 1 }

func (eventSourceMappingRule) Apply(c *Context) {
	Each(c, spec.TypeEventSourceMapping, func(r inventory.Resource, s *spec.EventSourceMapping) {
		if s.FunctionID == "" {
			c.Unresolved(r.ID, s.FunctionARN, "mapping targets a function ARN that could not be parsed")
			return
		}
		status := Active
		switch {
		case s.Enabled == nil:
			status = Unsettled
		case !*s.Enabled:
			status = Disabled
		}
		fn := strings.TrimPrefix(s.FunctionID, "lambda/")
		if s.Qualifier != "" {
			fn += ":" + s.Qualifier
		}
		source := "lambda:ListEventSourceMappings " + s.UUID
		detail := fmt.Sprintf("%s %s → %s, state %s", s.SourceType, lastSegment(s.Source.ARN), fn, s.State)
		if s.BatchSize > 0 {
			detail += fmt.Sprintf(", batch %d", s.BatchSize)
		}
		if src, ok := c.Local(s.FunctionID, &s.Source, "event source "+s.SourceType); ok {
			c.Edge(src, s.FunctionID, KindConsume, Certain, status, source, detail)
		}
		if s.OnFailure != nil {
			if dst, ok := c.Local(s.FunctionID, s.OnFailure, "on-failure destination"); ok {
				c.Edge(s.FunctionID, dst, KindPublish, Certain, status, source,
					fmt.Sprintf("records that fail %s go to %s", fn, lastSegment(s.OnFailure.ARN)))
			}
		}
	})
}

// deadLetterRule: failed asynchronous invocations go to a queue or topic.
type deadLetterRule struct{}

func (deadLetterRule) Name() string { return "lambda.dead-letter" }
func (deadLetterRule) Tier() int    { return 1 }

func (deadLetterRule) Apply(c *Context) {
	Each(c, spec.TypeLambdaFunction, func(r inventory.Resource, s *spec.LambdaFunction) {
		if s.DeadLetter == nil {
			return
		}
		if dst, ok := c.Local(r.ID, s.DeadLetter, "dead-letter target"); ok {
			c.Edge(r.ID, dst, KindPublish, Certain, Active, "lambda:ListFunctions "+s.FunctionName,
				"DeadLetterConfig: failed asynchronous invocations go to "+lastSegment(s.DeadLetter.ARN))
		}
	})
}

// lambdaResourcePolicyRule reads who may invoke a function.
//
// The design listed it as creating caller → function edges. It cannot: a
// resource policy grants permission, not use, and stale permissions are common
// (deployment tools rarely remove them). So it only corroborates an edge another
// rule found, and reports a permission no route uses as a stale-permission
// finding. A caller outside the inventory — S3, SNS, EventBridge, an API in
// another region — makes the function an entrypoint, with the reason.
type lambdaResourcePolicyRule struct{}

func (lambdaResourcePolicyRule) Name() string { return "lambda.resource-policy" }
func (lambdaResourcePolicyRule) Tier() int    { return 1 }

type policyDoc struct {
	Statement []struct {
		Sid       string
		Effect    string
		Principal json.RawMessage
		Action    json.RawMessage
		Condition map[string]map[string]json.RawMessage
	}
}

func (lambdaResourcePolicyRule) Apply(c *Context) {
	Each(c, spec.TypeLambdaFunction, func(r inventory.Resource, s *spec.LambdaFunction) {
		if len(s.ResourcePolicy) == 0 {
			return
		}
		var doc policyDoc
		if err := json.Unmarshal(s.ResourcePolicy, &doc); err != nil {
			c.b.warnings = append(c.b.warnings, fmt.Sprintf("%s: %s resource policy: %v", c.rule, r.ID, err))
			return
		}
		for _, st := range doc.Statement {
			if st.Effect != "Allow" {
				continue
			}
			actions := strOrList(st.Action)
			source := fmt.Sprintf("lambda:GetPolicy %s %s", s.FunctionName, st.Sid)
			if contains(actions, "lambda:InvokeFunctionUrl") {
				c.Trigger(r.ID, "has a Function URL (resource policy allows lambda:InvokeFunctionUrl)")
			}
			if !contains(actions, "lambda:InvokeFunction") && !contains(actions, "lambda:*") && !contains(actions, "*") {
				continue
			}
			services, principals := principalsOf(st.Principal)
			for _, p := range principals {
				if p == "*" {
					c.Trigger(r.ID, "anyone may invoke it (resource policy principal \"*\")")
				} else if a, ok := spec.ParseARN(p); !ok || a.Account != c.inv.AccountID {
					c.Trigger(r.ID, "principal "+p+" may invoke it (resource policy)")
				}
			}
			for _, svc := range services {
				if svc != "apigateway.amazonaws.com" {
					c.Trigger(r.ID, svc+" may invoke it (resource policy)")
					continue
				}
				apiID, ok := executeAPIID(sourceArn(st.Condition), c.inv.AccountID, c.inv.Region)
				switch {
				case !ok:
					c.Trigger(r.ID, "API Gateway may invoke it (resource policy names no API in this scan)")
				case !c.HasNode("apigw/" + apiID):
					c.Trigger(r.ID, "invoked by API Gateway API "+apiID+", which is not in the inventory")
				default:
					c.Corroborate("apigw/"+apiID, r.ID, KindInvoke, source,
						fmt.Sprintf("resource policy lets API %s invoke it", apiID),
						&Finding{Kind: "stale-permission", Node: r.ID, Target: "apigw/" + apiID,
							Detail: fmt.Sprintf("resource policy lets API %s invoke this function, but no route or authorizer of that API uses it", apiID)})
				}
			}
		}
	})
}

func strOrList(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

// principalsOf splits a policy principal into service principals and everything
// else ("*" or ARNs).
func principalsOf(raw json.RawMessage) (services, others []string) {
	var star string
	if json.Unmarshal(raw, &star) == nil {
		return nil, []string{star}
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, nil
	}
	services = strOrList(m["Service"])
	others = strOrList(m["AWS"])
	return services, others
}

// sourceArn finds aws:SourceArn under any condition operator (ArnLike,
// ArnEquals, StringLike, StringEquals), case-insensitively on the key.
func sourceArn(cond map[string]map[string]json.RawMessage) string {
	for _, kv := range cond {
		for k, v := range kv {
			if strings.EqualFold(k, "aws:SourceArn") {
				if l := strOrList(v); len(l) > 0 {
					return l[0]
				}
			}
		}
	}
	return ""
}

// executeAPIID extracts the API id from arn:aws:execute-api:<r>:<acct>:<id>/...,
// only when the ARN is in the scanned account and region.
func executeAPIID(arn, account, region string) (string, bool) {
	a, ok := spec.ParseARN(arn)
	if !ok || a.Service != "execute-api" || a.Account != account || a.Region != region {
		return "", false
	}
	id, _, _ := strings.Cut(a.Resource, "/")
	return id, id != "" && !strings.ContainsAny(id, "*?")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func lastSegment(arn string) string {
	if a, ok := spec.ParseARN(arn); ok {
		r := a.Resource
		if i := strings.LastIndexAny(r, "/:"); i >= 0 {
			return r[i+1:]
		}
		return r
	}
	return arn
}
