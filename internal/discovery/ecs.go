package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
)

// AWS batch limits. DescribeServices additionally requires every service in a
// call to belong to the same cluster, which is why services are batched per
// cluster rather than globally.
const (
	describeClustersBatch = 100
	describeServicesBatch = 10
)

// ECS collects clusters, services, and the task definitions they run.
//
// Task definitions are emitted as their own resources rather than inlined into
// services, for two reasons: several services can share one, and the linker's
// Tier-2 config scanning works on container environment variables, which live on
// the task definition. Keeping them separate means an env var is analysed once
// and its evidence points at the definition that actually declares it.
type ECS struct{}

func (*ECS) Service() string { return "ECS" }

func (c *ECS) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := ecs.NewFromConfig(s.Config())

	clusterARNs, err := c.listClusters(ctx, api)
	if err != nil {
		return warnOrFail(out, "ECS", "ListClusters", err)
	}

	// Task definitions are shared across services, so collect the set first and
	// describe each exactly once.
	taskDefs := make(map[string]struct{})

	for _, batch := range chunk(clusterARNs, describeClustersBatch) {
		resp, err := api.DescribeClusters(ctx, &ecs.DescribeClustersInput{
			Clusters: batch,
			Include:  []ecstypes.ClusterField{ecstypes.ClusterFieldTags},
		})
		if err != nil {
			if err := warnOrFail(out, "ECS", "DescribeClusters", err); err != nil {
				return err
			}
			continue
		}
		for _, f := range resp.Failures {
			out.Warn(failureWarning("DescribeClusters", f))
		}

		for _, cl := range resp.Clusters {
			name := aws.ToString(cl.ClusterName)
			out.Emit(newResource(s, resourceArgs{
				ID:   "ecs/cluster/" + name,
				Type: "ecs.cluster",
				ARN:  aws.ToString(cl.ClusterArn),
				Name: name,
				Tags: ecsTags(cl.Tags),
				API:  "ecs:DescribeClusters",
				Spec: clusterSpec{
					Name:               name,
					Status:             aws.ToString(cl.Status),
					ActiveServices:     cl.ActiveServicesCount,
					RunningTasks:       cl.RunningTasksCount,
					CapacityProviders:  cl.CapacityProviders,
					DefaultCapacityFor: capacityProviderNames(cl.DefaultCapacityProviderStrategy),
				},
				Raw: cl,
			}))

			if err := c.collectServices(ctx, api, s, out, name, taskDefs); err != nil {
				return err
			}
		}
	}

	return c.collectTaskDefinitions(ctx, api, s, out, taskDefs)
}

func (c *ECS) listClusters(ctx context.Context, api *ecs.Client) ([]string, error) {
	var arns []string
	p := ecs.NewListClustersPaginator(api, &ecs.ListClustersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		arns = append(arns, page.ClusterArns...)
	}
	return arns, nil
}

func (c *ECS) collectServices(
	ctx context.Context,
	api *ecs.Client,
	s *awsx.Session,
	out Emitter,
	cluster string,
	taskDefs map[string]struct{},
) error {
	var serviceARNs []string
	p := ecs.NewListServicesPaginator(api, &ecs.ListServicesInput{Cluster: aws.String(cluster)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return warnOrFail(out, "ECS", "ListServices", err)
		}
		serviceARNs = append(serviceARNs, page.ServiceArns...)
	}

	for _, batch := range chunk(serviceARNs, describeServicesBatch) {
		resp, err := api.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  aws.String(cluster),
			Services: batch,
			Include:  []ecstypes.ServiceField{ecstypes.ServiceFieldTags},
		})
		if err != nil {
			if err := warnOrFail(out, "ECS", "DescribeServices", err); err != nil {
				return err
			}
			continue
		}
		for _, f := range resp.Failures {
			out.Warn(failureWarning("DescribeServices", f))
		}

		for _, svc := range resp.Services {
			name := aws.ToString(svc.ServiceName)
			taskDef := aws.ToString(svc.TaskDefinition)
			if taskDef != "" {
				taskDefs[taskDef] = struct{}{}
			}

			out.Emit(newResource(s, resourceArgs{
				// Scoped by cluster because service names are only unique
				// within a cluster. A bare "ecs/<name>" would silently collapse
				// two different services in accounts that reuse names across
				// clusters — a class of bug that surfaces much later as a
				// mysteriously missing node. See docs/09-open-questions.md Q5.
				ID:   fmt.Sprintf("ecs/%s/%s", cluster, name),
				Type: "ecs.service",
				ARN:  aws.ToString(svc.ServiceArn),
				Name: name,
				Tags: ecsTags(svc.Tags),
				API:  "ecs:DescribeServices",
				Spec: serviceSpec{
					Cluster:            cluster,
					ServiceName:        name,
					Status:             aws.ToString(svc.Status),
					DesiredCount:       svc.DesiredCount,
					RunningCount:       svc.RunningCount,
					LaunchType:         string(svc.LaunchType),
					SchedulingStrategy: string(svc.SchedulingStrategy),
					TaskDefinition:     taskDef,
					TaskDefinitionID:   taskDefID(taskDef),
					RoleARN:            aws.ToString(svc.RoleArn),
					LoadBalancers:      loadBalancerSpecs(svc.LoadBalancers),
					Registries:         registryARNs(svc.ServiceRegistries),
					Network:            networkSpec(svc.NetworkConfiguration),
				},
				Raw: svc,
			}))
		}
	}
	return nil
}

func (c *ECS) collectTaskDefinitions(
	ctx context.Context,
	api *ecs.Client,
	s *awsx.Session,
	out Emitter,
	taskDefs map[string]struct{},
) error {
	// Sorted so a scan of an unchanged account issues its calls in the same
	// order every time, which keeps recorded fixtures stable.
	refs := make([]string, 0, len(taskDefs))
	for ref := range taskDefs {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	for _, ref := range refs {
		resp, err := api.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
			TaskDefinition: aws.String(ref),
			Include:        []ecstypes.TaskDefinitionField{ecstypes.TaskDefinitionFieldTags},
		})
		if err != nil {
			if err := warnOrFail(out, "ECS", "DescribeTaskDefinition", err); err != nil {
				return err
			}
			continue
		}

		td := resp.TaskDefinition
		if td == nil {
			continue
		}
		// Redact in place, before either the normalized spec or Raw is built,
		// so that no copy of a secret-shaped value survives into the inventory.
		redacted := redactContainerDefinitions(td.ContainerDefinitions)

		family := aws.ToString(td.Family)
		name := fmt.Sprintf("%s:%d", family, td.Revision)

		out.Emit(newResource(s, resourceArgs{
			ID:   "ecs/taskdef/" + name,
			Type: "ecs.taskdefinition",
			ARN:  aws.ToString(td.TaskDefinitionArn),
			Name: name,
			Tags: ecsTags(resp.Tags),
			API:  "ecs:DescribeTaskDefinition",
			Spec: taskDefinitionSpec{
				Family:           family,
				Revision:         td.Revision,
				NetworkMode:      string(td.NetworkMode),
				CPU:              aws.ToString(td.Cpu),
				Memory:           aws.ToString(td.Memory),
				TaskRoleARN:      aws.ToString(td.TaskRoleArn),
				ExecutionRoleARN: aws.ToString(td.ExecutionRoleArn),
				RequiresCompat:   compatibilities(td.RequiresCompatibilities),
				Containers:       containerSpecs(td.ContainerDefinitions, redacted),
			},
			Raw: td,
		}))
	}
	return nil
}

// ---------------------------------------------------------------- normalized specs

type clusterSpec struct {
	Name               string   `json:"name"`
	Status             string   `json:"status,omitempty"`
	ActiveServices     int32    `json:"activeServices"`
	RunningTasks       int32    `json:"runningTasks"`
	CapacityProviders  []string `json:"capacityProviders,omitempty"`
	DefaultCapacityFor []string `json:"defaultCapacityProviders,omitempty"`
}

type serviceSpec struct {
	Cluster            string             `json:"cluster"`
	ServiceName        string             `json:"serviceName"`
	Status             string             `json:"status,omitempty"`
	DesiredCount       int32              `json:"desiredCount"`
	RunningCount       int32              `json:"runningCount"`
	LaunchType         string             `json:"launchType,omitempty"`
	SchedulingStrategy string             `json:"schedulingStrategy,omitempty"`
	TaskDefinition     string             `json:"taskDefinition"`
	TaskDefinitionID   string             `json:"taskDefinitionId,omitempty"`
	RoleARN            string             `json:"roleArn,omitempty"`
	LoadBalancers      []loadBalancerSpec `json:"loadBalancers,omitempty"`
	Registries         []string           `json:"serviceRegistries,omitempty"`
	Network            *networkSpecT      `json:"network,omitempty"`
}

type loadBalancerSpec struct {
	TargetGroupARN string `json:"targetGroupArn,omitempty"`
	ContainerName  string `json:"containerName,omitempty"`
	ContainerPort  int32  `json:"containerPort,omitempty"`
}

type networkSpecT struct {
	Subnets        []string `json:"subnets,omitempty"`
	SecurityGroups []string `json:"securityGroups,omitempty"`
	PublicIP       string   `json:"assignPublicIp,omitempty"`
}

type taskDefinitionSpec struct {
	Family           string          `json:"family"`
	Revision         int32           `json:"revision"`
	NetworkMode      string          `json:"networkMode,omitempty"`
	CPU              string          `json:"cpu,omitempty"`
	Memory           string          `json:"memory,omitempty"`
	TaskRoleARN      string          `json:"taskRoleArn,omitempty"`
	ExecutionRoleARN string          `json:"executionRoleArn,omitempty"`
	RequiresCompat   []string        `json:"requiresCompatibilities,omitempty"`
	Containers       []containerSpec `json:"containers"`
}

type containerSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`

	// Essential mirrors the ECS field: if an essential container stops, the
	// whole task stops. The materializer needs it to decide what a failure
	// means locally.
	Essential bool `json:"essential"`

	Command    []string          `json:"command,omitempty"`
	EntryPoint []string          `json:"entryPoint,omitempty"`
	Env        map[string]string `json:"env,omitempty"`

	// Secrets records name → ARN, never a value. The ARN is a linking signal
	// (it names a Secrets Manager or SSM resource); the value never leaves AWS.
	// See docs/07-security.md, Guarantee 2.
	Secrets map[string]string `json:"secrets,omitempty"`

	PortMappings []portMapping `json:"portMappings,omitempty"`
	DependsOn    []string      `json:"dependsOn,omitempty"`
	LogGroup     string        `json:"logGroup,omitempty"`

	// Redacted lists what cloud-echo refused to copy out of AWS, e.g.
	// "env:DB_PASSWORD" or "command[2]". The planner uses it to generate a local
	// placeholder instead of an empty value, and it is how a user can tell a
	// redaction from a variable that was genuinely empty.
	Redacted []string `json:"redacted,omitempty"`
}

type portMapping struct {
	ContainerPort int32  `json:"containerPort"`
	HostPort      int32  `json:"hostPort,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Name          string `json:"name,omitempty"`
}

// ---------------------------------------------------------------- conversion

// redactContainerDefinitions removes secret-shaped values from env vars, command
// and entrypoint, mutating defs in place, and reports what it removed per
// container.
func redactContainerDefinitions(defs []ecstypes.ContainerDefinition) map[string][]string {
	out := map[string][]string{}
	for i := range defs {
		d := &defs[i]
		name := aws.ToString(d.Name)
		for j := range d.Environment {
			kv := &d.Environment[j]
			if v, red := redactValue(aws.ToString(kv.Name), aws.ToString(kv.Value)); red {
				kv.Value = aws.String(v)
				out[name] = append(out[name], "env:"+aws.ToString(kv.Name))
			}
		}
		var hit []int
		if d.Command, hit = redactArgs(d.Command); len(hit) > 0 {
			for _, n := range hit {
				out[name] = append(out[name], fmt.Sprintf("command[%d]", n))
			}
		}
		if d.EntryPoint, hit = redactArgs(d.EntryPoint); len(hit) > 0 {
			for _, n := range hit {
				out[name] = append(out[name], fmt.Sprintf("entryPoint[%d]", n))
			}
		}
		sort.Strings(out[name])
	}
	return out
}

func containerSpecs(defs []ecstypes.ContainerDefinition, redacted map[string][]string) []containerSpec {
	out := make([]containerSpec, 0, len(defs))
	for _, d := range defs {
		cs := containerSpec{
			Name:       aws.ToString(d.Name),
			Image:      aws.ToString(d.Image),
			Essential:  aws.ToBool(d.Essential),
			Command:    d.Command,
			EntryPoint: d.EntryPoint,
			Redacted:   redacted[aws.ToString(d.Name)],
		}

		if len(d.Environment) > 0 {
			cs.Env = make(map[string]string, len(d.Environment))
			for _, kv := range d.Environment {
				cs.Env[aws.ToString(kv.Name)] = aws.ToString(kv.Value)
			}
		}
		if len(d.Secrets) > 0 {
			cs.Secrets = make(map[string]string, len(d.Secrets))
			for _, sec := range d.Secrets {
				cs.Secrets[aws.ToString(sec.Name)] = aws.ToString(sec.ValueFrom)
			}
		}
		for _, pm := range d.PortMappings {
			cs.PortMappings = append(cs.PortMappings, portMapping{
				ContainerPort: aws.ToInt32(pm.ContainerPort),
				HostPort:      aws.ToInt32(pm.HostPort),
				Protocol:      string(pm.Protocol),
				Name:          aws.ToString(pm.Name),
			})
		}
		for _, dep := range d.DependsOn {
			cs.DependsOn = append(cs.DependsOn, aws.ToString(dep.ContainerName))
		}
		if d.LogConfiguration != nil {
			cs.LogGroup = d.LogConfiguration.Options["awslogs-group"]
		}

		out = append(out, cs)
	}
	return out
}

func loadBalancerSpecs(lbs []ecstypes.LoadBalancer) []loadBalancerSpec {
	out := make([]loadBalancerSpec, 0, len(lbs))
	for _, lb := range lbs {
		out = append(out, loadBalancerSpec{
			TargetGroupARN: aws.ToString(lb.TargetGroupArn),
			ContainerName:  aws.ToString(lb.ContainerName),
			ContainerPort:  aws.ToInt32(lb.ContainerPort),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func networkSpec(nc *ecstypes.NetworkConfiguration) *networkSpecT {
	if nc == nil || nc.AwsvpcConfiguration == nil {
		return nil
	}
	return &networkSpecT{
		Subnets:        nc.AwsvpcConfiguration.Subnets,
		SecurityGroups: nc.AwsvpcConfiguration.SecurityGroups,
		PublicIP:       string(nc.AwsvpcConfiguration.AssignPublicIp),
	}
}

func registryARNs(regs []ecstypes.ServiceRegistry) []string {
	out := make([]string, 0, len(regs))
	for _, r := range regs {
		out = append(out, aws.ToString(r.RegistryArn))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func capacityProviderNames(strat []ecstypes.CapacityProviderStrategyItem) []string {
	out := make([]string, 0, len(strat))
	for _, s := range strat {
		out = append(out, aws.ToString(s.CapacityProvider))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func compatibilities(cs []ecstypes.Compatibility) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, string(c))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func ecsTags(tags []ecstypes.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// taskDefID converts a task definition ARN or "family:revision" reference into
// the inventory ID of the corresponding resource, so the linker can follow it
// without re-parsing ARNs.
func taskDefID(ref string) string {
	if ref == "" {
		return ""
	}
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	return "ecs/taskdef/" + ref
}

func failureWarning(op string, f ecstypes.Failure) inventory.Warning {
	return inventory.Warning{
		Service: "ECS",
		Op:      op,
		Kind:    "api-error",
		Message: fmt.Sprintf("%s: %s (%s)",
			aws.ToString(f.Arn), aws.ToString(f.Reason), aws.ToString(f.Detail)),
	}
}

// ---------------------------------------------------------------- helpers

type resourceArgs struct {
	ID, Type, ARN, Name, API string
	Tags                     map[string]string
	Spec                     any
	Raw                      any
}

func newResource(s *awsx.Session, a resourceArgs) inventory.Resource {
	return inventory.Resource{
		ID:        a.ID,
		Type:      a.Type,
		ARN:       a.ARN,
		Region:    s.Region(),
		AccountID: s.AccountID(),
		Name:      a.Name,
		Tags:      a.Tags,
		Spec:      mustJSON(a.Spec),
		Raw:       mustJSON(a.Raw),
		Source: inventory.Provenance{
			API:         a.API,
			CollectedAt: time.Now().UTC(),
		},
	}
}

// mustJSON marshals a value that is known to be marshalable. A failure here is a
// programming error in a spec struct, not a runtime condition, so it is recorded
// inline rather than silently dropped.
func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(fmt.Sprintf("{%q:%q}", "marshalError", err.Error()))
	}
	return b
}

func chunk[T any](in []T, size int) [][]T {
	if len(in) == 0 {
		return nil
	}
	var out [][]T
	for i := 0; i < len(in); i += size {
		end := min(i+size, len(in))
		out = append(out, in[i:end])
	}
	return out
}
