package httpapi

import (
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestBindingProposalHTTP(t *testing.T) {
	_, _, _, _, do := openWorkspaceFixture(t)
	var pairing struct{ Code string }
	do("POST", "pairing-codes", testAdmin, map[string]string{"name": "家里电脑"}, 201, &pairing)
	var device struct {
		ID    string `json:"device_id"`
		Token string
	}
	do("POST", "devices/enroll", "", map[string]string{"code": pairing.Code, "hostname": "workstation", "os": "windows", "arch": "amd64"}, 201, &device)
	directory := "dir-" + strings.Repeat("a", 32)
	do("PUT", "devices/"+device.ID+"/directories", device.Token,
		map[string]any{"directories": []workspace.Directory{{ID: directory, Label: "素材目录"}}}, 204, nil)
	do("PUT", "devices/"+device.ID+"/inventory", device.Token, registry.Inventory{Agents: []registry.Agent{{
		ID: "dsh", Version: "0.2.1-rc.5", Status: "supported",
		Capabilities: &registry.Capabilities{Prompt: true, FinalReply: true, Resume: true},
	}}}, 204, nil)

	const discovery = "plugin/binding-candidates"
	for _, token := range []string{testAdmin, device.Token} {
		do("GET", discovery, token, nil, 401, nil)
	}
	var inventory struct{ Candidates []workspace.BindingCandidate }
	do("GET", discovery, testPlugin, nil, 200, &inventory)
	if len(inventory.Candidates) != 1 || inventory.Candidates[0].DeviceID != device.ID || len(inventory.Candidates[0].Agents) != 1 ||
		len(inventory.Candidates[0].Directories) != 1 || inventory.Candidates[0].Directories[0].ID != directory {
		t.Fatalf("unexpected candidate inventory: %#v", inventory)
	}

	const source = "00000000-0000-4000-8000-000000000042"
	do("POST", "conversation/messages", testAdmin, map[string]string{"id": source, "text": "接入素材项目"}, 202, nil)
	var claim workspace.Delivery
	do("POST", "plugin/deliveries/claim", testPlugin, nil, 200, &claim)
	if !claim.BindingProposalsSupported || claim.MessageID != source {
		t.Fatalf("binding proposal capability not advertised: %#v", claim)
	}
	path := "plugin/deliveries/" + claim.ID + "/binding-proposal"
	payload := map[string]any{"lease": claim.Lease, "name": "素材", "bindings": []map[string]any{{
		"device_id": device.ID, "directory_id": directory, "agent_kind": "dsh", "is_default": true,
	}}}
	for _, token := range []string{testAdmin, device.Token} {
		do("POST", path, token, payload, 401, nil)
	}
	var proposal workspace.BindingProposal
	do("POST", path, testPlugin, payload, 200, &proposal)
	if proposal.ID != "proposal-"+source || proposal.Status != "pending" || proposal.Bindings[0].DirectoryLabel != "素材目录" {
		t.Fatalf("unexpected proposal: %#v", proposal)
	}
	var snapshot workspace.Snapshot
	do("GET", "workspace", testAdmin, nil, 200, &snapshot)
	if len(snapshot.Projects) != 0 || len(snapshot.Tasks) != 0 || len(snapshot.BindingProposals) != 1 ||
		len(snapshot.Messages) != 2 || snapshot.Messages[1].ID != proposal.ID || snapshot.Messages[1].Kind != "binding_proposal" {
		t.Fatalf("expected a pending proposal before any project or task: %#v", snapshot)
	}

	decisionPath := "binding-proposals/" + proposal.ID + "/decision"
	decision := map[string]string{"decision": "accepted"}
	for _, token := range []string{testPlugin, device.Token} {
		do("PUT", decisionPath, token, decision, 401, nil)
	}
	do("PUT", decisionPath, testAdmin, decision, 200, &proposal)
	if proposal.Status != "accepted" || proposal.ProjectID == "" || proposal.DecidedAt == nil {
		t.Fatalf("decision did not save the project: %#v", proposal)
	}
	do("PUT", decisionPath, testAdmin, decision, 200, nil)
	do("PUT", decisionPath, testAdmin, map[string]string{"decision": "rejected"}, 409, nil)
	do("POST", path, testPlugin, payload, 200, &proposal)
	if proposal.Status != "accepted" {
		t.Fatalf("replayed proposal lost its decision: %#v", proposal)
	}
	do("GET", "workspace", testAdmin, nil, 200, &snapshot)
	if len(snapshot.Projects) != 1 || snapshot.Projects[0].ID != proposal.ProjectID || len(snapshot.Tasks) != 0 || snapshot.BindingProposals[0].Status != "accepted" {
		t.Fatalf("decision did not atomically create only the project: %#v", snapshot)
	}
}
