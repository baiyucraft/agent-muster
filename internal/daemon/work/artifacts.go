package work

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

const artifactInstructions = "\n\n要在任务详情交付文件，请在写完并关闭文件后运行 muster artifact -file <工作目录内的相对路径>。每轮最多登记 8 个文件，每个文件最多 32 MiB。"

func allowsArtifacts(run control.Run) bool {
	return artifactPlatformSupported && run.ArtifactsSupported && run.GitPolicy == nil && !run.GitPublish
}

func (worker *Worker) registerArtifact(ctx context.Context, runID, directory, name string) (artifact.Reference, error) {
	path := filepath.ToSlash(name)
	if !artifact.ValidPath(path) || !filepath.IsLocal(filepath.FromSlash(path)) {
		return artifact.Reference{}, errors.New("请指定工作目录内的相对文件路径")
	}
	select {
	case worker.artifactRegistration <- struct{}{}:
		defer func() { <-worker.artifactRegistration }()
	case <-ctx.Done():
		return artifact.Reference{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	file, err := openArtifactFile(directory, path)
	if err != nil {
		return artifact.Reference{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return artifact.Reference{}, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > artifact.MaxSize {
		return artifact.Reference{}, errors.New("只允许登记不超过 32 MiB 的普通文件")
	}
	body := make([]byte, info.Size())
	if _, err := io.ReadFull(file, body); err != nil {
		return artifact.Reference{}, err
	}
	var extra [1]byte
	if _, err := io.ReadFull(file, extra[:]); !errors.Is(err, io.EOF) {
		return artifact.Reference{}, errors.Join(errors.New("产物读取时未在登记大小处结束"), err)
	}
	digest := transferHash(body)
	ref := artifact.Reference{ID: artifact.ID(runID, path, digest), Path: path, SHA256: digest, Size: int64(len(body))}

	tx, err := worker.db.BeginTx(ctx, nil)
	if err != nil {
		return artifact.Reference{}, err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM runs WHERE id=?", runID).Scan(&state); err != nil {
		return artifact.Reference{}, err
	}
	if state != "started" {
		return artifact.Reference{}, errors.New("本轮未处于执行状态")
	}
	var count int
	var same bool
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(id=?),0) FROM run_artifacts WHERE run_id=?", ref.ID, runID).Scan(&count, &same); err != nil {
		return artifact.Reference{}, err
	}
	if !same {
		if count >= artifact.MaxCount {
			return artifact.Reference{}, errors.New("本轮已达到产物登记数量上限")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_artifacts(run_id,id,path,sha256,size,body) VALUES (?,?,?,?,?,?)`, runID, ref.ID, ref.Path, ref.SHA256, ref.Size, body); err != nil {
			return artifact.Reference{}, err
		}
	}
	// 元数据与文件副本共用一次本地提交，命令成功后即可随终态补报。
	return ref, tx.Commit()
}

func collectArtifacts(ctx context.Context, tx *sql.Tx, runID string) ([]artifact.Reference, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id,path,sha256,size FROM run_artifacts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []artifact.Reference
	for rows.Next() {
		var ref artifact.Reference
		if err := rows.Scan(&ref.ID, &ref.Path, &ref.SHA256, &ref.Size); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// ReadArtifact 根据运行和产物 ID 读取本机快照，并复核登记时的大小与摘要。
// 缺少快照时返回 artifact.ErrMissing，其他错误由调用方按读失败处理。
func (worker *Worker) ReadArtifact(ctx context.Context, runID, artifactID string) ([]byte, error) {
	if !artifact.ValidID(artifactID) {
		return nil, artifact.ErrMissing
	}
	var ref artifact.Reference
	ref.ID = artifactID
	var body []byte
	err := worker.db.QueryRowContext(ctx, "SELECT path,sha256,size,body FROM run_artifacts WHERE run_id=? AND id=?",
		runID, artifactID).Scan(&ref.Path, &ref.SHA256, &ref.Size, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, artifact.ErrMissing
	}
	if err != nil {
		return nil, err
	}
	if ref.Size < 0 || ref.Size > artifact.MaxSize || !artifact.ValidPath(ref.Path) ||
		ref.ID != artifact.ID(runID, ref.Path, ref.SHA256) || !ref.Matches(body) {
		return nil, errors.New("本机产物与登记信息不匹配")
	}
	return body, nil
}

type artifactReadClient interface {
	ArtifactRead(context.Context, control.Identity) (artifact.ReadRequest, error)
	ReportArtifact(context.Context, control.Identity, string, string, []byte) error
}

func (worker *Worker) artifactReadLoop(ctx context.Context, client artifactReadClient, identity control.Identity) {
	for ctx.Err() == nil {
		request, err := client.ArtifactRead(ctx, identity)
		if errors.Is(err, control.ErrUnauthorized) {
			return
		}
		delay := 2 * time.Second
		var status *control.HTTPError
		switch {
		case errors.As(err, &status) && status.Status == http.StatusNotFound:
			// 旧版 Hub 尚未提供下载接口，仍周期复查以支持在线升级。
			delay = 30 * time.Second
		case err == nil && request.ID != "":
			body, readErr := worker.ReadArtifact(ctx, request.RunID, request.ArtifactID)
			result := "available"
			if errors.Is(readErr, artifact.ErrMissing) {
				result = "missing"
			} else if readErr != nil {
				result = "failed"
			}
			// 请求和回复都使用同一 ID。响应丢失后，下一轮继续领取尚未完成的请求。
			if err := client.ReportArtifact(ctx, identity, request.ID, result, body); errors.Is(err, control.ErrUnauthorized) {
				return
			}
		}
		if !sleepContext(ctx, delay) {
			return
		}
	}
}
