package main

import (
	"errors"
	"strings"
	"testing"
)

// Without a setting, runs drive the first Chromium-based browser installed:
// Chrome, then Chromium, Edge and Brave, on every platform.
func TestBrowserCandidates(t *testing.T) {
	mac := browserCandidates("darwin", "/Users/maya", func(string) string { return "" }, nil)
	if len(mac) != 8 || mac[0] != "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" ||
		mac[5] != "/Users/maya/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" || !strings.HasSuffix(mac[6], "Brave Browser.app/Contents/MacOS/Brave Browser") {
		t.Fatalf("macOS = %q", mac)
	}
	env := map[string]string{"ProgramFiles": `C:\\Program Files`}
	win := browserCandidates("windows", "", func(k string) string { return env[k] }, nil)
	if len(win) != 4 || !strings.HasSuffix(win[2], `msedge.exe`) {
		t.Fatalf("Windows = %q", win)
	}
	onPath := map[string]bool{"microsoft-edge": true, "brave": true}
	linux := browserCandidates("linux", "", nil, func(name string) (string, error) {
		if onPath[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	})
	if strings.Join(linux, ",") != "/usr/bin/microsoft-edge,/usr/bin/brave" {
		t.Fatalf("Linux = %q", linux)
	}
}
