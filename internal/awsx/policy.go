package awsx

import (
	"bytes"
	"encoding/json"
)

// PolicyPath is where the generated scanner policy lives in the repo.
const PolicyPath = "policies/cloud-echo-scanner.json"

// PolicySid identifies the single statement in the generated policy.
const PolicySid = "CloudEchoDiscoveryReadOnly"

type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Sid      string   `json:"Sid"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource string   `json:"Resource"`
}

// ScannerPolicy renders the least-privilege IAM policy implied by the allow-list.
//
// Every action here is one a collector actually issues, and the drift test in
// policy_test.go fails if that stops being true in either direction. That
// property is the point: shipping ReadOnlyAccess would be far easier and far
// harder for a security team to approve, because it grants thousands of actions
// nobody can account for.
//
// Resource is "*" because the calls are account-wide List/Describe operations
// that do not support resource-level constraints. Narrowing it would produce a
// policy that looks tighter and silently fails at scan time.
func ScannerPolicy() ([]byte, error) {
	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{{
			Sid:      PolicySid,
			Effect:   "Allow",
			Action:   AllIAMActions(),
			Resource: "*",
		}},
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
