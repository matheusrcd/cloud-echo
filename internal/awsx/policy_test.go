package awsx

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updatePolicy = flag.Bool("update-policy", false,
	"rewrite policies/cloud-echo-scanner.json from the allow-list")

// TestScannerPolicyMatchesAllowList keeps the shipped policy honest.
//
// The policy is the artifact a security team actually reads and approves. If it
// drifts from what the collectors call, one of two bad things happens: a scan
// fails against a correctly-permissioned account, or users grant permissions the
// tool never uses. Regenerate with:
//
//	go test ./internal/awsx -run TestScannerPolicyMatchesAllowList -update-policy
func TestScannerPolicyMatchesAllowList(t *testing.T) {
	want, err := ScannerPolicy()
	if err != nil {
		t.Fatalf("rendering policy: %v", err)
	}

	path := filepath.Join("..", "..", PolicyPath)

	if *updatePolicy {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating policies dir: %v", err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("writing policy: %v", err)
		}
		t.Logf("wrote %s", PolicyPath)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\nregenerate with: go test ./internal/awsx -run %s -update-policy",
			PolicyPath, err, t.Name())
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Errorf("%s is out of date with the allow-list.\n\ngot:\n%s\nwant:\n%s\n\n"+
			"regenerate with: go test ./internal/awsx -run %s -update-policy",
			PolicyPath, got, want, t.Name())
	}
}

// TestPolicyGrantsNoForbiddenAction is a second, independent check on the same
// artifact. The allow-list validator already refuses these at init, but the
// policy is what a user pastes into IAM — it is worth asserting directly that
// the bytes we ship never contain them.
func TestPolicyGrantsNoForbiddenAction(t *testing.T) {
	policy, err := ScannerPolicy()
	if err != nil {
		t.Fatalf("rendering policy: %v", err)
	}
	for action, reason := range forbidden {
		if bytes.Contains(policy, []byte(`"`+action+`"`)) {
			t.Errorf("scanner policy grants %s — %s", action, reason)
		}
	}
}

// TestPolicyActionsAreAllReads catches an action that reached the policy without
// going through the naming check, e.g. via an IAMActions override.
func TestPolicyActionsAreAllReads(t *testing.T) {
	for _, action := range AllIAMActions() {
		_, op, ok := strings.Cut(action, ":")
		if !ok {
			t.Errorf("malformed action %q", action)
			continue
		}
		if !hasReadPrefix(op) {
			t.Errorf("policy action %q is not a Describe/List/Get/BatchGet call", action)
		}
	}
}

// TestAllowListRejectsBadEdits exercises the validator that runs at init, so a
// future contributor gets a test failure explaining the rule rather than a panic
// during a scan.
func TestAllowListRejectsBadEdits(t *testing.T) {
	original := services
	t.Cleanup(func() { services = original })

	cases := map[string][]Service{
		"mutating operation": {
			{SDKID: "ECS", IAMPrefix: "ecs", Ops: []string{"DeleteCluster"}},
		},
		"duplicate service": {
			{SDKID: "ECS", IAMPrefix: "ecs", Ops: []string{"ListClusters"}},
			{SDKID: "ECS", IAMPrefix: "ecs", Ops: []string{"DescribeClusters"}},
		},
		"forbidden action": {
			{SDKID: "Secrets Manager", IAMPrefix: "secretsmanager", Ops: []string{"GetSecretValue"}},
		},
		"override for unknown op": {
			{SDKID: "STS", IAMPrefix: "sts", Ops: []string{"GetCallerIdentity"},
				IAMActions: map[string][]string{"GetNotAnOp": nil}},
		},
		"uppercase IAM prefix": {
			{SDKID: "ECS", IAMPrefix: "ECS", Ops: []string{"ListClusters"}},
		},
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			services = bad
			if err := validateAllowList(); err == nil {
				t.Error("validator accepted an allow-list it should have rejected")
			}
		})
	}
}
