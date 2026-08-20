package cli

import (
	"testing"
)

// The port is the window's address. If it moves, a bookmark, a docked editor
// pane and a window reopened next week all stop finding it — which is the
// difference between an application and a page you have to go and start.
func TestStablePortIsStablePerRepositoryAndOutOfTheEphemeralRange(t *testing.T) {
	const a, b = "/Users/x/plum", "/Users/x/other"

	if stablePort(a) != stablePort(a) {
		t.Error("the same repository must always get the same port")
	}
	if stablePort(a) == stablePort(b) {
		t.Error("different repositories should not collide on this input")
	}
	for _, root := range []string{a, b, "", "/"} {
		// Above the well-known ports, and below the range every platform
		// allocates outgoing connections from, so the operating system will not
		// hand this port out from under the window.
		if p := stablePort(root); p < 17000 || p > 17999 {
			t.Errorf("stablePort(%q) = %d, outside the reserved range", root, p)
		}
	}
}
