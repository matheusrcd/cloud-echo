package linker

import (
	"fmt"

	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// Local accepts a reference's inventory id only when its ARN is in the scanned
// account and region.
//
// Ids are derived from names (sqs/<queue>, lambda/<function>), and names repeat
// across accounts and regions. A dead-letter queue in another account with the
// same name as a local queue would otherwise become an edge to the wrong queue —
// silently, and plausibly enough that nobody would question it. Anything outside
// the scan is recorded as unresolved instead.
func (c *Context) Local(node string, ref *spec.TargetRef, what string) (string, bool) {
	if ref == nil {
		return "", false
	}
	if ref.ID == "" {
		c.Unresolved(node, ref.ARN, fmt.Sprintf("%s: no collector for this kind of target", what))
		return "", false
	}
	if a, ok := spec.ParseARN(ref.ARN); ok {
		if a.Account != "" && a.Account != c.inv.AccountID {
			c.Unresolved(node, ref.ARN, fmt.Sprintf("%s: target is in account %s, outside this scan", what, a.Account))
			return "", false
		}
		if a.Region != "" && a.Region != c.inv.Region {
			c.Unresolved(node, ref.ARN, fmt.Sprintf("%s: target is in region %s, outside this scan", what, a.Region))
			return "", false
		}
	}
	return ref.ID, true
}
