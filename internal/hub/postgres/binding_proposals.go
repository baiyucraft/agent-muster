package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const proposalColumns = "id,message_id,name,bindings,status,COALESCE(project_id,''),created_at,decided_at"

func scanBindingProposal(row pgx.Row) (proposal workspace.BindingProposal, err error) {
	err = row.Scan(&proposal.ID, &proposal.MessageID, &proposal.Name, &proposal.Bindings,
		&proposal.Status, &proposal.ProjectID, &proposal.CreatedAt, &proposal.DecidedAt)
	return
}

// BindingCandidates 返回未撤销设备的注册目录和最后确认的 CLI 能力，供中心提案选择。
func (store *InventoryStore) BindingCandidates(ctx context.Context) ([]workspace.BindingCandidate, error) {
	return selectRows(ctx, store.pool, `SELECT v.device_id,v.name,v.last_seen_at,COALESCE(v.inventory,'{}'::jsonb),
		jsonb_agg(jsonb_build_object('id',d.directory_id,'label',d.label,'device_id',v.device_id) ORDER BY d.label,d.directory_id)
		FROM muster_devices v JOIN muster_directories d USING (device_id)
		WHERE v.revoked_at IS NULL GROUP BY v.device_id ORDER BY v.name,v.device_id`,
		func(row pgx.Row) (candidate workspace.BindingCandidate, err error) {
			var inventory registry.Inventory
			err = row.Scan(&candidate.DeviceID, &candidate.DeviceName, &candidate.LastSeenAt, &inventory, &candidate.Directories)
			candidate.Agents = make([]registry.Agent, 0, len(inventory.Agents))
			for _, agent := range inventory.Agents {
				candidate.Agents = append(candidate.Agents, registry.Agent{
					ID: agent.ID, Version: agent.Version, Status: agent.Status, Capabilities: agent.Capabilities,
				})
			}
			return
		})
}

// ProposeBinding 冻结当前租约下的项目绑定建议。相同意图重传会返回先前的提案和决定。
func (store *InventoryStore) ProposeBinding(ctx context.Context, deliveryID string, input workspace.BindingProposalInput) (proposal workspace.BindingProposal, err error) {
	input, err = input.Normalize()
	if err != nil {
		return proposal, err
	}
	lease := input.Lease
	input.Lease = ""
	encoded, err := json.Marshal(input)
	if err != nil {
		return proposal, err
	}
	digest := sha256.Sum256(encoded)
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		var messageID, kind string
		var authorized bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(message_id,''),kind,
			COALESCE(lease=$2 AND lease_until>=clock_timestamp(),false) AND NOT done AND task_id IS NULL AND dispatch_task_id IS NULL
			FROM muster_deliveries WHERE id=$1`, deliveryID, lease).Scan(&messageID, &kind, &authorized)
		if err != nil {
			return notFound(err)
		}
		if !authorized || lease == "" || kind != "message" || messageID == "" {
			return workspace.ErrConflict
		}
		var savedDigest []byte
		err = tx.QueryRow(ctx, `SELECT request_digest FROM muster_binding_proposals WHERE message_id=$1`, messageID).Scan(&savedDigest)
		if err == nil {
			if !bytes.Equal(savedDigest, digest[:]) {
				return workspace.ErrConflict
			}
			proposal, err = scanBindingProposal(tx.QueryRow(ctx, "SELECT "+proposalColumns+" FROM muster_binding_proposals WHERE message_id=$1", messageID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		for i := range input.Bindings {
			if err := readProposalTarget(ctx, tx, &input.Bindings[i]); err != nil {
				return err
			}
		}
		proposalID := "proposal-" + messageID
		proposal, err = scanBindingProposal(tx.QueryRow(ctx, `WITH card AS (
			INSERT INTO muster_messages (id,kind,body) VALUES ($1,'binding_proposal',$3) RETURNING created_at
		) INSERT INTO muster_binding_proposals (id,message_id,request_digest,name,bindings,created_at)
		SELECT $1,$2,$4,$3,$5,created_at FROM card RETURNING `+proposalColumns,
			proposalID, messageID, input.Name, digest[:], input.Bindings))
		if err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return proposal, err
}

// DecideBinding 保存管理员首个决定。接受同时新建项目，接受失败会保留待确认状态。
func (store *InventoryStore) DecideBinding(ctx context.Context, id, decision string) (proposal workspace.BindingProposal, err error) {
	if decision != "accepted" && decision != "rejected" {
		return proposal, workspace.ErrInvalidBindingProposal
	}
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		var err error
		proposal, err = scanBindingProposal(tx.QueryRow(ctx, "SELECT "+proposalColumns+" FROM muster_binding_proposals WHERE id=$1", id))
		if err != nil {
			return notFound(err)
		}
		if proposal.Status == decision {
			return nil
		}
		if proposal.Status != "pending" {
			return workspace.ErrConflict
		}
		var projectID string
		if decision == "accepted" {
			if err := createProposedProject(ctx, tx, proposal); err != nil {
				return err
			}
			projectID = proposal.MessageID
		}
		proposal, err = scanBindingProposal(tx.QueryRow(ctx, `UPDATE muster_binding_proposals
			SET status=$2,project_id=NULLIF($3,''),decided_at=clock_timestamp() WHERE id=$1 RETURNING `+proposalColumns, id, decision, projectID))
		if err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return proposal, err
}

func readProposalTarget(ctx context.Context, tx pgx.Tx, binding *workspace.ProposedBinding) error {
	var inventory registry.Inventory
	err := tx.QueryRow(ctx, `SELECT v.name,d.label,COALESCE(v.inventory,'{}'::jsonb)
		FROM muster_devices v JOIN muster_directories d USING (device_id)
		WHERE v.device_id=$1 AND d.directory_id=$2 AND v.revoked_at IS NULL FOR SHARE OF v`,
		binding.DeviceID, binding.DirectoryID).Scan(&binding.DeviceName, &binding.DirectoryLabel, &inventory)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.ErrConflict
	}
	if err != nil {
		return err
	}
	for _, agent := range inventory.Agents {
		if agent.ID == binding.AgentKind && agent.Status == "supported" && agent.Capabilities != nil &&
			agent.Capabilities.Prompt && agent.Capabilities.FinalReply && agent.Capabilities.Resume {
			return nil
		}
	}
	return workspace.ErrConflict
}

func createProposedProject(ctx context.Context, tx pgx.Tx, proposal workspace.BindingProposal) error {
	// 项目保存和提案决定共用工作区写入锁。已存在的项目只能由管理页显式编辑。
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM muster_projects WHERE id=$1)`, proposal.MessageID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return workspace.ErrConflict
	}
	project := workspace.Project{ID: proposal.MessageID, Name: proposal.Name, Bindings: make([]workspace.Binding, 0, len(proposal.Bindings))}
	for _, binding := range proposal.Bindings {
		if err := readProposalTarget(ctx, tx, &binding); err != nil {
			return err
		}
		bindingID, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		project.Bindings = append(project.Bindings, workspace.Binding{
			ID: bindingID.String(), DeviceID: binding.DeviceID, DirectoryID: binding.DirectoryID,
			AgentKind: binding.AgentKind, IsDefault: binding.IsDefault,
		})
	}
	return saveProject(ctx, tx, &project)
}
