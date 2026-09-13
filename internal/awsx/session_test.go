package awsx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewSessionReportsEndpointOverride exercises NewSession end to end — the
// credential chain, the STS identity call, the guard — against a local server
// standing in for AWS, and checks that the override is visible.
//
// The SDK honours AWS_ENDPOINT_URL silently. A user who left it exported from
// emulator work would otherwise scan the emulator while the output named an AWS
// account.
func TestNewSessionReportsEndpointOverride(t *testing.T) {
	var sawSTS bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("Action") == "GetCallerIdentity" {
			sawSTS = true
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
<GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:root</Arn><UserId>123456789012</UserId>
<Account>123456789012</Account></GetCallerIdentityResult>
<ResponseMetadata><RequestId>test</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	}))
	defer srv.Close()

	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_ENDPOINT_URL_SQS", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "123456789012")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-2")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")

	s, err := NewSession(context.Background(), Options{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !sawSTS {
		t.Error("NewSession did not verify credentials with sts:GetCallerIdentity")
	}
	if s.AccountID() != "123456789012" || s.Region() != "us-east-2" {
		t.Errorf("account=%q region=%q", s.AccountID(), s.Region())
	}
	ep := s.EndpointOverride()
	if !strings.Contains(ep, srv.URL) || !strings.Contains(ep, "AWS_ENDPOINT_URL_SQS") {
		t.Errorf("override not reported in full: %q", ep)
	}
}

func TestNoEndpointOverrideByDefault(t *testing.T) {
	t.Setenv("AWS_ENDPOINT_URL", "")
	if got := endpointOverride(testSession(t, &countingTransport{}).Config()); strings.Contains(got, "http") {
		t.Errorf("reported an override with none configured: %q", got)
	}
}
