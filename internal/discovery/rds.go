package discovery

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// RDS collects database instances and clusters.
//
// Two calls, not the three the design listed: DescribeDBInstances embeds each
// instance's subnet group (VPC and subnets), so DescribeDBSubnetGroups would be
// a permission asked for and never needed. Tags arrive in both responses, and
// so do clusters' custom endpoints.
//
// The node for an Aurora or Multi-AZ DB cluster is the cluster: applications
// connect to its writer, reader or custom endpoints. Its instances are recorded
// as members — their own endpoints still name the cluster — rather than as
// resources that would each look like a separate database.
//
// The RDS API also serves DocumentDB and Neptune: DescribeDBInstances lists a
// Neptune graph database next to a Postgres one. Those are not RDS engines, and
// a node claiming to be a relational database would be wrong in every later
// stage, so they are reported and skipped.
type RDS struct{}

func (*RDS) Service() string { return "RDS" }

// notRDS are engines the RDS API returns that are other services.
var notRDS = map[string]string{"docdb": "DocumentDB", "neptune": "Neptune"}

func (c *RDS) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := rds.NewFromConfig(s.Config())

	var clusters []rdstypes.DBCluster
	cp := rds.NewDescribeDBClustersPaginator(api, &rds.DescribeDBClustersInput{})
	for cp.HasMorePages() {
		page, err := cp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "RDS", "DescribeDBClusters", err); err != nil {
				return err
			}
			break
		}
		clusters = append(clusters, page.DBClusters...)
	}

	var instances []rdstypes.DBInstance
	ip := rds.NewDescribeDBInstancesPaginator(api, &rds.DescribeDBInstancesInput{})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, "RDS", "DescribeDBInstances", err); err != nil {
				return err
			}
			break
		}
		instances = append(instances, page.DBInstances...)
	}

	members := map[string][]rdstypes.DBInstance{}
	for _, in := range instances {
		if svc, ok := notRDS[aws.ToString(in.Engine)]; ok {
			if aws.ToString(in.DBClusterIdentifier) == "" {
				skipped(out, svc, "instance", aws.ToString(in.DBInstanceIdentifier))
			}
			continue
		}
		if cl := aws.ToString(in.DBClusterIdentifier); cl != "" {
			members[cl] = append(members[cl], in)
			continue
		}
		c.emitInstance(s, out, in)
	}
	for _, cl := range clusters {
		if svc, ok := notRDS[aws.ToString(cl.Engine)]; ok {
			skipped(out, svc, "cluster", aws.ToString(cl.DBClusterIdentifier))
			continue
		}
		c.emitCluster(s, out, cl, members[aws.ToString(cl.DBClusterIdentifier)])
	}
	return nil
}

func skipped(out Emitter, svc, what, id string) {
	out.Warn(inventory.Warning{
		Service: "RDS", Kind: "out-of-scope",
		Message: fmt.Sprintf("%s %s is %s, listed by the RDS API; cloud-echo has no %s collector", what, id, svc, svc),
	})
}

func (c *RDS) emitInstance(s *awsx.Session, out Emitter, in rdstypes.DBInstance) {
	id := aws.ToString(in.DBInstanceIdentifier)
	sp := spec.RDSInstance{
		Identifier:         id,
		Engine:             aws.ToString(in.Engine),
		EngineVersion:      aws.ToString(in.EngineVersion),
		Class:              aws.ToString(in.DBInstanceClass),
		Status:             aws.ToString(in.DBInstanceStatus),
		DBName:             aws.ToString(in.DBName),
		MasterUsername:     aws.ToString(in.MasterUsername),
		ResourceID:         aws.ToString(in.DbiResourceId),
		IAMAuth:            aws.ToBool(in.IAMDatabaseAuthenticationEnabled),
		MultiAZ:            aws.ToBool(in.MultiAZ),
		StorageType:        aws.ToString(in.StorageType),
		AllocatedGB:        aws.ToInt32(in.AllocatedStorage),
		Encrypted:          aws.ToBool(in.StorageEncrypted),
		PubliclyAccessible: aws.ToBool(in.PubliclyAccessible),
		ReplicaOf:          aws.ToString(in.ReadReplicaSourceDBInstanceIdentifier),
		Network:            instanceNetwork(in),
	}
	if e := in.Endpoint; e != nil {
		sp.Endpoint, sp.Port = aws.ToString(e.Address), aws.ToInt32(e.Port)
	}
	if ms := in.MasterUserSecret; ms != nil {
		sp.MasterSecretARN = aws.ToString(ms.SecretArn)
	}
	out.Emit(newResource(s, resourceArgs{
		// Instance identifiers are unique per account and region.
		ID:   "rds/" + id,
		Type: spec.TypeRDSInstance,
		ARN:  aws.ToString(in.DBInstanceArn),
		Name: id,
		Tags: rdsTags(in.TagList),
		API:  "rds:DescribeDBInstances",
		Spec: sp,
		Raw:  in,
	}))
}

func (c *RDS) emitCluster(s *awsx.Session, out Emitter, cl rdstypes.DBCluster, insts []rdstypes.DBInstance) {
	id := aws.ToString(cl.DBClusterIdentifier)
	sp := spec.RDSCluster{
		Identifier:            id,
		Engine:                aws.ToString(cl.Engine),
		EngineVersion:         aws.ToString(cl.EngineVersion),
		EngineMode:            aws.ToString(cl.EngineMode),
		Status:                aws.ToString(cl.Status),
		Endpoint:              aws.ToString(cl.Endpoint),
		ReaderEndpoint:        aws.ToString(cl.ReaderEndpoint),
		CustomEndpoints:       append([]string(nil), cl.CustomEndpoints...),
		Port:                  aws.ToInt32(cl.Port),
		DBName:                aws.ToString(cl.DatabaseName),
		MasterUsername:        aws.ToString(cl.MasterUsername),
		ResourceID:            aws.ToString(cl.DbClusterResourceId),
		IAMAuth:               aws.ToBool(cl.IAMDatabaseAuthenticationEnabled),
		DataAPI:               aws.ToBool(cl.HttpEndpointEnabled),
		MultiAZ:               aws.ToBool(cl.MultiAZ),
		Encrypted:             aws.ToBool(cl.StorageEncrypted),
		InternetAccessGateway: aws.ToBool(cl.InternetAccessGatewayEnabled),
		Members:               []spec.DBMember{},
	}
	sort.Strings(sp.CustomEndpoints)
	if ms := cl.MasterUserSecret; ms != nil {
		sp.MasterSecretARN = aws.ToString(ms.SecretArn)
	}
	if sv := cl.ServerlessV2ScalingConfiguration; sv != nil {
		sp.Serverless = &spec.DBServerless{
			MinACU: aws.ToFloat64(sv.MinCapacity), MaxACU: aws.ToFloat64(sv.MaxCapacity),
			AutoPauseSecs: aws.ToInt32(sv.SecondsUntilAutoPause),
		}
	}

	// Membership and the writer come from the cluster; class, status and
	// endpoint from the instances. A member the instance listing did not
	// return (denied, or not yet visible) is kept with what the cluster says.
	byID := map[string]rdstypes.DBInstance{}
	for _, in := range insts {
		byID[aws.ToString(in.DBInstanceIdentifier)] = in
	}
	var net *spec.DBNetwork
	for _, m := range cl.DBClusterMembers {
		mid := aws.ToString(m.DBInstanceIdentifier)
		dm := spec.DBMember{Identifier: mid, Writer: aws.ToBool(m.IsClusterWriter)}
		if in, ok := byID[mid]; ok {
			dm.Class, dm.Status = aws.ToString(in.DBInstanceClass), aws.ToString(in.DBInstanceStatus)
			if e := in.Endpoint; e != nil {
				dm.Endpoint = aws.ToString(e.Address)
			}
			if net == nil {
				net = instanceNetwork(in)
			}
		}
		sp.Members = append(sp.Members, dm)
	}
	sort.Slice(sp.Members, func(i, j int) bool { return sp.Members[i].Identifier < sp.Members[j].Identifier })

	// A cluster's security groups are its own; the VPC and subnets are only on
	// its instances' subnet group.
	var sgs []string
	for _, g := range cl.VpcSecurityGroups {
		sgs = append(sgs, aws.ToString(g.VpcSecurityGroupId))
	}
	if net != nil || len(sgs) > 0 || aws.ToString(cl.DBSubnetGroup) != "" {
		if net == nil {
			net = &spec.DBNetwork{}
		}
		net.SecurityGroups = sgs
		sort.Strings(net.SecurityGroups)
		if g := aws.ToString(cl.DBSubnetGroup); g != "" {
			net.SubnetGroup = g
		}
		sp.Network = net
	}

	out.Emit(newResource(s, resourceArgs{
		// Cluster identifiers are unique per account and region, in their own
		// namespace: "rds/cluster/" keeps an instance of the same name apart.
		ID:   "rds/cluster/" + id,
		Type: spec.TypeRDSCluster,
		ARN:  aws.ToString(cl.DBClusterArn),
		Name: id,
		Tags: rdsTags(cl.TagList),
		API:  "rds:DescribeDBClusters",
		Spec: sp,
		Raw:  map[string]any{"cluster": cl, "members": insts},
	}))
}

func instanceNetwork(in rdstypes.DBInstance) *spec.DBNetwork {
	n := &spec.DBNetwork{}
	if g := in.DBSubnetGroup; g != nil {
		n.VpcID, n.SubnetGroup = aws.ToString(g.VpcId), aws.ToString(g.DBSubnetGroupName)
		for _, sn := range g.Subnets {
			n.Subnets = append(n.Subnets, aws.ToString(sn.SubnetIdentifier))
		}
		sort.Strings(n.Subnets)
	}
	for _, sg := range in.VpcSecurityGroups {
		n.SecurityGroups = append(n.SecurityGroups, aws.ToString(sg.VpcSecurityGroupId))
	}
	sort.Strings(n.SecurityGroups)
	if n.VpcID == "" && n.SubnetGroup == "" && len(n.Subnets) == 0 && len(n.SecurityGroups) == 0 {
		return nil
	}
	return n
}

func rdsTags(tags []rdstypes.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}
