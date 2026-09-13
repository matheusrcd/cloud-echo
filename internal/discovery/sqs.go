package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
)

// SQS collects queues and the redrive relationships between them.
//
// The redrive policy is the reason this collector matters more than its size
// suggests: it is a Tier-1 declaration, so a queue → DLQ edge needs no inference
// at all. It arrives as a JSON string nested inside an attribute map, which is
// why it is parsed here rather than left for the linker to re-discover.
type SQS struct{}

func (*SQS) Service() string { return "SQS" }

func (c *SQS) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := sqs.NewFromConfig(s.Config())

	// MaxResults is not optional in practice. Without it ListQueues returns at
	// most 1000 queues and no NextToken, so a larger account is truncated with
	// no signal that anything is missing. Confirmed against the real API: the
	// same call returns NextToken only once MaxResults is set.
	var urls []string
	p := sqs.NewListQueuesPaginator(api, &sqs.ListQueuesInput{MaxResults: aws.Int32(1000)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return warnOrFail(out, "SQS", "ListQueues", err)
		}
		urls = append(urls, page.QueueUrls...)
	}

	for _, url := range urls {
		attrs, err := api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(url),
			AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
		})
		if err != nil {
			if err := warnOrFail(out, "SQS", "GetQueueAttributes", err); err != nil {
				return err
			}
			continue
		}

		tags, err := api.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(url)})
		if err != nil {
			// Tags are a filtering convenience, never a correctness input, so a
			// denial here must not cost us the queue itself.
			if err := warnOrFail(out, "SQS", "ListQueueTags", err); err != nil {
				return err
			}
			tags = &sqs.ListQueueTagsOutput{}
		}

		name := queueNameFromURL(url)
		spec := queueSpec{
			QueueName:         name,
			URL:               url,
			VisibilityTimeout: atoiAttr(attrs.Attributes, "VisibilityTimeout"),
			MessageRetention:  atoiAttr(attrs.Attributes, "MessageRetentionPeriod"),
			DelaySeconds:      atoiAttr(attrs.Attributes, "DelaySeconds"),
			MaxMessageSize:    atoiAttr(attrs.Attributes, "MaximumMessageSize"),
			FIFO:              attrs.Attributes["FifoQueue"] == "true",
			ContentDedup:      attrs.Attributes["ContentBasedDeduplication"] == "true",
			Policy:            rawIfPresent(attrs.Attributes["Policy"]),
		}
		spec.Redrive = parseRedrive(attrs.Attributes["RedrivePolicy"])

		out.Emit(newResource(s, resourceArgs{
			// Queue names are unique per account and region, so no extra scope
			// is needed here — unlike ECS services, which are unique only within
			// a cluster.
			ID:   "sqs/" + name,
			Type: "sqs.queue",
			ARN:  attrs.Attributes["QueueArn"],
			Name: name,
			Tags: tags.Tags,
			API:  "sqs:GetQueueAttributes",
			Spec: spec,
			Raw:  attrs.Attributes,
		}))
	}
	return nil
}

// parseRedrive pulls the DLQ relationship out of the JSON-in-a-string that
// GetQueueAttributes returns.
//
// A malformed policy is returned as nil rather than an error: a queue whose
// redrive policy we cannot read is still a queue worth knowing about, and losing
// it would be a worse outcome than losing one edge.
func parseRedrive(raw string) *redriveSpec {
	if raw == "" {
		return nil
	}
	var rp struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
		// AWS returns maxReceiveCount as a number, but has historically also
		// returned it as a string. json.Number accepts both.
		MaxReceiveCount json.Number `json:"maxReceiveCount"`
	}
	if err := json.Unmarshal([]byte(raw), &rp); err != nil || rp.DeadLetterTargetArn == "" {
		return nil
	}
	n, _ := strconv.Atoi(rp.MaxReceiveCount.String())
	return &redriveSpec{
		TargetARN:  rp.DeadLetterTargetArn,
		TargetID:   queueIDFromARN(rp.DeadLetterTargetArn),
		MaxReceive: n,
	}
}

// queueNameFromURL extracts the queue name from a queue URL, which is the last
// path segment in every URL form AWS uses.
func queueNameFromURL(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
}

// queueIDFromARN maps arn:aws:sqs:<region>:<account>:<name> to the inventory id.
func queueIDFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 || parts[2] != "sqs" {
		return ""
	}
	return "sqs/" + parts[5]
}

func atoiAttr(attrs map[string]string, key string) int {
	n, err := strconv.Atoi(attrs[key])
	if err != nil {
		return 0
	}
	return n
}

func rawIfPresent(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if !json.Valid([]byte(s)) {
		return json.RawMessage(fmt.Sprintf("%q", s))
	}
	return json.RawMessage(s)
}
