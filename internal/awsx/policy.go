package awsx

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
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
	Resource any      `json:"Resource"` // "*" or a list of ARNs
}

// ScannerPolicy renders the least-privilege IAM policy implied by the allow-list.
//
// Every action here is one a collector actually issues, and the drift test in
// policy_test.go fails if that stops being true in either direction. That
// property is the point: shipping ReadOnlyAccess would be far easier and far
// harder for a security team to approve, because it grants thousands of actions
// nobody can account for.
//
// Most actions use Resource "*", because they are account-wide List/Describe
// calls that do not support resource-level constraints; narrowing them would
// produce a policy that looks tighter and silently fails at scan time. Services
// that declare IAMResources get their own statement scoped to those ARNs — see
// API Gateway, whose single coarse action would otherwise reach API key values.
func ScannerPolicy() ([]byte, error) {
	scope := map[string]map[string]struct{}{} // action → resources
	for _, s := range services {
		res := s.IAMResources
		if len(res) == 0 {
			res = []string{"*"}
		}
		for _, a := range s.IAMActionsFor() {
			if scope[a] == nil {
				scope[a] = map[string]struct{}{}
			}
			for _, r := range res {
				scope[a][r] = struct{}{}
			}
		}
	}

	groups := map[string][]string{} // joined resource set → actions
	resOf := map[string][]string{}
	for a, rs := range scope {
		list := make([]string, 0, len(rs))
		for r := range rs {
			list = append(list, r)
		}
		sort.Strings(list)
		key := strings.Join(list, "\n")
		groups[key] = append(groups[key], a)
		resOf[key] = list
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	// The unscoped statement first, then scoped ones in a stable order.
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == "*") != (keys[j] == "*") {
			return keys[i] == "*"
		}
		return keys[i] < keys[j]
	})

	doc := policyDocument{Version: "2012-10-17"}
	for _, k := range keys {
		actions := groups[k]
		sort.Strings(actions)
		st := policyStatement{Effect: "Allow", Action: actions}
		if k == "*" {
			st.Sid = PolicySid
			st.Resource = "*"
		} else {
			prefix, _, _ := strings.Cut(actions[0], ":")
			st.Sid = PolicySid + "Scoped" + strings.ToUpper(prefix[:1]) + prefix[1:]
			st.Resource = resOf[k]
		}
		doc.Statement = append(doc.Statement, st)
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
