package discovery

import "github.com/matheusrcd/cloud-echo/internal/inventory/spec"

// The normalized spec types live in internal/inventory/spec, the contract
// between discovery and the stages that read the inventory. These aliases keep
// the collectors — and their tests — reading as they did before the move.
type (
	apiSpec            = spec.API
	authorizerSpec     = spec.Authorizer
	clusterSpec        = spec.ECSCluster
	containerSpec      = spec.Container
	dependency         = spec.Dependency
	functionSpec       = spec.LambdaFunction
	indexSpec          = spec.Index
	inlinePolicy       = spec.InlinePolicy
	integrationSpec    = spec.Integration
	keyPart            = spec.KeyPart
	loadBalancerSpec   = spec.LoadBalancer
	logSpec            = spec.Log
	mappingSpec        = spec.EventSourceMapping
	networkSpecT       = spec.Network
	policySpec         = spec.IAMPolicy
	portMapping        = spec.PortMapping
	queueSpec          = spec.SQSQueue
	redriveSpec        = spec.Redrive
	roleSpec           = spec.IAMRole
	routeSpec          = spec.Route
	serviceSpec        = spec.ECSService
	stageSpec          = spec.Stage
	streamSpec         = spec.Stream
	tableSpec          = spec.DynamoDBTable
	targetRef          = spec.TargetRef
	taskDefinitionSpec = spec.ECSTaskDefinition
	throughputSpec     = spec.Throughput
	ttlSpec            = spec.TTL
	vpcSpec            = spec.VPC
)

// The id-scheme helpers moved with the types; these keep call sites unchanged.
var (
	resourceIDFromARN   = spec.IDFromARN
	functionNameFromARN = spec.FunctionFromARN
	lambdaFromInvokeURI = spec.LambdaFromInvokeURI
	queueTargetFromURL  = spec.QueueFromURL
)
