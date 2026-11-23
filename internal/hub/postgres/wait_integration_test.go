package postgres

import (
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func TestAmendmentPreservesPendingWait(t *testing.T) {
	for _, scenario := range []string{"child finishes first", "amendment finishes first", "amendment replaces wait", "approval survives restart"} {
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
				run, err := store.ClaimRun(ctx, device, token)
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
			_, err = store.SubmitMessage(ctx, workspace.Message{ID: "follow-up", TaskID: "root", Text: "Use CSV"})
			check(err)
			delivery, err := store.ClaimDelivery(ctx)
			check(err)
			_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, TaskID: "root", Prompt: "Use CSV"})
			check(err)
			check(store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}))
			amend := claim("home", home, "run-follow-up")
			if amend.SessionID != "native" {
				t.Fatalf("amendment route changed: %+v", amend)
			}
			record("home", home, amend, 1, workspace.RunEvent{Kind: "started"})
			if got := detail(); len(got.Dependencies) != 1 || !got.Dependencies[0].Active || got.Task.WaitGeneration != 1 {
				t.Fatalf("amendment did not preserve active wait: %+v", got)
			}
			child := claim("office", office, "child-run")
			finishChild := func() { record("office", office, child, 1, workspace.RunEvent{Kind: "failed"}) }

			wantRuns := 3
			if scenario == "approval survives restart" {
				request := workspace.RunEvent{Kind: "approval_requested", Approval: &approval.Question{Question: "仍然使用 CSV 吗？", Options: []string{"使用", "改用 JSON"}}}
				record("home", home, amend, 2, request)
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
				record("home", home, amend, 3, workspace.RunEvent{Kind: "completed", Text: "旧轮次迟到结果"})
				if got := detail(); got.Approvals[0].Status != "answered" {
					t.Fatalf("旧轮次结果错误推进答复状态: %+v", got.Approvals)
				}
				record("home", home, decision, 1, workspace.RunEvent{Kind: "completed", Text: "已按答复处理"})
				if got := detail(); got.Approvals[0].Status != "handled" {
					t.Fatalf("答复未标记已处理: %+v", got.Approvals)
				}
				wantRuns = 4
			} else if scenario == "child finishes first" {
				finishChild()
				current := claim("home", home, "run-follow-up")
				if current.Lease != amend.Lease {
					t.Fatal("amendment ownership changed while executing")
				}
				record("home", home, amend, 2, workspace.RunEvent{Kind: "completed", Text: "Amended"})
			} else if scenario == "amendment finishes first" {
				record("home", home, amend, 2, workspace.RunEvent{Kind: "failed", Text: "Amendment encountered an error"})
				if task := detail().Task; task.Status != "waiting_dependencies" || task.ReportPending {
					t.Fatalf("terminal amendment should restore waiting: %+v", task)
				}
				finishChild()
			} else {
				transfer, err := store.CreateTransfer(ctx, "home", home, amend.ID, workspace.TransferRequest{
					ID: "replacement", TargetDeviceID: "office", Kind: "file", Name: "new.csv", Generation: amend.Generation, Lease: amend.Lease,
					SHA256: strings.Repeat("a", 64), Size: 1,
				})
				check(err)
				record("home", home, amend, 2, workspace.RunEvent{Kind: "waiting", TransferIDs: []string{transfer.ID}})
				finishChild()
				if got := detail(); got.Task.Status != "waiting_delivery" || got.Task.WaitGeneration != 2 ||
					got.Dependencies[0].Status != "superseded" || len(got.Runs) != 2 {
					t.Fatalf("old child affected replacement wait: %+v", got)
				}
				check(store.AcknowledgeTransfer(ctx, "office", office, transfer.ID, transfer.Ticket))
				check(store.AcknowledgeTransfer(ctx, "office", office, transfer.ID, transfer.Ticket))
			}

			got := detail()
			if got.Task.Status != "queued" || got.Task.ReportPending || len(got.Runs) != wantRuns {
				t.Fatalf("expected a single dependency wake and no interim report: %+v", got)
			}
			resume := claim("home", home, got.Runs[wantRuns-1].ID)
			if resume.SessionID != "native" || resume.Generation <= amend.Generation {
				t.Fatalf("wake did not resume original session: %+v", resume)
			}
			if scenario != "amendment replaces wait" && (len(resume.DependencyResults) != 1 ||
				resume.DependencyResults[0].TaskID != "child" || resume.DependencyResults[0].Status != "failed") {
				t.Fatalf("wake lost the preserved dependency result: %+v", resume)
			}
			record("home", home, resume, 1, workspace.RunEvent{Kind: "completed", Text: "Root delivered"})
			if report, err := store.ClaimDelivery(ctx); err != nil || report.ID != "report-"+resume.ID {
				t.Fatalf("unexpected root report %+v, %v", report, err)
			}
		})
	}
}
