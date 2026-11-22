package work

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

type testTransferHub struct {
	collaborationHub
	manifest control.Transfer
	ackCalls atomic.Int32
	received atomic.Bool
}

func (hub *testTransferHub) CreateTransfer(_ context.Context, _ control.Identity, _ string, request control.Manifest) (control.Transfer, error) {
	hub.manifest.ID = request.ID
	hub.manifest.SHA256 = request.SHA256
	hub.manifest.Size = request.Size
	hub.manifest.Name = request.Name
	hub.manifest.Kind = request.Kind
	hub.manifest.TargetDeviceID = request.TargetDeviceID
	return hub.manifest, nil
}

func (hub *testTransferHub) AuthorizeTransfer(_ context.Context, _ control.Identity, _ string) (control.Transfer, error) {
	result := hub.manifest
	if hub.received.Load() {
		result.ReceivedAt = "confirmed"
	}
	return result, nil
}

func (hub *testTransferHub) VerifyTransfer(_ context.Context, receiver control.Identity, id, ticket string) (control.Transfer, error) {
	if receiver.DeviceID != hub.manifest.TargetDeviceID || id != hub.manifest.ID || ticket != hub.manifest.Ticket {
		return control.Transfer{}, errors.New("unexpected receiver authorization")
	}
	return hub.manifest, nil
}

func (hub *testTransferHub) AckTransfer(_ context.Context, _ control.Identity, _, _ string) error {
	if hub.ackCalls.Add(1) == 1 {
		return errors.New("lost acknowledgement")
	}
	hub.received.Store(true)
	return nil
}

func (hub *testTransferHub) ReportTransferError(_ context.Context, _ control.Identity, _, _ string) error {
	return nil
}

func TestPeerHTTPSReplaysDurableInbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	open := func() *Worker {
		t.Helper()
		worker, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { worker.Close() })
		return worker
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "https://" + listener.Addr().String()
	listener.Close()
	receiverID := control.Identity{DeviceID: "device-receiver"}
	senderID := control.Identity{DeviceID: "device-sender"}
	hub := &testTransferHub{manifest: control.Transfer{Ticket: strings.Repeat("a", 64)}}
	receiver, err := openPeer(ctx, open(), hub, receiverID, PeerConfig{Address: address})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.close()
	sender := &collaboration{worker: open(), identity: senderID, client: hub, address: "configured"}
	run := control.Run{ID: "run-fixture", TaskID: "task-fixture", Generation: 1, Lease: strings.Repeat("A", 26)}
	if err := sender.worker.saveClaim(ctx, run); err != nil {
		t.Fatal(err)
	}
	id := transferKey(run.ID, "file", "fixture")
	body := []byte("name,size\r\nmaterial.png,42\r\n")
	hub.manifest.Peer = &control.Peer{
		DeviceID: receiverID.DeviceID, Address: receiver.address, Fingerprint: strings.Repeat("0", 64),
	}
	if _, err := sender.stageTransfer(ctx, run, id, receiverID.DeviceID, "index.csv", "file", body); err != nil {
		t.Fatal(err)
	}
	if err := sender.sendOne(ctx, id); !errors.Is(err, errPeerVerification) {
		t.Fatalf("wrong peer pin was not rejected: %v", err)
	}
	hub.manifest.Peer.Fingerprint = receiver.fingerprint
	if err := sender.sendOne(ctx, id); err == nil {
		t.Fatal("sender treated a lost acknowledgment as success")
	}
	var got string
	if err := receiver.worker.db.QueryRowContext(ctx, "SELECT body FROM peer_inbox WHERE id=?", id).Scan(&got); err != nil || got != string(body) {
		t.Fatalf("receiver did not durably record exact contents before acknowledgment: %q, %v", got, err)
	}
	if err := sender.sendOne(ctx, id); err != nil {
		t.Fatal(err)
	}
	var inbox, outbox int
	if err := receiver.worker.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM peer_inbox WHERE id=?", id).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	if err := sender.worker.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM peer_outbox WHERE id=?", id).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if inbox != 1 || outbox != 0 || !hub.received.Load() {
		t.Fatalf("unexpected replay state: inbox=%d outbox=%d confirmed=%v", inbox, outbox, hub.received.Load())
	}
}
