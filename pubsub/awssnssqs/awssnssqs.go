// Copyright 2018 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package awssnssqs provides two implementations of pubsub.Topic, one that
// sends messages to AWS SNS (Simple Notification Service), and one that sends
// messages to SQS (Simple Queuing Service). It also provides an implementation
// of pubsub.Subscription that receives messages from SQS.
//
// # URLs
//
// For pubsub.OpenTopic, awssnssqs registers for the scheme "awssns" for
// an SNS topic, and "awssqs" for an SQS topic. For pubsub.OpenSubscription,
// it registers for the scheme "awssqs".
//
// The default URL opener will use an AWS session with the default credentials
// and configuration.
//
// To customize the URL opener, or for more details on the URL format,
// see URLOpener.
// See https://gocloud.dev/concepts/urls/ for background information.
//
// # Message Delivery Semantics
//
// AWS SQS supports at-least-once semantics; applications must call Message.Ack
// after processing a message, or it will be redelivered.
// See https://godoc.org/gocloud.dev/pubsub#hdr-At_most_once_and_At_least_once_Delivery
// for more background.
//
// # Escaping
//
// Go CDK supports all UTF-8 strings; to make this work with services lacking
// full UTF-8 support, strings must be escaped (during writes) and unescaped
// (during reads). The following escapes are required for awssnssqs:
//   - Metadata keys: Characters other than "a-zA-z0-9_-.", and additionally "."
//     when it's at the start of the key or the previous character was ".",
//     are escaped using "__0x<hex>__". These characters were determined by
//     experimentation.
//   - Metadata values: Escaped using URL encoding.
//   - Message body: AWS SNS/SQS only supports UTF-8 strings. See the
//     BodyBase64Encoding enum in TopicOptions for strategies on how to send
//     non-UTF-8 message bodies. By default, non-UTF-8 message bodies are base64
//     encoded.
//
// # As
//
// awssnssqs exposes the following types for As:
//   - Topic: *sns.Client for OpenSNSTopic, *sqs.Client for OpenSQSTopic
//   - Subscription: *sqs.Client
//   - Message: sqstypes.Message
//   - Message.BeforeSend: *sns.PublishBatchRequestEntry or *sns.PublishInput (deprecated) for OpenSNSTopic, *sqstypes.SendMessageBatchRequestEntry for OpenSQSTopic
//   - Message.AfterSend: snstypes.PublishBatchResultEntry or *sns.PublishOutput (deprecated) for OpenSNSTopic, sqstypes.SendMessageBatchResultEntry for OpenSQSTopic
//   - Error: any error type returned by the service, notably smithy.APIError
package awssnssqs // import "github.com/alexandre-normand/go-cloud/pubsub/awssnssqs"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"github.com/google/wire"
	gcaws "github.com/alexandre-normand/go-cloud/aws"
	"github.com/alexandre-normand/go-cloud/gcerrors"
	"github.com/alexandre-normand/go-cloud/internal/escape"
	"github.com/alexandre-normand/go-cloud/pubsub"
	"github.com/alexandre-normand/go-cloud/pubsub/batcher"
	"github.com/alexandre-normand/go-cloud/pubsub/driver"
)

const (
	// base64EncodedKey is the Message Attribute key used to flag that the
	// message body is base64 encoded.
	base64EncodedKey = "base64encoded"
	// How long ReceiveBatch should wait if no messages are available; controls
	// the poll interval of requests to SQS.
	noMessagesPollDuration = 250 * time.Millisecond
)

var sendBatcherOptsSNS = &batcher.Options{
	MaxBatchSize: 10,  // SNS SendBatch supports 10 message at a time
	MaxHandlers:  100, // max concurrency for sends
}

var sendBatcherOptsSQS = &batcher.Options{
	MaxBatchSize: 10,  // SQS SendBatch supports 10 messages at a time
	MaxHandlers:  100, // max concurrency for sends
}

var recvBatcherOpts = &batcher.Options{
	// SQS supports receiving at most 10 messages at a time:
	// https://godoc.org/github.com/aws/aws-sdk-go/service/sqs#SQS.ReceiveMessage
	MaxBatchSize: 10,
	MaxHandlers:  100, // max concurrency for receives
}

var ackBatcherOpts = &batcher.Options{
	// SQS supports deleting/updating at most 10 messages at a time:
	// https://godoc.org/github.com/aws/aws-sdk-go/service/sqs#SQS.DeleteMessageBatch
	// https://godoc.org/github.com/aws/aws-sdk-go/service/sqs#SQS.ChangeMessageVisibilityBatch
	MaxBatchSize: 10,
	MaxHandlers:  100, // max concurrency for acks
}

func init() {
	lazy := new(lazySessionOpener)
	pubsub.DefaultURLMux().RegisterTopic(SNSScheme, lazy)
	pubsub.DefaultURLMux().RegisterTopic(SQSScheme, lazy)
	pubsub.DefaultURLMux().RegisterSubscription(SQSScheme, lazy)
}

// Set holds Wire providers for this package.
var Set = wire.NewSet(
	DialSNS,
	DialSQS,
)

// DialSNS gets an AWS SNS service client using the AWS SDK V2.
func DialSNS(cfg aws.Config) *sns.Client {
	return sns.NewFromConfig(cfg)
}

// DialSQS gets an AWS SQS service client using the AWS SDK V2.
func DialSQS(cfg aws.Config) *sqs.Client {
	return sqs.NewFromConfig(cfg)
}

// lazySessionOpener obtains the AWS session from the environment on the first
// call to OpenXXXURL.
type lazySessionOpener struct {
	init   sync.Once
	opener *URLOpener
	err    error
}

func (o *lazySessionOpener) defaultOpener(u *url.URL) (*URLOpener, error) {
	return &URLOpener{}, nil
}

func (o *lazySessionOpener) OpenTopicURL(ctx context.Context, u *url.URL) (*pubsub.Topic, error) {
	opener, err := o.defaultOpener(u)
	if err != nil {
		return nil, fmt.Errorf("open topic %v: failed to open default session: %v", u, err)
	}
	return opener.OpenTopicURL(ctx, u)
}

func (o *lazySessionOpener) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*pubsub.Subscription, error) {
	opener, err := o.defaultOpener(u)
	if err != nil {
		return nil, fmt.Errorf("open subscription %v: failed to open default session: %v", u, err)
	}
	return opener.OpenSubscriptionURL(ctx, u)
}

// SNSScheme is the URL scheme for pubsub.OpenTopic (for an SNS topic) that
// awssnssqs registers its URLOpeners under on pubsub.DefaultMux.
const SNSScheme = "awssns"

// SQSScheme is the URL scheme for pubsub.OpenTopic (for an SQS topic) and for
// pubsub.OpenSubscription that awssnssqs registers its URLOpeners under on
// pubsub.DefaultMux.
const SQSScheme = "awssqs"

// URLOpener opens AWS SNS/SQS URLs like "awssns:///sns-topic-arn" for
// SNS topics or "awssqs://sqs-queue-url" for SQS topics and subscriptions.
//
// For SNS topics, the URL's host+path is used as the topic Amazon Resource Name
// (ARN). Since ARNs have ":" in them, and ":" precedes a port in URL
// hostnames, leave the host blank and put the ARN in the path
// (e.g., "awssns:///arn:aws:service:region:accountid:resourceType/resourcePath").
//
// For SQS topics and subscriptions, the URL's host+path is prefixed with
// "https://" to create the queue URL.
//
// See https://pkg.go.dev/gocloud.dev/aws#V2ConfigFromURLParams.
//
// In addition, the following query parameters are supported:
//
//   - raw (for "awssqs" Subscriptions only): sets SubscriberOptions.Raw. The
//     value must be parseable by `strconv.ParseBool`.
//   - nacklazy (for "awssqs" Subscriptions only): sets SubscriberOptions.NackLazy. The
//     value must be parseable by `strconv.ParseBool`.
//   - waittime: sets SubscriberOptions.WaitTime, in time.ParseDuration formats.
//
// See gocloud.dev/aws/ConfigFromURLParams for other query parameters
// that affect the default AWS session.
type URLOpener struct {
	// TopicOptions specifies the options to pass to OpenTopic.
	TopicOptions TopicOptions
	// SubscriptionOptions specifies the options to pass to OpenSubscription.
	SubscriptionOptions SubscriptionOptions
}

// OpenTopicURL opens a pubsub.Topic based on u.
func (o *URLOpener) OpenTopicURL(ctx context.Context, u *url.URL) (*pubsub.Topic, error) {
	// Trim leading "/" if host is empty, so that
	// awssns:///arn:aws:service:region:accountid:resourceType/resourcePath
	// gives "arn:..." instead of "/arn:...".
	topicARN := strings.TrimPrefix(path.Join(u.Host, u.Path), "/")
	qURL := "https://" + path.Join(u.Host, u.Path)
	cfg, err := gcaws.V2ConfigFromURLParams(ctx, u.Query())
	if err != nil {
		return nil, fmt.Errorf("open topic %v: %v", u, err)
	}
	switch u.Scheme {
	case SNSScheme:
		return OpenSNSTopic(ctx, sns.NewFromConfig(cfg), topicARN, &o.TopicOptions), nil
	case SQSScheme:
		return OpenSQSTopic(ctx, sqs.NewFromConfig(cfg), qURL, &o.TopicOptions), nil
	default:
		return nil, fmt.Errorf("open topic %v: unsupported scheme", u)
	}
}

// OpenSubscriptionURL opens a pubsub.Subscription based on u.
func (o *URLOpener) OpenSubscriptionURL(ctx context.Context, u *url.URL) (*pubsub.Subscription, error) {
	// Clone the options since we might override Raw.
	opts := o.SubscriptionOptions
	q := u.Query()
	if rawStr := q.Get("raw"); rawStr != "" {
		var err error
		opts.Raw, err = strconv.ParseBool(rawStr)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q for raw: %v", rawStr, err)
		}
		q.Del("raw")
	}
	if nackLazyStr := q.Get("nacklazy"); nackLazyStr != "" {
		var err error
		opts.NackLazy, err = strconv.ParseBool(nackLazyStr)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q for nacklazy: %v", nackLazyStr, err)
		}
		q.Del("nacklazy")
	}
	if waitTimeStr := q.Get("waittime"); waitTimeStr != "" {
		var err error
		opts.WaitTime, err = time.ParseDuration(waitTimeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q for waittime: %v", waitTimeStr, err)
		}
		q.Del("waittime")
	}
	qURL := "https://" + path.Join(u.Host, u.Path)
	cfg, err := gcaws.V2ConfigFromURLParams(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("open subscription %v: %v", u, err)
	}
	return OpenSubscription(ctx, sqs.NewFromConfig(cfg), qURL, &opts), nil
}

type snsTopic struct {
	client *sns.Client
	arn    string
	opts   *TopicOptions
}

// BodyBase64Encoding is an enum of strategies for when to base64 message
// bodies.
type BodyBase64Encoding int

const (
	// NonUTF8Only means that message bodies that are valid UTF-8 encodings are
	// sent as-is. Invalid UTF-8 message bodies are base64 encoded, and a
	// MessageAttribute with key "base64encoded" is added to the message.
	// When receiving messages, the "base64encoded" attribute is used to determine
	// whether to base64 decode, and is then filtered out.
	NonUTF8Only BodyBase64Encoding = 0
	// Always means that all message bodies are base64 encoded.
	// A MessageAttribute with key "base64encoded" is added to the message.
	// When receiving messages, the "base64encoded" attribute is used to determine
	// whether to base64 decode, and is then filtered out.
	Always BodyBase64Encoding = 1
	// Never means that message bodies are never base64 encoded. Non-UTF-8
	// bytes in message bodies may be modified by SNS/SQS.
	Never BodyBase64Encoding = 2
)

func (e BodyBase64Encoding) wantEncode(b []byte) bool {
	switch e {
	case Always:
		return true
	case Never:
		return false
	case NonUTF8Only:
		return !utf8.Valid(b)
	}
	panic("unreachable")
}

// TopicOptions contains configuration options for topics.
type TopicOptions struct {
	// BodyBase64Encoding determines when message bodies are base64 encoded.
	// The default is NonUTF8Only.
	BodyBase64Encoding BodyBase64Encoding

	// BatcherOptions adds constraints to the default batching done for sends.
	BatcherOptions batcher.Options
}

// OpenSNSTopic opens a topic that sends to the SNS topic with the given Amazon
// Resource Name (ARN), using AWS SDK V2.
func OpenSNSTopic(ctx context.Context, client *sns.Client, topicARN string, opts *TopicOptions) *pubsub.Topic {
	if opts == nil {
		opts = &TopicOptions{}
	}
	bo := sendBatcherOptsSNS.NewMergedOptions(&opts.BatcherOptions)
	return pubsub.NewTopic(openSNSTopic(ctx, client, topicARN, opts), bo)
}

var OpenSNSTopicV2 = OpenSNSTopic

// openSNSTopic returns the driver for OpenSNSTopic. This function exists so the test
// harness can get the driver interface implementation if it needs to.
func openSNSTopic(ctx context.Context, client *sns.Client, topicARN string, opts *TopicOptions) driver.Topic {
	return &snsTopic{
		client: client,
		arn:    topicARN,
		opts:   opts,
	}
}

var stringDataType = aws.String("String")

// encodeMetadata encodes the keys and values of md as needed.
func encodeMetadata(md map[string]string) map[string]string {
	retval := map[string]string{}
	for k, v := range md {
		// See the package comments for more details on escaping of metadata
		// keys & values.
		k = escape.HexEscape(k, func(runes []rune, i int) bool {
			c := runes[i]
			switch {
			case escape.IsASCIIAlphanumeric(c):
				return false
			case c == '_' || c == '-':
				return false
			case c == '.' && i != 0 && runes[i-1] != '.':
				return false
			}
			return true
		})
		retval[k] = escape.URLEscape(v)
	}
	return retval
}

// maybeEncodeBody decides whether body should base64-encoded based on opt, and
// returns the (possibly encoded) body as a string, along with a boolean
// indicating whether encoding occurred.
func maybeEncodeBody(body []byte, opt BodyBase64Encoding) (string, bool) {
	if opt.wantEncode(body) {
		return base64.StdEncoding.EncodeToString(body), true
	}
	return string(body), false
}

// Defines values for Metadata keys used by the driver for setting message
// attributes on SNS ([sns.PublishBatchRequestEntry]/[snstypes.PublishBatchRequestEntry])
// and SQS ([sqs.SendMessageBatchRequestEntry]/[sqstypes.SendMessageBatchRequestEntry])
// messages.
//
// For example, to set a deduplication ID and message group ID on a message:
//
//	import (
//		"github.com/alexandre-normand/go-cloud/pubsub"
//		"github.com/alexandre-normand/go-cloud/pubsub/awssnssqs"
//	)
//
//	message := pubsub.Message{
//		Body: []byte("Hello, World!"),
//		Metadata: map[string]string{
//			awssnssqs.MetadataKeyDeduplicationID: "my-dedup-id",
//			awssnssqs.MetadataKeyMessageGroupID:  "my-group-id",
//		},
//	}
const (
	MetadataKeyDeduplicationID = "DeduplicationId"
	MetadataKeyMessageGroupID  = "MessageGroupId"
)

// reviseSnsEntryAttributes sets attributes on a [snstypes.PublishBatchRequestEntry] based on [driver.Message.Metadata].
func reviseSnsEntryAttributes(dm *driver.Message, entry *snstypes.PublishBatchRequestEntry) {
	if dedupID, ok := dm.Metadata[MetadataKeyDeduplicationID]; ok {
		entry.MessageDeduplicationId = aws.String(dedupID)
	}
	if groupID, ok := dm.Metadata[MetadataKeyMessageGroupID]; ok {
		entry.MessageGroupId = aws.String(groupID)
	}
}

// SendBatch implements driver.Topic.SendBatch.
func (t *snsTopic) SendBatch(ctx context.Context, dms []*driver.Message) error {
	req := &sns.PublishBatchInput{
		TopicArn: &t.arn,
	}
	for _, dm := range dms {
		attrs := map[string]snstypes.MessageAttributeValue{}
		for k, v := range encodeMetadata(dm.Metadata) {
			attrs[k] = snstypes.MessageAttributeValue{
				DataType:    stringDataType,
				StringValue: aws.String(v),
			}
		}
		body, didEncode := maybeEncodeBody(dm.Body, t.opts.BodyBase64Encoding)
		if didEncode {
			attrs[base64EncodedKey] = snstypes.MessageAttributeValue{
				DataType:    stringDataType,
				StringValue: aws.String("true"),
			}
		}
		if len(attrs) == 0 {
			attrs = nil
		}
		entry := &snstypes.PublishBatchRequestEntry{
			Id:                aws.String(strconv.Itoa(len(req.PublishBatchRequestEntries))),
			MessageAttributes: attrs,
			Message:           aws.String(body),
		}
		reviseSnsEntryAttributes(dm, entry)
		if dm.BeforeSend != nil {
			// A previous revision used the non-batch API PublishInput, which takes
			// a *sns.PublishInput. For backwards compatibility for As, continue
			// to support that type. If it is requested, create a PublishInput
			// with the fields from PublishBatchRequestEntry that were set, and
			// then copy all of the matching fields back after calling dm.BeforeSend.
			var pi *sns.PublishInput
			asFunc := func(i any) bool {
				if p, ok := i.(**sns.PublishInput); ok {
					pi = &sns.PublishInput{
						// Id does not exist on PublishInput.
						MessageAttributes: entry.MessageAttributes,
						Message:           entry.Message,
					}
					*p = pi
					return true
				}
				if p, ok := i.(**snstypes.PublishBatchRequestEntry); ok {
					*p = entry
					return true
				}
				return false
			}
			if err := dm.BeforeSend(asFunc); err != nil {
				return err
			}
			if pi != nil {
				// Copy all of the fields that may have been modified back to the entry.
				entry.MessageAttributes = pi.MessageAttributes
				entry.Message = pi.Message
				entry.MessageDeduplicationId = pi.MessageDeduplicationId
				entry.MessageGroupId = pi.MessageGroupId
				entry.MessageStructure = pi.MessageStructure
				entry.Subject = pi.Subject
			}
		}
		req.PublishBatchRequestEntries = append(req.PublishBatchRequestEntries, *entry)
	}
	resp, err := t.client.PublishBatch(ctx, req)
	if err != nil {
		return err
	}
	if numFailed := len(resp.Failed); numFailed > 0 {
		first := resp.Failed[0]
		return fmt.Errorf("sns.PublishBatch failed for %d message(s): %s, %s", numFailed, aws.ToString(first.Code), aws.ToString(first.Message))
	}
	if len(resp.Successful) == len(dms) {
		for n, dm := range dms {
			if dm.AfterSend != nil {
				asFunc := func(i any) bool {
					if p, ok := i.(*snstypes.PublishBatchResultEntry); ok {
						*p = resp.Successful[n]
						return true
					}
					if p, ok := i.(**sns.PublishOutput); ok {
						// For backwards compability.
						*p = &sns.PublishOutput{
							MessageId:      resp.Successful[n].MessageId,
							SequenceNumber: resp.Successful[n].SequenceNumber,
						}
						return true
					}
					return false
				}
				if err := dm.AfterSend(asFunc); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// IsRetryable implements driver.Topic.IsRetryable.
func (t *snsTopic) IsRetryable(error) bool {
	// The client handles retries.
	return false
}

// As implements driver.Topic.As.
func (t *snsTopic) As(i any) bool {
	c, ok := i.(**sns.Client)
	if !ok {
		return false
	}
	*c = t.client
	return true
}

// ErrorAs implements driver.Topic.ErrorAs.
func (t *snsTopic) ErrorAs(err error, i any) bool {
	return errorAs(err, i)
}

// ErrorCode implements driver.Topic.ErrorCode.
func (t *snsTopic) ErrorCode(err error) gcerrors.ErrorCode {
	return errorCode(err)
}

// Close implements driver.Topic.Close.
func (*snsTopic) Close() error { return nil }

type sqsTopic struct {
	client *sqs.Client
	qURL   string
	opts   *TopicOptions
}

// OpenSQSTopic opens a topic that sends to the SQS topic with the given SQS
// queue URL, using AWS SDK V2.
func OpenSQSTopic(ctx context.Context, client *sqs.Client, qURL string, opts *TopicOptions) *pubsub.Topic {
	if opts == nil {
		opts = &TopicOptions{}
	}
	bo := sendBatcherOptsSQS.NewMergedOptions(&opts.BatcherOptions)
	return pubsub.NewTopic(openSQSTopic(ctx, client, qURL, opts), bo)
}

var OpenSQSTopicV2 = OpenSQSTopic

// openSQSTopic returns the driver for OpenSQSTopic. This function exists so the test
// harness can get the driver interface implementation if it needs to.
func openSQSTopic(ctx context.Context, client *sqs.Client, qURL string, opts *TopicOptions) driver.Topic {
	return &sqsTopic{
		client: client,
		qURL:   qURL,
		opts:   opts,
	}
}

// reviseSqsEntryAttributes sets attributes on a [sqstypes.SendMessageBatchRequestEntry] based on [driver.Message.Metadata].
func reviseSqsEntryAttributes(dm *driver.Message, entry *sqstypes.SendMessageBatchRequestEntry) {
	if dedupID, ok := dm.Metadata[MetadataKeyDeduplicationID]; ok {
		entry.MessageDeduplicationId = aws.String(dedupID)
	}
	if groupID, ok := dm.Metadata[MetadataKeyMessageGroupID]; ok {
		entry.MessageGroupId = aws.String(groupID)
	}
}

// SendBatch implements driver.Topic.SendBatch.
func (t *sqsTopic) SendBatch(ctx context.Context, dms []*driver.Message) error {
	req := &sqs.SendMessageBatchInput{
		QueueUrl: aws.String(t.qURL),
	}
	for _, dm := range dms {
		attrs := map[string]sqstypes.MessageAttributeValue{}
		for k, v := range encodeMetadata(dm.Metadata) {
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    stringDataType,
				StringValue: aws.String(v),
			}
		}
		body, didEncode := maybeEncodeBody(dm.Body, t.opts.BodyBase64Encoding)
		if didEncode {
			attrs[base64EncodedKey] = sqstypes.MessageAttributeValue{
				DataType:    stringDataType,
				StringValue: aws.String("true"),
			}
		}
		if len(attrs) == 0 {
			attrs = nil
		}
		entry := &sqstypes.SendMessageBatchRequestEntry{
			Id:                aws.String(strconv.Itoa(len(req.Entries))),
			MessageAttributes: attrs,
			MessageBody:       aws.String(body),
		}
		reviseSqsEntryAttributes(dm, entry)
		if dm.BeforeSend != nil {
			asFunc := func(i any) bool {
				if p, ok := i.(**sqstypes.SendMessageBatchRequestEntry); ok {
					*p = entry
					return true
				}
				return false
			}
			if err := dm.BeforeSend(asFunc); err != nil {
				return err
			}
		}
		req.Entries = append(req.Entries, *entry)
	}
	resp, err := t.client.SendMessageBatch(ctx, req)
	if err != nil {
		return err
	}
	if numFailed := len(resp.Failed); numFailed > 0 {
		first := resp.Failed[0]
		return fmt.Errorf("sqs.SendMessageBatch failed for %d message(s): %s, %s", numFailed, aws.ToString(first.Code), aws.ToString(first.Message))
	}
	if len(resp.Successful) == len(dms) {
		for n, dm := range dms {
			if dm.AfterSend != nil {
				asFunc := func(i any) bool {
					if p, ok := i.(*sqstypes.SendMessageBatchResultEntry); ok {
						*p = resp.Successful[n]
						return true
					}
					return false
				}
				if err := dm.AfterSend(asFunc); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// IsRetryable implements driver.Topic.IsRetryable.
func (t *sqsTopic) IsRetryable(error) bool {
	// The client handles retries.
	return false
}

// As implements driver.Topic.As.
func (t *sqsTopic) As(i any) bool {
	c, ok := i.(**sqs.Client)
	if !ok {
		return false
	}
	*c = t.client
	return true
}

// ErrorAs implements driver.Topic.ErrorAs.
func (t *sqsTopic) ErrorAs(err error, i any) bool {
	return errorAs(err, i)
}

// ErrorCode implements driver.Topic.ErrorCode.
func (t *sqsTopic) ErrorCode(err error) gcerrors.ErrorCode {
	return errorCode(err)
}

// Close implements driver.Topic.Close.
func (*sqsTopic) Close() error { return nil }

func errorCode(err error) gcerrors.ErrorCode {
	var code string
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	} else {
		return gcerrors.Unknown
	}
	ec, ok := errorCodeMap[code]
	if !ok {
		return gcerrors.Unknown
	}
	return ec
}

var errorCodeMap = map[string]gcerrors.ErrorCode{
	(&snstypes.AuthorizationErrorException{}).ErrorCode():        gcerrors.PermissionDenied,
	(&snstypes.KMSAccessDeniedException{}).ErrorCode():           gcerrors.PermissionDenied,
	(&snstypes.KMSDisabledException{}).ErrorCode():               gcerrors.FailedPrecondition,
	(&snstypes.KMSInvalidStateException{}).ErrorCode():           gcerrors.FailedPrecondition,
	(&snstypes.KMSOptInRequired{}).ErrorCode():                   gcerrors.FailedPrecondition,
	(&sqstypes.MessageNotInflight{}).ErrorCode():                 gcerrors.FailedPrecondition,
	(&sqstypes.PurgeQueueInProgress{}).ErrorCode():               gcerrors.FailedPrecondition,
	(&sqstypes.QueueDeletedRecently{}).ErrorCode():               gcerrors.FailedPrecondition,
	(&sqstypes.QueueNameExists{}).ErrorCode():                    gcerrors.FailedPrecondition,
	(&snstypes.InternalErrorException{}).ErrorCode():             gcerrors.Internal,
	(&snstypes.InvalidParameterException{}).ErrorCode():          gcerrors.InvalidArgument,
	(&snstypes.InvalidParameterValueException{}).ErrorCode():     gcerrors.InvalidArgument,
	(&sqstypes.BatchEntryIdsNotDistinct{}).ErrorCode():           gcerrors.InvalidArgument,
	(&sqstypes.BatchRequestTooLong{}).ErrorCode():                gcerrors.InvalidArgument,
	(&sqstypes.EmptyBatchRequest{}).ErrorCode():                  gcerrors.InvalidArgument,
	(&sqstypes.InvalidAttributeName{}).ErrorCode():               gcerrors.InvalidArgument,
	(&sqstypes.InvalidBatchEntryId{}).ErrorCode():                gcerrors.InvalidArgument,
	(&sqstypes.InvalidIdFormat{}).ErrorCode():                    gcerrors.InvalidArgument,
	(&sqstypes.InvalidMessageContents{}).ErrorCode():             gcerrors.InvalidArgument,
	(&sqstypes.ReceiptHandleIsInvalid{}).ErrorCode():             gcerrors.InvalidArgument,
	(&sqstypes.TooManyEntriesInBatchRequest{}).ErrorCode():       gcerrors.InvalidArgument,
	(&sqstypes.UnsupportedOperation{}).ErrorCode():               gcerrors.InvalidArgument,
	(&snstypes.InvalidSecurityException{}).ErrorCode():           gcerrors.PermissionDenied,
	(&snstypes.KMSNotFoundException{}).ErrorCode():               gcerrors.NotFound,
	(&snstypes.NotFoundException{}).ErrorCode():                  gcerrors.NotFound,
	(&sqstypes.QueueDoesNotExist{}).ErrorCode():                  gcerrors.NotFound,
	"AWS.SimpleQueueService.NonExistentQueue":                    gcerrors.NotFound,
	(&snstypes.FilterPolicyLimitExceededException{}).ErrorCode(): gcerrors.ResourceExhausted,
	(&snstypes.SubscriptionLimitExceededException{}).ErrorCode(): gcerrors.ResourceExhausted,
	(&snstypes.TopicLimitExceededException{}).ErrorCode():        gcerrors.ResourceExhausted,
	(&sqstypes.OverLimit{}).ErrorCode():                          gcerrors.ResourceExhausted,
	(&snstypes.KMSThrottlingException{}).ErrorCode():             gcerrors.ResourceExhausted,
	(&snstypes.ThrottledException{}).ErrorCode():                 gcerrors.ResourceExhausted,
	"RequestCanceled": gcerrors.Canceled,
	(&snstypes.EndpointDisabledException{}).ErrorCode():            gcerrors.Unknown,
	(&snstypes.PlatformApplicationDisabledException{}).ErrorCode(): gcerrors.Unknown,
}

type subscription struct {
	client *sqs.Client
	qURL   string
	opts   *SubscriptionOptions
}

// SubscriptionOptions will contain configuration for subscriptions.
type SubscriptionOptions struct {
	// Raw determines how the Subscription will process message bodies.
	//
	// If the subscription is expected to process messages sent directly to
	// SQS, or messages from SNS topics configured to use "raw" delivery,
	// set this to true. Message bodies will be passed through untouched.
	//
	// If false, the Subscription will use best-effort heuristics to
	// identify whether message bodies are raw or SNS JSON; this may be
	// inefficient for raw messages.
	//
	// See https://aws.amazon.com/sns/faqs/#Raw_message_delivery.
	Raw bool

	// NackLazy determines what Nack does.
	//
	// By default, Nack uses ChangeMessageVisibility to set the VisibilityTimeout
	// for the nacked message to 0, so that it will be redelivered immediately.
	// Set NackLazy to true to bypass this behavior; Nack will do nothing,
	// and the message will be redelivered after the existing VisibilityTimeout
	// expires (defaults to 30s, but can be configured per queue).
	//
	// See https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html.
	NackLazy bool

	// WaitTime passed to ReceiveMessage to enable long polling.
	// https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-short-and-long-polling.html#sqs-long-polling.
	// Note that a non-zero WaitTime can delay delivery of messages
	// by up to that duration.
	WaitTime time.Duration

	// ReceiveBatcherOptions adds constraints to the default batching done for receives.
	ReceiveBatcherOptions batcher.Options

	// AckBatcherOptions adds constraints to the default batching done for acks.
	AckBatcherOptions batcher.Options
}

// OpenSubscription opens a subscription based on AWS SQS for the given SQS
// queue URL, using AWS SDK V2. The queue is assumed to be subscribed to some SNS topic, though
// there is no check for this.
func OpenSubscription(ctx context.Context, client *sqs.Client, qURL string, opts *SubscriptionOptions) *pubsub.Subscription {
	if opts == nil {
		opts = &SubscriptionOptions{}
	}
	rbo := recvBatcherOpts.NewMergedOptions(&opts.ReceiveBatcherOptions)
	abo := ackBatcherOpts.NewMergedOptions(&opts.AckBatcherOptions)
	return pubsub.NewSubscription(openSubscription(ctx, client, qURL, opts), rbo, abo)
}

var OpenSubscriptionV2 = OpenSubscription

// openSubscription returns a driver.Subscription.
func openSubscription(ctx context.Context, client *sqs.Client, qURL string, opts *SubscriptionOptions) driver.Subscription {
	return &subscription{
		client: client,
		qURL:   qURL,
		opts:   opts,
	}
}

// ReceiveBatch implements driver.Subscription.ReceiveBatch.
func (s *subscription) ReceiveBatch(ctx context.Context, maxMessages int) ([]*driver.Message, error) {
	var ms []*driver.Message
	req := &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(s.qURL),
		MaxNumberOfMessages:   int32(maxMessages),
		MessageAttributeNames: []string{"All"},
		AttributeNames:        []sqstypes.QueueAttributeName{"All"},
	}
	if s.opts.WaitTime != 0 {
		req.WaitTimeSeconds = int32(s.opts.WaitTime.Seconds())
	}
	output, err := s.client.ReceiveMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, m := range output.Messages {
		m := m
		bodyStr := aws.ToString(m.Body)
		rawAttrs := map[string]string{}
		for k, v := range m.MessageAttributes {
			rawAttrs[k] = aws.ToString(v.StringValue)
		}
		bodyStr, rawAttrs = extractBody(bodyStr, rawAttrs, s.opts.Raw)

		decodeIt := false
		attrs := map[string]string{}
		for k, v := range rawAttrs {
			// See BodyBase64Encoding for details on when we base64 decode message bodies.
			if k == base64EncodedKey {
				decodeIt = true
				continue
			}
			// See the package comments for more details on escaping of metadata
			// keys & values.
			attrs[escape.HexUnescape(k)] = escape.URLUnescape(v)
		}

		var b []byte
		if decodeIt {
			var err error
			b, err = base64.StdEncoding.DecodeString(bodyStr)
			if err != nil {
				// Fall back to using the raw message.
				b = []byte(bodyStr)
			}
		} else {
			b = []byte(bodyStr)
		}

		m2 := &driver.Message{
			LoggableID: aws.ToString(m.MessageId),
			Body:       b,
			Metadata:   attrs,
			AckID:      m.ReceiptHandle,
			AsFunc: func(i any) bool {
				p, ok := i.(*sqstypes.Message)
				if !ok {
					return false
				}
				*p = m
				return true
			},
		}
		ms = append(ms, m2)
	}
	if len(ms) == 0 {
		// When we return no messages and no error, the portable type will call
		// ReceiveBatch again immediately. Sleep for a bit to avoid hammering SQS
		// with RPCs.
		time.Sleep(noMessagesPollDuration)
	}
	return ms, nil
}

func extractBody(bodyStr string, rawAttrs map[string]string, raw bool) (body string, attributes map[string]string) {
	// If the user told us that message bodies are raw, or if there are
	// top-level MessageAttributes, then it's raw.
	// (SNS JSON message can have attributes, but they are encoded in
	// the JSON instead of being at the top level).
	raw = raw || len(rawAttrs) > 0
	if raw {
		// For raw messages, the attributes are at the top level
		// and we leave bodyStr alone.
		return bodyStr, rawAttrs
	}

	// It might be SNS JSON; try to parse the raw body as such.
	// https://aws.amazon.com/sns/faqs/#Raw_message_delivery
	// If it parses as JSON and has a TopicArn field, assume it's SNS JSON.
	var bodyJSON struct {
		TopicArn          string
		Message           string
		MessageAttributes map[string]struct{ Value string }
	}
	if err := json.Unmarshal([]byte(bodyStr), &bodyJSON); err == nil && bodyJSON.TopicArn != "" {
		// It looks like SNS JSON. Get attributes from the decoded struct,
		// and update the body to be the JSON Message field.
		for k, v := range bodyJSON.MessageAttributes {
			rawAttrs[k] = v.Value
		}
		return bodyJSON.Message, rawAttrs
	}
	// It doesn't look like SNS JSON, either because it
	// isn't JSON or because the JSON doesn't have a TopicArn
	// field. Treat it as raw.
	//
	// As above in the other "raw" case, we leave bodyStr
	// alone. There can't be any top-level attributes (because
	// then we would have known it was raw earlier).
	return bodyStr, rawAttrs
}

// SendAcks implements driver.Subscription.SendAcks.
func (s *subscription) SendAcks(ctx context.Context, ids []driver.AckID) error {
	req := &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(s.qURL)}
	for _, id := range ids {
		req.Entries = append(req.Entries, sqstypes.DeleteMessageBatchRequestEntry{
			Id:            aws.String(strconv.Itoa(len(req.Entries))),
			ReceiptHandle: id.(*string),
		})
	}
	resp, err := s.client.DeleteMessageBatch(ctx, req)
	if err != nil {
		return err
	}
	// Note: DeleteMessageBatch doesn't return failures when you try
	// to Delete an id that isn't found.
	if numFailed := len(resp.Failed); numFailed > 0 {
		first := resp.Failed[0]
		return fmt.Errorf("sqs.DeleteMessageBatch failed for %d message(s): %s, %s", numFailed, aws.ToString(first.Code), aws.ToString(first.Message))
	}
	return nil
}

// CanNack implements driver.CanNack.
func (s *subscription) CanNack() bool { return true }

// SendNacks implements driver.Subscription.SendNacks.
func (s *subscription) SendNacks(ctx context.Context, ids []driver.AckID) error {
	if s.opts.NackLazy {
		return nil
	}
	req := &sqs.ChangeMessageVisibilityBatchInput{QueueUrl: aws.String(s.qURL)}
	for _, id := range ids {
		req.Entries = append(req.Entries, sqstypes.ChangeMessageVisibilityBatchRequestEntry{
			Id:                aws.String(strconv.Itoa(len(req.Entries))),
			ReceiptHandle:     id.(*string),
			VisibilityTimeout: 1,
		})
	}
	resp, err := s.client.ChangeMessageVisibilityBatch(ctx, req)
	if err != nil {
		return err
	}
	// Note: ChangeMessageVisibilityBatch returns failures when you try to
	// modify an id that isn't found; drop those.
	var firstFail sqstypes.BatchResultErrorEntry
	numFailed := 0
	for _, fail := range resp.Failed {
		if aws.ToString(fail.Code) == (&sqstypes.ReceiptHandleIsInvalid{}).ErrorCode() {
			continue
		}
		if numFailed == 0 {
			firstFail = fail
		}
		numFailed++
	}
	if numFailed > 0 {
		return fmt.Errorf("sqs.ChangeMessageVisibilityBatch failed for %d message(s): %s, %s", numFailed, aws.ToString(firstFail.Code), aws.ToString(firstFail.Message))
	}
	return nil
}

// IsRetryable implements driver.Subscription.IsRetryable.
func (*subscription) IsRetryable(error) bool {
	// The client handles retries.
	return false
}

// As implements driver.Subscription.As.
func (s *subscription) As(i any) bool {
	c, ok := i.(**sqs.Client)
	if !ok {
		return false
	}
	*c = s.client
	return true
}

// ErrorAs implements driver.Subscription.ErrorAs.
func (s *subscription) ErrorAs(err error, i any) bool {
	return errorAs(err, i)
}

// ErrorCode implements driver.Subscription.ErrorCode.
func (s *subscription) ErrorCode(err error) gcerrors.ErrorCode {
	return errorCode(err)
}

func errorAs(err error, i any) bool {
	return errors.As(err, i)
}

// Close implements driver.Subscription.Close.
func (*subscription) Close() error { return nil }
