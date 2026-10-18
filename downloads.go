package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Downloads (§2.7): the browser saves every download under the host's
// download directory by its guid (Browser.setDownloadBehavior allowAndName);
// the run whose tab started one moves the finished file into the agent's
// workspace (downloads/), where the turn's deliverable publishing makes it
// a room file sourced to the job — the one path every file an agent
// produces takes. The checkpoint names the file.

// downloadStart is one download the browser announced.
type downloadStart struct {
	Frame string // the frame that started it (a tab's main frame is its target id)
	Name  string // the suggested file name
	Done  bool
}

// collectDownloads moves this run's finished downloads into the
// workspace; a checkpoint names them.
func (rn *runner) collectDownloads(r *runState) *checkpoint {
	saved := rn.host.finishedDownloads(r.Tab, rn.downloads)
	if len(saved) == 0 {
		return nil
	}
	sortStrings(saved)
	return &checkpoint{Kind: cpDownload, Artifacts: saved,
		Reason: "downloaded " + strings.Join(saved, ", ") + " into your workspace; it becomes a room file with this turn's files. Resume the run to keep going"}
}

// finishedDownloads reads the browser's download events and moves the
// tab's finished files into dir.
func (h *localHost) finishedDownloads(tabID, dir string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return nil
	}
	for _, ev := range h.conn.drain("", "Browser.download") {
		var p struct {
			GUID              string `json:"guid"`
			FrameID           string `json:"frameId"`
			SuggestedFilename string `json:"suggestedFilename"`
			State             string `json:"state"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.GUID == "" {
			continue
		}
		switch ev.Method {
		case "Browser.downloadWillBegin":
			h.downloads[p.GUID] = downloadStart{Frame: p.FrameID, Name: p.SuggestedFilename}
		case "Browser.downloadProgress":
			d, ok := h.downloads[p.GUID]
			switch {
			case !ok:
			case p.State == "completed":
				d.Done = true
				h.downloads[p.GUID] = d
			case p.State == "canceled":
				delete(h.downloads, p.GUID)
			}
		}
	}
	var saved []string
	for guid, d := range h.downloads {
		if !d.Done || d.Frame != tabID {
			continue
		}
		name, err := moveDownload(filepath.Join(h.dl, guid), dir, d.Name)
		delete(h.downloads, guid)
		if err == nil {
			saved = append(saved, name)
		}
	}
	return saved
}

// moveDownload moves a finished download into dir under a safe, unused
// name and answers the workspace-relative path.
func moveDownload(src, dir, suggested string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := safeFileName(suggested)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	dst := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
	}
	if err := os.Rename(src, dst); err != nil {
		if err := copyFile(src, dst); err != nil {
			return "", err
		}
		_ = os.Remove(src)
	}
	return filepath.Join(filepath.Base(dir), filepath.Base(dst)), nil
}

// safeFileName keeps a suggested name's last path segment, without control
// or path characters; "download" when nothing is left.
func safeFileName(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, `\`, "/"))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
	s = strings.TrimLeft(strings.TrimSpace(s), ".")
	if s == "" {
		return "download"
	}
	return s
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
