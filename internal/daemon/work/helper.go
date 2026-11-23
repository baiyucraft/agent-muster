package work

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/helper"
)

var helperKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type localHelper struct {
	peer     *collaboration
	address  string
	listener net.Listener
	mu       sync.Mutex
	active   *activeRun
}

type activeRun struct {
	run        control.Run
	directory  string
	capability string
	ctx        context.Context
}

func startHelper(ctx context.Context, peer *collaboration) (*localHelper, error) {
	listener, err := helper.Listen()
	if err != nil {
		return nil, err
	}
	service := &localHelper{peer: peer, address: listener.Addr().String(), listener: listener}
	go helper.Serve(ctx, listener, service.handle)
	return service, nil
}

func (service *localHelper) setRun(ctx context.Context, run control.Run, directory string) ([]string, func(), error) {
	if service == nil {
		return nil, func() {}, nil
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, nil, err
	}
	capability := hex.EncodeToString(secret[:])
	runContext, cancel := context.WithCancel(ctx)
	active := &activeRun{run: run, directory: directory, capability: capability, ctx: runContext}
	service.mu.Lock()
	service.active = active
	service.mu.Unlock()
	return []string{"MUSTER_PIPE=" + service.address, "MUSTER_RUN_CAPABILITY=" + capability}, func() {
		service.mu.Lock()
		if service.active == active {
			service.active = nil
		}
		service.mu.Unlock()
		cancel()
	}, nil
}

func (service *localHelper) handle(ctx context.Context, request helper.Request) (any, error) {
	service.mu.Lock()
	active := service.active
	service.mu.Unlock()
	if active == nil || subtle.ConstantTimeCompare([]byte(request.Capability), []byte(active.capability)) != 1 {
		return nil, errors.New("当前运行未给此命令授权")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(active.ctx, cancel)
	defer stop()
	if err := active.ctx.Err(); err != nil {
		return nil, err
	}
	run := active.run
	proof := control.LeaseProof{Generation: run.Generation, Lease: run.Lease}
	switch request.Command {
	case "status":
		return map[string]any{"run_id": run.ID, "task_id": run.TaskID, "root_task_id": run.RootTaskID, "project_id": run.ProjectID, "binding_id": run.BindingID, "parent_device_id": run.ParentDeviceID, "attachments": run.Attachments, "delegation_targets": run.DelegationTargets, "dependency_results": run.DependencyResults}, nil
	case "artifact":
		if !allowsArtifacts(run) {
			return nil, errors.New("本轮未启用普通目录产物登记")
		}
		return service.peer.worker.registerArtifact(ctx, run.ID, active.directory, request.File)
	case "delegate", "spawn":
		if !helperKey.MatchString(request.Key) || request.Prompt == "" || request.Title == "" || request.TargetDeviceID == "" || request.BindingID == "" {
			return nil, errors.New("delegate 需要使用稳定 key、任务标题、提示词、目标设备与绑定")
		}
		project := request.ProjectID
		if project == "" {
			project = run.ProjectID
		}
		promptID := transferKey(run.ID, "prompt", request.Key)
		delegation := control.Delegation{LeaseProof: proof, RequestKey: request.Key, ProjectID: project, BindingID: request.BindingID, Title: request.Title, PromptTransferID: promptID, TransferIDs: request.TransferIDs}
		return service.peer.stageDelegation(ctx, run, request.TargetDeviceID, delegation, []byte(request.Prompt))
	case "send":
		if !helperKey.MatchString(request.Key) || request.TargetDeviceID == "" || (request.File == "") == (request.Message == "") {
			return nil, errors.New("send 需要稳定 key、目标设备以及一项文件路径或文本消息")
		}
		kind := "file"
		if request.Message != "" {
			kind = "message"
		}
		id := transferKey(run.ID, kind, request.Key)
		if request.Message != "" {
			if len(request.Message) > 64<<10 {
				return nil, errors.New("消息正文超过 64 KiB")
			}
			return service.peer.stageTransfer(ctx, run, id, request.TargetDeviceID, "message.txt", kind, []byte(request.Message))
		}
		body, err := readProjectFile(active.directory, request.File)
		if err != nil {
			return nil, err
		}
		return service.peer.stageTransfer(ctx, run, id, request.TargetDeviceID, filepath.Base(request.File), kind, body)
	case "receive":
		for _, item := range run.Attachments {
			if item.ID == request.TransferID && item.Kind != "prompt" {
				return service.peer.worker.exportTransfer(ctx, active.directory, item)
			}
		}
		return nil, errors.New("此运行没有收到该投递")
	case "wait":
		if len(request.TaskIDs) == 0 && len(request.TransferIDs) == 0 {
			return nil, errors.New("wait 需要依赖任务或投递 ID")
		}
		event := control.RunEvent{Kind: "waiting", Generation: run.Generation, Lease: run.Lease, WaitForTaskIDs: request.TaskIDs, TransferIDs: request.TransferIDs}
		if err := service.peer.client.ValidateWait(ctx, service.peer.identity, run.ID, event); err != nil {
			return nil, fmt.Errorf("Hub 未确认等待依赖: %w", err)
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		_, err = service.peer.worker.db.ExecContext(ctx, "INSERT INTO helper_waits(run_id,payload) VALUES (?,?) ON CONFLICT(run_id) DO UPDATE SET payload=excluded.payload", run.ID, encoded)
		return map[string]bool{"recorded": err == nil}, err
	default:
		return nil, errors.New("未知 muster 命令")
	}
}

func transferKey(run, kind, key string) string {
	digest := sha256.Sum256([]byte(run + "\x00" + kind + "\x00" + key))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("transfer-%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func (peer *collaboration) stageTransfer(ctx context.Context, run control.Run, id, target, name, kind string, body []byte) (control.Transfer, error) {
	if !peer.enabled() {
		return control.Transfer{}, errors.New("设备互联尚未启动")
	}
	if len(body) > control.MaxPeerBody {
		return control.Transfer{}, errors.New("投递内容超过 32 MiB")
	}
	manifest := newManifest(run, id, target, name, kind, body)
	tx, err := peer.worker.db.BeginTx(ctx, nil)
	if err != nil {
		return control.Transfer{}, err
	}
	defer tx.Rollback()
	if target == run.ParentDeviceID && (kind == "file" || kind == "message") {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM peer_manifests
			WHERE run_id=? AND id<>? AND json_extract(CAST(data AS TEXT),'$.target_device_id')=?
			AND json_extract(CAST(data AS TEXT),'$.kind') IN ('file','message')`, run.ID, id, target).Scan(&count); err != nil {
			return control.Transfer{}, err
		}
		if count >= 31 {
			return control.Transfer{}, errors.New("本轮已达到 31 个返回附件的上限")
		}
	}
	if err := saveTransfer(ctx, tx, run.ID, manifest, body); err != nil {
		return control.Transfer{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.Transfer{}, err
	}
	transfer, err := peer.publishTransfer(ctx, id)
	// This metadata goes to the agent via the local CLI. Authentication
	// material is used only by the device-to-device transport.
	transfer.Ticket = ""
	transfer.Peer = nil
	return transfer, err
}

func newManifest(run control.Run, id, target, name, kind string, body []byte) control.Manifest {
	return control.Manifest{
		LeaseProof: control.LeaseProof{Generation: run.Generation, Lease: run.Lease},
		ID:         id, TargetDeviceID: target, Name: name, Kind: kind, SHA256: transferHash(body), Size: int64(len(body)),
	}
}

func saveTransfer(ctx context.Context, tx *sql.Tx, runID string, manifest control.Manifest, body []byte) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	var published bool
	err = tx.QueryRowContext(ctx, `
		INSERT INTO peer_manifests(id,run_id,data) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET id=excluded.id
		WHERE (peer_manifests.run_id,peer_manifests.data)=(excluded.run_id,excluded.data)
		RETURNING published`, manifest.ID, runID, data).Scan(&published)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("同一投递 key 已保存不同内容")
	}
	if err != nil || published {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO peer_outbox(id,sha256,body) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING`, manifest.ID, manifest.SHA256, body)
	return err
}

func (peer *collaboration) publishTransfer(ctx context.Context, id string) (control.Transfer, error) {
	peer.publishMu.Lock()
	defer peer.publishMu.Unlock()
	var runID string
	var data []byte
	var published bool
	err := peer.worker.db.QueryRowContext(ctx, `SELECT run_id,data,published FROM peer_manifests WHERE id=?`, id).Scan(&runID, &data, &published)
	if err != nil {
		return control.Transfer{}, err
	}
	if published {
		var manifest control.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return control.Transfer{}, err
		}
		return control.Transfer{ID: id, RunID: runID, SourceDeviceID: peer.identity.DeviceID, TargetDeviceID: manifest.TargetDeviceID, Name: manifest.Name, Kind: manifest.Kind, SHA256: manifest.SHA256, Size: manifest.Size}, nil
	}
	var manifest control.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return control.Transfer{}, err
	}
	remote, err := peer.client.CreateTransfer(ctx, peer.identity, runID, manifest)
	if err != nil {
		return control.Transfer{}, err
	}
	if remote.ID != id {
		return control.Transfer{}, errors.New("Hub 返回了不匹配的投递 ID")
	}
	_, err = peer.worker.db.ExecContext(ctx, `UPDATE peer_manifests SET published=1 WHERE id=?`, id)
	return remote, err
}

func (worker *Worker) publishedReplies(ctx context.Context, runID, target string) ([]string, error) {
	rows, err := worker.db.QueryContext(ctx, `SELECT id FROM peer_manifests
		WHERE run_id=? AND published=1 AND json_extract(CAST(data AS TEXT),'$.target_device_id')=?
		AND json_extract(CAST(data AS TEXT),'$.kind') IN ('file','message') ORDER BY rowid`, runID, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func readProjectFile(directory, name string) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, errors.New("请指定工作目录内的相对文件路径")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > control.MaxPeerBody {
		return nil, errors.New("只允许投递不超过 32 MiB 的普通文件")
	}
	data, err := io.ReadAll(io.LimitReader(file, control.MaxPeerBody+1))
	if len(data) > control.MaxPeerBody {
		return nil, errors.New("投递文件超过 32 MiB")
	}
	return data, err
}

func (worker *Worker) readTransfer(ctx context.Context, id string) ([]byte, error) {
	if !transferID.MatchString(id) {
		return nil, errors.New("投递 ID 格式无效")
	}
	var body []byte
	if err := worker.db.QueryRowContext(ctx, "SELECT body FROM peer_inbox WHERE id=?", id).Scan(&body); err != nil {
		return nil, err
	}
	return body, nil
}

func (worker *Worker) readPrompt(ctx context.Context, id string) ([]byte, error) {
	if !transferID.MatchString(id) {
		return nil, errors.New("提示词投递 ID 格式无效")
	}
	var body []byte
	var hash string
	var size int64
	if err := worker.db.QueryRowContext(ctx, `SELECT body,sha256,size FROM peer_inbox WHERE id=? AND kind='prompt'`, id).Scan(&body, &hash, &size); err != nil {
		return nil, err
	}
	if int64(len(body)) != size || transferHash(body) != hash {
		return nil, errors.New("本机提示词与投递记录不匹配")
	}
	return body, nil
}

func (worker *Worker) exportTransfer(ctx context.Context, directory string, item control.TransferReference) (string, error) {
	body, err := worker.readTransfer(ctx, item.ID)
	if err != nil {
		return "", err
	}
	if int64(len(body)) != item.Size || transferHash(body) != item.SHA256 {
		return "", errors.New("本机附件与投递记录不匹配")
	}
	if !filepath.IsLocal(item.Name) || strings.ContainsAny(item.Name, "/\\") {
		return "", errors.New("无效的附件名称")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	folder := filepath.Join(".muster-inbox", item.ID)
	if err := root.MkdirAll(folder, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(folder, item.Name)
	file, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		existing, err := root.ReadFile(path)
		if err == nil && transferHash(existing) == item.SHA256 {
			return path, nil
		}
		return "", errors.New("目标附件路径已有不同文件")
	}
	if err != nil {
		return "", err
	}
	_, err = file.Write(body)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		_ = root.Remove(path)
	}
	return path, err
}

func (worker *Worker) requestedWait(ctx context.Context, runID string) (control.RunEvent, error) {
	var encoded []byte
	err := worker.db.QueryRowContext(ctx, "SELECT payload FROM helper_waits WHERE run_id=?", runID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return control.RunEvent{}, nil
	}
	if err != nil {
		return control.RunEvent{}, err
	}
	var event control.RunEvent
	err = json.Unmarshal(encoded, &event)
	return event, err
}
