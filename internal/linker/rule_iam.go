package linker

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

const ruleIAM = "iam.policy-resource"

// iamPolicyRule reads what each workload's role permits on the queues, tables
// and functions in the inventory, and turns it into intent: publish, consume,
// read, write, invoke.
//
// A permission is not a use. Deployment tools grant generously and rarely take
// grants back, so a role alone never makes an edge more than medium — the same
// reasoning that keeps Lambda resource policies to corroboration. What Tier 3
// adds is the verb: when the configuration names a table (Tier 2) and the role
// may write it, two independent sources agree, and the edge is high.
//
// Evaluation follows IAM's: an Allow in the role's policies, no unconditional
// explicit Deny, and — if the role has a permissions boundary — an Allow there
// too. Grants that reach every resource of a service (Resource "*", table/*)
// draw nothing; they would link a role to the whole account.
type iamPolicyRule struct{}

func (iamPolicyRule) Name() string { return ruleIAM }
func (iamPolicyRule) Tier() int    { return 3 }

var serviceOfType = map[string]string{
	spec.TypeSQSQueue: "sqs", spec.TypeDynamoDBTable: "dynamodb", spec.TypeLambdaFunction: "lambda",
	spec.TypeRDSInstance: "rds", spec.TypeRDSCluster: "rds",
}

// actionClass is one intent and the actions that state it.
type actionClass struct {
	kind Kind
	// reverse edges point from the target to the holder: a queue or stream
	// triggers the workload that receives from it.
	reverse bool
	actions []string
	arns    func(t *iamTarget, action string) []string
}

var (
	ownARN = func(t *iamTarget, _ string) []string { return []string{t.arn} }
	// Only Query and Scan act on an index; GetItem on one is not an action.
	readARNs = func(t *iamTarget, action string) []string {
		if action == "dynamodb:Query" || action == "dynamodb:Scan" {
			return append([]string{t.arn}, t.indexARNs...)
		}
		return []string{t.arn}
	}
	stream = func(t *iamTarget, _ string) []string {
		if t.streamARN == "" {
			return nil
		}
		return []string{t.streamARN}
	}
	// A grant on fn:* covers qualified invocations only; either way it names
	// the function.
	functionARNs = func(t *iamTarget, _ string) []string { return []string{t.arn, t.arn + ":$LATEST"} }
	masterSecret = func(t *iamTarget, _ string) []string {
		if t.secretARN == "" {
			return nil
		}
		return []string{t.secretARN}
	}

	// Reading a database's managed master secret is how a workload gets its
	// password: it connects. IAM database authentication (rds-db:connect)
	// names a resource id and a database user instead of an ARN and is not
	// read yet — see docs/03-linker.md.
	readsDBSecret = actionClass{kind: KindConnect, actions: []string{"secretsmanager:GetSecretValue"}, arns: masterSecret}
)

// actionClasses are the verbs Tier 3 reads, by target type. Metadata calls
// (GetQueueAttributes, DescribeTable) state no intent and are not read.
var actionClasses = map[string][]actionClass{
	spec.TypeSQSQueue: {
		{kind: KindPublish, actions: []string{"sqs:SendMessage"}, arns: ownARN},
		{kind: KindConsume, reverse: true, actions: []string{"sqs:ReceiveMessage"}, arns: ownARN},
	},
	spec.TypeDynamoDBTable: {
		{kind: KindRead, actions: []string{"dynamodb:GetItem", "dynamodb:BatchGetItem", "dynamodb:Query", "dynamodb:Scan", "dynamodb:PartiQLSelect"}, arns: readARNs},
		{kind: KindWrite, actions: []string{"dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:BatchWriteItem",
			"dynamodb:PartiQLInsert", "dynamodb:PartiQLUpdate", "dynamodb:PartiQLDelete"}, arns: ownARN},
		{kind: KindConsume, reverse: true, actions: []string{"dynamodb:GetRecords"}, arns: stream},
	},
	spec.TypeLambdaFunction: {
		{kind: KindInvoke, actions: []string{"lambda:InvokeFunction"}, arns: functionARNs},
	},
	// The Data API runs SQL over HTTPS against a cluster, by its ARN.
	spec.TypeRDSCluster: {
		{kind: KindConnect, actions: []string{"rds-data:ExecuteStatement", "rds-data:BatchExecuteStatement"}, arns: ownARN},
		readsDBSecret,
	},
	spec.TypeRDSInstance: {readsDBSecret},
}

type iamTarget struct {
	id, typ, arn string
	indexARNs    []string
	streamARN    string
	secretARN    string // a database's managed master secret
	// policy is the target's own resource policy (queue policy, function
	// policy): it may grant a role what the role's policies do not.
	policy json.RawMessage
}

// roleView is a role with every document that shapes it, parsed.
type roleView struct {
	id, name, arn  string
	identity       []statement
	boundary       []statement
	hasBoundary    bool
	boundaryLabel  string
	boundaryUnread string
	unread         []string
}

// grant is one statement allowing one action on one target ARN, and the
// entries that made it match.
type grant struct {
	st             *statement
	action, entry  string // the Action and Resource entries that matched
	granted, actOn string // the catalog action and the ARN it was checked on
}

func (iamPolicyRule) Apply(c *Context) {
	targets := iamTargets(c)
	mappings := map[[2]string]bool{} // source → function, from event source mappings
	Each(c, spec.TypeEventSourceMapping, func(_ inventory.Resource, m *spec.EventSourceMapping) {
		mappings[[2]string{m.Source.ID, m.FunctionID}] = true
	})
	servicesOf := map[string][]string{} // task definition → services running it
	Each(c, spec.TypeECSService, func(r inventory.Resource, s *spec.ECSService) {
		servicesOf[s.TaskDefinitionID] = append(servicesOf[s.TaskDefinitionID], r.ID)
	})

	roles := map[string]bool{}
	Each(c, spec.TypeIAMRole, func(r inventory.Resource, role *spec.IAMRole) {
		roles[r.ID] = true
		rv := readRole(c, r, role)
		for _, ref := range role.AssumedBy {
			holders := servicesOf[ref]
			if !strings.HasPrefix(ref, "ecs/taskdef/") && c.HasNode(ref) {
				holders = []string{ref}
			}
			for _, h := range holders {
				evaluate(c, h, rv, targets, mappings)
			}
		}
	})

	// A workload whose role is not in the inventory — deleted, or unreadable
	// by the scan — had nothing evaluated. Say so, rather than let an absence
	// of Tier-3 edges read as "its role grants nothing".
	missing := func(holder, role string) {
		if role != "" && !roles[role] {
			c.Finding("unscanned", holder, role, "role not in the inventory: its permissions were not evaluated")
		}
	}
	Each(c, spec.TypeLambdaFunction, func(r inventory.Resource, fn *spec.LambdaFunction) { missing(r.ID, fn.RoleID) })
	Each(c, spec.TypeECSService, func(r inventory.Resource, s *spec.ECSService) {
		if td, ok := lookup[spec.ECSTaskDefinition](c, s.TaskDefinitionID); ok {
			missing(r.ID, td.TaskRoleID)
		}
	})
}

// iamTargets lists the nodes Tier 3 can say something about, with the ARNs
// each class of action is checked against. Hand-written inventories may omit
// ARNs; they are rebuilt from the name the way AWS forms them.
func iamTargets(c *Context) []*iamTarget {
	var out []*iamTarget
	acct, region := c.inv.AccountID, c.inv.Region
	for _, r := range c.inv.Resources {
		if serviceOfType[r.Type] == "" || !c.HasNode(r.ID) {
			continue
		}
		t := &iamTarget{id: r.ID, typ: r.Type, arn: r.ARN}
		switch r.Type {
		case spec.TypeSQSQueue:
			if t.arn == "" {
				t.arn = fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, acct, r.Name)
			}
			var q spec.SQSQueue
			if json.Unmarshal(r.Spec, &q) == nil {
				t.policy = q.Policy
			}
		case spec.TypeDynamoDBTable:
			if t.arn == "" {
				t.arn = fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", region, acct, r.Name)
			}
			var tb spec.DynamoDBTable
			if json.Unmarshal(r.Spec, &tb) == nil {
				for _, ix := range append(append([]spec.Index{}, tb.GSIs...), tb.LSIs...) {
					t.indexARNs = append(t.indexARNs, t.arn+"/index/"+ix.Name)
				}
				if tb.Stream != nil && tb.Stream.Enabled && tb.Stream.ARN != "" {
					t.streamARN = tb.Stream.ARN
				}
			}
		case spec.TypeRDSCluster:
			if t.arn == "" {
				t.arn = fmt.Sprintf("arn:aws:rds:%s:%s:cluster:%s", region, acct, r.Name)
			}
			var cl spec.RDSCluster
			if json.Unmarshal(r.Spec, &cl) == nil {
				t.secretARN = cl.MasterSecretARN
			}
		case spec.TypeRDSInstance:
			if t.arn == "" {
				t.arn = fmt.Sprintf("arn:aws:rds:%s:%s:db:%s", region, acct, r.Name)
			}
			var in spec.RDSInstance
			if json.Unmarshal(r.Spec, &in) == nil {
				t.secretARN = in.MasterSecretARN
			}
		case spec.TypeLambdaFunction:
			if t.arn == "" {
				t.arn = fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", region, acct, r.Name)
			}
			var fn spec.LambdaFunction
			if json.Unmarshal(r.Spec, &fn) == nil {
				t.policy = fn.ResourcePolicy
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// readRole parses every document that shapes a role. What cannot be read is
// recorded, never guessed.
func readRole(c *Context, r inventory.Resource, role *spec.IAMRole) *roleView {
	rv := &roleView{id: r.ID, name: role.RoleName, arn: r.ARN, unread: append([]string(nil), role.Unread...)}
	if rv.arn == "" {
		rv.arn = fmt.Sprintf("arn:aws:iam::%s:role%s%s", c.inv.AccountID, pathOr(role.Path), role.RoleName)
	}
	add := func(doc json.RawMessage, source, label string, into *[]statement) bool {
		sts, err := parseStatements(doc, source, label)
		if err != nil {
			c.b.warnings = append(c.b.warnings, fmt.Sprintf("%s: %s %s: %v", c.rule, r.ID, label, err))
			return false
		}
		*into = append(*into, sts...)
		return true
	}
	for _, p := range role.InlinePolicies {
		if !add(p.Document, "iam:GetRolePolicy "+role.RoleName+" "+p.Name, "inline policy "+p.Name, &rv.identity) {
			rv.unread = append(rv.unread, "inline policy "+p.Name)
		}
	}
	for _, a := range role.AttachedPolicies {
		label := "attached policy " + lastSegment(a.ARN)
		pol, ok := lookup[spec.IAMPolicy](c, a.ID)
		if !ok {
			rv.unread = append(rv.unread, label)
			continue
		}
		if pol.AWSManaged {
			label = "AWS managed policy " + pol.PolicyName
		} else {
			label = "managed policy " + pol.PolicyName
		}
		if !add(pol.Document, "iam:GetPolicyVersion "+pol.PolicyName+" "+pol.DefaultVersion, label, &rv.identity) {
			rv.unread = append(rv.unread, label)
		}
	}
	if b := role.PermissionsBoundary; b != nil {
		rv.hasBoundary = true
		label := "permissions boundary " + lastSegment(b.ARN)
		rv.boundaryLabel = label
		pol, ok := lookup[spec.IAMPolicy](c, b.ID)
		if !ok || !add(pol.Document, "iam:GetPolicyVersion "+pol.PolicyName+" "+pol.DefaultVersion, label, &rv.boundary) {
			rv.boundaryUnread = label
		}
	}
	return rv
}

func pathOr(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// cancelled says why an allowed action is not effective: an unconditional
// explicit Deny, or a boundary that does not allow it. A boundary that could
// not be read cancels nothing — it lowers confidence instead (see evaluate).
func (rv *roleView) cancelled(action, arn string) string {
	for _, st := range rv.identity {
		if !st.Allow && !st.Conditional {
			if _, ok := st.action(action); ok {
				if _, ok := st.resource(arn); ok {
					return "an explicit Deny in " + st.Label + " cancels it"
				}
			}
		}
	}
	if !rv.hasBoundary || rv.boundaryUnread != "" {
		return ""
	}
	allowed := false
	for _, st := range rv.boundary {
		_, aok := st.action(action)
		_, rok := st.resource(arn)
		if !aok || !rok {
			continue
		}
		if !st.Allow && !st.Conditional {
			return "the " + st.Label + " denies it"
		}
		allowed = allowed || st.Allow
	}
	if !allowed {
		return "the " + rv.boundaryLabel + " does not allow it"
	}
	return ""
}

// evaluate turns what one role allows into edges and findings for one holder.
func evaluate(c *Context, holder string, rv *roleView, targets []*iamTarget, mappings map[[2]string]bool) {
	api := strings.HasPrefix(holder, "apigw/")
	ev := &iamEval{role: rv.name, complete: len(rv.unread) == 0 && rv.boundaryUnread == "", permitted: map[string]bool{}}
	if !api {
		// An API's behaviour is its routes, which Tier 1 reads in full; the
		// role it assumes only corroborates them, and says nothing about what
		// its configuration names.
		c.b.evals[holder] = ev
	}
	if len(rv.unread) > 0 {
		c.Finding("unscanned", holder, rv.id, fmt.Sprintf("role %s: could not read %s; its permissions were evaluated without them",
			rv.name, strings.Join(rv.unread, ", ")))
	}
	if rv.boundaryUnread != "" {
		c.Finding("unscanned", holder, rv.id, fmt.Sprintf("role %s: its %s could not be read, so nothing the role allows can be confirmed; its edges are low",
			rv.name, rv.boundaryUnread))
	}

	broadVia := map[string]grant{}
	for _, t := range targets {
		type classResult struct {
			cl       actionClass
			specific []grant // grants that name this resource, or a pattern it is one of
		}
		var results []classResult
		permitted := false
		for _, cl := range actionClasses[t.typ] {
			var allowed []grant
			blocked := map[string][]grant{}
			for _, a := range cl.actions {
				for _, arn := range cl.arns(t, a) {
					var here []grant
					for i := range rv.identity {
						st := &rv.identity[i]
						if !st.Allow {
							continue
						}
						ae, aok := st.action(a)
						re, rok := st.resource(arn)
						if aok && rok {
							here = append(here, grant{st: st, action: ae, entry: re, granted: a, actOn: arn})
						}
					}
					if len(here) == 0 {
						continue
					}
					if why := rv.cancelled(a, arn); why != "" {
						blocked[why] = append(blocked[why], here...)
						continue
					}
					allowed = append(allowed, here...)
				}
			}
			if len(allowed) > 0 {
				permitted = true
			}
			var specific []grant
			for _, g := range allowed {
				if nameSegment(g.entry) == "*" {
					// Named by the grant's service, not the target's: a
					// GetSecretValue on * reaches every secret, not every database.
					svc, _, _ := strings.Cut(strings.ToLower(g.granted), ":")
					if _, seen := broadVia[svc]; !seen {
						broadVia[svc] = g
					}
					continue
				}
				specific = append(specific, g)
			}
			if len(specific) > 0 {
				results = append(results, classResult{cl, specific})
			}
			if !api {
				reportBlocked(c, holder, t, blocked)
			}
		}
		if permitted || resourcePolicyMayGrant(t.policy, rv.arn) {
			ev.permitted[t.id] = true
		}
		if len(results) == 0 {
			continue
		}

		// Intent comes from actions a statement names. sqs:* on a queue
		// permits sending and receiving and says which neither; drawing both
		// would invent a consumer. Such a grant is a reference.
		var stated []classResult
		for _, r := range results {
			for _, g := range r.specific {
				if !wideAction(g.action) {
					stated = append(stated, r)
					break
				}
			}
		}
		if len(stated) == 0 {
			var all []grant
			for _, r := range results {
				all = append(all, r.specific...)
			}
			claim(c, holder, t, actionClass{kind: KindReferences}, all, rv, targets, api, false)
			continue
		}
		for _, r := range stated {
			// Lambda polls a queue or a stream for its event source mapping,
			// with the function's own role. Without a mapping, a function that
			// may receive is most likely holding a leftover grant.
			lambdaPoll := r.cl.reverse && strings.HasPrefix(holder, "lambda/") && !mappings[[2]string{t.id, holder}]
			claim(c, holder, t, r.cl, r.specific, rv, targets, api, lambdaPoll)
		}
	}

	if api {
		return
	}
	reportOutside(c, holder, rv)
	svcs := make([]string, 0, len(broadVia))
	for s := range broadVia {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	for _, s := range svcs {
		g := broadVia[s]
		c.Finding("broad-access", holder, s, fmt.Sprintf(
			"role %s may use every %s resource (%s on %s, %s): a grant this wide links to nothing in particular, so it draws no edge",
			rv.name, s, g.action, g.entry, g.st.Label))
	}
}

// claim records one intent of one holder on one target, with a piece of
// evidence per statement that grants it.
func claim(c *Context, holder string, t *iamTarget, cl actionClass, grants []grant, rv *roleView,
	targets []*iamTarget, api, lambdaPoll bool) {
	conf, why := Low, ""
	for _, g := range grants {
		if !strings.ContainsAny(nameSegment(g.entry), "*?") || matchCount(g.entry, t.typ, targets) == 1 {
			conf = Medium
			break
		}
	}
	if conf == Low {
		why = " — a pattern that matches several resources"
	}
	if rv.boundaryUnread != "" {
		conf, why = Low, " — the permissions boundary could not be read"
	}
	if lambdaPoll {
		conf, why = Low, " — a function receives through an event source mapping, and none links these two"
	}

	from, to := holder, t.id
	if cl.reverse {
		from, to = t.id, holder
	}
	type stmtKey struct {
		st    *statement
		entry string
	}
	actions := map[stmtKey][]string{}
	var order []stmtKey
	for _, g := range grants {
		k := stmtKey{g.st, g.entry}
		if _, ok := actions[k]; !ok {
			order = append(order, k)
		}
		if !contains(actions[k], g.granted) {
			actions[k] = append(actions[k], g.granted)
		}
	}
	for _, k := range order {
		detail := fmt.Sprintf("role %s: Allow %s on %s (%s)", rv.name, strings.Join(actions[k], ", "), k.entry, k.st.Label)
		if k.st.Conditional {
			detail += ", under a condition"
		}
		if cl.kind == KindReferences {
			detail += " — every action, so no intent"
		}
		detail += why
		if api {
			c.Corroborate(from, to, cl.kind, k.st.Source, detail, nil)
		} else {
			c.Permit(from, to, cl.kind, conf, k.st.Source, detail)
		}
	}
}

// reportOutside records grants naming one exact queue, table or function that
// is not a node: in another account or region — the namesake trap, in a policy —
// or deleted. Grants on services without nodes (logs, X-Ray, KMS) are the
// infrastructure every role carries, and are not reported.
func reportOutside(c *Context, holder string, rv *roleView) {
	typeOf := map[string]string{"sqs": spec.TypeSQSQueue, "dynamodb": spec.TypeDynamoDBTable, "lambda": spec.TypeLambdaFunction,
		"rds": spec.TypeRDSCluster}
	for _, st := range rv.identity {
		if !st.Allow {
			continue
		}
		for _, entry := range st.Resource {
			a, ok := spec.ParseARN(entry)
			typ := typeOf[a.Service]
			if !ok || typ == "" || strings.ContainsAny(entry, "*?") {
				continue
			}
			var actions []string
			for _, cl := range actionClasses[typ] {
				for _, act := range cl.actions {
					if _, ok := st.action(act); ok {
						actions = append(actions, act)
					}
				}
			}
			ref := &spec.TargetRef{ARN: entry, ID: spec.IDFromARN(entry)}
			if len(actions) == 0 || ref.ID == "" {
				continue
			}
			what := fmt.Sprintf("role %s allows %s (%s)", rv.name, strings.Join(actions, ", "), st.Label)
			if id, ok := c.Local(holder, ref, what); ok && !c.HasNode(id) {
				c.Unresolved(holder, entry, what+": the target is not in the inventory")
			}
		}
	}
}

// matchCount is how many targets of one type a resource pattern reaches.
func matchCount(entry, typ string, targets []*iamTarget) int {
	n := 0
	for _, t := range targets {
		if t.typ != typ {
			continue
		}
		arns := append([]string{t.arn, t.arn + ":$LATEST", t.streamARN, t.secretARN}, t.indexARNs...)
		for _, a := range arns {
			if a != "" && wildMatch(entry, a, false) {
				n++
				break
			}
		}
	}
	return n
}

// reportBlocked records grants a Deny or the boundary cancels, so that the
// missing edge can be explained. Only grants that name both the action and the
// resource count: "dynamodb:* except Delete*" and a boundary trimming a sqs:*
// on * are guardrails doing their job, not a grant that can never work.
func reportBlocked(c *Context, holder string, t *iamTarget, blocked map[string][]grant) {
	whys := make([]string, 0, len(blocked))
	for w := range blocked {
		whys = append(whys, w)
	}
	sort.Strings(whys)
	for _, why := range whys {
		var actions, labels []string
		for _, g := range blocked[why] {
			if nameSegment(g.entry) == "*" || wideAction(g.action) {
				continue
			}
			if !contains(actions, g.granted) {
				actions = append(actions, g.granted)
			}
			if !contains(labels, g.st.Label) {
				labels = append(labels, g.st.Label)
			}
		}
		if len(actions) == 0 {
			continue
		}
		c.Finding("blocked", holder, t.id, fmt.Sprintf("its role allows %s on it (%s), but %s: no edge",
			strings.Join(actions, ", "), strings.Join(labels, ", "), why))
	}
}

// resourcePolicyMayGrant reports whether a queue or function policy allows
// the role (or anyone) something. Coarse on purpose: it only stops Tier 3 from
// calling a reference unpermitted, never draws an edge.
func resourcePolicyMayGrant(raw json.RawMessage, roleARN string) bool {
	if len(raw) == 0 {
		return false
	}
	var doc policyDoc
	if json.Unmarshal(raw, &doc) != nil {
		return true // unreadable: assume it might
	}
	for _, st := range doc.Statement {
		if st.Effect != "Allow" {
			continue
		}
		_, principals := principalsOf(st.Principal)
		for _, p := range principals {
			if p == "*" || p == roleARN {
				return true
			}
		}
	}
	return false
}
