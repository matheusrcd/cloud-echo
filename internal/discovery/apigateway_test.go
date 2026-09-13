package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func collectREST(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "apigateway")
	out := &captureEmitter{}
	if err := (&APIGateway{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func collectHTTP(t *testing.T) (*captureEmitter, *fixtureTransport) {
	t.Helper()
	tr := loadFixture(t, "orders", "apigatewayv2")
	out := &captureEmitter{}
	if err := (&APIGatewayV2{}).Collect(context.Background(), fixtureSession(tr), out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	tr.assertAllMatched(t)
	return out, tr
}

func apiOf(t *testing.T, out *captureEmitter, id string) apiSpec {
	t.Helper()
	r, ok := out.byID(id)
	if !ok {
		t.Fatalf("missing %s", id)
	}
	var s apiSpec
	specOf(t, r, &s)
	return s
}

func routeOf(t *testing.T, s apiSpec, key string) routeSpec {
	t.Helper()
	for _, r := range s.Routes {
		if r.RouteKey == key {
			return r
		}
	}
	t.Fatalf("%s has no route %q", s.APIID, key)
	return routeSpec{}
}

// TestAPIGatewayIDsAreAPIIDsNotNames: the fixture has two REST APIs named
// orders-rest, which API Gateway allows. A name-based id would collapse them.
func TestAPIGatewayIDsAreAPIIDsNotNames(t *testing.T) {
	out, _ := collectREST(t)
	if got, want := out.ids(), []string{"apigw/a1b2c3d4e5", "apigw/f6g7h8i9j0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids: got %q want %q", got, want)
	}
	a, b := apiOf(t, out, "apigw/a1b2c3d4e5"), apiOf(t, out, "apigw/f6g7h8i9j0")
	if a.Name != b.Name {
		t.Fatalf("fixture no longer has two same-named APIs: %q vs %q", a.Name, b.Name)
	}
}

func TestAPIGatewayRESTFollowsPagination(t *testing.T) {
	out, tr := collectREST(t)
	if n := tr.count("API Gateway", "GetRestApis"); n != 2 {
		t.Errorf("GetRestApis: %d calls, want 2", n)
	}
	if n := tr.count("API Gateway", "GetResources"); n != 3 {
		t.Errorf("GetResources: %d calls, want 3 (two pages for one API, one for the other)", n)
	}
	s := apiOf(t, out, "apigw/a1b2c3d4e5")
	routeOf(t, s, "GET /health") // page two
	if len(s.Routes) != 7 {
		t.Errorf("want 7 routes across both pages, got %d", len(s.Routes))
	}
}

// TestAPIGatewayRESTResolvesTargets covers every integration shape in the
// fixture, from Lambda proxy to caller-credential passthrough.
func TestAPIGatewayRESTResolvesTargets(t *testing.T) {
	s := apiOf(t, func() *captureEmitter { o, _ := collectREST(t); return o }(), "apigw/a1b2c3d4e5")

	for _, tc := range []struct {
		route, service, target, qualifier, action string
		templated                                 bool
	}{
		{"GET /orders", "lambda", "lambda/orders-fn", "", "", false},
		{"GET /orders/{id}", "lambda", "lambda/orders-fn", "live", "", false},
		{"POST /webhooks", "sqs", "sqs/inbox", "", "", false},
		{"GET /health", "mock", "", "", "", false},
		{"GET /legacy", "http", "", "", "", false},
		{"GET /reports", "dynamodb", "", "", "Query", false},
		// The function name comes from a stage variable: no target, never a
		// made-up lambda/${stageVariables.fn} node.
		{"ANY /v2", "", "", "", "", true},
	} {
		it := routeOf(t, s, tc.route).Integration
		if it == nil {
			t.Errorf("%s: no integration", tc.route)
			continue
		}
		var target string
		if it.Target != nil {
			target = it.Target.ID
		}
		if it.Service != tc.service || target != tc.target || it.Qualifier != tc.qualifier ||
			it.Action != tc.action || it.Templated != tc.templated {
			t.Errorf("%s: got service=%q target=%q qualifier=%q action=%q templated=%v",
				tc.route, it.Service, target, it.Qualifier, it.Action, it.Templated)
		}
	}

	wh := routeOf(t, s, "POST /webhooks")
	if !wh.APIKeyRequired || wh.Integration.Credentials != "arn:aws:iam::123456789012:role/apigw-integration-role" {
		t.Errorf("webhooks: apiKeyRequired=%v credentials=%q", wh.APIKeyRequired, wh.Integration.Credentials)
	}
	if o := routeOf(t, s, "GET /orders"); o.Authorization != "CUSTOM" || o.AuthorizerID != "auth01" {
		t.Errorf("GET /orders authorization: %+v", o)
	}
	if len(s.Authorizers) != 1 || s.Authorizers[0].Function == nil || s.Authorizers[0].Function.ID != "lambda/authorizer-fn" {
		t.Errorf("TOKEN authorizer function not resolved: %+v", s.Authorizers)
	}
}

// TestAPIGatewayRESTPolicyIsDecoded uses the policy exactly as a real account
// returned it: the body of a JSON string literal, with \" and \/ escapes.
func TestAPIGatewayRESTPolicyIsDecoded(t *testing.T) {
	out, _ := collectREST(t)
	s := apiOf(t, out, "apigw/a1b2c3d4e5")
	var doc struct {
		Statement []struct{ Action, Resource string }
	}
	if err := json.Unmarshal(s.ResourcePolicy, &doc); err != nil {
		t.Fatalf("resource policy is not usable JSON: %v\n%s", err, s.ResourcePolicy)
	}
	if len(doc.Statement) != 1 || doc.Statement[0].Action != "execute-api:Invoke" ||
		!strings.HasSuffix(doc.Statement[0].Resource, "a1b2c3d4e5/*") {
		t.Errorf("policy decoded wrongly: %+v", doc)
	}
	if len(out.warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", out.warnings)
	}
}

func TestDecodeRestAPIPolicy(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		`{"a":"b"}`:        `{"a":"b"}`,
		`{\"a\":\"x\/y\"}`: `{"a":"x/y"}`,
	} {
		got, err := decodeRestAPIPolicy(in)
		if err != nil || string(got) != want {
			t.Errorf("%q: got %q, %v want %q", in, got, err, want)
		}
	}
	if _, err := decodeRestAPIPolicy(`{not json`); err == nil {
		t.Error("accepted a policy that is neither JSON nor escaped JSON")
	}
}

func TestAPIGatewayEndpoints(t *testing.T) {
	out, _ := collectREST(t)
	if e := apiOf(t, out, "apigw/a1b2c3d4e5").Endpoint; e != "https://a1b2c3d4e5.execute-api.us-east-1.amazonaws.com" {
		t.Errorf("REST endpoint: %q", e)
	}
	// The default endpoint is disabled: advertising one would give the linker
	// a URL nothing answers on.
	if s := apiOf(t, out, "apigw/f6g7h8i9j0"); s.Endpoint != "" || !s.DefaultEndpointDisabled || s.EndpointType != "EDGE" {
		t.Errorf("disabled endpoint: %+v", s)
	}
}

// TestAPIGatewaySecretsNeverReachTheInventory: static upstream keys in
// parameter mappings, secrets in stage variables, and mapping-template bodies
// must be absent from the serialized resource — Raw included. References must
// survive: they say where a value comes from, not what it is.
func TestAPIGatewaySecretsNeverReachTheInventory(t *testing.T) {
	rest, _ := collectREST(t)
	http, _ := collectHTTP(t)

	var blob []byte
	for _, r := range append(rest.resources, http.resources...) {
		b, _ := json.Marshal(r)
		blob = append(blob, b...)
	}
	for _, secret := range []string{
		"not-a-real-upstream-key", "not-a-real-password", "not-a-real-v2-key", "not-a-real-token-123",
		"MessageBody=$util.urlEncode", `statusCode\": 200`, `ok\": true`,
	} {
		if bytes.Contains(blob, []byte(secret)) {
			t.Errorf("%q survived into the serialized resources", secret)
		}
	}
	for _, kept := range []string{"method.request.header.X-Trace", "$request.header.X-Trace", "$request.body",
		// A secret-looking key whose value is a reference: without the mapping-
		// expression rule the key-name rule would redact where a token comes from.
		"method.request.header.Authorization",
		"application/x-www-form-urlencoded", "https://sqs.us-east-1.amazonaws.com/123456789012/inbox"} {
		if !bytes.Contains(blob, []byte(kept)) {
			t.Errorf("%q was lost — it is a reference or a plain value, not a secret", kept)
		}
	}

	s := apiOf(t, rest, "apigw/a1b2c3d4e5")
	if want := []string{"route:POST /webhooks:integration.request.header.x-api-key", "stage:prod:DB_PASSWORD"}; !reflect.DeepEqual(s.Redacted, want) {
		t.Errorf("REST redacted: got %q want %q", s.Redacted, want)
	}
	if ct := routeOf(t, s, "POST /webhooks").Integration.TemplateContentTypes; !reflect.DeepEqual(ct, []string{"application/json"}) {
		t.Errorf("template content types: %q", ct)
	}
	h := apiOf(t, http, "apigw/h1t2t3p4a5")
	if want := []string{"integration:httpin1:overwrite:header.x-api-key", "stage:prod:API_TOKEN"}; !reflect.DeepEqual(h.Redacted, want) {
		t.Errorf("HTTP redacted: got %q want %q", h.Redacted, want)
	}
}

func TestAPIGatewayV2RoutesAndSharedIntegrations(t *testing.T) {
	out, _ := collectHTTP(t)
	s := apiOf(t, out, "apigw/h1t2t3p4a5")

	a, b := routeOf(t, s, "POST /orders"), routeOf(t, s, "GET /orders/{id}")
	if a.Integration.ID != "fnint01" || b.Integration.ID != "fnint01" || a.Integration.Target.ID != "lambda/orders-fn" {
		t.Errorf("shared integration not carried by both routes: %+v / %+v", a.Integration, b.Integration)
	}
	if a.Method != "POST" || a.Path != "/orders" || a.Authorization != "JWT" {
		t.Errorf("route key not split: %+v", a)
	}

	ev := routeOf(t, s, "POST /events").Integration
	if ev.Service != "sqs" || ev.Action != "SendMessage" || ev.Target == nil || ev.Target.ID != "sqs/inbox" {
		t.Errorf("SQS service integration: %+v", ev)
	}
	if ev.Credentials != "arn:aws:iam::123456789012:role/apigw-integration-role" {
		t.Errorf("credentials: %q", ev.Credentials)
	}

	d := routeOf(t, s, "$default")
	if d.Method != "" || d.Path != "" || d.Integration.Service != "http" {
		t.Errorf("$default route: %+v", d)
	}
	if v := routeOf(t, s, "GET /v2").Integration; !v.Templated || v.Target != nil {
		t.Errorf("templated v2 integration resolved to a target: %+v", v)
	}
}

func TestAPIGatewayV2Authorizers(t *testing.T) {
	out, _ := collectHTTP(t)
	s := apiOf(t, out, "apigw/h1t2t3p4a5")
	by := map[string]authorizerSpec{}
	for _, a := range s.Authorizers {
		by[a.Type] = a
	}
	if j := by["JWT"]; j.JWTIssuer != "https://accounts.google.com" || !reflect.DeepEqual(j.JWTAudience, []string{"orders-client"}) {
		t.Errorf("JWT authorizer: %+v", j)
	}
	if r := by["REQUEST"]; r.Function == nil || r.Function.ID != "lambda/authorizer-fn" || r.Credentials == "" {
		t.Errorf("REQUEST authorizer: %+v", r)
	}
}

func TestAPIGatewayV2FollowsPaginationAndTypesWebSockets(t *testing.T) {
	out, tr := collectHTTP(t)
	if n := tr.count("ApiGatewayV2", "GetApis"); n != 2 {
		t.Errorf("GetApis: %d calls, want 2", n)
	}
	if n := tr.count("ApiGatewayV2", "GetRoutes"); n != 3 {
		t.Errorf("GetRoutes: %d calls, want 3 (two pages for the HTTP API, one for the WebSocket API)", n)
	}
	routeOf(t, apiOf(t, out, "apigw/h1t2t3p4a5"), "GET /callback") // page two

	ws, ok := out.byID("apigw/w1s2k3t4a5")
	if !ok || ws.Type != "apigateway.websocket" {
		t.Fatalf("WebSocket API missing or mistyped: %+v", ws.Type)
	}
	var s apiSpec
	specOf(t, ws, &s)
	c := routeOf(t, s, "$connect")
	if c.Integration == nil || c.Integration.Target == nil || c.Integration.Target.ID != "lambda/ws-connect-fn" {
		t.Errorf("$connect integration: %+v", c.Integration)
	}
}

func TestQueueTargetFromURL(t *testing.T) {
	for u, want := range map[string]string{
		"https://sqs.us-east-2.amazonaws.com/123456789012/orders": "sqs/orders",
		"https://sqs.us-east-2.amazonaws.com/123456789012":        "",
		"https://example.com/123456789012/orders":                 "",
		"https://sqs.us-east-2.amazonaws.com/123/a/b":             "",
	} {
		got := ""
		if tg := queueTargetFromURL(u); tg != nil {
			got = tg.ID
		}
		if got != want {
			t.Errorf("%s: got %q want %q", u, got, want)
		}
	}
}

// TestIAMReadsAPIGatewayCredentialRoles: the role an API assumes to write to a
// queue is what Tier 3 needs, and the caller-passthrough marker
// arn:aws:iam::*:user/* is neither a role nor another account.
func TestIAMReadsAPIGatewayCredentialRoles(t *testing.T) {
	rest, _ := collectREST(t)
	http, _ := collectHTTP(t)
	warn := &captureEmitter{}
	refs := assumedRoles(append(rest.resources, http.resources...), "123456789012", warn)

	if want := []string{"apigw/a1b2c3d4e5", "apigw/h1t2t3p4a5"}; !reflect.DeepEqual(refs["apigw-integration-role"], want) {
		t.Errorf("assumedBy: got %q want %q (deduplicated across routes)", refs["apigw-integration-role"], want)
	}
	if len(refs) != 1 {
		t.Errorf("unexpected roles queued: %v", refs)
	}
	if len(warn.warnings) != 0 {
		t.Errorf("caller-credential passthrough produced warnings: %+v", warn.warnings)
	}
}

func TestAPIGatewayUsesExactlyTheAllowListedOperations(t *testing.T) {
	_, tr := collectREST(t)
	assertOpsMatchAllowList(t, "API Gateway", tr)
	_, tr2 := collectHTTP(t)
	assertOpsMatchAllowList(t, "ApiGatewayV2", tr2)
}

// TestAPIGatewaySpecOrderIsCanonical: found in the M1 round trip, where the same
// HTTP API scanned from AWS and from Floci listed its stages in opposite orders.
// The fixture serves stages and authorizers deliberately out of order.
func TestAPIGatewaySpecOrderIsCanonical(t *testing.T) {
	out, _ := collectHTTP(t)
	s := apiOf(t, out, "apigw/h1t2t3p4a5")
	var stages, auths []string
	for _, st := range s.Stages {
		stages = append(stages, st.Name)
	}
	for _, a := range s.Authorizers {
		auths = append(auths, a.Name)
	}
	if !reflect.DeepEqual(stages, []string{"$default", "prod"}) || !reflect.DeepEqual(auths, []string{"jwt", "request-auth"}) {
		t.Errorf("not canonical: stages=%q authorizers=%q", stages, auths)
	}
	ws := apiOf(t, out, "apigw/w1s2k3t4a5")
	if ws.Routes == nil {
		t.Error("routes must be a list, never null")
	}
}
