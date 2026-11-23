package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/helper"
	"golang.org/x/sys/windows"
)

func startArtifactTestRun(t *testing.T, worker *Worker, name string) control.Run {
	t.Helper()
	run := control.Run{ID: name, TaskID: "task-test", ArtifactsSupported: true}
	if err := worker.saveClaim(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := worker.markLaunching(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := worker.stageEvent(t.Context(), run.ID, control.RunEvent{Kind: "started"}); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestArtifactRegistrationPersistsVersionsAndRecoversReferences(t *testing.T) {
	root := t.TempDir()
	worker, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if worker != nil {
			worker.Close()
		}
	}()
	run := startArtifactTestRun(t, worker, "run-artifact")
	directory := t.TempDir()
	output := filepath.Join(directory, "output")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(output, "result.txt")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := startHelper(t.Context(), &collaboration{worker: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer service.listener.Close()
	env, finishRun, err := service.setRun(t.Context(), run, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer finishRun()
	capability := strings.TrimPrefix(env[1], "MUSTER_RUN_CAPABILITY=")
	request := helper.Request{Command: "artifact", File: `output\result.txt`, Capability: "invalid"}
	write("first")
	if _, err := helper.Call(t.Context(), service.address, request); err == nil {
		t.Fatal("accepted request without the run capability")
	}
	request.Capability = capability
	register := func() artifact.Reference {
		t.Helper()
		body, err := helper.Call(t.Context(), service.address, request)
		if err != nil {
			t.Fatal(err)
		}
		var ref artifact.Reference
		if err := json.Unmarshal(body, &ref); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	first := register()
	request.File = "output/result.txt"
	if retry := register(); retry != first || first.Path != request.File {
		t.Fatalf("retry = %+v, first = %+v", retry, first)
	}
	write("second")
	second := register()
	if first.ID == second.ID {
		t.Fatal("file update reused the prior version")
	}
	finishRun()
	if _, err := helper.Call(t.Context(), service.address, request); err == nil {
		t.Fatal("accepted expired run capability")
	}
	write("third")
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	worker, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.recoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.registerArtifact(t.Context(), run.ID, directory, request.File); err == nil {
		t.Fatal("accepted registration after the terminal event")
	}
	var body string
	if err := worker.db.QueryRowContext(t.Context(), "SELECT payload FROM outbox WHERE run_id=? ORDER BY sequence DESC LIMIT 1", run.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var event control.RunEvent
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatal(err)
	}
	if event.Kind != "interrupted" || len(event.Artifacts) != 2 || event.Artifacts[0] != first || event.Artifacts[1] != second {
		t.Fatalf("recovered event = %+v", event)
	}
	for _, tc := range []struct {
		ref  artifact.Reference
		want string
	}{{first, "first"}, {second, "second"}} {
		got, err := worker.ReadArtifact(t.Context(), run.ID, tc.ref.ID)
		if err != nil || string(got) != tc.want {
			t.Fatalf("read version %s = %q, %v", tc.ref.ID, got, err)
		}
	}
	if _, err := worker.ReadArtifact(t.Context(), "different-run", first.ID); !errors.Is(err, artifact.ErrMissing) {
		t.Fatalf("wrong run returned %v", err)
	}
	if _, err := worker.db.ExecContext(t.Context(), "UPDATE run_artifacts SET body=? WHERE id=?", []byte("wrong"), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ReadArtifact(t.Context(), run.ID, first.ID); err == nil || errors.Is(err, artifact.ErrMissing) {
		t.Fatalf("corrupt snapshot returned %v", err)
	}
}

func TestArtifactsRejectUnsafePathsAndEnforceLimits(t *testing.T) {
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	run := startArtifactTestRun(t, worker, "run-artifact-limits")
	directory := t.TempDir()
	register := func(name string) (artifact.Reference, error) {
		return worker.registerArtifact(t.Context(), run.ID, directory, name)
	}
	for _, path := range []string{"", ".", "../outside", "child/../file", "NUL", "file:stream", "invalid.", filepath.Join(directory, "x")} {
		if _, err := register(path); err == nil {
			t.Errorf("accepted invalid path %q", path)
		}
	}
	file := filepath.Join(directory, "file")
	if err := os.WriteFile(file, []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(directory, "hardlink")
	if err := os.Link(file, hardlink); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hardlink", "file"} {
		if _, err := register(name); err == nil {
			t.Errorf("accepted multiply linked file %q", name)
		}
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	t.Run("symbolic-link", func(t *testing.T) {
		if err := os.Symlink(file, filepath.Join(directory, "symlink")); err != nil {
			t.Skipf("Windows account cannot create symbolic links: %v", err)
		}
		if _, err := register("symlink"); err == nil {
			t.Fatal("accepted symbolic link")
		}
	})
	if err := os.Mkdir(filepath.Join(directory, "target"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "target", "file"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	// 目录联接可由普通 Windows 用户创建，覆盖没有符号链接权限的环境。
	cmd := exec.CommandContext(t.Context(), "cmd.exe", "/d", "/c", "mklink", "/J", "linked-directory", "target")
	cmd.Dir = directory
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating junction: %v\n%s", err, output)
	}
	if _, err := register("linked-directory/file"); err == nil {
		t.Fatal("accepted file under a reparse point")
	}
	if err := os.Truncate(file, artifact.MaxSize+1); err != nil {
		t.Fatal(err)
	}
	if _, err := register("file"); err == nil {
		t.Fatal("accepted oversized file")
	}
	var last artifact.Reference
	for index := range artifact.MaxCount {
		content := []byte{byte(index)}
		if index == 0 {
			content = nil
		}
		if err := os.WriteFile(file, content, 0600); err != nil {
			t.Fatal(err)
		}
		last, err = register("file")
		if err != nil {
			t.Fatal(err)
		}
	}
	if again, err := register("file"); err != nil || again != last {
		t.Fatalf("retry at the quota = %+v, %v", again, err)
	}
	if err := os.WriteFile(file, []byte("over quota"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := register("file"); err == nil {
		t.Fatal("accepted new version at the quota")
	}
}

func TestArtifactRegistrationPermitWaitCanBeCancelled(t *testing.T) {
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	worker.artifactRegistration <- struct{}{}
	defer func() { <-worker.artifactRegistration }()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := worker.registerArtifact(ctx, "run-test", t.TempDir(), "missing.txt"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for the registration permit returned %v", err)
	}
}

func TestArtifactSnapshotOpenExcludesWriters(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "artifact.txt")
	writer, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.WriteString("result"); err != nil {
		t.Fatal(err)
	}
	if file, err := openArtifactFile(directory, "artifact.txt"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		if file != nil {
			file.Close()
		}
		t.Fatalf("snapshot open with a live writer returned %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := openArtifactFile(directory, "artifact.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.WriteFile(path, []byte("changed"), 0600); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("write during a snapshot read returned %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatalf("write after closing snapshot: %v", err)
	}
}

type artifactPollingHub struct {
	*fakeHub
	active   <-chan struct{}
	requests []artifact.ReadRequest
	reports  chan artifactPollReport
	attempts int
}

type artifactPollReport struct {
	id     string
	status string
	body   string
}

func (hub *artifactPollingHub) ArtifactRead(ctx context.Context, _ control.Identity) (artifact.ReadRequest, error) {
	select {
	case <-hub.active:
	case <-ctx.Done():
		return artifact.ReadRequest{}, ctx.Err()
	}
	if len(hub.requests) == 0 {
		return artifact.ReadRequest{}, nil
	}
	return hub.requests[0], nil
}

func (hub *artifactPollingHub) ReportArtifact(ctx context.Context, _ control.Identity, id, status string, body []byte) error {
	select {
	case hub.reports <- artifactPollReport{id, status, string(body)}:
	case <-ctx.Done():
		return ctx.Err()
	}
	hub.attempts++
	if hub.attempts == 1 {
		return errors.New("simulated lost upload")
	}
	hub.requests = hub.requests[1:]
	return nil
}

func TestArtifactPollingRetriesPreviousSnapshotDuringAnotherRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	directory, err := worker.RegisterDirectory(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory.Path, "out.txt"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	run := startArtifactTestRun(t, worker, "run-artifact")
	ref, err := worker.registerArtifact(ctx, run.ID, directory.Path, "out.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory.Path, "out.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt, err := worker.registerArtifact(ctx, run.ID, directory.Path, "out.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.db.ExecContext(ctx, "UPDATE run_artifacts SET body=? WHERE id=?", []byte("corrupt"), corrupt.ID); err != nil {
		t.Fatal(err)
	}
	if err := worker.stageEvent(ctx, run.ID, control.RunEvent{Kind: "completed"}); err != nil {
		t.Fatal(err)
	}
	active := make(chan struct{})
	hub := &artifactPollingHub{
		fakeHub: &fakeHub{run: control.Run{ID: "run-next", TaskID: "task-next", DirectoryID: directory.ID}},
		active:  active,
		requests: []artifact.ReadRequest{
			{ID: "available", RunID: run.ID, ArtifactID: ref.ID},
			{ID: "missing", RunID: "missing-run", ArtifactID: ref.ID},
			{ID: "failed", RunID: run.ID, ArtifactID: corrupt.ID},
		},
		reports: make(chan artifactPollReport),
	}
	done := make(chan error, 1)
	go func() {
		done <- worker.RunWithExecutor(ctx, hub, control.Identity{}, nil,
			func(ctx context.Context, _, _, _ string, markStarted func() error, _ func(string) error) (string, error) {
				if err := markStarted(); err != nil {
					return "", err
				}
				close(active)
				<-ctx.Done()
				return "", ctx.Err()
			})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Error(err)
		}
	}()
	for _, want := range []artifactPollReport{
		{"available", "available", "snapshot"},
		{"available", "available", "snapshot"},
		{"missing", "missing", ""},
		{"failed", "failed", ""},
	} {
		select {
		case got := <-hub.reports:
			if got != want {
				t.Fatalf("artifact poll report = %+v, want %+v", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("waiting for artifact poll report %+v: %v", want, ctx.Err())
		}
	}
}
