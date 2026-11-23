package postgres

import (
	"context"
	"errors"
	"reflect"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

var _ workspace.Store = (*InventoryStore)(nil)

const projectColumns = "id, name, device_id, directory_id, directory_label, agent_kind, git_policy"
const messageColumns = "id, kind, body, COALESCE(task_id, requested_task_id), status, created_at"
const taskColumns = `t.id, t.project_id, t.project_name, t.device_id, t.directory_id, t.status, t.title, t.result,
	EXISTS (SELECT 1 FROM muster_deliveries d WHERE d.task_id = t.id AND d.kind = 'report' AND NOT d.done),
	t.updated_at,COALESCE(t.parent_id,''),t.root_id,t.binding_id,t.agent_kind,t.waiting_reason,
	COALESCE((SELECT generation FROM muster_runs WHERE id=t.current_run_id),0),t.input_generation,
	t.control_mode,
	EXISTS (SELECT 1 FROM muster_runs r WHERE r.id=t.current_run_id AND r.claimed_at IS NOT NULL AND r.status IN ('queued','running')),
	COALESCE((SELECT r.control_supported FROM muster_runs r WHERE r.id=t.current_run_id),false),
	COALESCE((t.git_policy->>'revision')::bigint,0),t.git_publish`

// WorkspaceEvents 按游标升序返回指定位置之后最多 128 条工作区事件。
// 超过当前工作区位置的游标返回冲突。
func (store *InventoryStore) WorkspaceEvents(ctx context.Context, after int64) ([]int64, error) {
	var current, oldest int64
	err := store.pool.QueryRow(ctx, `
		SELECT revision,COALESCE((SELECT MIN(revision) FROM muster_workspace_events),revision+1)
		FROM muster_workspace`).Scan(&current, &oldest)
	if err != nil {
		return nil, err
	}
	if after < oldest-1 || after > current {
		return nil, workspace.ErrConflict
	}
	return selectRows(ctx, store.pool,
		"SELECT revision FROM muster_workspace_events WHERE revision>$1 ORDER BY revision LIMIT 128",
		func(row pgx.Row) (int64, error) {
			var value int64
			err := row.Scan(&value)
			return value, err
		}, after)
}

// Workspace 在同一只读事务中读取对话、任务、项目、目录及对应游标。
func (store *InventoryStore) Workspace(ctx context.Context) (snapshot workspace.Snapshot, err error) {
	err = pgx.BeginTxFunc(ctx, store.pool, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT revision FROM muster_workspace").Scan(&snapshot.Cursor); err != nil {
			return err
		}
		snapshot.Messages, err = selectRows(ctx, tx, "SELECT "+messageColumns+" FROM muster_messages ORDER BY sequence", scanMessage)
		if err != nil {
			return err
		}
		snapshot.Approvals, err = selectRows(ctx, tx, "SELECT "+approvalColumns+" ORDER BY a.created_at,a.id", scanApproval)
		if err != nil {
			return err
		}
		snapshot.Tasks, err = selectRows(ctx, tx, "SELECT "+taskColumns+" FROM muster_tasks t ORDER BY t.updated_at DESC,t.id", scanTask)
		if err != nil {
			return err
		}
		snapshot.Projects, err = loadProjects(ctx, tx)
		if err != nil {
			return err
		}
		snapshot.Directories, err = selectRows(ctx, tx, `
			SELECT d.directory_id,d.label,d.device_id FROM muster_directories d
			JOIN muster_devices v USING (device_id) WHERE v.revoked_at IS NULL
			ORDER BY d.label,d.device_id,d.directory_id`, scanDirectory)
		return err
	})
	return snapshot, err
}

// TaskDetail 在同一只读事务中读取任务、运行、子任务及协作记录。
func (store *InventoryStore) TaskDetail(ctx context.Context, id string) (detail workspace.Detail, err error) {
	err = pgx.BeginTxFunc(ctx, store.pool, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	}, func(tx pgx.Tx) error {
		detail.Task, err = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM muster_tasks t WHERE t.id=$1", id))
		if err != nil {
			return notFound(err)
		}
		detail.Runs, err = selectRows(ctx, tx, `
			SELECT r.id,r.task_id,t.directory_id,r.status,r.session_id,r.outcome,r.created_at,r.updated_at,r.generation,r.lease_until,r.diagnostics,r.git_verification,
			CASE WHEN p.id IS NOT NULL THEN jsonb_build_object('id',p.id,'revision',p.policy_revision,'remote',p.remote,
				'target_ref',p.target_ref,'candidate_oid',p.verification->>'candidate_oid','status',p.status,'observed_oid',p.observed_oid) END
			FROM muster_runs r JOIN muster_tasks t ON t.id=r.task_id LEFT JOIN muster_git_push_intents p ON p.run_id=r.id
			WHERE r.task_id=$1 ORDER BY r.sequence`, scanRun, id)
		if err != nil {
			return err
		}
		detail.Messages, err = selectRows(ctx, tx, "SELECT "+messageColumns+" FROM muster_messages WHERE task_id=$1 ORDER BY sequence", scanMessage, id)
		if err != nil {
			return err
		}
		detail.Approvals, err = selectRows(ctx, tx, "SELECT "+approvalColumns+
			" WHERE a.task_id=$1 OR t.root_id=$1 ORDER BY a.created_at,a.id", scanApproval, id)
		if err != nil {
			return err
		}
		detail.Children, err = selectRows(ctx, tx, "SELECT "+taskColumns+" FROM muster_tasks t WHERE t.parent_id=$1 ORDER BY t.id", scanTask, id)
		if err != nil {
			return err
		}
		detail.Dependencies, err = selectRows(ctx, tx, `
			SELECT d.task_id,d.depends_on_task_id,d.generation,d.status,
				t.waiting_reason='children' AND d.generation=t.input_generation
			FROM muster_task_dependencies d JOIN muster_tasks t ON t.id=d.task_id
			WHERE d.task_id=$1 ORDER BY d.generation,d.depends_on_task_id`, func(row pgx.Row) (workspace.Dependency, error) {
			var value workspace.Dependency
			err := row.Scan(&value.TaskID, &value.DependsOnTaskID, &value.Generation, &value.Status, &value.Active)
			return value, err
		}, id)
		if err != nil {
			return err
		}
		detail.Transfers, err = selectRows(ctx, tx, "SELECT "+transferColumns+` FROM muster_transfers tr WHERE tr.task_id=$1
			OR EXISTS(SELECT 1 FROM muster_run_transfers rt JOIN muster_runs r ON r.id=rt.run_id
				WHERE r.task_id=$1 AND rt.transfer_id=tr.id)
			OR EXISTS(SELECT 1 FROM muster_wait_transfers w WHERE w.task_id=$1 AND w.transfer_id=tr.id)
			ORDER BY tr.created_at,tr.id`, scanTransfer, id)
		if err != nil {
			return err
		}
		detail.Inputs, err = taskInputs(ctx, tx, id)
		if err != nil {
			return err
		}
		detail.WaitedTransfers, err = selectRows(ctx, tx, `
			SELECT w.transfer_id,w.generation,
				t.waiting_reason IN ('children','transfers') AND w.generation=t.input_generation,
				CASE WHEN tr.received_at IS NOT NULL THEN 'received' ELSE 'waiting' END,tr.received_at
			FROM muster_wait_transfers w JOIN muster_tasks t ON t.id=w.task_id
			JOIN muster_transfers tr ON tr.id=w.transfer_id
			WHERE w.task_id=$1 ORDER BY w.generation,w.transfer_id`, func(row pgx.Row) (workspace.TransferWait, error) {
			var value workspace.TransferWait
			err := row.Scan(&value.TransferID, &value.Generation, &value.Active, &value.Status, &value.ReceivedAt)
			return value, err
		}, id)
		return err
	})
	return detail, err
}

func taskInputs(ctx context.Context, tx pgx.Tx, id string) ([]workspace.TaskInput, error) {
	return selectRows(ctx, tx, `
		WITH messages AS (
			SELECT id,status,created_at FROM muster_messages WHERE task_id=$1 AND kind='user'
		), runs AS (
			SELECT id,status,generation,created_at,claimed_at,prompt_transfer_id,dependency_results
			FROM muster_runs WHERE task_id=$1
		)
		SELECT COALESCE(a.id,m.id,r.id),CASE
			WHEN a.id IS NOT NULL THEN 'approval'
			WHEN m.id IS NOT NULL THEN 'message'
			WHEN r.prompt_transfer_id<>'' THEN 'delegation'
			WHEN r.dependency_results IS NOT NULL THEN 'dependency'
			ELSE 'execution' END,
			CASE WHEN t.status='waiting' AND t.waiting_reason='delivery' AND t.current_run_id=r.id
				THEN 'waiting_delivery' ELSE COALESCE(r.status,m.status) END,
			COALESCE(m.id,''),COALESCE(r.id,''),COALESCE(r.generation,0),
			ARRAY(SELECT transfer_id FROM muster_run_transfers WHERE run_id=r.id AND direction='input' ORDER BY transfer_id),
			COALESCE(m.created_at,r.created_at),r.claimed_at
		FROM messages m FULL JOIN runs r ON r.id='run-' || m.id
		LEFT JOIN muster_approvals a ON a.resume_run_id=r.id
		CROSS JOIN muster_tasks t WHERE t.id=$1
		ORDER BY COALESCE(m.created_at,r.created_at),COALESCE(m.id,r.id)`, func(row pgx.Row) (workspace.TaskInput, error) {
		var value workspace.TaskInput
		err := row.Scan(&value.ID, &value.Kind, &value.Status, &value.MessageID, &value.RunID,
			&value.Generation, &value.TransferIDs, &value.CreatedAt, &value.ClaimedAt)
		return value, err
	}, id)
}

// Projects 返回已登记项目及各自的设备、目录和 Agent 绑定。
func (store *InventoryStore) Projects(ctx context.Context) ([]workspace.Project, error) {
	var projects []workspace.Project
	err := pgx.BeginTxFunc(ctx, store.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		projects, err = loadProjects(ctx, tx)
		return err
	})
	return projects, err
}

func loadProjects(ctx context.Context, tx pgx.Tx) ([]workspace.Project, error) {
	projects, err := selectRows(ctx, tx, "SELECT "+projectColumns+" FROM muster_projects ORDER BY name,id", scanProject)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		projects[i].Bindings, err = projectBindings(ctx, tx, projects[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return projects, nil
}

func projectBindings(ctx context.Context, tx pgx.Tx, id string) ([]workspace.Binding, error) {
	return selectRows(ctx, tx, `
		SELECT b.id,b.device_id,b.directory_id,COALESCE(d.label,''),b.agent_kind,b.is_default
		FROM muster_project_bindings b LEFT JOIN muster_directories d USING (device_id,directory_id)
		WHERE b.project_id=$1 ORDER BY b.is_default DESC,b.id`, scanBinding, id)
}

func scanBinding(row pgx.Row) (value workspace.Binding, err error) {
	err = row.Scan(&value.ID, &value.DeviceID, &value.DirectoryID, &value.DirectoryLabel, &value.AgentKind, &value.IsDefault)
	return
}

// SaveProject 保存项目与绑定，要求恰好一个默认绑定，并兼容原有单绑定输入。
func (store *InventoryStore) SaveProject(ctx context.Context, project workspace.Project) (workspace.Project, error) {
	if project.GitPolicy != nil {
		policy := project.GitPolicy.Clone()
		policy.Revision = 0
		if err := policy.Validate(); err != nil {
			return workspace.Project{}, err
		}
		project.GitPolicy = &policy
	}
	err := store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if len(project.Bindings) == 0 {
			if project.AgentKind == "" {
				project.AgentKind = "dsh"
			}
			project.Bindings = []workspace.Binding{{
				ID: project.ID, DeviceID: project.DeviceID, DirectoryID: project.DirectoryID,
				AgentKind: project.AgentKind, IsDefault: true,
			}}
		}
		seen := make(map[string]bool, len(project.Bindings))
		var selected workspace.Binding
		for _, binding := range project.Bindings {
			if seen[binding.ID] || (binding.AgentKind != "dsh" && binding.AgentKind != "codex") {
				return workspace.ErrConflict
			}
			seen[binding.ID] = true
			err := tx.QueryRow(ctx, `SELECT d.label FROM muster_directories d JOIN muster_devices v USING (device_id)
				WHERE d.device_id=$1 AND d.directory_id=$2 AND v.revoked_at IS NULL FOR SHARE OF v`,
				binding.DeviceID, binding.DirectoryID).Scan(&binding.DirectoryLabel)
			if err != nil {
				return notFound(err)
			}
			if binding.IsDefault {
				if selected.ID != "" {
					return workspace.ErrConflict
				}
				selected = binding
			}
		}
		if selected.ID == "" {
			return workspace.ErrConflict
		}
		project.DeviceID, project.DirectoryID, project.DirectoryLabel, project.AgentKind =
			selected.DeviceID, selected.DirectoryID, selected.DirectoryLabel, selected.AgentKind
		if _, err := tx.Exec(ctx, `
			INSERT INTO muster_projects (id,name,device_id,directory_id,directory_label,agent_kind) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (id) DO UPDATE SET (name,device_id,directory_id,directory_label,agent_kind) =
				(EXCLUDED.name,EXCLUDED.device_id,EXCLUDED.directory_id,EXCLUDED.directory_label,EXCLUDED.agent_kind)`,
			project.ID, project.Name, project.DeviceID, project.DirectoryID, project.DirectoryLabel, project.AgentKind); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE muster_project_bindings SET is_default=false WHERE project_id=$1 AND is_default", project.ID); err != nil {
			return err
		}
		if err := saveProjectPolicy(ctx, tx, &project); err != nil {
			return err
		}
		for _, binding := range project.Bindings {
			tag, err := tx.Exec(ctx, `INSERT INTO muster_project_bindings (id,project_id,device_id,directory_id,agent_kind,is_default)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET
					(device_id,directory_id,agent_kind,is_default)=(EXCLUDED.device_id,EXCLUDED.directory_id,EXCLUDED.agent_kind,EXCLUDED.is_default)
				WHERE muster_project_bindings.project_id=EXCLUDED.project_id`, binding.ID, project.ID, binding.DeviceID,
				binding.DirectoryID, binding.AgentKind, binding.IsDefault)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return workspace.ErrConflict
			}
		}
		var err error
		project.Bindings, err = projectBindings(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return project, err
}

// saveProjectPolicy 沿用工作区写入锁，使规则版本与绑定和事件游标一起提交。
func saveProjectPolicy(ctx context.Context, tx pgx.Tx, project *workspace.Project) error {
	var prior *gitdelivery.Policy
	var revision int64
	err := tx.QueryRow(ctx, "SELECT git_policy,git_policy_revision FROM muster_projects WHERE id=$1", project.ID).
		Scan(&prior, &revision)
	if err != nil {
		return err
	}
	if project.GitPolicy == nil && !project.ClearGitPolicy {
		project.GitPolicy = prior
		return nil
	}
	if project.GitPolicy != nil {
		project.GitPolicy.Revision = revision
	}
	if reflect.DeepEqual(prior, project.GitPolicy) {
		return nil
	}
	revision++
	if project.GitPolicy != nil {
		project.GitPolicy.Revision = revision
	}
	_, err = tx.Exec(ctx, "UPDATE muster_projects SET git_policy=$2,git_policy_revision=$3 WHERE id=$1",
		project.ID, project.GitPolicy, revision)
	return err
}

// SubmitMessage 事务性保存用户消息和插件投递。
// 同一消息 ID 的重试须保留原文与任务意图。
func (store *InventoryStore) SubmitMessage(ctx context.Context, input workspace.Message) (message workspace.Message, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		var requested string
		err := tx.QueryRow(ctx, "SELECT requested_task_id FROM muster_messages WHERE id=$1", input.ID).Scan(&requested)
		if err == nil {
			message, err = scanMessage(tx.QueryRow(ctx, "SELECT "+messageColumns+" FROM muster_messages WHERE id=$1", input.ID))
			if err == nil && (message.Kind != "user" || input.Text != message.Text || input.TaskID != requested) {
				return workspace.ErrConflict
			}
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if input.TaskID != "" {
			var mode string
			if err := tx.QueryRow(ctx, "SELECT control_mode FROM muster_tasks WHERE id=$1", input.TaskID).Scan(&mode); err != nil {
				return notFound(err)
			}
			if mode == "cancel" {
				return workspace.ErrConflict
			}
		}
		message, err = scanMessage(tx.QueryRow(ctx, `
			INSERT INTO muster_messages (id,kind,body,requested_task_id,task_id,status)
			VALUES ($1,'user',$2,$3,NULLIF($3,''),'accepted') RETURNING `+messageColumns,
			input.ID, input.Text, input.TaskID))
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO muster_deliveries (id,kind,body,message_id,task_id)
			VALUES ($1,'message',$2,$3,NULLIF($4,''))`, "message-"+input.ID, input.Text, input.ID, input.TaskID); err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return message, err
}

// Visible writes and their revision commit in order. CLI work and polling waits stay outside the transaction.
func (store *InventoryStore) writeWorkspace(ctx context.Context, apply func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		var revision int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM muster_workspace FOR UPDATE").Scan(&revision); err != nil {
			return err
		}
		return apply(tx)
	})
}

func advanceWorkspace(ctx context.Context, tx pgx.Tx) error {
	var revision int64
	if err := tx.QueryRow(ctx, "UPDATE muster_workspace SET revision=revision+1 RETURNING revision").Scan(&revision); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "INSERT INTO muster_workspace_events (revision) VALUES ($1)", revision)
	return err
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return registry.ErrNotFound
	}
	return err
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func selectRows[T any](ctx context.Context, db querier, sql string, scan func(pgx.Row) (T, error), args ...any) ([]T, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []T{}
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func scanDirectory(row pgx.Row) (value workspace.Directory, err error) {
	err = row.Scan(&value.ID, &value.Label, &value.DeviceID)
	return
}

func scanProject(row pgx.Row) (value workspace.Project, err error) {
	err = row.Scan(&value.ID, &value.Name, &value.DeviceID, &value.DirectoryID, &value.DirectoryLabel, &value.AgentKind, &value.GitPolicy)
	return
}

func scanMessage(row pgx.Row) (value workspace.Message, err error) {
	err = row.Scan(&value.ID, &value.Kind, &value.Text, &value.TaskID, &value.Status, &value.CreatedAt)
	return
}

func scanTask(row pgx.Row) (value workspace.Task, err error) {
	var control taskControlState
	err = row.Scan(&value.ID, &value.ProjectID, &value.ProjectName, &value.DeviceID, &value.DirectoryID,
		&value.Status, &value.Title, &value.Result, &value.ReportPending, &value.UpdatedAt,
		&value.ParentTaskID, &value.RootTaskID, &value.BindingID, &value.AgentKind, &value.WaitingReason,
		&value.CurrentRunGeneration, &value.WaitGeneration, &control.mode, &control.active, &control.supported, &value.GitPolicyRevision, &value.GitPublish)
	control.status = value.Status
	value.ControlActions = control.actions()
	if value.Status == "waiting" {
		switch value.WaitingReason {
		case "children":
			value.Status = "waiting_dependencies"
		case "delivery", "transfers":
			value.Status = "waiting_delivery"
		}
	}
	value.Status = control.displayStatus(value.Status)
	return
}

func scanRun(row pgx.Row) (value workspace.Run, err error) {
	err = row.Scan(&value.ID, &value.TaskID, &value.DirectoryID, &value.Status, &value.SessionID,
		&value.Text, &value.CreatedAt, &value.UpdatedAt, &value.Generation, &value.LeaseUntil, &value.Diagnostics, &value.GitVerification, &value.GitPublication)
	return
}
