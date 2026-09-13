package discovery

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
)

// DynamoDB collects tables, their key schemas, and their streams.
//
// The key schema is not decoration: it is the minimum needed to recreate a table
// locally with the same shape, and getting an attribute type wrong produces a
// table that accepts writes in production and rejects them locally — the worst
// possible failure for a tool whose promise is fidelity.
type DynamoDB struct{}

func (*DynamoDB) Service() string { return "DynamoDB" }

func (c *DynamoDB) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := dynamodb.NewFromConfig(s.Config())

	var names []string
	p := dynamodb.NewListTablesPaginator(api, &dynamodb.ListTablesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return warnOrFail(out, "DynamoDB", "ListTables", err)
		}
		names = append(names, page.TableNames...)
	}

	for _, name := range names {
		desc, err := api.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
		if err != nil {
			if err := warnOrFail(out, "DynamoDB", "DescribeTable", err); err != nil {
				return err
			}
			continue
		}
		t := desc.Table
		if t == nil {
			continue
		}
		arn := aws.ToString(t.TableArn)

		spec := tableSpec{
			TableName:   name,
			KeySchema:   keySchema(t.KeySchema, t.AttributeDefinitions),
			BillingMode: billingMode(t),
			GSIs:        globalIndexes(t.GlobalSecondaryIndexes, t.AttributeDefinitions),
			Throughput:  throughput(t.ProvisionedThroughput),
			LSIs:        localIndexes(t.LocalSecondaryIndexes, t.AttributeDefinitions),
			ItemCount:   aws.ToInt64(t.ItemCount),
		}
		if t.StreamSpecification != nil && aws.ToBool(t.StreamSpecification.StreamEnabled) {
			spec.Stream = &streamSpec{
				Enabled:  true,
				ViewType: string(t.StreamSpecification.StreamViewType),
				ARN:      aws.ToString(t.LatestStreamArn),
			}
		}

		// TTL is a separate call because DescribeTable does not return it. It
		// matters locally: a table whose TTL attribute is missing will keep rows
		// the real one would have expired.
		if ttl, err := api.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{
			TableName: aws.String(name),
		}); err != nil {
			if err := warnOrFail(out, "DynamoDB", "DescribeTimeToLive", err); err != nil {
				return err
			}
		} else if d := ttl.TimeToLiveDescription; d != nil {
			switch d.TimeToLiveStatus {
			case ddbtypes.TimeToLiveStatusEnabled, ddbtypes.TimeToLiveStatusEnabling:
				spec.TTL = &ttlSpec{Enabled: true, Attribute: aws.ToString(d.AttributeName)}
			}
		}

		var tags map[string]string
		if tagOut, err := api.ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{
			ResourceArn: aws.String(arn),
		}); err != nil {
			if err := warnOrFail(out, "DynamoDB", "ListTagsOfResource", err); err != nil {
				return err
			}
		} else {
			tags = ddbTags(tagOut.Tags)
		}

		out.Emit(newResource(s, resourceArgs{
			// Table names are unique per account and region.
			ID:   "ddb/" + name,
			Type: "dynamodb.table",
			ARN:  arn,
			Name: name,
			Tags: tags,
			API:  "dynamodb:DescribeTable",
			Spec: spec,
			Raw:  t,
		}))
	}
	return nil
}

type tableSpec struct {
	TableName   string    `json:"tableName"`
	KeySchema   []keyPart `json:"keySchema"`
	BillingMode string    `json:"billingMode,omitempty"`

	// Throughput is set only for PROVISIONED tables. A round trip through Floci
	// had to read it from Raw because the spec lacked it, and a provisioned
	// table cannot be created without it.
	Throughput *throughputSpec `json:"provisionedThroughput,omitempty"`
	GSIs       []indexSpec     `json:"globalSecondaryIndexes,omitempty"`
	LSIs       []indexSpec     `json:"localSecondaryIndexes,omitempty"`
	Stream     *streamSpec     `json:"stream,omitempty"`
	TTL        *ttlSpec        `json:"ttl,omitempty"`

	// ItemCount is AWS's own estimate, updated roughly every six hours. It is
	// recorded for sizing hints only and is never treated as exact.
	ItemCount int64 `json:"itemCountEstimate,omitempty"`
}

type keyPart struct {
	Name string `json:"name"`
	Type string `json:"type"` // S | N | B
	Role string `json:"role"` // HASH | RANGE
}

type indexSpec struct {
	Name       string          `json:"name"`
	KeySchema  []keyPart       `json:"keySchema"`
	Projection string          `json:"projection,omitempty"`
	NonKeyAttr []string        `json:"nonKeyAttributes,omitempty"`
	Throughput *throughputSpec `json:"provisionedThroughput,omitempty"`
}

type throughputSpec struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

// throughput reports capacity only when it is real. On-demand tables still
// return a ProvisionedThroughput block, zeroed, which would read as "provisioned
// at 0" if copied through.
func throughput(pt *ddbtypes.ProvisionedThroughputDescription) *throughputSpec {
	if pt == nil || (aws.ToInt64(pt.ReadCapacityUnits) == 0 && aws.ToInt64(pt.WriteCapacityUnits) == 0) {
		return nil
	}
	return &throughputSpec{Read: aws.ToInt64(pt.ReadCapacityUnits), Write: aws.ToInt64(pt.WriteCapacityUnits)}
}

type streamSpec struct {
	Enabled  bool   `json:"enabled"`
	ViewType string `json:"viewType,omitempty"`
	ARN      string `json:"arn,omitempty"`
}

type ttlSpec struct {
	Enabled   bool   `json:"enabled"`
	Attribute string `json:"attribute,omitempty"`
}

// keySchema joins the key schema with the attribute definitions.
//
// AWS returns these as two separate lists — the schema names the attributes and
// their roles, the definitions carry their types — and a local table cannot be
// created from either half alone.
func keySchema(ks []ddbtypes.KeySchemaElement, defs []ddbtypes.AttributeDefinition) []keyPart {
	types := make(map[string]string, len(defs))
	for _, d := range defs {
		types[aws.ToString(d.AttributeName)] = string(d.AttributeType)
	}
	out := make([]keyPart, 0, len(ks))
	for _, k := range ks {
		name := aws.ToString(k.AttributeName)
		out = append(out, keyPart{Name: name, Type: types[name], Role: string(k.KeyType)})
	}
	return out
}

func globalIndexes(idx []ddbtypes.GlobalSecondaryIndexDescription, defs []ddbtypes.AttributeDefinition) []indexSpec {
	out := make([]indexSpec, 0, len(idx))
	for _, i := range idx {
		out = append(out, indexSpec{
			Name:       aws.ToString(i.IndexName),
			KeySchema:  keySchema(i.KeySchema, defs),
			Projection: projectionType(i.Projection),
			NonKeyAttr: nonKeyAttrs(i.Projection),
			Throughput: throughput(i.ProvisionedThroughput),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func localIndexes(idx []ddbtypes.LocalSecondaryIndexDescription, defs []ddbtypes.AttributeDefinition) []indexSpec {
	out := make([]indexSpec, 0, len(idx))
	for _, i := range idx {
		out = append(out, indexSpec{
			Name:       aws.ToString(i.IndexName),
			KeySchema:  keySchema(i.KeySchema, defs),
			Projection: projectionType(i.Projection),
			NonKeyAttr: nonKeyAttrs(i.Projection),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func projectionType(p *ddbtypes.Projection) string {
	if p == nil {
		return ""
	}
	return string(p.ProjectionType)
}

func nonKeyAttrs(p *ddbtypes.Projection) []string {
	if p == nil || len(p.NonKeyAttributes) == 0 {
		return nil
	}
	return p.NonKeyAttributes
}

// billingMode normalizes the two ways AWS reports it. Older tables report no
// BillingModeSummary at all and are provisioned by definition.
func billingMode(t *ddbtypes.TableDescription) string {
	if t.BillingModeSummary != nil && t.BillingModeSummary.BillingMode != "" {
		return string(t.BillingModeSummary.BillingMode)
	}
	return string(ddbtypes.BillingModeProvisioned)
}

func ddbTags(tags []ddbtypes.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}
