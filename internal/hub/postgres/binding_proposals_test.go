package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/google/uuid"
)

func TestBindingProposalPersistsAndAcceptsAfterReclaim(t *testing.T) {
	ctx, address, store, binding, token := openBindingProposalFixture(t)
	candidates, err := store.BindingCandidates(ctx)
	checkNotification(t, err)
	if len(candidates) != 1 || candidates[0].DeviceID != binding.DeviceID || candidates[0].LastSeenAt != nil ||
		len(candidates[0].Directories) != 1 || len(candidates[0].Agents) != 1 || candidates[0].Agents[0].Status != "supported" {
		t.Fatalf("候选未按登记信息呈现离线设备: %+v", candidates)
	}
	delivery := claimBindingProposalMessage(t, ctx, store)
	input := workspace.BindingProposalInput{Lease: delivery.Lease, Name: " 索引项目 ", Bindings: []workspace.ProposedBinding{binding}}
	badLease := input
	badLease.Lease = "different-lease"
	if _, err := store.ProposeBinding(ctx, delivery.ID, badLease); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("非当前租约登记提案返回 %v", err)
	}
	proposal, err := store.ProposeBinding(ctx, delivery.ID, input)
	checkNotification(t, err)
	if proposal.ID != "proposal-"+delivery.MessageID || proposal.Status != "pending" || proposal.Name != "索引项目" ||
		proposal.ProjectID != "" || proposal.DecidedAt != nil || proposal.Bindings[0].DeviceName != "Home" || proposal.Bindings[0].DirectoryLabel != "Folder" {
		t.Fatalf("未保存已规范化的绑定提案: %+v", proposal)
	}
	before, err := store.Workspace(ctx)
	checkNotification(t, err)
	if len(before.Projects) != 0 || len(before.Tasks) != 0 || len(before.BindingProposals) != 1 || len(before.Messages) != 2 ||
		before.Messages[1].ID != proposal.ID || before.Messages[1].Kind != "binding_proposal" {
		t.Fatalf("提案和对话卡片没有一同入库: %+v", before)
	}
	if _, err := store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, ProjectID: "unused", Title: "分派", Prompt: "运行"}); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("同一消息有提案时仍可派发: %v", err)
	}
	input.Bindings[0].DirectoryLabel = "模型给出的标签"
	if repeated, err := store.ProposeBinding(ctx, delivery.ID, input); err != nil || repeated.ID != proposal.ID {
		t.Fatalf("相同目标重放失败: %+v / %v", repeated, err)
	}
	changed := input
	changed.Name = "其他项目"
	if _, err := store.ProposeBinding(ctx, delivery.ID, changed); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("变化后的提案重放返回 %v", err)
	}
	_, err = store.pool.Exec(ctx, `UPDATE muster_deliveries SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, delivery.ID)
	checkNotification(t, err)
	store.Close()
	store, err = OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	reclaimed, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	if reclaimed.ID != delivery.ID || reclaimed.Lease == delivery.Lease || reclaimed.BindingProposal == nil || reclaimed.BindingProposal.ID != proposal.ID {
		t.Fatalf("重启重领未带回已有提案: %+v", reclaimed)
	}
	if _, err := store.ProposeBinding(ctx, delivery.ID, input); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("配对前一次租约仍能使用: %v", err)
	}
	checkNotification(t, store.Report(ctx, binding.DeviceID, token, registry.Inventory{Agents: []registry.Agent{{ID: "codex", Status: "missing"}}}))
	if _, err := store.DecideBinding(ctx, proposal.ID, "accepted"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("CLI 缺失时仍接受提案: %v", err)
	}
	checkNotification(t, store.Report(ctx, binding.DeviceID, token, proposalTestInventory()))
	accepted, err := store.DecideBinding(ctx, proposal.ID, "accepted")
	checkNotification(t, err)
	if accepted.Status != "accepted" || accepted.ProjectID != delivery.MessageID || accepted.DecidedAt == nil {
		t.Fatalf("接受未返回新项目: %+v", accepted)
	}
	acceptReplay, err := store.DecideBinding(ctx, proposal.ID, "accepted")
	checkNotification(t, err)
	if !acceptReplay.DecidedAt.Equal(*accepted.DecidedAt) {
		t.Fatal("同一决定重放更新了决策时间")
	}
	if _, err := store.DecideBinding(ctx, proposal.ID, "rejected"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("相反的决定更改了已确认提案: %v", err)
	}
	input.Lease = reclaimed.Lease
	replayedProposal, err := store.ProposeBinding(ctx, delivery.ID, input)
	checkNotification(t, err)
	if replayedProposal.Status != "accepted" || replayedProposal.ProjectID != accepted.ProjectID {
		t.Fatalf("重放返回了过期的提案状态: %+v", replayedProposal)
	}
	checkNotification(t, store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{
		Lease: reclaimed.Lease, Status: "handled", Text: "请在对话卡片确认。",
	}))
	after, err := store.Workspace(ctx)
	checkNotification(t, err)
	if after.Cursor != before.Cursor+2 || len(after.Projects) != 1 || len(after.Tasks) != 0 || len(after.Messages) != 2 || after.Messages[0].Status != "handled" ||
		after.Projects[0].ID != accepted.ProjectID || len(after.Projects[0].Bindings) != 1 || after.Projects[0].Bindings[0].AgentKind != binding.AgentKind {
		t.Fatalf("接受、重放或结束投递后应只保留提案卡片: %+v", after)
	}
}

func TestBindingProposalConcurrentDecisionsAndDispatch(t *testing.T) {
	ctx, _, store, binding, _ := openBindingProposalFixture(t)
	project, err := store.SaveProject(ctx, workspace.Project{ID: uuid.NewString(), Name: "手工项目", DeviceID: binding.DeviceID, DirectoryID: binding.DirectoryID})
	checkNotification(t, err)
	delivery := claimBindingProposalMessage(t, ctx, store)
	_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, ProjectID: project.ID, Title: "运行", Prompt: "运行"})
	checkNotification(t, err)
	if _, err := store.ProposeBinding(ctx, delivery.ID, workspace.BindingProposalInput{
		Lease: delivery.Lease, Name: "冲突项目", Bindings: []workspace.ProposedBinding{binding},
	}); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("已派发的消息仍能提案: %v", err)
	}
	checkNotification(t, store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}))
	delivery = claimBindingProposalMessage(t, ctx, store)
	proposal, err := store.ProposeBinding(ctx, delivery.ID, workspace.BindingProposalInput{
		Lease: delivery.Lease, Name: "并发项目", Bindings: []workspace.ProposedBinding{binding},
	})
	checkNotification(t, err)
	answers := make(chan error, 2)
	for _, decision := range []string{"accepted", "rejected"} {
		go func() {
			_, err := store.DecideBinding(ctx, proposal.ID, decision)
			answers <- err
		}()
	}
	first, second := <-answers, <-answers
	if !(first == nil && errors.Is(second, workspace.ErrConflict) || second == nil && errors.Is(first, workspace.ErrConflict)) {
		t.Fatalf("两个决定应只有一个被持久化: %v / %v", first, second)
	}
	snapshot, err := store.Workspace(ctx)
	checkNotification(t, err)
	wantProjects := 1
	if snapshot.BindingProposals[0].Status == "accepted" {
		wantProjects++
	}
	if len(snapshot.Projects) != wantProjects || len(snapshot.Tasks) != 1 {
		t.Fatalf("决定和项目写入不同步: %+v", snapshot)
	}
}

func openBindingProposalFixture(t *testing.T) (context.Context, string, *InventoryStore, workspace.ProposedBinding, [32]byte) {
	t.Helper()
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	binding := workspace.ProposedBinding{DeviceID: "d-" + strings.Repeat("a", 32), DirectoryID: "dir-" + strings.Repeat("b", 32), AgentKind: "codex", IsDefault: true}
	token := [32]byte{1}
	_, err = store.pool.Exec(ctx, `INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
		VALUES ($1,$2,'Home','home','windows','amd64')`, binding.DeviceID, token[:])
	checkNotification(t, err)
	checkNotification(t, store.ReplaceDirectories(ctx, binding.DeviceID, token, []workspace.Directory{{ID: binding.DirectoryID, Label: "Folder"}}))
	checkNotification(t, store.Report(ctx, binding.DeviceID, token, proposalTestInventory()))
	return ctx, address, store, binding, token
}

func proposalTestInventory() registry.Inventory {
	return registry.Inventory{Agents: []registry.Agent{{ID: "codex", Version: "1.0.0", Status: "supported",
		Capabilities: &registry.Capabilities{Prompt: true, FinalReply: true, Resume: true},
	}}}
}

func claimBindingProposalMessage(t *testing.T, ctx context.Context, store *InventoryStore) workspace.Delivery {
	t.Helper()
	message, err := store.SubmitMessage(ctx, workspace.Message{ID: uuid.NewString(), Text: "为这台设备登记一个项目"})
	checkNotification(t, err)
	delivery, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	if delivery.MessageID != message.ID || !delivery.BindingProposalsSupported || delivery.BindingProposal != nil {
		t.Fatalf("新领取的提案源消息无效: %+v", delivery)
	}
	return delivery
}
