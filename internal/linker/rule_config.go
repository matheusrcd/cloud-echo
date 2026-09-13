package linker

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// configValueRule reads what workloads' configuration names: ECS container env
// vars and command lines, Lambda env vars, API Gateway stage variables.
//
// It is the tier that finds third parties — a URL matching nothing in the
// account is an integration the gateway will mock — and the one that names the
// env var holding a reference, which the materializer rewrites to a local
// address. It knows *that* a workload names a resource, not what it does with
// it: edges to AWS resources are references (see KindReferences), and Tier 3
// supplies the intent.
type configValueRule struct{}

func (configValueRule) Name() string { return "config.value-scan" }
func (configValueRule) Tier() int    { return 2 }

func (configValueRule) Apply(c *Context) {
	s := newScanner(c)

	// An ECS service runs its task definition's configuration; the task
	// definition is not a node, so the service holds what it declares. A task
	// definition no service runs (a one-off RunTask) has no node to hold it.
	Each(c, spec.TypeECSService, func(r inventory.Resource, svc *spec.ECSService) {
		td, ok := lookup[spec.ECSTaskDefinition](c, svc.TaskDefinitionID)
		if !ok {
			ref := svc.TaskDefinitionID
			if ref == "" {
				ref = svc.TaskDefinition
			}
			c.Finding("unscanned", r.ID, ref, "task definition not in the inventory: its configuration was not scanned")
			return
		}
		for _, ct := range td.Containers {
			src := fmt.Sprintf("ecs:DescribeTaskDefinition %s:%d container %s", td.Family, td.Revision, ct.Name)
			for _, k := range sortedKeys(ct.Env) {
				s.scan(r.ID, configValue{Key: k, Label: k, Value: ct.Env[k], Source: src + " env " + k})
			}
			for _, v := range argValues(ct.Command, "command", src) {
				s.scan(r.ID, v)
			}
			for _, v := range argValues(ct.EntryPoint, "entryPoint", src) {
				s.scan(r.ID, v)
			}
		}
	})

	Each(c, spec.TypeLambdaFunction, func(r inventory.Resource, fn *spec.LambdaFunction) {
		if fn.EnvUnreadable != "" {
			c.Finding("unscanned", r.ID, "environment", fmt.Sprintf(
				"environment variables could not be read (%s): references in them were not scanned", fn.EnvUnreadable))
		}
		src := "lambda:ListFunctions " + fn.FunctionName + " env "
		for _, k := range sortedKeys(fn.Env) {
			s.scan(r.ID, configValue{Key: k, Label: k, Value: fn.Env[k], Source: src + k})
		}
	})

	// A stage variable an integration URI uses was already interpreted by
	// apigw.integration, per stage. The rest are handed to the API's backends
	// at request time; they are attributed to the API, which holds them.
	for _, typ := range apiTypes {
		Each(c, typ, func(r inventory.Resource, api *spec.API) {
			used := map[string]bool{}
			for _, rt := range api.Routes {
				if rt.Integration != nil {
					for _, m := range stageVarRef.FindAllStringSubmatch(rt.Integration.URI, -1) {
						used[m[1]] = true
					}
				}
			}
			call := "apigatewayv2:GetStages"
			if typ == spec.TypeRESTAPI {
				call = "apigateway:GetStages"
			}
			for _, st := range api.Stages {
				for _, k := range sortedKeys(st.Variables) {
					if used[k] {
						continue
					}
					s.scan(r.ID, configValue{Key: k, Label: "stage variable " + k, Value: st.Variables[k],
						Source: fmt.Sprintf("%s %s stage %s variable %s", call, api.APIID, st.Name, k)})
				}
			}
		})
	}
}

// argValues turns a command line into values. --flag=value and --flag value
// carry the flag as their key, so a bare name after --table is read with the
// hint the flag gives; positional arguments have no key and only yield what
// names itself (a URL, an ARN).
func argValues(args []string, field, src string) []configValue {
	var out []configValue
	for i, a := range args {
		label := fmt.Sprintf("%s[%d]", field, i)
		v := configValue{Label: label, Value: a, Source: fmt.Sprintf("%s %s", src, label)}
		if k, val, ok := cutFlag(a); ok {
			v.Key, v.Value, v.Label = k, val, fmt.Sprintf("%s (%s)", a[:len(a)-len(val)-1], label)
		} else if i > 0 && isBareFlag(args[i-1]) && !isFlag(a) {
			v.Key, v.Label = trimDashes(args[i-1]), fmt.Sprintf("%s (%s)", args[i-1], label)
		}
		out = append(out, v)
	}
	return out
}

func isFlag(a string) bool     { return len(a) > 1 && a[0] == '-' }
func isBareFlag(a string) bool { _, _, ok := cutFlag(a); return isFlag(a) && !ok }
func trimDashes(a string) string {
	for len(a) > 0 && a[0] == '-' {
		a = a[1:]
	}
	return a
}

func cutFlag(a string) (key, value string, ok bool) {
	if !isFlag(a) {
		return "", "", false
	}
	for i := 0; i < len(a); i++ {
		if a[i] == '=' {
			return trimDashes(a[:i]), a[i+1:], true
		}
	}
	return "", "", false
}

// lookup decodes the spec of the resource with the given id. Like Each, a spec
// that does not decode is a collector bug, reported as a warning.
func lookup[T any](c *Context, id string) (*T, bool) {
	if id == "" {
		return nil, false
	}
	if c.byID == nil {
		c.byID = map[string]int{}
		for i, r := range c.inv.Resources {
			c.byID[r.ID] = i
		}
	}
	i, ok := c.byID[id]
	if !ok {
		return nil, false
	}
	var v T
	if err := json.Unmarshal(c.inv.Resources[i].Spec, &v); err != nil {
		c.b.warnings = append(c.b.warnings, fmt.Sprintf("%s: cannot decode %s spec: %v", c.rule, id, err))
		return nil, false
	}
	return &v, true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
