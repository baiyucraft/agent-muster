package deviceupdate

import "testing"

func TestValidVersionAndCompare(t *testing.T) {
	valid := []string{"0.0.0", "1.0.0", "0.12.3", "100000000.999999999.9"}
	for _, version := range valid {
		if !ValidVersion(version) {
			t.Errorf("ValidVersion(%q) = false", version)
		}
	}
	invalid := []string{"", "v1.2.3", "01.2.3", "1.02.3", "1.2", "1.2.3-rc1", "1.2.3+build", "1000000000.1.1"}
	for _, version := range invalid {
		if ValidVersion(version) {
			t.Errorf("ValidVersion(%q) = true", version)
		}
	}
	if CompareVersion("1.9.0", "1.10.0") >= 0 || CompareVersion("2.0.0", "1.99.99") <= 0 ||
		CompareVersion("1.0.0", "1.0.0") != 0 {
		t.Fatal("version comparison did not compare numeric components")
	}
}

func TestManifestURLAndProgressValidation(t *testing.T) {
	for _, test := range []struct {
		url   string
		valid bool
	}{
		{"https://releases.example.org/1.2.3.json", true},
		{"http://127.0.0.1:8080/manifest", true},
		{"http://[::1]:8080/manifest", true},
		{"http://releases.example.org/manifest", true},
		{"http://203.0.113.10:13011/manifest", true},
		{"http:///manifest", false},
		{"ftp://releases.example.org/manifest", false},
		{"file:///manifest", false},
		{"https://user@releases.example.org/manifest", false},
		{"http://releases.example.org/manifest#section", false},
		{"https://releases.example.org/a b", false},
	} {
		if got := ValidURL(test.url); got != test.valid {
			t.Errorf("ValidURL(%q) = %v, want %v", test.url, got, test.valid)
		}
	}
	check := CheckRequest{Version: "1.0.0", Report: &Report{
		AssignmentID: "upd-0123456789abcdef0123456789abcdef",
		Status:       StatusFailed,
		Error:        "health check failed",
	}}
	if err := check.Validate(); err != nil {
		t.Fatal(err)
	}
	check.Report.Error = "control\ncharacter"
	if err := check.Validate(); err == nil {
		t.Fatal("control characters in error should be rejected")
	}
}
