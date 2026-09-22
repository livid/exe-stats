package stats

import (
	"strings"
	"testing"
)

// TestSharedBlocks: the chrome and the pop-up menu button are served
// whole, and the template has them under the same names.
func TestSharedBlocks(t *testing.T) {
	if !strings.Contains(ChromeCSS(), ".titlebar") || !strings.Contains(PopupCSS(), ".popup select") || !strings.Contains(PopupCSS(), ".popup .well b") {
		t.Fatal("a shared block is missing its rules")
	}
	f := Funcs()
	for _, name := range []string{"chrome", "popup"} {
		if _, ok := f[name]; !ok {
			t.Errorf("no %q template func", name)
		}
	}
}
