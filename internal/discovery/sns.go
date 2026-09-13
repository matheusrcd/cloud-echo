package discovery

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/matheusrcd/cloud-echo/internal/awsx"
	"github.com/matheusrcd/cloud-echo/internal/inventory"
	"github.com/matheusrcd/cloud-echo/internal/inventory/spec"
)

// SNS collects topics and what each delivers to.
//
// The design listed three calls. ListSubscriptionsByTopic returns a
// subscription's protocol and endpoint only: its filter policy, raw delivery and
// dead-letter queue come from GetSubscriptionAttributes, one call per confirmed
// subscription — a pending one has no ARN to ask with. No response carries tags,
// hence ListTagsForResource per topic. Every denial past ListTopics costs what
// it would have read, recorded in the topic's Unread, never the topic.
//
// Endpoints are sanitized before Spec or Raw is built: SNS masks a basic-auth
// password in an HTTPS endpoint itself, but returns a token in its query in
// full; an email address or a phone number is a person's, and is withheld.
type SNS struct{}

const snsSvc = "SNS"

func (*SNS) Service() string { return snsSvc }

func (c *SNS) Collect(ctx context.Context, s *awsx.Session, out Emitter) error {
	api := sns.NewFromConfig(s.Config())

	var arns []string
	tp := sns.NewListTopicsPaginator(api, &sns.ListTopicsInput{})
	for tp.HasMorePages() {
		page, err := tp.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, snsSvc, "ListTopics", err); err != nil {
				return err
			}
			break
		}
		for _, t := range page.Topics {
			arns = append(arns, aws.ToString(t.TopicArn))
		}
	}
	sort.Strings(arns)

	for _, arn := range arns {
		r, err := c.topic(ctx, api, s, out, arn)
		if err != nil {
			return err
		}
		if r != nil {
			out.Emit(*r)
		}
	}
	return nil
}

type rawTopic struct {
	Attributes    map[string]string `json:"attributes"`
	Subscriptions []rawSubscription `json:"subscriptions"`
}

type rawSubscription struct {
	snstypes.Subscription
	Attributes map[string]string `json:"attributes,omitempty"`
}

// topic reads one topic; nil when it was deleted after ListTopics listed it.
func (c *SNS) topic(ctx context.Context, api *sns.Client, s *awsx.Session, out Emitter, arn string) (*inventory.Resource, error) {
	name := arn[strings.LastIndex(arn, ":")+1:]
	sp := spec.SNSTopic{TopicName: name, Subscriptions: []spec.SNSSubscription{}}
	raw := rawTopic{}

	attrs, err := api.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: aws.String(arn)})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		if err := warnOrFail(out, snsSvc, "GetTopicAttributes", err); err != nil {
			return nil, err
		}
		sp.Unread = append(sp.Unread, "attributes")
	} else {
		a := attrs.Attributes
		raw.Attributes = a
		sp.FIFO = a["FifoTopic"] == "true"
		sp.ContentDedup = a["ContentBasedDeduplication"] == "true"
		sp.KMSKeyID = a["KmsMasterKeyId"]
		if p := a["Policy"]; p != "" && json.Valid([]byte(p)) {
			sp.Policy = json.RawMessage(p)
		}
		if p := a["DeliveryPolicy"]; p != "" && json.Valid([]byte(p)) {
			sp.DeliveryPolicy = json.RawMessage(p)
		}
	}

	var tags map[string]string
	if t, err := api.ListTagsForResource(ctx, &sns.ListTagsForResourceInput{ResourceArn: aws.String(arn)}); err != nil {
		if err := warnOrFail(out, snsSvc, "ListTagsForResource", err); err != nil {
			return nil, err
		}
	} else if len(t.Tags) > 0 {
		tags = map[string]string{}
		for _, tag := range t.Tags {
			tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
	}

	var subs []snstypes.Subscription
	p := sns.NewListSubscriptionsByTopicPaginator(api, &sns.ListSubscriptionsByTopicInput{TopicArn: aws.String(arn)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if err := warnOrFail(out, snsSvc, "ListSubscriptionsByTopic", err); err != nil {
				return nil, err
			}
			sp.Unread = append(sp.Unread, "subscriptions")
			break
		}
		subs = append(subs, page.Subscriptions...)
	}

	attrsUnread := false
	for _, sub := range subs {
		sub.Endpoint = aws.String(sanitizeEndpoint(aws.ToString(sub.Protocol), aws.ToString(sub.Endpoint)))
		ss := spec.SNSSubscription{
			ARN:      aws.ToString(sub.SubscriptionArn),
			Protocol: aws.ToString(sub.Protocol),
			Endpoint: aws.ToString(sub.Endpoint),
			Owner:    aws.ToString(sub.Owner),
		}
		if ss.ARN == "PendingConfirmation" {
			ss.ARN, ss.Pending = "", true
		}
		if ss.Protocol == "sqs" || ss.Protocol == "lambda" {
			ss.Target = &spec.TargetRef{ARN: ss.Endpoint, ID: resourceIDFromARN(ss.Endpoint)}
		}
		rs := rawSubscription{Subscription: sub}
		if ss.ARN != "" && !attrsUnread {
			a, err := api.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{SubscriptionArn: aws.String(ss.ARN)})
			if err != nil {
				if err := warnOrFail(out, snsSvc, "GetSubscriptionAttributes", err); err != nil {
					return nil, err
				}
				// One denial is every denial: the rest are not asked.
				attrsUnread = true
				sp.Unread = append(sp.Unread, "subscription attributes")
			} else {
				sa := a.Attributes
				if e, ok := sa["Endpoint"]; ok {
					sa["Endpoint"] = sanitizeEndpoint(ss.Protocol, e)
				}
				rs.Attributes = sa
				ss.Pending = ss.Pending || sa["PendingConfirmation"] == "true"
				ss.RawDelivery = sa["RawMessageDelivery"] == "true"
				ss.FilterPolicy = sa["FilterPolicy"]
				if ss.FilterPolicy != "" {
					ss.FilterScope = sa["FilterPolicyScope"]
				}
				ss.RoleARN = sa["SubscriptionRoleArn"]
				if rp := sa["RedrivePolicy"]; rp != "" {
					var r struct {
						DeadLetterTargetArn string `json:"deadLetterTargetArn"`
					}
					if json.Unmarshal([]byte(rp), &r) == nil && r.DeadLetterTargetArn != "" {
						ss.DeadLetter = &spec.TargetRef{ARN: r.DeadLetterTargetArn, ID: resourceIDFromARN(r.DeadLetterTargetArn)}
					}
				}
			}
		}
		sp.Subscriptions = append(sp.Subscriptions, ss)
		raw.Subscriptions = append(raw.Subscriptions, rs)
	}
	sort.SliceStable(sp.Subscriptions, func(i, j int) bool {
		a, b := sp.Subscriptions[i], sp.Subscriptions[j]
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		return a.Endpoint < b.Endpoint
	})

	r := newResource(s, resourceArgs{
		ID: "sns/" + name, Type: spec.TypeSNSTopic, ARN: arn, Name: name,
		Tags: tags, API: "sns:GetTopicAttributes", Spec: sp, Raw: raw,
	})
	return &r, nil
}

// sanitizeEndpoint withholds what a subscription's endpoint must not carry
// out of AWS: a person's email address or phone number entirely, a URL's
// credentials and secret-bearing query values.
func sanitizeEndpoint(protocol, endpoint string) string {
	switch protocol {
	case "email", "email-json", "sms":
		if endpoint == "" {
			return ""
		}
		return marker("personal-data")
	case "http", "https":
		if looksLikeURL(endpoint) {
			v, _ := sanitizeURL(endpoint)
			return v
		}
	}
	return endpoint
}
