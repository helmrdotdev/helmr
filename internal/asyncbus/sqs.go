package asyncbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const defaultReceiveWait = 20 * time.Second

type Message struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	ID          string `json:"id"`
	FairGroupID string `json:"fair_group_id,omitempty"`
}

type Publisher interface {
	Publish(context.Context, Message) (string, error)
}

type Subscriber interface {
	Publisher
	Receive(context.Context) ([]ReceivedMessage, error)
	Delete(context.Context, ReceivedMessage) error
}

type ReceivedMessage struct {
	Message       Message
	ReceiptHandle string
}

type SQSBus struct {
	client      *sqs.Client
	busURL      string
	receiveWait time.Duration
}

func Open(ctx context.Context, busURI string) (Subscriber, error) {
	busURI = strings.TrimSpace(busURI)
	if busURI == "" {
		return nil, errors.New("async bus URI is required")
	}
	parsed, err := url.Parse(busURI)
	if err != nil {
		return nil, fmt.Errorf("parse async bus URI: %w", err)
	}
	switch parsed.Scheme {
	case "sqs+https", "sqs+http":
		return NewSQSBus(ctx, strings.TrimPrefix(busURI, "sqs+"))
	default:
		return nil, fmt.Errorf("unsupported async bus URI scheme %q", parsed.Scheme)
	}
}

func NewSQSBus(ctx context.Context, busURL string) (*SQSBus, error) {
	busURL = strings.TrimSpace(busURL)
	if busURL == "" {
		return nil, errors.New("SQS bus URL is required")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &SQSBus{
		client:      sqs.NewFromConfig(cfg),
		busURL:      busURL,
		receiveWait: defaultReceiveWait,
	}, nil
}

func (b *SQSBus) Publish(ctx context.Context, message Message) (string, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(b.busURL),
		MessageBody: aws.String(string(payload)),
	}
	if groupID := strings.TrimSpace(message.FairGroupID); groupID != "" {
		input.MessageGroupId = aws.String(groupID)
	}
	output, err := b.client.SendMessage(ctx, input)
	if err != nil {
		return "", err
	}
	return aws.ToString(output.MessageId), nil
}

func (b *SQSBus) Receive(ctx context.Context) ([]ReceivedMessage, error) {
	waitSeconds := int32(b.receiveWait / time.Second)
	if waitSeconds <= 0 {
		waitSeconds = int32(defaultReceiveWait / time.Second)
	}
	output, err := b.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(b.busURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     waitSeconds,
	})
	if err != nil {
		return nil, err
	}
	messages := make([]ReceivedMessage, 0, len(output.Messages))
	for _, item := range output.Messages {
		var message Message
		_ = json.Unmarshal([]byte(aws.ToString(item.Body)), &message)
		messages = append(messages, ReceivedMessage{
			Message:       message,
			ReceiptHandle: aws.ToString(item.ReceiptHandle),
		})
	}
	return messages, nil
}

func (b *SQSBus) Delete(ctx context.Context, message ReceivedMessage) error {
	if strings.TrimSpace(message.ReceiptHandle) == "" {
		return nil
	}
	_, err := b.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(b.busURL),
		ReceiptHandle: aws.String(message.ReceiptHandle),
	})
	if err != nil {
		return fmt.Errorf("delete async bus message: %w", err)
	}
	return nil
}
