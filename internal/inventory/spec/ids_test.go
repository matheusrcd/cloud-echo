package spec

import "testing"

func TestQueueFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://sqs.us-east-2.amazonaws.com/123456789012/orders":         "arn:aws:sqs:us-east-2:123456789012:orders",
		"https://sqs.us-east-2.amazonaws.com/123456789012/orders.fifo":    "arn:aws:sqs:us-east-2:123456789012:orders.fifo",
		"https://eu-west-1.queue.amazonaws.com/123456789012/legacy-jobs":  "arn:aws:sqs:eu-west-1:123456789012:legacy-jobs",
		"https://queue.amazonaws.com/123456789012/legacy-jobs":            "arn:aws:sqs:us-east-1:123456789012:legacy-jobs",
		"https://sqs.us-east-2.amazonaws.com/123456789012/orders/extra":   "",
		"https://sqs.us-east-2.amazonaws.com/123456789012/":               "",
		"https://sqs.us-east-2.amazonaws.com/orders":                      "",
		"https://sqs.us-east-2.amazonaws.com/123456789012/orders?x=1":     "",
		"http://localhost:4566/123456789012/orders":                       "",
		"https://sqs.us-east-2.amazonaws.com.evil.example/123456789012/x": "",
	} {
		got := QueueFromURL(in)
		switch {
		case want == "" && got != nil:
			t.Errorf("%s: accepted as %s", in, got.ARN)
		case want != "" && (got == nil || got.ARN != want):
			t.Errorf("%s: got %+v, want %s", in, got, want)
		}
	}
}
