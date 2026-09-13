package discovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// exchange is one recorded request/response pair.
//
// An exchange is matched on the operation name plus an optional substring. That
// substring is checked against the URL and the body both, because the discriminator
// lives in different places per protocol: the JSON body for ECS/SQS/DynamoDB, the
// path for Lambda (GET /functions/<name>/policy), the form-encoded body for IAM.
// Stricter schemes — full request equality, ordered replay — make fixtures brittle
// against harmless changes like a new Include field.
type exchange struct {
	Op    string `json:"op"`
	Match string `json:"match,omitempty"`

	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Response is a JSON body. Body is a raw one, for Query-protocol services
	// like IAM that answer in XML. Exactly one should be set.
	Response json.RawMessage `json:"response,omitempty"`
	Body     string          `json:"body,omitempty"`

	// service is the fixture file the exchange came from, e.g. "iam". It is
	// set by the loader, never by the fixture author.
	service string
}

// fixtureTransport replays recorded AWS responses and records which operations
// the collector actually invoked.
//
// Service and operation come from the request context, not from headers. The SDK
// puts both there before building the request, for every protocol — which is what
// lets one transport serve JSON, REST and Query services, and what lets two
// services share an operation name (lambda:GetPolicy and iam:GetPolicy) without
// one answering the other's request.
type fixtureTransport struct {
	t         *testing.T
	exchanges []exchange

	mu       sync.Mutex
	observed map[string]int // "SDKID:Operation" → calls
	// unmatched records calls with no fixture, so the test can report all of
	// them at once instead of failing on the first.
	unmatched []string
}

func readExchanges(t *testing.T, account, service string) []exchange {
	t.Helper()
	path := filepath.Join("testdata", "accounts", account, service+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var ex []exchange
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	for i := range ex {
		ex[i].service = service
		if (len(ex[i].Response) == 0) == (ex[i].Body == "") {
			t.Fatalf("%s exchange %d (%s): set exactly one of response and body", path, i, ex[i].Op)
		}
	}
	return ex
}

func loadFixture(t *testing.T, account, service string) *fixtureTransport {
	t.Helper()
	return &fixtureTransport{t: t, exchanges: readExchanges(t, account, service), observed: map[string]int{}}
}

// loadFixtures merges several service fixtures into one transport, so a test can
// drive a full multi-collector scan. Exchanges stay namespaced by service.
func loadFixtures(t *testing.T, account string, services ...string) *fixtureTransport {
	t.Helper()
	merged := &fixtureTransport{t: t, observed: map[string]int{}}
	for _, svc := range services {
		merged.exchanges = append(merged.exchanges, readExchanges(t, account, svc)...)
	}
	return merged
}

func (f *fixtureTransport) Do(req *http.Request) (*http.Response, error) {
	service := awsmiddleware.GetServiceID(req.Context())
	op := awsmiddleware.GetOperationName(req.Context())
	if service == "" || op == "" {
		return nil, fmt.Errorf("request carries no service/operation in its context — "+
			"the fixture transport depends on the SDK setting both (%s %s)", req.Method, req.URL)
	}

	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	haystack := req.URL.RequestURI() + "\n" + string(body)

	f.mu.Lock()
	f.observed[service+":"+op]++
	f.mu.Unlock()

	for _, ex := range f.exchanges {
		if ex.Op != op || serviceKey(ex.service) != serviceKey(service) {
			continue
		}
		if ex.Match != "" && !strings.Contains(haystack, ex.Match) {
			continue
		}
		return ex.httpResponse(req), nil
	}

	f.mu.Lock()
	f.unmatched = append(f.unmatched, fmt.Sprintf("%s:%s %s body=%s", service, op, req.URL.RequestURI(), body))
	f.mu.Unlock()
	return nil, fmt.Errorf("no fixture for %s:%s (%s, body %s)", service, op, req.URL.RequestURI(), body)
}

// serviceKey lets a fixture file name match an SDK service id: "apigateway" is
// the file for "API Gateway", whose id contains a space.
func serviceKey(s string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(strings.ToLower(s))
}

func (ex exchange) httpResponse(req *http.Request) *http.Response {
	status := ex.Status
	if status == 0 {
		status = 200
	}
	h := http.Header{}
	payload := []byte(ex.Response)
	if ex.Body != "" {
		payload = []byte(ex.Body)
		h.Set("Content-Type", "text/xml")
	} else {
		h.Set("Content-Type", "application/json")
	}
	for k, v := range ex.Headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Request:    req,
	}
}

// operations returns the distinct operations invoked on one service.
func (f *fixtureTransport) operations(sdkID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for key := range f.observed {
		if svc, op, _ := strings.Cut(key, ":"); svc == sdkID {
			out = append(out, op)
		}
	}
	sort.Strings(out)
	return out
}

// count returns how many times one operation was invoked.
func (f *fixtureTransport) count(sdkID, op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observed[sdkID+":"+op]
}

func (f *fixtureTransport) assertAllMatched(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.unmatched {
		t.Errorf("unmatched request: %s", u)
	}
}

func fixtureSession(tr *fixtureTransport) *awsx.Session {
	return awsx.NewTestSession(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIATEST", "secret", ""),
		HTTPClient:  tr,
	}, "123456789012", "us-east-1")
}

// captureEmitter collects what a collector produced.
type captureEmitter struct {
	mu        sync.Mutex
	resources []inventory.Resource
	warnings  []inventory.Warning
}

func (c *captureEmitter) Emit(r inventory.Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resources = append(c.resources, r)
}

func (c *captureEmitter) Warn(w inventory.Warning) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warnings = append(c.warnings, w)
}

func (c *captureEmitter) byID(id string) (inventory.Resource, bool) {
	for _, r := range c.resources {
		if r.ID == id {
			return r, true
		}
	}
	return inventory.Resource{}, false
}

func (c *captureEmitter) ids() []string {
	out := make([]string, 0, len(c.resources))
	for _, r := range c.resources {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out
}

// specOf decodes a resource's normalized spec into v.
func specOf(t *testing.T, r inventory.Resource, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Spec, v); err != nil {
		t.Fatalf("decoding spec of %s: %v", r.ID, err)
	}
}

// assertOpsMatchAllowList is the drift check every collector runs.
//
// It compares the operations a collector *actually invoked*, observed by the
// fixture transport, against the awsx allow-list — deliberately not against a
// list the collector declares about itself, because a declaration can drift from
// the code while observed calls cannot.
//
// Both directions are failures. An operation called but not allow-listed is a
// scan that dies against a correctly-permissioned account. An operation
// allow-listed but never called is a permission users are asked to grant for
// nothing, and every unnecessary permission is a reason for a security team to
// refuse the whole tool.
func assertOpsMatchAllowList(t *testing.T, sdkID string, tr *fixtureTransport) {
	t.Helper()

	var allowed []string
	for _, s := range awsx.Services() {
		if s.SDKID == sdkID {
			allowed = s.Ops
		}
	}
	if allowed == nil {
		t.Fatalf("%s is not in the awsx allow-list", sdkID)
	}

	observed := map[string]bool{}
	for _, op := range tr.operations(sdkID) {
		observed[op] = true
	}

	allowedSet := map[string]bool{}
	for _, op := range allowed {
		allowedSet[op] = true
		if !observed[op] {
			t.Errorf("allow-list grants %s:%s but the collector never calls it — "+
				"either use it or stop asking users for the permission", sdkID, op)
		}
	}
	for op := range observed {
		if !allowedSet[op] {
			t.Errorf("collector called %s:%s, which is not on the allow-list", sdkID, op)
		}
	}
}
