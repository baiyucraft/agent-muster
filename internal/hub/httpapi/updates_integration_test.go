package httpapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

func TestDeviceUpdateLifecycle(t *testing.T) {
	ctx, _, _, server, do := openWorkspaceFixture(t)
	client, err := control.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(name string) control.Identity {
		t.Helper()
		var pairing struct{ Code string }
		do(http.MethodPost, "pairing-codes", testAdmin, map[string]string{"name": name}, http.StatusCreated, &pairing)
		identity, err := client.Enroll(ctx, pairing.Code, "windows-test")
		if err != nil {
			t.Fatal(err)
		}
		return identity
	}
	device := enroll("Update target")
	other := enroll("Unaffected device")
	assignURL := "device-updates/" + device.DeviceID
	checkURL := "devices/" + device.DeviceID + "/update/check"
	manifestURL := "http://releases.example.org/muster/1.2.0.json"
	assignment := deviceupdate.Assignment{Version: "1.2.0", ManifestURL: manifestURL}
	readState := func() deviceupdate.DeviceState {
		t.Helper()
		var result struct{ Devices []deviceupdate.DeviceState }
		do(http.MethodGet, "device-updates", testAdmin, nil, http.StatusOK, &result)
		for _, state := range result.Devices {
			if state.DeviceID == device.DeviceID {
				return state
			}
		}
		t.Fatalf("device state missing: %#v", result.Devices)
		return deviceupdate.DeviceState{}
	}
	poll := func(version string, report *deviceupdate.Report) *deviceupdate.Target {
		t.Helper()
		target, err := client.CheckUpdate(ctx, device, deviceupdate.CheckRequest{Version: version, Report: report})
		if err != nil {
			t.Fatal(err)
		}
		return target
	}
	assign := func(input deviceupdate.Assignment, want int) deviceupdate.DeviceState {
		t.Helper()
		var result deviceupdate.DeviceState
		var output any
		if want == http.StatusOK {
			output = &result
		}
		do(http.MethodPut, assignURL, testAdmin, input, want, output)
		return result
	}

	if state := readState(); state.Status != deviceupdate.StatusIdle || state.Target != nil || state.CurrentVersion != "" {
		t.Fatalf("unexpected initial state: %#v", state)
	}
	do(http.MethodGet, "device-updates", device.Token, nil, http.StatusUnauthorized, nil)
	do(http.MethodPut, assignURL, other.Token, assignment, http.StatusUnauthorized, nil)
	do(http.MethodPost, checkURL, testAdmin, deviceupdate.CheckRequest{Version: "1.0.0"}, http.StatusUnauthorized, nil)
	do(http.MethodPost, checkURL, other.Token, deviceupdate.CheckRequest{Version: "1.0.0"}, http.StatusUnauthorized, nil)
	assign(deviceupdate.Assignment{Version: "1.2.0", ManifestURL: "http://user@releases.example.org/manifest.json"}, http.StatusBadRequest)

	first := assign(assignment, http.StatusOK)
	if first.Status != deviceupdate.StatusPending || first.Target == nil {
		t.Fatalf("assignment to offline device: %#v", first)
	}
	if replay := assign(assignment, http.StatusOK); replay.Target == nil || replay.Target.ID != first.Target.ID {
		t.Fatalf("assignment replay changed: %#v", replay)
	}
	if target, err := client.CheckUpdate(ctx, other, deviceupdate.CheckRequest{Version: "1.0.0"}); err != nil || target != nil {
		t.Fatalf("unrelated device received update: %#v, %v", target, err)
	}
	if target := poll("1.0.0", nil); target == nil || target.ID != first.Target.ID || target.ManifestURL != manifestURL {
		t.Fatalf("offline assignment not delivered: %#v", target)
	}
	report := &deviceupdate.Report{AssignmentID: first.Target.ID, Status: deviceupdate.StatusApplying}
	if target := poll("1.0.0", report); target == nil || target.ID != report.AssignmentID {
		t.Fatalf("application confirmation: %#v", target)
	}
	if target := poll("1.0.0", nil); target == nil || target.ID != report.AssignmentID {
		t.Fatalf("active update unable to recover: %#v", target)
	}
	report.Status = deviceupdate.StatusWaiting
	poll("1.0.0", report)
	if state := readState(); state.Status != deviceupdate.StatusApplying {
		t.Fatalf("late progress replaced applying state: %#v", state)
	}
	assign(deviceupdate.Assignment{Version: "1.3.0", ManifestURL: manifestURL}, http.StatusConflict)
	report.Status, report.Error = deviceupdate.StatusRolledBack, "new release failed health check"
	if target := poll("1.0.0", report); target != nil {
		t.Fatalf("rollback returned another update: %#v", target)
	}
	if state := readState(); state.Status != deviceupdate.StatusRolledBack || state.Error != report.Error {
		t.Fatalf("rollback was not recorded: %#v", state)
	}
	assignment.Retry = true
	second := assign(assignment, http.StatusOK)
	if second.Target == nil || second.Target.ID == report.AssignmentID || second.Status != deviceupdate.StatusPending {
		t.Fatalf("failed assignment did not get a new ID: %#v", second)
	}
	if replay := assign(assignment, http.StatusOK); replay.Target == nil || replay.Target.ID != second.Target.ID {
		t.Fatalf("retry replay changed the assignment again: %#v", replay)
	}
	if target := poll("1.0.0", report); target == nil || target.ID != second.Target.ID {
		t.Fatalf("stale rollback interrupted new assignment: %#v", target)
	}
	report = &deviceupdate.Report{AssignmentID: second.Target.ID, Status: deviceupdate.StatusApplying}
	poll("1.0.0", report)
	report.Status = deviceupdate.StatusSucceeded
	if target := poll("1.2.0", report); target != nil {
		t.Fatalf("succeeded update still active: %#v", target)
	}
	report.Status, report.Error = deviceupdate.StatusFailed, "late failure"
	poll("1.2.0", report)
	if state := readState(); state.Status != deviceupdate.StatusSucceeded || state.CurrentVersion != "1.2.0" || state.Error != "" {
		t.Fatalf("invalid final device state: %#v", state)
	}
	assign(deviceupdate.Assignment{Version: "1.1.0", ManifestURL: manifestURL}, http.StatusConflict)

	do(http.MethodDelete, "devices/"+device.DeviceID, testAdmin, nil, http.StatusNoContent, nil)
	if _, err := client.CheckUpdate(ctx, device, deviceupdate.CheckRequest{Version: "1.2.0"}); !errors.Is(err, control.ErrUnauthorized) {
		t.Fatalf("revoked device could poll: %v", err)
	}
}
