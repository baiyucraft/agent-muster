package gitwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	commitRecordName   = "pending-commit.json"
	verifiedCommitName = "candidate.oid"
)

type commitRecord struct {
	Parent string   `json:"parent"`
	Paths  []string `json:"paths"`
}

// recordCommit 将提交范围写入修复工作树之外的任务目录。写入完成前不改动 Git 索引或 HEAD。
func (work Workspace) recordCommit(record commitRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeCommitRecord(work.taskDir, commitRecordName, data)
}

// writeCommitRecord 在内容落盘后替换同一目录中的记录，进程中断时只读旧版或完整新版。
func writeCommitRecord(dir, name string, data []byte) error {
	file, err := os.CreateTemp(dir, "commit-state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, name))
}

// reconcileCommit 先核对上次已接受的提交，再恢复尚未记账的写入。未获接受的 HEAD 会阻止后续交付。
func (work Workspace) reconcileCommit(ctx context.Context, head string) error {
	verified, err := work.verifiedCommit()
	if err != nil {
		return err
	}
	data, err := readCommitRecord(filepath.Join(work.taskDir, commitRecordName), maxCommitOutput)
	if errors.Is(err, os.ErrNotExist) {
		if head == verified {
			return nil
		}
		return errors.New("Git 修复工作区 HEAD 与最近一次受控提交不符，已保留修复工作树")
	}
	if err != nil {
		return err
	}
	var pending commitRecord
	if err := json.Unmarshal(data, &pending); err != nil || !validOID(pending.Parent) {
		return errors.New("待核对的 Git 候选记录无效")
	}
	selected, err := commitSelection(pending.Paths)
	if err != nil {
		return errors.New("待核对的 Git 候选记录文件清单无效")
	}
	if head == verified && pending.Parent == verified {
		return work.clearCommitRecord()
	}
	if head != verified && pending.Parent != verified {
		return errors.New("待核对的 Git 提交与最近一次受控提交不符，已保留修复工作树")
	}
	if err := work.verifySelectedCommit(ctx, pending.Parent, head, selected); err != nil {
		return fmt.Errorf("上次 Git 候选提交需要本机核对，已保留修复工作树: %w", err)
	}
	return work.acceptCommit(head)
}

// verifiedCommit 缺少已确认记录时返回任务冻结基线，存在记录时只接受完整提交 ID。
func (work Workspace) verifiedCommit() (string, error) {
	data, err := readCommitRecord(filepath.Join(work.taskDir, verifiedCommitName), 65)
	if errors.Is(err, os.ErrNotExist) {
		return work.BaseOID, nil
	}
	if err != nil {
		return "", err
	}
	oid := strings.TrimSuffix(string(data), "\n")
	if !validOID(oid) {
		return "", errors.New("Git 最近一次受控提交记录无效")
	}
	return oid, nil
}

// acceptCommit 先记录受控 HEAD 再移除待核对记录，允许在两步之间退出后重放核对。
func (work Workspace) acceptCommit(head string) error {
	if err := writeCommitRecord(work.taskDir, verifiedCommitName, []byte(head+"\n")); err != nil {
		return err
	}
	return work.clearCommitRecord()
}

func readCommitRecord(name string, limit int) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || hasCommitReparsePoint(info) || info.Size() > int64(limit) {
		return nil, errors.New("Git 候选记录须为规定范围内的普通文件")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if len(data) > limit {
		return nil, errors.New("Git 候选记录超出允许大小")
	}
	return data, err
}

func (work Workspace) clearCommitRecord() error {
	err := os.Remove(filepath.Join(work.taskDir, commitRecordName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
