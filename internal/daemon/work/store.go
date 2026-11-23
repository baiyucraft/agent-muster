// Package work 持久化设备接收的任务运行，并管理本机执行和协作交接。
package work

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS directories (
	id TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	path TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	directory_id TEXT NOT NULL,
	prompt TEXT NOT NULL,
	session_id TEXT NOT NULL,
	requested_session_id TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('claimed','starting','started','completed','failed','interrupted')),
	text TEXT NOT NULL DEFAULT '',
	sequence INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS outbox (
	run_id TEXT NOT NULL REFERENCES runs(id),
	sequence INTEGER NOT NULL,
	payload TEXT NOT NULL,
	PRIMARY KEY (run_id, sequence)
);
CREATE TABLE IF NOT EXISTS run_claims (
	id TEXT PRIMARY KEY REFERENCES runs(id),
	data BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS peer_credentials (
	id INTEGER PRIMARY KEY CHECK(id=1),
	cert BLOB NOT NULL,
	key BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS peer_inbox (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	size INTEGER NOT NULL,
	body BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS peer_outbox (
	id TEXT PRIMARY KEY,
	sha256 TEXT NOT NULL,
	body BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS peer_manifests (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES runs(id),
	data BLOB NOT NULL,
	published INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS helper_waits (
	run_id TEXT PRIMARY KEY REFERENCES runs(id),
	payload BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS delegations (
	run_id TEXT NOT NULL REFERENCES runs(id),
	request_key TEXT NOT NULL,
	payload BLOB NOT NULL,
	reply BLOB,
	rejected_status INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, request_key)
);`

// Directory 保存工作目录的设备内标识、显示名和本机路径。
type Directory struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Path  string `json:"path"`
}

// RunStatus 表示本机运行状态、原生会话及是否有未确认上报。
type RunStatus struct {
	ID        string `json:"id"`
	TaskID    string `json:"taskId"`
	State     string `json:"state"`
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	Pending   bool   `json:"pending"`
}

// Open 获取工作区独占锁并初始化本机 SQLite 存储。
func Open(root string) (*Worker, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	lock, err := lockWorkspace(filepath.Join(root, "worker.lock"))
	if err != nil {
		return nil, err
	}
	// Forward slashes keep SQLite URI handling consistent on Windows. The URI
	// also keeps spaces and '#' in a user profile directory out of the DSN.
	databasePath, err := filepath.Abs(filepath.Join(root, "work.db"))
	if err != nil {
		lock.Close()
		return nil, err
	}
	databasePath = filepath.ToSlash(databasePath)
	if !strings.HasPrefix(databasePath, "/") {
		databasePath = "/" + databasePath
	}
	address := url.URL{Scheme: "file", Path: databasePath}
	query := address.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		query.Add("_pragma", pragma)
	}
	address.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", address.String())
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		lock.Close()
		return nil, err
	}
	return &Worker{db: db, lock: lock, wake: make(chan struct{}, 1)}, nil
}

// Close 关闭本机数据库并释放工作区独占锁。
func (worker *Worker) Close() error {
	return errors.Join(worker.db.Close(), worker.lock.Close())
}

// RegisterDirectory 登记规范化后的目录，已登记路径复用原目录 ID。
func (worker *Worker) RegisterDirectory(ctx context.Context, selected string) (Directory, error) {
	directory, err := canonicalDirectory(selected)
	if err != nil {
		return Directory{}, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Directory{}, err
	}
	id := "dir-" + hex.EncodeToString(random[:])
	label := strings.TrimSpace(filepath.Base(directory))
	if label == "" || label == "." || label == string(filepath.Separator) {
		label = "工作目录"
	}
	record := Directory{}
	err = worker.db.QueryRowContext(ctx, `
		INSERT INTO directories (id, label, path) VALUES (?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET path=excluded.path
		RETURNING id, label, path
	`, id, trimUTF8(label, 120), directory).Scan(&record.ID, &record.Label, &record.Path)
	if err == nil {
		worker.notify()
	}
	return record, err
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("请选择本机的绝对目录")
	}
	path, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("所选位置不是目录")
	}
	return path, nil
}

// Directories 返回已登记的工作目录，按显示名和 ID 排序。
func (worker *Worker) Directories(ctx context.Context) ([]Directory, error) {
	rows, err := worker.db.QueryContext(ctx, `SELECT id,label,path FROM directories ORDER BY label,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	directories := make([]Directory, 0)
	for rows.Next() {
		var directory Directory
		if err := rows.Scan(&directory.ID, &directory.Label, &directory.Path); err != nil {
			return nil, err
		}
		directories = append(directories, directory)
	}
	return directories, rows.Err()
}

// LatestRun 优先返回活动运行，否则返回最新的本机运行。没有运行时返回 nil。
func (worker *Worker) LatestRun(ctx context.Context) (*RunStatus, error) {
	var run RunStatus
	err := worker.db.QueryRowContext(ctx, `
		SELECT id,task_id,
			CASE WHEN state='completed' AND EXISTS(SELECT 1 FROM helper_waits WHERE run_id=runs.id) THEN 'waiting' ELSE state END,
			session_id,text, EXISTS (SELECT 1 FROM outbox WHERE run_id=runs.id)
		FROM runs
		ORDER BY CASE WHEN state IN ('claimed','starting','started') THEN 0 ELSE 1 END, rowid DESC LIMIT 1
	`).Scan(&run.ID, &run.TaskID, &run.State, &run.SessionID, &run.Text, &run.Pending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &run, err
}

func (worker *Worker) saveClaim(ctx context.Context, run control.Run) error {
	encoded, err := json.Marshal(run)
	if err != nil {
		return err
	}
	tx, err := worker.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO runs (id,task_id,directory_id,prompt,session_id,requested_session_id,state)
		VALUES (?,?,?,?,?,?,'claimed')
		ON CONFLICT (id) DO UPDATE SET id=excluded.id
		WHERE (runs.task_id,runs.directory_id,runs.prompt,runs.requested_session_id)
		    = (excluded.task_id,excluded.directory_id,excluded.prompt,excluded.requested_session_id)
		RETURNING state
	`, run.ID, run.TaskID, run.DirectoryID, run.Prompt, run.SessionID, run.SessionID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("Hub 重发了内容不同的运行记录")
	}
	if err == nil && state != "claimed" {
		return errors.New("Hub 重发了已在本机处理的运行记录")
	}
	if err != nil {
		return err
	}
	// Claim retries keep an immutable task and initial input. The lease may be
	// reissued, so persist the current generation and proof with the claim.
	if _, err = tx.ExecContext(ctx, "INSERT INTO run_claims(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", run.ID, encoded); err != nil {
		return err
	}
	return tx.Commit()
}

func (worker *Worker) nextClaim(ctx context.Context) (*control.Run, error) {
	var run control.Run
	var full []byte
	err := worker.db.QueryRowContext(ctx, `
		SELECT r.id,r.task_id,r.directory_id,r.prompt,r.requested_session_id,c.data
		FROM runs r LEFT JOIN run_claims c ON c.id=r.id WHERE r.state='claimed' ORDER BY r.rowid LIMIT 1
	`).Scan(&run.ID, &run.TaskID, &run.DirectoryID, &run.Prompt, &run.SessionID, &full)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(full) != 0 {
		if err := json.Unmarshal(full, &run); err != nil {
			return nil, err
		}
	}
	return &run, nil
}

func (worker *Worker) markLaunching(ctx context.Context, runID string) error {
	result, err := worker.db.ExecContext(ctx, `UPDATE runs SET state='starting' WHERE id=? AND state='claimed'`, runID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return errors.New("本机运行记录无法开始")
	}
	return err
}

func (worker *Worker) stageEvent(ctx context.Context, runID string, event control.RunEvent) error {
	return worker.stageEventWithResult(ctx, runID, event, control.Manifest{}, nil)
}

func (worker *Worker) stageEventWithResult(ctx context.Context, runID string, event control.RunEvent, manifest control.Manifest, body []byte) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	tx, err := worker.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if manifest.ID != "" {
		if err := saveTransfer(ctx, tx, runID, manifest, body); err != nil {
			return err
		}
	}
	var sequence int64
	// 请示补写之前保存的原始回执只留在本机，终态事件仍通过同一个 outbox 提交。
	err = tx.QueryRowContext(ctx, `
		UPDATE runs SET
			sequence=sequence+1,
			session_id=COALESCE(NULLIF(?,''),session_id),
			state=CASE WHEN ?='session' THEN state WHEN ? IN ('waiting','approval_requested') THEN 'completed' ELSE ? END,
			text=CASE WHEN ? IN ('approval_requested','interrupted') AND text<>'' THEN text ELSE ? END
		WHERE id=? AND state IN ('starting','started')
		RETURNING sequence
	`, event.SessionID, event.Kind, event.Kind, event.Kind, event.Kind, event.Text, runID).Scan(&sequence)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox (run_id,sequence,payload) VALUES (?,?,?)`, runID, sequence, string(data)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	worker.notify()
	return nil
}

func (worker *Worker) recoverInterrupted(ctx context.Context) error {
	// The Windows Job Object terminates the previous worker's process tree.
	// Preserve the announced session and record an interrupted result once.
	rows, err := worker.db.QueryContext(ctx, `SELECT r.id,r.session_id,c.data FROM runs r LEFT JOIN run_claims c ON c.id=r.id WHERE r.state IN ('starting','started')`)
	if err != nil {
		return err
	}
	var runs []control.Run
	for rows.Next() {
		var run control.Run
		var saved []byte
		if err := rows.Scan(&run.ID, &run.SessionID, &saved); err != nil {
			rows.Close()
			return err
		}
		if len(saved) != 0 {
			var claim control.Run
			if err := json.Unmarshal(saved, &claim); err != nil {
				rows.Close()
				return err
			}
			run.Generation, run.Lease = claim.Generation, claim.Lease
		}
		runs = append(runs, run)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, run := range runs {
		event := control.RunEvent{Kind: "interrupted", SessionID: run.SessionID, Generation: run.Generation, Lease: run.Lease, Text: "设备在本轮执行期间退出，结果需核对。"}
		if err := worker.stageEvent(ctx, run.ID, event); err != nil {
			return err
		}
	}
	return nil
}

func trimUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func lockedError(err error) error {
	return fmt.Errorf("无法打开本机执行记录，可能已有设备端在运行: %w", err)
}
