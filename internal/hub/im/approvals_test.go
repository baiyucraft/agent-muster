package im

import (
	"context"
	"errors"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

type deliveryStore struct {
	delivery  Delivery
	confirmed string
	retries   int
}

func (store *deliveryStore) ClaimApprovalNotification(context.Context, Recipient) (Delivery, error) {
	return store.delivery, nil
}

func (store *deliveryStore) CompleteApprovalNotification(_ context.Context, _ Recipient, _ Delivery, messageID string) error {
	store.confirmed = messageID
	return nil
}

func (store *deliveryStore) RetryApprovalNotification(context.Context, Recipient, Delivery) error {
	store.retries++
	return nil
}

func (*deliveryStore) AnswerApprovalFromIM(context.Context, Recipient, Reply) (workspace.Approval, error) {
	return workspace.Approval{}, ErrReplyUnavailable
}

type deliveryTransport struct {
	key      string
	updated  string
	failSend bool
}

func (channel *deliveryTransport) Send(_ context.Context, _, key string, _ workspace.Approval) (string, error) {
	channel.key = key
	if channel.failSend {
		return "", errors.New("test: lost response")
	}
	return "message", nil
}

func (channel *deliveryTransport) Update(_ context.Context, id string, _ workspace.Approval) error {
	channel.updated = id
	return nil
}

func (*deliveryTransport) Listen(ctx context.Context, _ func(context.Context, Reply) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestDeliveryRetriesSendWithSameKeyThenUpdatesOriginalCard(t *testing.T) {
	ctx := context.Background()
	recipient := Recipient{AppID: "application", OpenID: "recipient"}
	store := &deliveryStore{delivery: Delivery{
		Approval: workspace.Approval{ID: "approval", Status: "pending"},
		Lease:    "lease",
	}}
	channel := &deliveryTransport{failSend: true}
	if deliver(ctx, store, recipient, channel) || channel.key != "approval" || store.retries != 1 || store.confirmed != "" {
		t.Fatal("failed send did not enter retry")
	}
	channel.failSend = false
	if !deliver(ctx, store, recipient, channel) || channel.key != "approval" || store.confirmed != "message" {
		t.Fatal("retry did not confirm the original request key")
	}
	store.delivery.Approval.Status = "answered"
	store.delivery.MessageID = store.confirmed
	channel.key = ""
	if !deliver(ctx, store, recipient, channel) || channel.key != "" || channel.updated != "message" {
		t.Fatal("answer did not update the original card")
	}
}
