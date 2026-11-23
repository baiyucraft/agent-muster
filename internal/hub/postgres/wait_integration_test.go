package postgres

import (
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func TestAmendmentPreservesPendingWait(t *testing.T) {
	for _, scenario := range []string{
		"child finishes first",
		"child finishes before append",
		"dependency queued before acceptance",
		"amendment replaces queued dependency",
		"explicit dependency recovery runs before amendment",
		"legacy dependency recovery runs before amendment",
		"center answers without append",
		"amendment finishes first",
		"amendment replaces wait",
		"approval survives restart",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx, address := newTestSchema(t)
			store, err := OpenInventory(ctx, address)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var home, office [32]byte
			home[0], office[0] = 1, 2
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = store.pool.Exec(ctx, `INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
				VALUES ('home',$1,'Home','home','windows','amd64'),('office',$2,'Office','office','windows','amd64')`, home[:], office[:])
			check(err)
			_, err = store.pool.Exec(ctx, `
				INSERT INTO muster_directories(device_id,directory_id,label) VALUES ('home','folder','Folder');
				INSERT INTO muster_projects(id,name,device_id,directory_id,directory_label)
					VALUES ('project','Project','home','folder','Folder');
				INSERT INTO muster_project_bindings(id,project_id,device_id,directory_id,agent_kind,is_default)
					VALUES ('project','project','home','folder','dsh',true);
				INSERT INTO muster_tasks(id,project_id,project_name,device_id,directory_id,title,status,current_run_id,root_id,parent_id,binding_id)
					VALUES ('root','project','Project','home','folder','Root','queued','root-run','root',NULL,'project'),
					       ('child','project','Project','office','folder','Child','queued','child-run','root','root','project');
				INSERT INTO muster_runs(id,task_id,input,device_id)
					VALUES ('root-run','root','root input','home'),('child-run','child','','office');
				INSERT INTO muster_delegations(parent_run_id,request_key,child_task_id,request_digest)
					VALUES ('root-run','key','child','\x00');
			`, pgx.QueryExecModeSimpleProtocol)
			check(err)

			claim := func(device string, token [32]byte, want string) workspace.RunClaim {
				t.Helper()
				run, err := store.ClaimRunWithControl(ctx, device, token, true)
				if err != nil || run.ID != want {
					t.Fatalf("claim %s got %s, err %v", device, run.ID, err)
				}
				return run
			}
			record := func(device string, token [32]byte, run workspace.RunClaim, seq int64, event workspace.RunEvent) {
				t.Helper()
				event.Generation, event.Lease = run.Generation, run.Lease
				check(store.RecordRunEvent(ctx, device, token, run.ID, seq, event))
			}
			detail := func() workspace.Detail {
				t.Helper()
				task, err := store.TaskDetail(ctx, "root")
				check(err)
				return task
			}

			parent := claim("home", home, "root-run")
			record("home", home, parent, 1, workspace.RunEvent{Kind: "started", SessionID: "native"})
			record("home", home, parent, 2, workspace.RunEvent{Kind: "waiting", WaitForTaskIDs: []string{"child"}})
			child := claim("office", office, "child-run")
			finishChild := func() { record("office", office, child, 1, workspace.RunEvent{Kind: "failed"}) }

			replacesQueuedDependency := scenario == "amendment replaces queued dependency"
			legacyRecovery := scenario == "legacy dependency recovery runs before amendment"
			explicitRecovery := scenario == "explicit dependency recovery runs before amendment" || legacyRecovery
			dependencyQueued := scenario == "dependency queued before acceptance" || replacesQueuedDependency || explicitRecovery
			var originalWake string
			if dependencyQueued {
				finishChild()
				got := detail()
				if got.Task.Status != "queued" || len(got.Runs) != 2 || got.Runs[1].Status != "queued" {
					t.Fatalf("子任务完成后，尚无追加要求时应安排依赖续接: %+v", got)
				}
				originalWake = got.Runs[1].ID
			}
			if explicitRecovery {
				wake := claim("home", home, originalWake)
				record("home", home, wake, 1, workspace.RunEvent{Kind: "started"})
				controlTaskForTest(t, ctx, store, "root", "stop_current", firstControlRequest, "stopping")
				record("home", home, wake, 2, workspace.RunEvent{Kind: "stopped"})
				controlTaskForTest(t, ctx, store, "root", "resume", secondControlRequest, "queued")
				recovery := detail().Runs[2].ID
				if legacyRecovery {
					_, err = store.pool.Exec(ctx, "UPDATE muster_runs SET ready_priority=$2 WHERE id=$1", recovery, priorityDependency)
					check(err)
				}

				_, err = store.SubmitMessage(ctx, workspace.Message{ID: "follow-up", TaskID: "root", Text: "Use CSV"})
				check(err)
				if legacyRecovery {
					store.Close()
					store, err = OpenInventory(ctx, address)
					check(err)
					defer store.Close()
				}
				delivery, err := store.ClaimDelivery(ctx)
				check(err)
				_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, TaskID: "root", Prompt: "Use CSV"})
				check(err)
				resumed := claim("home", home, recovery)
				if resumed.SessionID != "native" || len(resumed.DependencyResults) != 1 {
					t.Fatalf("显式恢复没有保留原生会话与依赖结果: %+v", resumed)
				}
				record("home", home, resumed, 1, workspace.RunEvent{Kind: "completed", Text: "已核对现场"})
				claim("home", home, "run-follow-up")
				return
			}
			_, err = store.SubmitMessage(ctx, workspace.Message{ID: "follow-up", TaskID: "root", Text: "Use CSV"})
			check(err)
			waitingForCenter := scenario == "child finishes before append" || scenario == "center answers without append"
			if waitingForCenter {
				finishChild()
				finishChild()
			}
			if waitingForCenter || dependencyQueued {
				claim("home", home, "")
			}
			beforeAppend := detail()
			if beforeAppend.Task.ReportPending || len(beforeAppend.Dependencies) != 1 {
				t.Fatalf("等待中心处理追加消息时丢失了依赖或提前汇报: %+v", beforeAppend)
			}
			delivery, err := store.ClaimDelivery(ctx)
			check(err)
			if delivery.Kind != "message" || delivery.MessageID != "follow-up" {
				t.Fatalf("中心没有领取到用户追加消息: %+v", delivery)
			}
			wantRuns := 3
			previous := parent
			completion := workspace.Completion{Lease: delivery.Lease, Status: "handled"}
			if scenario == "center answers without append" {
				completion.Text = "已直接回答"
				check(store.CompleteDelivery(ctx, delivery.ID, completion))
				check(store.CompleteDelivery(ctx, delivery.ID, completion))
				wantRuns = 2
			} else {
				_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, TaskID: "root", Prompt: "Use CSV"})
				check(err)
				check(store.CompleteDelivery(ctx, delivery.ID, completion))
				previous = claim("home", home, "run-follow-up")
				if previous.SessionID != "native" {
					t.Fatalf("amendment route changed: %+v", previous)
				}
				record("home", home, previous, 1, workspace.RunEvent{Kind: "started"})
				if got := detail(); len(got.Dependencies) != 1 || !got.Dependencies[0].Active || got.Task.WaitGeneration != 1 {
					t.Fatalf("amendment did not preserve active wait: %+v", got)
				}
			}

			switch scenario {
			case "center answers without append":
				// 中心直接答复后无需设备处理该消息，原有依赖可继续调度。
			case "child finishes before append", "dependency queued before acceptance":
				record("home", home, previous, 2, workspace.RunEvent{Kind: "completed", Text: "Amended"})
			case "approval survives restart":
				request := workspace.RunEvent{Kind: "approval_requested", Approval: &approval.Question{Question: "仍然使用 CSV 吗？", Options: []string{"使用", "改用 JSON"}}}
				record("home", home, previous, 2, request)
				finishChild()
				claim("home", home, "")
				pending := detail()
				if pending.Task.Status != "waiting_reply" || pending.Task.ReportPending ||
					len(pending.Approvals) != 1 || len(pending.Runs) != 2 ||
					!pending.Dependencies[0].Active || pending.Dependencies[0].Status != "failed" {
					t.Fatalf("请示期间丢失依赖状态或提前续接: %+v", pending)
				}
				answered, err := store.AnswerApproval(ctx, pending.Approvals[0].ID, "使用")
				check(err)
				if answered.Status != "answered" || answered.Answer != "使用" {
					t.Fatalf("答复写入异常: %+v", answered)
				}
				store.Close()
				store, err = OpenInventory(ctx, address)
				check(err)
				defer store.Close()
				afterRestart := detail()
				if afterRestart.Task.Status != "queued" || len(afterRestart.Runs) != 3 ||
					afterRestart.Approvals[0].Status != "answered" || afterRestart.Approvals[0].Answer != "使用" {
					t.Fatalf("重启后的答复或运行未恢复: %+v", afterRestart)
				}
				decision := claim("home", home, afterRestart.Runs[2].ID)
				if decision.SessionID != "native" {
					t.Fatalf("答复未在原生会话续接: %+v", decision)
				}
				record("home", home, previous, 3, workspace.RunEvent{Kind: "completed", Text: "旧轮次迟到结果"})
				if got := detail(); got.Approvals[0].Status != "answered" {
					t.Fatalf("旧轮次结果错误推进答复状态: %+v", got.Approvals)
				}
				record("home", home, decision, 1, workspace.RunEvent{Kind: "completed", Text: "已按答复处理"})
				if got := detail(); got.Approvals[0].Status != "handled" {
					t.Fatalf("答复未标记已处理: %+v", got.Approvals)
				}
				wantRuns = 4
			case "child finishes first":
				finishChild()
				current := claim("home", home, "run-follow-up")
				if current.Lease != previous.Lease {
					t.Fatal("amendment ownership changed while executing")
				}
				record("home", home, previous, 2, workspace.RunEvent{Kind: "completed", Text: "Amended"})
			case "amendment finishes first":
				record("home", home, previous, 2, workspace.RunEvent{Kind: "failed", Text: "Amendment encountered an error"})
				if task := detail().Task; task.Status != "waiting_dependencies" || task.ReportPending {
					t.Fatalf("terminal amendment should restore waiting: %+v", task)
				}
				finishChild()
			case "amendment replaces wait", "amendment replaces queued dependency":
				transfer, err := store.CreateTransfer(ctx, "home", home, previous.ID, workspace.TransferRequest{
					ID: "replacement", TargetDeviceID: "office", Kind: "file", Name: "new.csv", Generation: previous.Generation, Lease: previous.Lease,
					SHA256: strings.Repeat("a", 64), Size: 1,
				})
				check(err)
				record("home", home, previous, 2, workspace.RunEvent{Kind: "waiting", TransferIDs: []string{transfer.ID}})
				finishChild()
				if got := detail(); got.Task.Status != "waiting_delivery" || got.Task.WaitGeneration != 2 ||
					(!replacesQueuedDependency && (got.Dependencies[0].Status != "superseded" || len(got.Runs) != 2)) {
					t.Fatalf("old child affected replacement wait: %+v", got)
				}
				claim("home", home, "")
				check(store.AcknowledgeTransfer(ctx, "office", office, transfer.ID, transfer.Ticket))
				check(store.AcknowledgeTransfer(ctx, "office", office, transfer.ID, transfer.Ticket))
			}

			got := detail()
			if got.Task.Status != "queued" || got.Task.ReportPending || (!replacesQueuedDependency && len(got.Runs) != wantRuns) {
				t.Fatalf("expected a single dependency wake and no interim report: %+v", got)
			}
			var dependencyRunID string
			for _, run := range got.Runs {
				if run.Status != "queued" {
					continue
				}
				if dependencyRunID != "" {
					t.Fatalf("同一依赖等待产生了多条可执行续接: %+v", got.Runs)
				}
				dependencyRunID = run.ID
			}
			if dependencyRunID == "" {
				t.Fatalf("没有可执行的依赖续接: %+v", got.Runs)
			}
			resume := claim("home", home, dependencyRunID)
			if resume.SessionID != "native" || resume.Generation <= previous.Generation {
				t.Fatalf("wake did not resume original session: %+v", resume)
			}
			if scenario == "amendment replaces wait" || replacesQueuedDependency {
				if resume.ID == originalWake || len(resume.DependencyResults) != 0 {
					t.Fatalf("替换等待后，续接未使用新交接输入: %+v", resume)
				}
			} else if len(resume.DependencyResults) != 1 ||
				resume.DependencyResults[0].TaskID != "child" || resume.DependencyResults[0].Status != "failed" {
				t.Fatalf("wake lost the preserved dependency result: %+v", resume)
			}
			record("home", home, resume, 1, workspace.RunEvent{Kind: "completed", Text: "Root delivered"})
			claim("home", home, "")
			if report, err := store.ClaimDelivery(ctx); err != nil || report.ID != "report-"+resume.ID {
				t.Fatalf("unexpected root report %+v, %v", report, err)
			}
		})
	}
}
