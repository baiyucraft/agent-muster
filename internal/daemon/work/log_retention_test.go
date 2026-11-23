package work

import (
	"testing"
	"time"
)

func TestParseLogRetention(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"", 30 * 24 * time.Hour, true},
		{"0", 0, true},
		{" 7 ", 7 * 24 * time.Hour, true},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"999999999", 0, false},
	} {
		t.Run(test.value, func(t *testing.T) {
			retention, err := parseLogRetention(test.value)
			if retention != test.want || (err == nil) != test.valid {
				t.Fatalf("留存天数 %q 的解析结果为 %s，错误 %v", test.value, retention, err)
			}
		})
	}
}
