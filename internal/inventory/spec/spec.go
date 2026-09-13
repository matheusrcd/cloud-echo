// Package spec holds the normalized, type-specific shapes collectors write into
// inventory.Resource.Spec and every later stage reads.
//
// It is the contract between discovery and the linker (and, later, the
// planner). Defining it once is the point: if the linker redeclared the fields
// it reads, a rename on one side would silently break the other, and the first
// sign would be a wrong graph. The package has no AWS dependency; an offline
// stage can import it without pulling in a collector.
package spec

import "encoding/json"

// Resource types, as written to inventory.Resource.Type.
const (
	TypeECSCluster         = "ecs.cluster"
	TypeECSService         = "ecs.service"
	TypeECSTaskDefinition  = "ecs.taskdefinition"
	TypeSQSQueue           = "sqs.queue"
	TypeDynamoDBTable      = "dynamodb.table"
	TypeLambdaFunction     = "lambda.function"
	TypeEventSourceMapping = "lambda.event-source-mapping"
	TypeIAMRole            = "iam.role"
	TypeIAMPolicy          = "iam.policy"
	TypeRESTAPI            = "apigateway.rest"
	TypeHTTPAPI            = "apigateway.http"
	TypeWebSocketAPI       = "apigateway.websocket"
	TypeRDSInstance        = "rds.instance"
	TypeRDSCluster         = "rds.cluster"
	TypeCache              = "elasticache.cache"
	TypeLoadBalancer       = "elbv2.load-balancer"
	TypeTargetGroup        = "elbv2.target-group"
)

// Types lists every type a collector may emit. A test in discovery checks
// collectors against it, so a new type cannot appear without the contract
// knowing about it.
var Types = []string{
	TypeECSCluster, TypeECSService, TypeECSTaskDefinition, TypeSQSQueue, TypeDynamoDBTable,
	TypeLambdaFunction, TypeEventSourceMapping, TypeIAMRole, TypeIAMPolicy,
	TypeRESTAPI, TypeHTTPAPI, TypeWebSocketAPI, TypeRDSInstance, TypeRDSCluster, TypeCache,
	TypeLoadBalancer, TypeTargetGroup,
}

type API struct {
	APIID    string `json:"apiId"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"` // REST | HTTP | WEBSOCKET

	// EndpointType is EDGE, REGIONAL or PRIVATE for REST APIs; v2 APIs are
	// always regional.
	EndpointType string `json:"endpointType,omitempty"`

	// Endpoint is the default invoke URL — a Tier-2 signal when another
	// workload's config contains it. Empty when the default endpoint is
	// disabled and only custom domains serve the API.
	Endpoint                string `json:"endpoint,omitempty"`
	DefaultEndpointDisabled bool   `json:"defaultEndpointDisabled,omitempty"`

	Routes      []Route      `json:"routes"`
	Stages      []Stage      `json:"stages,omitempty"`
	Authorizers []Authorizer `json:"authorizers,omitempty"`

	// ResourcePolicy is the REST API's resource policy, decoded (see
	// decodeRestAPIPolicy). v2 APIs have none.
	ResourcePolicy json.RawMessage `json:"resourcePolicy,omitempty"`

	// Redacted lists what was withheld, e.g. "stage:prod:DB_PASSWORD" or
	// "route:POST /webhooks:integration.request.header.x-api-key".
	Redacted []string `json:"redacted,omitempty"`
}

type Route struct {
	// RouteKey is "METHOD /path" for REST and HTTP APIs (synthesized for
	// REST), or a bare key such as "$default" or "$connect".
	RouteKey       string       `json:"routeKey"`
	Method         string       `json:"method,omitempty"`
	Path           string       `json:"path,omitempty"`
	Authorization  string       `json:"authorization,omitempty"` // NONE | AWS_IAM | CUSTOM | COGNITO_USER_POOLS | JWT
	AuthorizerID   string       `json:"authorizerId,omitempty"`
	APIKeyRequired bool         `json:"apiKeyRequired,omitempty"`
	Integration    *Integration `json:"integration,omitempty"`
}

type Integration struct {
	// ID is set for v2 integrations, which several routes can share.
	ID string `json:"id,omitempty"`

	Type    string `json:"type"`              // AWS_PROXY | AWS | HTTP | HTTP_PROXY | MOCK
	Subtype string `json:"subtype,omitempty"` // v2 service integrations, e.g. SQS-SendMessage
	URI     string `json:"uri,omitempty"`

	HTTPMethod     string `json:"httpMethod,omitempty"`
	ConnectionType string `json:"connectionType,omitempty"` // INTERNET | VPC_LINK
	ConnectionID   string `json:"connectionId,omitempty"`

	// Credentials is the role API Gateway assumes to call the target — an
	// application identity, so the IAM collector reads it.
	Credentials string `json:"credentials,omitempty"`

	// Service and Action describe what the integration calls ("lambda",
	// "sqs"/"SendMessage", "http"), and Target the resource when it can be
	// named from the definition alone.
	Service   string     `json:"service,omitempty"`
	Action    string     `json:"action,omitempty"`
	Target    *TargetRef `json:"target,omitempty"`
	Qualifier string     `json:"qualifier,omitempty"`

	// Templated is set when the URI depends on stage variables
	// (…function:${stageVariables.fn}…). The target cannot be resolved from
	// the definition alone, and a guessed one would be a node that does not
	// exist.
	Templated bool `json:"templated,omitempty"`

	RequestParameters    map[string]string `json:"requestParameters,omitempty"`
	TemplateContentTypes []string          `json:"templateContentTypes,omitempty"`
	TimeoutMs            int32             `json:"timeoutMs,omitempty"`
	PayloadFormat        string            `json:"payloadFormatVersion,omitempty"`
}

type Stage struct {
	Name         string            `json:"name"`
	DeploymentID string            `json:"deploymentId,omitempty"`
	AutoDeploy   bool              `json:"autoDeploy,omitempty"`
	Variables    map[string]string `json:"variables,omitempty"`
}

type Authorizer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // TOKEN | REQUEST | COGNITO_USER_POOLS | JWT

	// Function is the Lambda that authorizes requests — in the request path, so
	// a synchronous edge for the linker.
	Function  *TargetRef `json:"function,omitempty"`
	Qualifier string     `json:"qualifier,omitempty"`

	IdentitySource []string `json:"identitySource,omitempty"`
	Providers      []string `json:"providerArns,omitempty"` // Cognito user pools
	JWTIssuer      string   `json:"jwtIssuer,omitempty"`
	JWTAudience    []string `json:"jwtAudience,omitempty"`
	Credentials    string   `json:"credentials,omitempty"`
}

type DynamoDBTable struct {
	TableName   string    `json:"tableName"`
	KeySchema   []KeyPart `json:"keySchema"`
	BillingMode string    `json:"billingMode,omitempty"`

	// Throughput is set only for PROVISIONED tables. A round trip through Floci
	// had to read it from Raw because the spec lacked it, and a provisioned
	// table cannot be created without it.
	Throughput *Throughput `json:"provisionedThroughput,omitempty"`
	GSIs       []Index     `json:"globalSecondaryIndexes,omitempty"`
	LSIs       []Index     `json:"localSecondaryIndexes,omitempty"`
	Stream     *Stream     `json:"stream,omitempty"`
	TTL        *TTL        `json:"ttl,omitempty"`

	// ItemCount is AWS's own estimate, updated roughly every six hours. It is
	// recorded for sizing hints only and is never treated as exact.
	ItemCount int64 `json:"itemCountEstimate,omitempty"`
}

type KeyPart struct {
	Name string `json:"name"`
	Type string `json:"type"` // S | N | B
	Role string `json:"role"` // HASH | RANGE
}

type Index struct {
	Name       string      `json:"name"`
	KeySchema  []KeyPart   `json:"keySchema"`
	Projection string      `json:"projection,omitempty"`
	NonKeyAttr []string    `json:"nonKeyAttributes,omitempty"`
	Throughput *Throughput `json:"provisionedThroughput,omitempty"`
}

type Throughput struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

type Stream struct {
	Enabled  bool   `json:"enabled"`
	ViewType string `json:"viewType,omitempty"`
	ARN      string `json:"arn,omitempty"`
}

type TTL struct {
	Enabled   bool   `json:"enabled"`
	Attribute string `json:"attribute,omitempty"`
}

type ECSCluster struct {
	Name               string   `json:"name"`
	Status             string   `json:"status,omitempty"`
	ActiveServices     int32    `json:"activeServices"`
	RunningTasks       int32    `json:"runningTasks"`
	CapacityProviders  []string `json:"capacityProviders,omitempty"`
	DefaultCapacityFor []string `json:"defaultCapacityProviders,omitempty"`
}

type ECSService struct {
	Cluster            string         `json:"cluster"`
	ServiceName        string         `json:"serviceName"`
	Status             string         `json:"status,omitempty"`
	DesiredCount       int32          `json:"desiredCount"`
	RunningCount       int32          `json:"runningCount"`
	LaunchType         string         `json:"launchType,omitempty"`
	SchedulingStrategy string         `json:"schedulingStrategy,omitempty"`
	TaskDefinition     string         `json:"taskDefinition"`
	TaskDefinitionID   string         `json:"taskDefinitionId,omitempty"`
	RoleARN            string         `json:"roleArn,omitempty"`
	LoadBalancers      []LoadBalancer `json:"loadBalancers,omitempty"`
	Registries         []string       `json:"serviceRegistries,omitempty"`
	Network            *Network       `json:"network,omitempty"`
}

type LoadBalancer struct {
	TargetGroupARN string `json:"targetGroupArn,omitempty"`
	ContainerName  string `json:"containerName,omitempty"`
	ContainerPort  int32  `json:"containerPort,omitempty"`
}

type Network struct {
	Subnets        []string `json:"subnets,omitempty"`
	SecurityGroups []string `json:"securityGroups,omitempty"`
	PublicIP       string   `json:"assignPublicIp,omitempty"`
}

type ECSTaskDefinition struct {
	Family           string      `json:"family"`
	Revision         int32       `json:"revision"`
	NetworkMode      string      `json:"networkMode,omitempty"`
	CPU              string      `json:"cpu,omitempty"`
	Memory           string      `json:"memory,omitempty"`
	TaskRoleARN      string      `json:"taskRoleArn,omitempty"`
	TaskRoleID       string      `json:"taskRoleId,omitempty"`
	ExecutionRoleARN string      `json:"executionRoleArn,omitempty"`
	RequiresCompat   []string    `json:"requiresCompatibilities,omitempty"`
	Containers       []Container `json:"containers"`
}

type Container struct {
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

	PortMappings []PortMapping `json:"portMappings,omitempty"`

	// DependsOn keeps the condition as well as the container: START, COMPLETE,
	// SUCCESS and HEALTHY order a task differently, and a round trip that knew
	// only the names had to guess.
	DependsOn []Dependency `json:"dependsOn,omitempty"`

	// LogGroup is the awslogs group, kept as a convenience for mapping
	// workloads to their logs. Log is the full configuration, needed to
	// recreate the container faithfully.
	LogGroup string `json:"logGroup,omitempty"`
	Log      *Log   `json:"log,omitempty"`

	// Redacted lists what cloud-echo refused to copy out of AWS, e.g.
	// "env:DB_PASSWORD" or "command[2]". The planner uses it to generate a local
	// placeholder instead of an empty value, and it is how a user can tell a
	// redaction from a variable that was genuinely empty.
	Redacted []string `json:"redacted,omitempty"`
}

type Dependency struct {
	Container string `json:"container"`
	Condition string `json:"condition"`
}

type Log struct {
	Driver string `json:"driver"`
	// Options are redacted like env vars: drivers such as splunk, datadog and
	// firelens outputs take credentials here (splunk-token, apikey).
	Options map[string]string `json:"options,omitempty"`
	// SecretOptions maps option name to a Secrets Manager or SSM ARN, never a
	// value — the same treatment as secrets[].
	SecretOptions map[string]string `json:"secretOptions,omitempty"`
}

type PortMapping struct {
	ContainerPort int32  `json:"containerPort"`
	HostPort      int32  `json:"hostPort,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Name          string `json:"name,omitempty"`
}

type IAMRole struct {
	RoleName string `json:"roleName"`
	Path     string `json:"path,omitempty"`

	// AssumedBy lists the workloads in this inventory running as the role. It
	// is why the role was collected at all.
	AssumedBy []string `json:"assumedBy"`

	TrustPolicy      json.RawMessage `json:"trustPolicy,omitempty"`
	InlinePolicies   []InlinePolicy  `json:"inlinePolicies,omitempty"`
	AttachedPolicies []TargetRef     `json:"attachedPolicies,omitempty"`

	// PermissionsBoundary caps what the role's policies can grant: effective
	// permission is their intersection. A role whose policy says dynamodb:* under
	// a boundary that allows only SQS cannot touch DynamoDB, and an edge inferred
	// from the policy alone would be wrong.
	PermissionsBoundary *TargetRef `json:"permissionsBoundary,omitempty"`

	// Unread lists the parts of the role the collector could not read: "inline
	// policies" (the list was refused), "inline policy <name>", "attached
	// policies". Without it a partly read role is indistinguishable from one
	// that grants nothing, and "this workload cannot touch that queue" would be
	// claimed from a blind spot.
	Unread []string `json:"unread,omitempty"`
}

type InlinePolicy struct {
	Name     string          `json:"name"`
	Document json.RawMessage `json:"document"`
}

type IAMPolicy struct {
	PolicyName     string          `json:"policyName"`
	Path           string          `json:"path,omitempty"`
	AWSManaged     bool            `json:"awsManaged"`
	DefaultVersion string          `json:"defaultVersion"`
	Document       json.RawMessage `json:"document"`
}

type LambdaFunction struct {
	FunctionName  string   `json:"functionName"`
	Runtime       string   `json:"runtime,omitempty"`
	Handler       string   `json:"handler,omitempty"`
	PackageType   string   `json:"packageType,omitempty"` // Zip | Image
	Architectures []string `json:"architectures,omitempty"`
	MemoryMB      int32    `json:"memoryMb"`
	TimeoutSec    int32    `json:"timeoutSec"`

	// RoleARN is the execution role, and for Lambda it *is* the application's
	// identity — the Tier-3 input for this function.
	RoleARN string `json:"roleArn"`
	RoleID  string `json:"roleId,omitempty"`

	Env           map[string]string `json:"env,omitempty"`
	Redacted      []string          `json:"redacted,omitempty"`
	EnvUnreadable string            `json:"envUnreadable,omitempty"`

	Layers     []string   `json:"layers,omitempty"`
	VPC        *VPC       `json:"vpc,omitempty"`
	DeadLetter *TargetRef `json:"deadLetter,omitempty"`
	LogGroup   string     `json:"logGroup,omitempty"`

	// ResourcePolicy names who may invoke the function — API Gateway, SNS, S3,
	// EventBridge. It is how a Lambda's non-queue triggers are found, and so how
	// entrypoints are classified. Kept raw; interpreting it is the linker's job.
	ResourcePolicy json.RawMessage `json:"resourcePolicy,omitempty"`
}

type EventSourceMapping struct {
	UUID  string `json:"uuid"`
	State string `json:"state"`

	// Enabled is whether the mapping delivers events, and is null when the
	// state cannot say: a mapping caught mid-update or mid-creation reports
	// "Updating" or "Creating" whether it is enabled or not. Scanned against a
	// real account, a live mapping whose batch size was being changed read as
	// disabled under a two-state rule — and the linker would have dropped a real
	// edge. Null tells it to keep the edge and say it is unsettled.
	Enabled      *bool `json:"enabled"`
	Transitional bool  `json:"transitional,omitempty"`

	BatchSize   int32 `json:"batchSize,omitempty"`
	BatchWindow int32 `json:"maxBatchingWindowSec,omitempty"`

	Source     TargetRef `json:"source"`
	SourceType string    `json:"sourceType"` // sqs | dynamodb-stream | kinesis | kafka | mq | unknown

	FunctionARN string `json:"functionArn"`
	FunctionID  string `json:"functionId,omitempty"`
	// Qualifier is the alias or version the mapping invokes, when it is not
	// $LATEST. See the known gap on the Lambda type.
	Qualifier string `json:"qualifier,omitempty"`

	StartingPos string     `json:"startingPosition,omitempty"`
	Filters     []string   `json:"filters,omitempty"`
	OnFailure   *TargetRef `json:"onFailure,omitempty"`
}

// TargetRef is an ARN plus the inventory id it maps to, when cloud-echo has a
// collector for that service. The id is derived from the ARN alone, so it is set
// even if the target was not in this scan — a dangling id is itself information
// (the target is out of scope, or was deleted).
type TargetRef struct {
	ARN string `json:"arn"`
	ID  string `json:"id,omitempty"`
}

type VPC struct {
	VpcID          string   `json:"vpcId"`
	Subnets        []string `json:"subnets,omitempty"`
	SecurityGroups []string `json:"securityGroups,omitempty"`
}

type SQSQueue struct {
	QueueName         string `json:"queueName"`
	URL               string `json:"url"`
	VisibilityTimeout int    `json:"visibilityTimeout"`
	MessageRetention  int    `json:"messageRetentionPeriod,omitempty"`
	DelaySeconds      int    `json:"delaySeconds,omitempty"`
	MaxMessageSize    int    `json:"maximumMessageSize,omitempty"`
	FIFO              bool   `json:"fifo,omitempty"`
	ContentDedup      bool   `json:"contentBasedDeduplication,omitempty"`

	// Redrive is the DLQ relationship, already parsed out of the JSON blob AWS
	// returns. Nil when the queue has no DLQ configured.
	Redrive *Redrive `json:"redrive,omitempty"`

	// Policy is the queue's access policy, kept raw. Its Principal entries name
	// who may send to this queue, which is a linking signal the linker will want
	// — but interpreting IAM policy shapes belongs there, not here.
	Policy json.RawMessage `json:"policy,omitempty"`
}

type Redrive struct {
	TargetARN string `json:"deadLetterTargetArn"`
	// TargetID is the inventory id of the DLQ, resolved from the ARN so the
	// linker can follow it without re-parsing.
	TargetID   string `json:"deadLetterTargetId,omitempty"`
	MaxReceive int    `json:"maxReceiveCount"`
}

// RDSInstance is a standalone database instance. An instance that belongs to an
// Aurora or Multi-AZ DB cluster is not a resource of its own: applications
// connect to the cluster's endpoints, so it is one of the cluster's Members.
type RDSInstance struct {
	Identifier    string `json:"identifier"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engineVersion"`
	Class         string `json:"instanceClass"`
	Status        string `json:"status"`

	// Endpoint is the host applications connect to — the Tier-2 signal. It is
	// empty while the instance is being created.
	Endpoint string `json:"endpoint,omitempty"`
	Port     int32  `json:"port,omitempty"`
	DBName   string `json:"dbName,omitempty"`

	MasterUsername string `json:"masterUsername,omitempty"`
	// MasterSecretARN names the Secrets Manager secret RDS manages for the
	// master password — never its value. A workload that reads it, or has it
	// injected, connects to this database.
	MasterSecretARN string `json:"masterSecretArn,omitempty"`
	// ResourceID is the DbiResourceId, which IAM database authentication
	// grants name instead of the identifier.
	ResourceID string `json:"resourceId,omitempty"`
	IAMAuth    bool   `json:"iamAuth,omitempty"`

	MultiAZ            bool   `json:"multiAz,omitempty"`
	StorageType        string `json:"storageType,omitempty"`
	AllocatedGB        int32  `json:"allocatedGb,omitempty"`
	Encrypted          bool   `json:"encrypted,omitempty"`
	PubliclyAccessible bool   `json:"publiclyAccessible,omitempty"`
	// ReplicaOf is the source instance of a read replica.
	ReplicaOf string     `json:"replicaOf,omitempty"`
	Network   *DBNetwork `json:"network,omitempty"`
}

// RDSCluster is an Aurora or Multi-AZ DB cluster.
type RDSCluster struct {
	Identifier    string `json:"identifier"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engineVersion"`
	EngineMode    string `json:"engineMode,omitempty"`
	Status        string `json:"status"`

	// Endpoint is the writer; ReaderEndpoint balances across readers;
	// CustomEndpoints are the user-defined ones. All three name this cluster,
	// and so do its members' own endpoints.
	Endpoint        string     `json:"endpoint,omitempty"`
	ReaderEndpoint  string     `json:"readerEndpoint,omitempty"`
	CustomEndpoints []string   `json:"customEndpoints,omitempty"`
	Port            int32      `json:"port,omitempty"`
	DBName          string     `json:"dbName,omitempty"`
	Members         []DBMember `json:"members"`

	MasterUsername  string `json:"masterUsername,omitempty"`
	MasterSecretARN string `json:"masterSecretArn,omitempty"`
	ResourceID      string `json:"resourceId,omitempty"` // DbClusterResourceId
	IAMAuth         bool   `json:"iamAuth,omitempty"`
	// DataAPI is whether rds-data:ExecuteStatement reaches the cluster over
	// HTTPS — a way in that needs no network route and no password.
	DataAPI bool `json:"dataApi,omitempty"`

	Serverless *DBServerless `json:"serverless,omitempty"`
	MultiAZ    bool          `json:"multiAz,omitempty"`
	Encrypted  bool          `json:"encrypted,omitempty"`
	// InternetAccessGateway is set for clusters outside any VPC (Aurora's
	// express configuration), reached through an AWS-managed gateway: no
	// subnets and no security groups apply.
	InternetAccessGateway bool       `json:"internetAccessGateway,omitempty"`
	Network               *DBNetwork `json:"network,omitempty"`
}

type DBMember struct {
	Identifier string `json:"identifier"`
	Class      string `json:"instanceClass,omitempty"`
	Writer     bool   `json:"writer,omitempty"`
	Status     string `json:"status,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
}

type DBServerless struct {
	MinACU        float64 `json:"minAcu"`
	MaxACU        float64 `json:"maxAcu"`
	AutoPauseSecs int32   `json:"autoPauseSecs,omitempty"`
}

type DBNetwork struct {
	VpcID          string   `json:"vpcId,omitempty"`
	SubnetGroup    string   `json:"subnetGroup,omitempty"`
	Subnets        []string `json:"subnets,omitempty"`
	SecurityGroups []string `json:"securityGroups,omitempty"`
}

// Cache is an ElastiCache cache, whichever of its three APIs describes it: a
// Valkey/Redis replication group, a serverless cache, or a standalone cache
// cluster (memcached, or a legacy Redis node outside any replication group).
// All three are one kind of node — something a workload connects to and keeps
// data in — and differ only in the shape of their endpoints.
type Cache struct {
	Kind          string `json:"kind"` // replication-group | serverless | cache-cluster
	Identifier    string `json:"identifier"`
	Engine        string `json:"engine"` // valkey | redis | memcached
	EngineVersion string `json:"engineVersion,omitempty"`
	Status        string `json:"status"`
	NodeType      string `json:"nodeType,omitempty"`

	// PrimaryEndpoint takes writes and ReaderEndpoint balances reads, for a
	// replication group without cluster mode and for a serverless cache.
	// ConfigurationEndpoint is how a client discovers the nodes of a sharded
	// replication group or a memcached cluster. Any of them, or a node's own
	// endpoint, names this cache.
	PrimaryEndpoint       string      `json:"primaryEndpoint,omitempty"`
	ReaderEndpoint        string      `json:"readerEndpoint,omitempty"`
	ConfigurationEndpoint string      `json:"configurationEndpoint,omitempty"`
	Port                  int32       `json:"port,omitempty"`
	Nodes                 []CacheNode `json:"nodes,omitempty"`

	ClusterMode bool `json:"clusterMode,omitempty"`
	MultiAZ     bool `json:"multiAz,omitempty"`
	// TransitEncryption means clients must speak TLS (rediss://) — a local
	// cache that does not would reject them, or they it.
	TransitEncryption bool `json:"transitEncryption,omitempty"`
	// AuthToken is whether AUTH is required; the token is never read.
	AuthToken  bool          `json:"authToken,omitempty"`
	UserGroups []string      `json:"userGroups,omitempty"`
	Limits     *CacheLimits  `json:"limits,omitempty"`
	Network    *CacheNetwork `json:"network,omitempty"`
}

type CacheNode struct {
	ID       string `json:"id"`
	Role     string `json:"role,omitempty"` // primary | replica, for replication groups
	Endpoint string `json:"endpoint,omitempty"`
}

// CacheLimits are a serverless cache's caps.
type CacheLimits struct {
	MaxStorageGB     int32 `json:"maxStorageGb,omitempty"`
	MaxECPUPerSecond int32 `json:"maxEcpuPerSecond,omitempty"`
}

// CacheNetwork records what reachability needs. A replication group or cache
// cluster names its subnet group (the subnet ids are one more call away, and
// nothing reads them yet); a serverless cache lists its subnets directly.
//
// No security group is not no security group: a cache cluster created without
// one runs under its VPC's default group, and DescribeCacheClusters lists none.
// Seen on the validation account; reachability (Tier 4) must read an empty list
// as "the default group".
type CacheNetwork struct {
	SubnetGroup    string   `json:"subnetGroup,omitempty"`
	Subnets        []string `json:"subnets,omitempty"`
	SecurityGroups []string `json:"securityGroups,omitempty"`
}

// ELB is an Application or Network Load Balancer. It is a node, like an API:
// it runs nothing and holds nothing, but it is where traffic enters and how it
// is routed — an internet-facing one is an entrypoint — and its listeners are
// its definition, which a local environment has to reproduce.
type ELB struct {
	Name           string        `json:"name"`
	Type           string        `json:"type"`   // application | network
	Scheme         string        `json:"scheme"` // internet-facing | internal
	State          string        `json:"state"`
	DNSName        string        `json:"dnsName"`
	VpcID          string        `json:"vpcId,omitempty"`
	Subnets        []string      `json:"subnets,omitempty"`
	SecurityGroups []string      `json:"securityGroups,omitempty"`
	Listeners      []ELBListener `json:"listeners"`
}

type ELBListener struct {
	// ARN is what an API Gateway VPC link integration names: with two
	// listeners, the load balancer alone does not say which one (found
	// rebuilding the ELBv2 round's topology in Floci).
	ARN          string   `json:"arn"`
	Port         int32    `json:"port"`
	Protocol     string   `json:"protocol"`
	Certificates []string `json:"certificates,omitempty"` // ARNs
	// Rules, highest priority first and the default last. A Network Load
	// Balancer's listener has only its default.
	Rules []ELBRule `json:"rules"`
}

type ELBRule struct {
	Priority   string         `json:"priority"` // "10" … or "default"
	Conditions []ELBCondition `json:"conditions,omitempty"`
	Actions    []ELBAction    `json:"actions"`
}

// ELBCondition is one match of a rule. Header and query-string values are
// redacted like env vars: a shared secret in a header is how a CDN proves to
// the load balancer that a request came through it.
type ELBCondition struct {
	Field  string   `json:"field"`            // path-pattern | host-header | http-header | query-string | http-request-method | source-ip
	Header string   `json:"header,omitempty"` // for http-header
	Values []string `json:"values,omitempty"`
}

type ELBAction struct {
	Type         string       `json:"type"` // forward | redirect | fixed-response | authenticate-oidc | authenticate-cognito | jwt-validation
	TargetGroups []ELBForward `json:"targetGroups,omitempty"`
	Redirect     string       `json:"redirect,omitempty"`
	// A fixed response's status and content type; its body is free text and
	// withheld, like an API Gateway mapping template.
	FixedStatus string `json:"fixedStatus,omitempty"`
	// Issuer names the identity provider an authenticate action sends users
	// to; the client secret is never recorded.
	Issuer string `json:"issuer,omitempty"`
}

type ELBForward struct {
	ARN    string `json:"arn"`
	Weight int32  `json:"weight,omitempty"`
}

// TargetGroup is where a load balancer sends traffic. It is configuration,
// not a node: what receives the traffic is. ECS services name their target
// groups themselves (ECSService.LoadBalancers); Lambda and ALB targets are
// listed here, because only the target group knows them.
type TargetGroup struct {
	Name             string      `json:"name"`
	TargetType       string      `json:"targetType"` // ip | instance | lambda | alb
	Protocol         string      `json:"protocol,omitempty"`
	Port             int32       `json:"port,omitempty"`
	VpcID            string      `json:"vpcId,omitempty"`
	HealthCheckPath  string      `json:"healthCheckPath,omitempty"`
	LoadBalancerARNs []string    `json:"loadBalancerArns,omitempty"`
	Targets          []TargetRef `json:"targets,omitempty"`
}
