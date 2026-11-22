package work

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

func (peer *collaboration) stageDelegation(ctx context.Context, run control.Run, target string, request control.Delegation, prompt []byte) (json.RawMessage, error) {
	if !peer.enabled() {
		return nil, errors.New("设备互联尚未启动")
	}
	slices.Sort(request.TransferIDs)
	request.TransferIDs = append([]string(nil), request.TransferIDs...)
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	tx, err := peer.worker.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO delegations(run_id,request_key,payload) VALUES (?,?,?)
		ON CONFLICT(run_id,request_key) DO UPDATE SET request_key=excluded.request_key WHERE delegations.payload=excluded.payload`, run.ID, request.RequestKey, payload)
	if err != nil {
		return nil, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return nil, errors.Join(err, errors.New("同一子任务 key 已保存不同请求"))
	}
	manifest := newManifest(run, request.PromptTransferID, target, "prompt.txt", "prompt", prompt)
	if err := saveTransfer(ctx, tx, run.ID, manifest, prompt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return peer.dispatchDelegation(ctx, run.ID, request.RequestKey)
}

func (peer *collaboration) dispatchDelegation(ctx context.Context, runID, key string) (json.RawMessage, error) {
	peer.delegateMu.Lock()
	defer peer.delegateMu.Unlock()
	var payload, reply []byte
	var rejected int
	err := peer.worker.db.QueryRowContext(ctx, `SELECT payload,reply,rejected_status FROM delegations WHERE run_id=? AND request_key=?`, runID, key).Scan(&payload, &reply, &rejected)
	if err != nil {
		return nil, err
	}
	if len(reply) != 0 {
		return json.RawMessage(reply), nil
	}
	if rejected != 0 {
		return nil, &control.HTTPError{Status: rejected}
	}
	var request control.Delegation
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, err
	}
	_, err = peer.publishTransfer(ctx, request.PromptTransferID)
	if err == nil {
		reply, err = peer.client.Delegate(ctx, peer.identity, runID, request)
	}
	if err != nil {
		if status := rejectedDelegation(err); status != 0 {
			if _, persistErr := peer.worker.db.ExecContext(ctx, `UPDATE delegations SET rejected_status=? WHERE run_id=? AND request_key=?`, status, runID, key); persistErr != nil {
				return nil, persistErr
			}
		}
		return nil, err
	}
	_, err = peer.worker.db.ExecContext(ctx, `UPDATE delegations SET reply=? WHERE run_id=? AND request_key=?`, []byte(reply), runID, key)
	return reply, err
}

func (peer *collaboration) flushDelegations(ctx context.Context) error {
	rows, err := peer.worker.db.QueryContext(ctx, `SELECT run_id,request_key FROM delegations WHERE reply IS NULL AND rejected_status=0 ORDER BY rowid`)
	if err != nil {
		return err
	}
	type pendingDelegation struct{ run, key string }
	var pending []pendingDelegation
	for rows.Next() {
		var item pendingDelegation
		if err := rows.Scan(&item.run, &item.key); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range pending {
		if _, err := peer.dispatchDelegation(ctx, item.run, item.key); err != nil && rejectedDelegation(err) == 0 {
			return err
		}
	}
	return nil
}

func rejectedDelegation(err error) int {
	var status *control.HTTPError
	if errors.As(err, &status) {
		switch status.Status {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
			return status.Status
		}
	}
	return 0
}
