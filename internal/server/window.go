package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// A window, not a tab.
//
// `plum explore` opens a browser because you are about to read one recording and
// then close it. `plum watch` is meant to sit on a second screen for a week, and
// a tab cannot do that: it gets buried behind the other forty, it has an address
// bar advertising that it is a web page, and it has no identity in the dock to
// click back to.
//
// A Chromium-based browser started with --app= gives a frame with no tab strip
// and no address bar, its own dock entry, and — because it also gets its own
// --user-data-dir — its own window position remembered across restarts. That is
// most of what separates "an application" from "a page I left open", and it
// costs a different exec.Command rather than a dependency.
//
// If no such browser is installed, this reports false and the caller falls back
// to a tab. A window is better; a tab still works.
// OpenWindow opens the frame for a server that is already running — the second
// `plum watch` in a repository raises the first one's window rather than
// starting a rival. It falls back to a tab so that "already running" never
// means "and you cannot see it".
func OpenWindow(url, profileDir string) {
	if !openWindow(url, profileDir) {
		openBrowser(url)
	}
}

func openWindow(url, profileDir string) bool {
	bin := chromium()
	if bin == "" {
		return false
	}
	if profileDir != "" {
		if err := os.MkdirAll(profileDir, 0o755); err != nil {
			return false
		}
	}
	args := []string{
		"--app=" + url,
		"--no-first-run",
		"--no-default-browser-check",
	}
	if profileDir != "" {
		args = append(args, "--user-data-dir="+profileDir)
	}
	if runtime.GOOS == "linux" {
		// Window managers key on the class to give the window its own icon and
		// its own slot in the switcher rather than grouping it with the browser.
		args = append(args, "--class=plum")
	}
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		return false
	}
	// The browser outlives this call and we never look at its exit status, but
	// something has to reap it or it stays a zombie for as long as plum runs.
	go func() { _ = cmd.Wait() }()
	return true
}

// chromium finds a browser that understands --app=. Order is preference, not
// popularity: whichever is found first is the one the window will keep using,
// so it needs to be stable across runs rather than dependent on what is open.
func chromium() string {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		var dirs []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if v := os.Getenv(env); v != "" {
				dirs = append(dirs, v)
			}
		}
		for _, d := range dirs {
			candidates = append(candidates,
				filepath.Join(d, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(d, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
				filepath.Join(d, "Microsoft", "Edge", "Application", "msedge.exe"),
			)
		}
		candidates = append(candidates, "chrome.exe", "msedge.exe")
	default:
		candidates = []string{
			"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
			"brave-browser", "microsoft-edge",
		}
	}
	for _, c := range candidates {
		if strings.ContainsRune(c, filepath.Separator) {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}
