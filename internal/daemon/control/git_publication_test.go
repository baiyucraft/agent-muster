package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

func TestGitPublicationUsesRunLeaseAndExactIntent(t *testing.T) {
	const run = "run-fixture"
	identity := testIdentity()
	request := gitdelivery.PushRequest{
		Generation: 2, Lease: "fixture-lease",
		Verification:           gitdelivery.Verification{CandidateOID: strings.Repeat("a", 40)},
		DestinationFingerprint: strings.Repeat("b", 64),
	}
	intent := gitdelivery.PushIntent{
		Publication: gitdelivery.Publication{
			ID: "push-fixture", Revision: 1, Remote: "origin", TargetRef: "refs/heads/delivery",
			CandidateOID: request.Verification.CandidateOID, Status: "pending",
		}, DestinationFingerprint: request.DestinationFingerprint, PushAllowed: true,
	}
	observation := gitdelivery.Observation{Generation: request.Generation, Lease: request.Lease, Status: "same_commit", ObservedOID: intent.CandidateOID}
	path := "/api/v1/devices/" + identity.DeviceID + "/runs/" + run + "/git-push"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+identity.Token || r.URL.RawQuery != "" {
			t.Error("Git 推送请求未使用设备令牌")
		}
		response := intent
		switch {
		case r.Method == http.MethodPost && r.URL.Path == path:
			var got gitdelivery.PushRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || !reflect.DeepEqual(got, request) {
				t.Errorf("推送意图请求与候选证据不一致：%+v，%v", got, err)
			}
		case r.Method == http.MethodPut && r.URL.Path == path+"/"+intent.ID+"/observation":
			var got gitdelivery.Observation
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != observation {
				t.Errorf("远端回读请求不匹配：%+v，%v", got, err)
			}
			response.Status = observation.Status
			response.ObservedOID = observation.ObservedOID
			response.PushAllowed = false
		default:
			t.Errorf("错误的 Git 推送请求：%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	identity.HubURL = server.URL
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.AuthorizeGitPush(t.Context(), identity, run, request)
	if err != nil || got != intent {
		t.Fatalf("推送意图返回 %+v，错误 %v", got, err)
	}
	got, err = client.ObserveGitPush(t.Context(), identity, run, intent.ID, observation)
	if err != nil || got.ID != intent.ID || got.Status != observation.Status || got.ObservedOID != observation.ObservedOID || got.PushAllowed {
		t.Fatalf("读回返回 %+v，错误 %v", got, err)
	}
}
