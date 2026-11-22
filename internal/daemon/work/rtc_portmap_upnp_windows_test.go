package work

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestRTCPortMappingUPnP(t *testing.T) {
	for _, test := range []struct {
		name      string
		want      string
		address   string
		lease     int
		occupied  bool
		replace   bool
		add       bool
		wantPurge bool
	}{
		{name: "global", want: "global", address: "1.2.3.4", lease: 180, add: true, wantPurge: true},
		{name: "private", want: "private", address: "192.168.1.2"},
		{name: "occupied", want: "unsupported", address: "1.2.3.4", occupied: true},
		{name: "ignored_lease", want: "unsupported", address: "1.2.3.4", add: true, wantPurge: true},
		{name: "another_owner", want: "unsupported", address: "1.2.3.4", add: true, replace: true, lease: 180},
	} {
		t.Run(test.name, func(t *testing.T) {
			const serviceType = "urn:schemas-upnp-org:service:WANIPConnection:1"
			local := netip.MustParseAddrPort("127.0.0.1:43210")
			var label string
			var added, deleted int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					fmt.Fprint(w, `<root><device><deviceList><device><serviceList><service><serviceType>`+serviceType+`</serviceType><controlURL>/control</controlURL></service></serviceList></device></deviceList></device></root>`)
					return
				}
				_, action, _ := strings.Cut(strings.Trim(request.Header.Get("SOAPAction"), `"`), "#")
				content := ""
				switch action {
				case "GetExternalIPAddress":
					content = "<NewExternalIPAddress>" + test.address + "</NewExternalIPAddress>"
				case "GetSpecificPortMappingEntry":
					if label == "" && !test.occupied {
						w.WriteHeader(http.StatusInternalServerError)
						fmt.Fprint(w, `<Envelope><Body><Fault><detail><UPnPError><errorCode>714</errorCode></UPnPError></detail></Fault></Body></Envelope>`)
						return
					}
					owner := label
					if test.replace || test.occupied {
						owner = "other client"
					}
					content = fmt.Sprintf("<NewInternalPort>43210</NewInternalPort><NewInternalClient>127.0.0.1</NewInternalClient><NewEnabled>1</NewEnabled><NewPortMappingDescription>%s</NewPortMappingDescription><NewLeaseDuration>%d</NewLeaseDuration>", owner, test.lease)
				case "AddPortMapping":
					added++
					var envelope struct {
						Address string `xml:"Body>AddPortMapping>NewInternalClient"`
						Source  int    `xml:"Body>AddPortMapping>NewInternalPort"`
						Port    int    `xml:"Body>AddPortMapping>NewExternalPort"`
						Proto   string `xml:"Body>AddPortMapping>NewProtocol"`
						Lease   int    `xml:"Body>AddPortMapping>NewLeaseDuration"`
						Label   string `xml:"Body>AddPortMapping>NewPortMappingDescription"`
					}
					if err := xml.NewDecoder(request.Body).Decode(&envelope); err != nil || envelope.Address != "127.0.0.1" ||
						envelope.Source != 43210 || envelope.Port != 43210 || envelope.Proto != "UDP" || envelope.Lease != 180 ||
						!strings.HasPrefix(envelope.Label, "Muster-") {
						t.Errorf("invalid temporary mapping request: %v", err)
					}
					label = envelope.Label
				case "DeletePortMapping":
					deleted++
				default:
					t.Errorf("unexpected request %q", action)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				fmt.Fprintf(w, `<Envelope><Body><u:%[1]sResponse xmlns:u="%[2]s">%[3]s</u:%[1]sResponse></Body></Envelope>`, action, serviceType, content)
			}))
			defer server.Close()
			location, err := url.Parse(server.URL + "/device.xml")
			if err != nil {
				t.Fatal(err)
			}
			service, ok := rtcReadUPnPDevice(context.Background(), local.Addr(), location, server.Client())
			if !ok {
				t.Fatal("failed to parse valid gateway description")
			}
			mapped, status, release := service.mapPort(context.Background(), local)
			release()
			release()
			if status != test.want || mapped.IsValid() != (test.want == "global") || (mapped.IsValid() && mapped.Port() != local.Port()) {
				t.Fatalf("mapping = %v, status = %q", mapped, status)
			}
			if (added == 1) != test.add || (deleted == 1) != test.wantPurge || added > 1 || deleted > 1 {
				t.Fatalf("add count = %d; delete count = %d", added, deleted)
			}
		})
	}
}

func TestRTCPortMappingUPnPLocationIsLocal(t *testing.T) {
	gateway := netip.MustParseAddr("192.168.1.1")
	for _, location := range []string{"http://example.com/a", "https://192.168.1.1/a", "http://192.168.1.2/a", "http://192.168.1.1@example.com/a", "http://guest@192.168.1.1/a"} {
		if rtcUPnPLocation([]byte("HTTP/1.1 200 OK\r\nLOCATION: "+location+"\r\n"), gateway) != nil {
			t.Errorf("discovery incorrectly accepted %s", location)
		}
	}
	if rtcUPnPLocation([]byte("HTTP/1.1 200 OK\r\nLOCATION: http://192.168.1.1:12345/igd.xml\r\n"), gateway) == nil {
		t.Fatal("discovery rejected an HTTP location on the gateway")
	}
}
