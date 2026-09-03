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
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// exchange is one recorded request/response pair.
//
// Fixtures are matched on the operation name plus an optional substring of the
// request body, which is enough to disambiguate the repeated calls a real
// collector makes (DescribeServices per cluster, DescribeTaskDefinition per
// family) while staying readable in a diff. A stricter scheme — full request
// equality, or ordered replay — makes fixtures brittle against harmless changes
// like a new Include field.
type exchange struct {
	Op       string          `json:"op"`
	Match    string          `json:"match,omitempty"`
	Status   int             `json:"status,omitempty"`
	Response json.RawMessage `json:"response"`
}

// fixtureTransport replays recorded AWS responses and records which operations
// the collector actually invoked.
type fixtureTransport struct {
	t         *testing.T
	exchanges []exchange

	mu       sync.Mutex
	observed map[string]int
	// unmatched records calls with no fixture, so the test can report all of
	// them at once instead of failing on the first.
	unmatched []string
}

func loadFixture(t *testing.T, account, service string) *fixtureTransport {
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
	return &fixtureTransport{t: t, exchanges: ex, observed: map[string]int{}}
}

func (f *fixtureTransport) Do(req *http.Request) (*http.Response, error) {
	// ECS speaks JSON 1.1, where the operation is in X-Amz-Target as
	// "<ServicePrefix>.<Operation>".
	target := req.Header.Get("X-Amz-Target")
	op := target
	if i := strings.LastIndex(target, "."); i >= 0 {
		op = target[i+1:]
	}

	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}

	f.mu.Lock()
	f.observed[op]++
	f.mu.Unlock()

	for _, ex := range f.exchanges {
		if ex.Op != op {
			continue
		}
		if ex.Match != "" && !bytes.Contains(body, []byte(ex.Match)) {
			continue
		}
		status := ex.Status
		if status == 0 {
			status = 200
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.1"}},
			Body:       io.NopCloser(bytes.NewReader(ex.Response)),
			Request:    req,
		}, nil
	}

	f.mu.Lock()
	f.unmatched = append(f.unmatched, fmt.Sprintf("%s body=%s", op, body))
	f.mu.Unlock()
	return nil, fmt.Errorf("no fixture for %s with body %s", op, body)
}

// operations returns the distinct operations the collector invoked.
func (f *fixtureTransport) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.observed))
	for op := range f.observed {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
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
	for _, op := range tr.operations() {
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

// loadFixtures merges several service fixtures into one transport, so a test can
// drive a full multi-collector scan.
//
// It fails on an operation name shared by two services, because the transport
// matches on the last segment of X-Amz-Target: a collision would silently serve
// one service's response to another's request, and the resulting test failure
// would point nowhere useful.
func loadFixtures(t *testing.T, account string, services ...string) *fixtureTransport {
	t.Helper()

	merged := &fixtureTransport{t: t, observed: map[string]int{}}
	owner := map[string]string{}

	for _, svc := range services {
		one := loadFixture(t, account, svc)
		for _, ex := range one.exchanges {
			if prev, dup := owner[ex.Op]; dup && prev != svc {
				t.Fatalf("fixture operation %q is defined by both %s and %s; "+
					"the transport matches on operation name alone and cannot tell them apart",
					ex.Op, prev, svc)
			}
			owner[ex.Op] = svc
		}
		merged.exchanges = append(merged.exchanges, one.exchanges...)
	}
	return merged
}
