package linker

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// A configValue is one string in a workload's configuration and where it
// lives. The location becomes the evidence — it is also what the materializer
// needs to rewrite the value to a local address — and the key is a hint about
// what a bare name refers to.
type configValue struct {
	Key    string // env var, stage variable or flag name; "" for a positional argument
	Label  string // how details name it: "QUEUE_URL", "command[3]"
	Value  string
	Source string // "ecs:DescribeTaskDefinition orders-api:41 container app env QUEUE_URL"
}

var (
	// markerRe matches the collectors' redaction markers. They are replaced
	// before scanning so a URL with its password withheld still yields its host:
	// postgres://app:<redacted:url-credentials>@orders-db… is a database
	// dependency, and the host is exactly what redaction kept on purpose.
	markerRe = regexp.MustCompile(`<redacted:[^<>]*>`)

	arnRe = regexp.MustCompile(`\barn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:[0-9]*:[^\s"',;()\[\]{}<>]+`)
	urlRe = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>,;()\[\]{}]+`)

	// awsHostRe finds AWS hostnames written without a scheme: DB_HOST=…rds…
	awsHostRe = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9.-]*\.(?:amazonaws\.com(?:\.cn)?|on\.aws|api\.aws)\b`)

	hostRe       = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9_-]*[a-z0-9])?)*$`)
	executeAPIRe = regexp.MustCompile(`^([a-z0-9]+)\.execute-api\.([a-z0-9-]+)\.amazonaws\.com$`)
)

// scanner turns configuration values into edges and findings for one link run.
type scanner struct {
	c *Context
	// names maps a table, queue, function or topic name to the nodes carrying it.
	// Names repeat across types: a table and a queue called "orders" is an
	// ordinary account, not a corner case.
	names map[string][]nameCandidate
	db    *dbIndex
}

type nameCandidate struct{ id, typ string }

var namedTypes = []string{spec.TypeDynamoDBTable, spec.TypeSQSQueue, spec.TypeLambdaFunction, spec.TypeSNSTopic}

func newScanner(c *Context) *scanner {
	s := &scanner{c: c, names: map[string][]nameCandidate{}, db: newDBIndex(c)}
	for _, r := range c.inv.Resources {
		if contains(namedTypes, r.Type) && c.HasNode(r.ID) && r.Name != "" {
			s.names[r.Name] = append(s.names[r.Name], nameCandidate{r.ID, r.Type})
		}
	}
	for _, cs := range s.names {
		sort.Slice(cs, func(i, j int) bool { return cs[i].id < cs[j].id })
	}
	return s
}

// scan links everything one value names. Patterns are tried from strongest to
// weakest: ARNs and AWS URLs name a resource outright; any other http(s) URL is
// a dependency outside the account; a bare name is only a hint.
func (s *scanner) scan(holder string, v configValue) {
	if v.Value == "" || markerRe.FindString(v.Value) == v.Value {
		return
	}
	val := markerRe.ReplaceAllString(v.Value, "REDACTED")

	for _, arn := range arnRe.FindAllString(val, -1) {
		s.arn(holder, v, arn)
	}
	for _, u := range urlRe.FindAllString(val, -1) {
		s.url(holder, v, u)
	}
	for _, h := range awsHostRe.FindAllString(urlRe.ReplaceAllString(val, " "), -1) {
		s.awsHost(holder, v, strings.ToLower(h), "")
	}
	if v.Key != "" {
		s.name(holder, v, strings.TrimSpace(val))
	}
}

func (s *scanner) arn(holder string, v configValue, arn string) {
	if s.dbSecret(holder, v, arn) {
		return
	}
	ref := &spec.TargetRef{ARN: arn, ID: spec.IDFromARN(arn)}
	if to, ok := s.c.Local(holder, ref, v.Label); ok && to != holder {
		s.c.Edge(holder, to, KindReferences, High, Active, v.Source,
			fmt.Sprintf("%s holds the ARN of %s", v.Label, to))
	}
}

func (s *scanner) url(holder string, v configValue, u string) {
	scheme, host, port := splitURL(u)
	switch {
	case scheme == "s3":
		s.c.Unresolved(holder, "s3://"+host, fmt.Sprintf("%s names an S3 bucket; cloud-echo has no S3 collector", v.Label))
	case host == "" || scheme == "file":
	case s.c.elbs().byDNS[strings.TrimPrefix(strings.ToLower(host), "dualstack.")] != "":
		// A load balancer's exact DNS name, whatever its shape (an emulator's
		// is not AWS's).
		s.elbHost(holder, v, strings.ToLower(host))
	case isAWSHost(host):
		s.awsHost(holder, v, host, u)
	case isLocalHost(host):
		// A sidecar in the same task (an X-Ray daemon, a secrets cache) or the
		// metadata endpoint: part of the workload, not a dependency of it.
	case net.ParseIP(host) == nil && !hostRe.MatchString(host):
		// Templated (https://${DOMAIN}/…) or malformed: nothing to name.
	case net.ParseIP(host) == nil && !strings.Contains(host, "."):
		// Public DNS names have a dot. A bare one is an internal name — ECS
		// Service Connect, Cloud Map, a container link — most likely one of the
		// account's own services; made external, the gateway would mock it.
		s.c.Unresolved(holder, host, fmt.Sprintf("%s calls %s, a name with no domain (Service Connect, Cloud Map or a container link); resolving it needs a collector cloud-echo does not have yet", v.Label, host))
	case scheme == "http" || scheme == "https":
		// The node comes from the host and port already validated here; a path
		// net/url rejects (a stray %) must not cost the dependency.
		base := scheme + "://" + host
		if strings.Contains(host, ":") {
			base = scheme + "://[" + host + "]"
		}
		if port != "" {
			base += ":" + port
		}
		if to, ok := s.c.External(base); ok {
			s.c.Edge(holder, to, KindHTTP, High, Active, v.Source, fmt.Sprintf("%s calls %s", v.Label, u))
		}
	default:
		target := scheme + "://" + host
		if port != "" {
			target += ":" + port
		}
		s.c.Unresolved(holder, target, fmt.Sprintf("%s names a non-HTTP endpoint (%s) outside AWS; the gateway stands in only for HTTP, so this dependency will not exist locally", v.Label, scheme))
	}
}

// awsHost resolves an AWS hostname, from a URL (u set) or written bare. AWS
// endpoints are never third parties: the gateway sends AWS traffic to Floci,
// and an ext/ node for sqs.us-east-1.amazonaws.com would be a mock of AWS
// itself. A host is resolved to a node, reported, or — bare and naming no
// resource, like a service principal — ignored.
func (s *scanner) awsHost(holder string, v configValue, host, u string) {
	if u != "" {
		if q := spec.QueueFromURL(strings.TrimRight(u, "/")); q != nil {
			if to, ok := s.c.Local(holder, q, v.Label); ok && to != holder {
				s.c.Edge(holder, to, KindReferences, High, Active, v.Source,
					fmt.Sprintf("%s holds the URL of %s", v.Label, to))
			}
			return
		}
	}
	report := func(what string) {
		s.c.Unresolved(holder, host, fmt.Sprintf("%s names %s", v.Label, what))
	}
	switch {
	case executeAPIRe.MatchString(host):
		m := executeAPIRe.FindStringSubmatch(host)
		id := "apigw/" + m[1]
		switch {
		case m[2] != s.c.inv.Region:
			report(fmt.Sprintf("the invoke URL of API %s in region %s, outside this scan", m[1], m[2]))
		case !s.c.HasNode(id):
			report(fmt.Sprintf("the invoke URL of API %s, which is not in the inventory (another account, or deleted)", m[1]))
		case id != holder:
			s.c.Edge(holder, id, KindHTTP, High, Active, v.Source,
				fmt.Sprintf("%s holds the invoke URL of %s", v.Label, id))
		}
	case strings.HasSuffix(host, ".rds.amazonaws.com"):
		s.rdsHost(holder, v, host)
	case strings.HasSuffix(host, ".cache.amazonaws.com"):
		s.cacheHost(holder, v, host)
	case isELBHost(host):
		s.elbHost(holder, v, host)
	case strings.Contains(host, ".lambda-url."):
		report("a Lambda Function URL; resolving it to a function needs lambda:ListFunctionUrlConfigs, which is not collected")
	case u != "":
		report("an AWS endpoint that is not a resource cloud-echo models")
	}
}

// name links a value that is exactly the name of a table, queue or function.
//
// This is the weakest signal Tier 2 has, so it never goes above medium, and
// ambiguity lowers it rather than being settled silently. The key name is the
// one tie-breaker: TABLE_NAME=orders, in an account with a table and a queue
// called orders, points at the table — medium, with the queue kept as a
// low-confidence candidate and the ambiguity reported. A key that names a type
// no candidate has (QUEUE_NAME=x where only a table is called x) lowers all of
// them: the queue it means may simply not be in this scan.
func (s *scanner) name(holder string, v configValue, value string) {
	var cands []nameCandidate
	for _, c := range s.names[value] {
		if c.id != holder {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return
	}
	hint := keyHint(v.Key)
	var hinted []nameCandidate
	for _, c := range cands {
		if c.typ == hint {
			hinted = append(hinted, c)
		}
	}

	winner, why := "", ""
	switch {
	case len(hinted) == 1:
		winner, why = hinted[0].id, fmt.Sprintf("the key names a %s", typeNoun(hint))
	case hint == "" && len(cands) == 1 && specificName(value):
		winner = cands[0].id
	}

	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.id
	}
	for _, c := range cands {
		conf, detail := Low, fmt.Sprintf("%s = %q, the name of %s", v.Label, value, c.id)
		switch {
		case c.id == winner:
			conf = Medium
			if why != "" {
				detail += " — " + why
			}
		case winner != "":
			detail += " — a candidate only: the key names a " + typeNoun(hint)
		case len(cands) > 1:
			detail += " — a candidate only: the name fits " + strings.Join(ids, " and ")
		case hint != "":
			detail += fmt.Sprintf(" — a candidate only: the key names a %s, and no %s has this name", typeNoun(hint), typeNoun(hint))
		default:
			detail += " — a candidate only: a single common word matches by coincidence as easily as by design"
		}
		s.c.Edge(holder, c.id, KindReferences, conf, Active, v.Source, detail)
	}

	if len(cands) > 1 {
		how := "all are low-confidence candidates to choose from"
		if winner != "" {
			how = fmt.Sprintf("the key points at %s (medium); the others are low-confidence candidates", winner)
		}
		s.c.Finding("ambiguous", holder, value, fmt.Sprintf("%s = %q names %s; %s", v.Label, value, strings.Join(ids, " and "), how))
	}
}

// keyHint reads what type a configuration key says its value is. Only an
// unambiguous hint counts: QUEUE_TABLE names two types and hints neither.
func keyHint(key string) string {
	found := map[string]bool{}
	for _, t := range tokens(key) {
		switch t {
		case "TABLE", "TABLES", "DDB", "DYNAMO", "DYNAMODB":
			found[spec.TypeDynamoDBTable] = true
		case "QUEUE", "QUEUES", "SQS", "DLQ":
			found[spec.TypeSQSQueue] = true
		case "FUNCTION", "FUNC", "FN", "LAMBDA":
			found[spec.TypeLambdaFunction] = true
		case "TOPIC", "TOPICS", "SNS":
			found[spec.TypeSNSTopic] = true
		}
	}
	if len(found) != 1 {
		return ""
	}
	for t := range found {
		return t
	}
	return ""
}

func typeNoun(typ string) string {
	switch typ {
	case spec.TypeDynamoDBTable:
		return "table"
	case spec.TypeSQSQueue:
		return "queue"
	case spec.TypeLambdaFunction:
		return "function"
	case spec.TypeSNSTopic:
		return "topic"
	}
	return typ
}

// specificName is a name unlikely to occur by coincidence: it has a separator
// or a digit. "orders-audit" names something; "orders", "info" or "jobs" could
// be any value.
func specificName(v string) bool {
	return len(v) >= 4 && strings.ContainsAny(v, "-_.0123456789")
}

// tokens splits snake, kebab, dotted and camelCase names into upper-case words.
func tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToUpper(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// splitURL returns a URL's scheme, host and port without net/url, which rejects
// values real configuration carries (a redacted password, a templated path).
func splitURL(u string) (scheme, host, port string) {
	i := strings.Index(u, "://")
	scheme, rest := strings.ToLower(u[:i]), u[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if strings.HasPrefix(rest, "[") {
		if j := strings.Index(rest, "]"); j > 0 {
			return scheme, strings.ToLower(rest[1:j]), strings.TrimPrefix(rest[j+1:], ":")
		}
	}
	host, port, _ = strings.Cut(rest, ":")
	return scheme, strings.ToLower(strings.TrimSuffix(host, ".")), port
}

func isAWSHost(h string) bool {
	for _, suffix := range []string{".amazonaws.com", ".amazonaws.com.cn", ".on.aws", ".api.aws"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

func isLocalHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}
