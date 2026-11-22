package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestFitWindowBounds(t *testing.T) {
	primary := &application.Screen{IsPrimary: true, WorkArea: application.Rect{Width: 1920, Height: 1040}}
	secondary := &application.Screen{
		ScaleFactor: 1.5,
		WorkArea:    application.Rect{X: -1050, Y: -250, Width: 1050, Height: 900},
	}
	secondaryBounds := application.Rect{X: -950, Y: -200, Width: 800, Height: 620}
	tests := []struct {
		name    string
		saved   application.Rect
		screens []*application.Screen
		want    application.Rect
	}{
		{
			name:    "preserve negative coordinates",
			saved:   secondaryBounds,
			screens: []*application.Screen{primary, secondary},
			want:    secondaryBounds,
		},
		{
			name:    "unplugged secondary",
			saved:   secondaryBounds,
			screens: []*application.Screen{primary},
			want:    application.Rect{X: 560, Y: 210, Width: 800, Height: 620},
		},
		{
			name:    "fit oversized window and taskbar",
			saved:   application.Rect{X: 120, Y: 80, Width: 2300, Height: 1200},
			screens: []*application.Screen{primary, secondary},
			want:    application.Rect{Width: 1920, Height: 1040},
		},
		{
			name:    "enforce minimum size on restore",
			saved:   application.Rect{X: 1800, Y: 950, Width: 100, Height: 70},
			screens: []*application.Screen{primary},
			want:    application.Rect{X: 1300, Y: 460, Width: 620, Height: 580},
		},
		{
			name:  "no screen information yet",
			saved: secondaryBounds,
			want:  secondaryBounds,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := fitWindowBounds(test.saved, test.screens); got != test.want {
				t.Fatalf("fitWindowBounds() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestWindowHeaderVisible(t *testing.T) {
	screens := []*application.Screen{{WorkArea: application.Rect{Width: 1000, Height: 800}}}
	bounds := application.Rect{X: -600, Y: 20, Width: 760, Height: 680}
	if !windowHeaderVisible(bounds, screens) {
		t.Fatal("partially visible window should retain its position when opened from the tray")
	}
	bounds.Y = -20
	if windowHeaderVisible(bounds, screens) {
		t.Fatal("window with only its body visible should be moved back")
	}
}

func TestWindowBoundsPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile", "window-placement.json")
	if got := readWindowBounds(path); got != (application.Rect{}) {
		t.Fatalf("missing placement = %+v", got)
	}

	initial := application.Rect{X: -800, Y: -120, Width: 760, Height: 680}
	updated := application.Rect{X: 45, Y: 60, Width: 900, Height: 700}
	for _, bounds := range []application.Rect{initial, updated} {
		if err := writeWindowBounds(path, bounds); err != nil {
			t.Fatal(err)
		}
		if got := readWindowBounds(path); got != bounds {
			t.Fatalf("readWindowBounds() = %+v, want %+v", got, bounds)
		}
	}

	for _, malformed := range []string{`{"Width":`, `{"Width":-1,"Height":680}`, `{"X":2147483647,"Width":760,"Height":680}`} {
		if err := os.WriteFile(path, []byte(malformed), 0600); err != nil {
			t.Fatal(err)
		}
		if got := readWindowBounds(path); got != (application.Rect{}) {
			t.Fatalf("malformed placement %s read as %+v", malformed, got)
		}
	}
}
