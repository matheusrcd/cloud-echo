package discovery

import (
	"math"
	"regexp"
	"strings"
	"unicode"
)

// Secret-shaped value redaction.
//
// Configuration values are the linker's richest Tier-2 signal — a queue URL, a
// table name, a database hostname in an env var is how cloud-echo learns what a
// workload talks to. The same env vars are also where people put plaintext
// secrets, Lambda especially. Copying them verbatim into inventory.json would move
// secret values out of AWS onto a laptop, which docs/07-security.md Guarantee 2
// says never happens.
//
// So values are redacted at collection time, before they reach Spec, Raw, or
// disk. The rules are ordered to keep linking signal wherever it is separable
// from the secret:
//
//   - an ARN is a reference, never a secret, even under SECRET_ARN;
//   - a URL with embedded credentials loses the password and keeps the host,
//     because the host is exactly what links a service to its database;
//   - a URL without credentials is kept even under a suspicious key name, since
//     AUTH_URL=https://auth.example.com is an integration, not a secret.
//
// Where the two cannot be separated, redaction wins. A false positive costs one
// linking hint the user can restore by hand; a false negative leaks a credential,
// and that cannot be taken back. This is a heuristic and will miss things — the
// inventory stays gitignored and sensitive regardless.

const redactedMarker = "<redacted:%s>"

func marker(reason string) string {
	return strings.Replace(redactedMarker, "%s", reason, 1)
}

// isMarker reports whether a value is already a redaction marker. Redaction must
// be idempotent: scanning an environment seeded from a redacted inventory — a
// local Floci, in the M1 round trip — re-redacted two of three markers and
// counted them as new redactions, so the "redacted" list stopped meaning "what
// cloud-echo refused to copy out of AWS".
func isMarker(v string) bool {
	return strings.HasPrefix(v, "<redacted:") && strings.HasSuffix(v, ">") && !strings.ContainsAny(v[1:len(v)-1], "<>")
}

var (
	arnPattern = regexp.MustCompile(`^arn:aws[a-z-]*:`)

	credentialPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),                                // AWS access key id
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),                         // PEM
		regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT
		regexp.MustCompile(`\b(ghp|gho|ghs|ghu|ghr)_[A-Za-z0-9]{20,}`),                   // GitHub
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),                             // GitHub fine-grained
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),                                 // GitLab
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),                             // Slack
		regexp.MustCompile(`\b(sk|rk)_(live|test)_[A-Za-z0-9]{10,}`),                     // Stripe
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),                                  // Google API key
		regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),              // Authorization header value
		regexp.MustCompile(`(?i)(^|[;&\s])(password|pwd|passwd)\s*=\s*[^;&\s]+`),         // connection string
	}

	// Single tokens that mark a key as secret-bearing. Matching is on whole
	// tokens, so PASS matches DB_PASS but not BYPASS or PASSTHROUGH.
	secretTokens = map[string]bool{
		"PASSWORD": true, "PASSWD": true, "PASS": true, "PASSPHRASE": true,
		"SECRET": true, "SECRETS": true,
		"TOKEN": true, "TOKENS": true,
		"CREDENTIAL": true, "CREDENTIALS": true, "CREDS": true,
		"APIKEY": true, "PRIVATEKEY": true, "AUTHORIZATION": true,
	}

	// KEY is only secret-bearing after one of these; PARTITION_KEY and
	// SORT_KEY are schema names, not secrets.
	keyQualifiers = map[string]bool{
		"API": true, "ACCESS": true, "SECRET": true, "PRIVATE": true, "AUTH": true,
		"SIGNING": true, "ENCRYPTION": true, "MASTER": true, "CLIENT": true, "LICENSE": true,
	}
)

// redactValue decides whether an env var value can leave AWS, returning the value
// to store and whether anything was removed.
func redactValue(key, value string) (string, bool) {
	if value == "" || arnPattern.MatchString(value) || isMarker(value) {
		return value, false
	}
	// URLs are sanitized component by component rather than all-or-nothing:
	// the host is the linking signal, and it is never the secret.
	if looksLikeURL(value) {
		return sanitizeURL(value)
	}
	if credentialPattern(value) {
		return marker("credential-pattern"), true
	}
	if secretKeyName(key) {
		return marker("key-name"), true
	}
	if highEntropy(value) {
		return marker("high-entropy"), true
	}
	return value, false
}

// redactArgs applies the same rules to a command line, which is the other place
// secrets hide: --db-password=hunter2, or --token followed by the value.
func redactArgs(args []string) ([]string, []int) {
	if len(args) == 0 {
		return args, nil
	}
	out := make([]string, len(args))
	var hit []int
	for i, a := range args {
		out[i] = a

		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "-") {
			if r, red := redactValue(strings.TrimLeft(k, "-"), v); red {
				out[i] = k + "=" + r
				hit = append(hit, i)
			}
			continue
		}
		if i > 0 && strings.HasPrefix(args[i-1], "-") && !strings.Contains(args[i-1], "=") &&
			secretKeyName(strings.TrimLeft(args[i-1], "-")) && !strings.HasPrefix(a, "-") && !isMarker(a) {
			if !arnPattern.MatchString(a) && !looksLikeURL(a) {
				out[i] = marker("key-name")
				hit = append(hit, i)
				continue
			}
		}
		if r, red := redactValue("", a); red {
			out[i] = r
			hit = append(hit, i)
		}
	}
	return out, hit
}

// secretKeyName tokenizes snake, kebab, dotted and camelCase names.
func secretKeyName(key string) bool {
	toks := tokenize(key)
	for i, t := range toks {
		if secretTokens[t] {
			return true
		}
		if t == "KEY" && i > 0 && keyQualifiers[toks[i-1]] {
			return true
		}
	}
	return false
}

func tokenize(s string) []string {
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, strings.ToUpper(string(cur)))
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
	return toks
}

// sanitizeURL removes secrets from a URL while keeping its scheme, host and port.
//
// Secrets travel in URLs in three places, all common in Lambda env vars:
//
//   - userinfo:  postgres://app:<password>@orders-db.../orders
//   - the path:  https://hooks.slack.com/services/T0../B0../<token>
//   - the query: https://api.example.com/v1?api_key=<key>, or a presigned
//     S3 URL's X-Amz-Signature
//
// Each component is checked on its own, so a Slack webhook keeps
// hooks.slack.com — which is exactly what marks it as an external integration —
// and loses only the token. It works on the string rather than round-tripping
// through net/url, which would re-encode parts of the value that are meant to
// survive unchanged.
func sanitizeURL(v string) (string, bool) {
	i := strings.Index(v, "://")
	scheme, rest := v[:i+3], v[i+3:]

	authEnd := len(rest)
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		authEnd = j
	}
	authority, tail := rest[:authEnd], rest[authEnd:]
	changed := false

	// net/url splits userinfo on the last '@', so a raw '@' in a password does
	// not turn half of it into the host. Match that.
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		if user, pass, ok := strings.Cut(authority[:at], ":"); ok && pass != "" && !isMarker(pass) {
			authority = user + ":" + marker("url-credentials") + authority[at:]
			changed = true
		}
	}

	var fragment string
	if h := strings.Index(tail, "#"); h >= 0 {
		tail, fragment = tail[:h], tail[h:]
	}
	var query string
	hasQuery := false
	if q := strings.Index(tail, "?"); q >= 0 {
		tail, query, hasQuery = tail[:q], tail[q+1:], true
	}

	segs := strings.Split(tail, "/")
	for k, s := range segs {
		if s != "" && (credentialPattern(s) || highEntropy(s)) {
			segs[k] = marker("url-path")
			changed = true
		}
	}

	if hasQuery {
		params := strings.Split(query, "&")
		for k, p := range params {
			key, val, ok := strings.Cut(p, "=")
			if !ok || val == "" || isMarker(val) {
				continue
			}
			if secretKeyName(key) || signatureParams[strings.ToLower(key)] ||
				credentialPattern(val) || highEntropy(val) {
				params[k] = key + "=" + marker("url-query")
				changed = true
			}
		}
		query = strings.Join(params, "&")
	}

	out := scheme + authority + strings.Join(segs, "/")
	if hasQuery {
		out += "?" + query
	}
	return out + fragment, changed
}

// signatureParams are query parameters whose value authorizes the request on
// its own — a presigned URL is a credential with an expiry date.
var signatureParams = map[string]bool{
	"x-amz-signature": true, "x-amz-credential": true, "x-amz-security-token": true,
	"signature": true, "sig": true, "awsaccesskeyid": true,
}

func credentialPattern(v string) bool {
	for _, p := range credentialPatterns {
		if p.MatchString(v) {
			return true
		}
	}
	return false
}

func looksLikeURL(v string) bool {
	i := strings.Index(v, "://")
	return i > 0 && !strings.ContainsAny(v[:i], " \t/")
}

// highEntropy flags long, dense, random-looking tokens under innocuous names.
//
// It requires upper case, lower case *and* digits. Punctuation deliberately does
// not count as a class: an earlier version counted it, and flagged an RDS
// endpoint (lowercase, digits, dots, dashes) as a secret — the single most
// valuable linking signal in a task definition. Hostnames, hex digests, UUIDs and
// camelCase identifiers all fail the three-class test, and the two shapes most
// likely to sneak past it are excluded explicitly as well.
func highEntropy(v string) bool {
	if len(v) < 24 || strings.ContainsAny(v, " \t\n") {
		return false
	}
	if hostnamePattern.MatchString(v) || digestPattern.MatchString(v) {
		return false
	}
	var lower, upper, digit bool
	for _, r := range v {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		}
	}
	if !(lower && upper && digit) {
		return false
	}
	return shannon(v) >= 4.0
}

var (
	hostnamePattern = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:\d{1,5})?$`)
	digestPattern   = regexp.MustCompile(`^[a-z0-9]+:[0-9a-f]{32,}$`)
)

func shannon(s string) float64 {
	freq := map[rune]float64{}
	for _, r := range s {
		freq[r]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, c := range freq {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}
