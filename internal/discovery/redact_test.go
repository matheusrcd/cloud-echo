package discovery

import (
	"reflect"
	"strings"
	"testing"
)

// TestRedactKeepsLinkingSignal is the half of the redaction rules that is easy to
// get wrong in the safe-looking direction. Every value here is something the
// linker needs, several under names that look suspicious. Redacting them would
// not leak anything, but it would quietly blind the part of cloud-echo that
// decides what talks to what.
func TestRedactKeepsLinkingSignal(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"TABLE_NAME", "orders"},
		{"QUEUE_URL", "https://sqs.us-east-1.amazonaws.com/123456789012/orders-events"},
		{"DB_HOST", "orders-db.cluster-abc123.us-east-1.rds.amazonaws.com"},
		{"SECRET_ARN", "arn:aws:secretsmanager:us-east-1:123456789012:secret:prod/orders/db-AbCdEf"},
		{"DB_PASSWORD_SECRET_ARN", "arn:aws:secretsmanager:us-east-1:123456789012:secret:x"},
		{"AUTH_URL", "https://auth.example.com/oauth"},
		{"TOKEN_ENDPOINT", "https://auth.example.com/oauth/token"},
		{"PARTITION_KEY", "pk"},
		{"SORT_KEY", "sk"},
		{"LOG_LEVEL", "info"},
		{"PWD", "/app"},
		{"BYPASS_CACHE", "true"},
		{"PASSTHROUGH_MODE", "on"},
		{"IMAGE_DIGEST", "sha256:9f2b1c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"},
		{"CORRELATION_ID", "550e8400-e29b-41d4-a716-446655440000"},
		{"SERVICE_CLASS", "OrdersProcessingServiceConfiguration"},
		{"PAYMENTS_URL", "https://api.payments.example.com"},
		// Regression: an earlier entropy rule counted punctuation as a character
		// class and redacted this — the one value that links a service to its DB.
		{"CACHE_HOST", "sessions.abc123.ng.0001.use1.cache.amazonaws.com"},
		{"DB_ENDPOINT", "orders-db.cluster-abc123.us-east-1.rds.amazonaws.com:5432"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			got, red := redactValue(tc.key, tc.value)
			if red || got != tc.value {
				t.Errorf("%s=%q was redacted to %q — this is a linking signal, not a secret",
					tc.key, tc.value, got)
			}
		})
	}
}

func TestRedactRemovesSecrets(t *testing.T) {
	for _, tc := range []struct{ key, value, reason string }{
		{"DB_PASSWORD", "hunter2", "key-name"},
		{"DB_PASS", "hunter2", "key-name"},
		{"dbPassword", "hunter2", "key-name"},
		{"db.password", "hunter2", "key-name"},
		{"API_KEY", "abc123", "key-name"},
		{"CLIENT_SECRET", "abc123", "key-name"},
		{"GITHUB_TOKEN", "abc123", "key-name"},
		{"SECRET_NAME", "prod/orders/db", "key-name"}, // accepted loss: see redact.go
		{"X", "AKIA" + "IOSFODNN7EXAMPLE", "credential-pattern"},
		{"STRIPE", "sk_" + "live_51HxxxxxxxxxxxxxxxxxxxxQ", "credential-pattern"},
		{"GH", "gh" + "p_abcdefghijklmnopqrstuvwxyz0123456789", "credential-pattern"},
		{"SLACK_HOOK_ID", "xo" + "xb-123456789012-abcdefghij", "credential-pattern"},
		{"HEADER", "Bearer abcdefghijklmnop.qrstuv", "credential-pattern"},
		{"SESSION", "eyJhbGciOiJIUzI1NiJ9" + ".eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "credential-pattern"},
		{"CERT", "-----BEGIN RSA PRIV" + "ATE KEY-----\nMIIE...", "credential-pattern"},
		{"CONN", "Server=db;Database=orders;User Id=sa;Password=p4ss;", "credential-pattern"},
		{"BLOB", "q8Zr+T2vK9mXw4Lp7Yb1Nc6Hj3Fs0Ud5Ge=Aa", "high-entropy"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			got, red := redactValue(tc.key, tc.value)
			if !red {
				t.Fatalf("%s=%q was kept — it would be copied out of AWS", tc.key, tc.value)
			}
			if want := marker(tc.reason); got != want {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}

// TestRedactKeepsHostOfCredentialURL is the case the whole rule ordering exists
// for. DATABASE_URL carries both a password and the one piece of information that
// links this service to its database. Dropping the value would lose the link;
// keeping it would leak the password. The password goes, the host stays.
func TestRedactKeepsHostOfCredentialURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			"postgres://app:hunter2@orders-db.cluster-abc.us-east-1.rds.amazonaws.com:5432/orders",
			"postgres://app:<redacted:url-credentials>@orders-db.cluster-abc.us-east-1.rds.amazonaws.com:5432/orders",
		},
		{
			"redis://:s3cret@sessions.abc123.ng.0001.use1.cache.amazonaws.com:6379/0",
			"redis://:<redacted:url-credentials>@sessions.abc123.ng.0001.use1.cache.amazonaws.com:6379/0",
		},
		{
			// A raw '@' in the password: net/url splits on the last '@', and so
			// must we, or the host becomes half a password.
			"amqp://svc:p@ss@mq.internal:5672/vhost",
			"amqp://svc:<redacted:url-credentials>@mq.internal:5672/vhost",
		},
	} {
		got, red := redactValue("DATABASE_URL", tc.in)
		if !red || got != tc.want {
			t.Errorf("\n  in: %s\n got: %s\nwant: %s", tc.in, got, tc.want)
		}
	}

	// A username alone is not a secret, and the URL is kept whole.
	in := "https://user@api.example.com/v1"
	if got, red := redactValue("API_URL", in); red || got != in {
		t.Errorf("username-only URL was altered: %q", got)
	}
}

func TestRedactArgs(t *testing.T) {
	in := []string{"node", "server.js", "--db-password=hunter2", "--port", "8080",
		"--token", "abc123", "--queue-url=https://sqs.us-east-1.amazonaws.com/123456789012/q",
		"--key", "AKIA" + "IOSFODNN7EXAMPLE"}
	want := []string{"node", "server.js", "--db-password=<redacted:key-name>", "--port", "8080",
		"--token", "<redacted:key-name>", "--queue-url=https://sqs.us-east-1.amazonaws.com/123456789012/q",
		"--key", "<redacted:credential-pattern>"}

	got, hit := redactArgs(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if !reflect.DeepEqual(hit, []int{2, 6, 9}) {
		t.Errorf("redacted positions: got %v want [2 6 9]", hit)
	}
	if in[2] != "--db-password=hunter2" {
		t.Error("redactArgs mutated its input")
	}
}

// TestRedactedMarkerIsNotItselfSecretShaped guards against a marker that trips
// the blueprint's own secret scanner downstream, or that the linker could match
// as a resource name.
func TestRedactedMarkerIsNotItselfSecretShaped(t *testing.T) {
	for _, reason := range []string{"key-name", "credential-pattern", "high-entropy", "url-credentials"} {
		m := marker(reason)
		if _, red := redactValue("VALUE", m); red {
			t.Errorf("marker %q is itself redacted", m)
		}
		if !strings.HasPrefix(m, "<redacted:") {
			t.Errorf("unexpected marker shape %q", m)
		}
	}
}
