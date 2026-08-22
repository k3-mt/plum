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
	// If plum has actually been installed as an application, that is the window
	// to open — it has the title bar plum draws itself, which a shortcut window
	// does not. "Actually installed" is read from disk, not from the browser:
	// the browser's profile goes on claiming an uninstalled app is installed,
	// but the app bundle it writes is deleted on uninstall, so the bundle's
	// presence is the one signal that does not lie.
	if openInstalledApp(url) {
		return
	}
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
	// The shortcut window. The installed app, when there is one, was already
	// opened by openInstalledApp before this was reached; this is the frame for
	// when plum is not installed, and it shows the invitation to install it.
	args := []string{
		"--app=" + url,
		"--no-first-run",
		"--no-default-browser-check",
		// The frame is the browser's, not ours, and on macOS it is drawn by the
		// system in the system's appearance — so on a machine set to Light the
		// window opens as a white title bar sitting on top of a near-black page.
		// The page cannot reach that strip: it is native furniture, and a
		// --app= window is not an installed app, so window controls overlay is
		// refused and there is no way to take the strip over and paint it.
		// Forcing the browser's own theme dark is what is left, and it is
		// enough — the frame stops disagreeing with the page.
		// Unconditional rather than following the system, because plum has one
		// palette and it is a dark one. A window that matched a Light desktop
		// would match nothing that is actually in the window.
		"--force-dark-mode",
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

// openInstalledApp opens the installed plum application for this repository's
// window, and reports whether it did. Only macOS is wired: there the install is
// a real .app bundle whose Info.plist records the URL it was installed for, so
// the one belonging to this window can be found and opened exactly as clicking
// its dock icon would. Elsewhere this is a no-op and the shortcut window is used
// — the install page still works there, it just cannot be skipped past.
func openInstalledApp(url string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	bundle := installedBundle(url)
	if bundle == "" {
		return false
	}
	// `open` a bundle raises it if it is already running, so a second `plum
	// watch` brings the app forward rather than starting a rival — the same
	// promise the shortcut window makes.
	cmd := exec.Command("open", bundle)
	return cmd.Start() == nil
}

// IsInstalled reports whether plum has been installed as an application for the
// window at this URL. It is the fact the front door turns on: an installed
// window is the application and shows the test view; an uninstalled one is a
// shortcut and shows the invitation to install. Only macOS can answer with
// certainty (the app bundle on disk); elsewhere it reports false and the client
// falls back to reading the display mode.
func IsInstalled(url string) bool {
	return installedBundle(url) != ""
}

// installedBundle returns the path of the installed app whose window is this
// URL, or "" for none. Chrome writes these under ~/Applications/Chrome Apps*,
// each a .app whose Info.plist key CrAppModeShortcutURL is the address it opens
// — matching on that ties the app to this repository's port rather than to any
// plum, so two repositories do not open each other's window.
func installedBundle(url string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	appsDir := filepath.Join(home, "Applications", "Chrome Apps.localized")
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		// The unlocalised name is the fallback some Chrome builds use.
		appsDir = filepath.Join(home, "Applications", "Chrome Apps")
		entries, err = os.ReadDir(appsDir)
		if err != nil {
			return ""
		}
	}
	// Chrome stores the URL with a trailing slash; matching on that boundary
	// keeps port 1717 from being found inside a bundle for port 17177.
	needle := strings.TrimRight(url, "/") + "/"
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".app") {
			continue
		}
		plist := filepath.Join(appsDir, e.Name(), "Contents", "Info.plist")
		// The URL is stored as a plain string in the plist, so its presence in
		// the file's bytes is enough to match without parsing the format — which
		// may be XML or binary, and Go's standard library reads neither.
		b, err := os.ReadFile(plist)
		if err == nil && strings.Contains(string(b), needle) {
			return filepath.Join(appsDir, e.Name())
		}
	}
	return ""
}
