package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// After an action on the page it started on, the agent is answered only
// what changed (PRD §2.3): it read the whole page a moment ago, and every
// line again costs it time and tokens (QA 2026-10-19: a quiz turn read the
// same 12,000-character page after every click). A new page, or one that
// changed too much, is answered whole; browser_view always is.

// samePage reports whether two observations are of one document: the
// same address, loaded once (the page key's time origin).
func samePage(a, b *page) bool {
	if a == nil || b == nil || a.URL == "" || a.URL != b.URL {
		return false
	}
	var ka, kb []json.RawMessage
	if json.Unmarshal(a.PageKey, &ka) != nil || json.Unmarshal(b.PageKey, &kb) != nil || len(ka) == 0 || len(kb) == 0 {
		return false
	}
	return string(ka[0]) == string(kb[0])
}

// pageChanges is what changed from before to now on one document, and
// whether it is small enough to answer as changes.
type pageChanges struct {
	added, gone       []string // text lines
	controls          []string // added or changed controls' lines
	controlsGone      []int
	unchangedControls int
}

// changesOf compares two observations of one document. A control is
// matched by its number; its line without the off-screen mark is its
// state (a scroll moves controls on and off the screen, not a change).
func changesOf(before, now *page) (pageChanges, bool) {
	var c pageChanges
	c.added, c.gone = lineChanges(before.Text, now.Text)
	was := map[int]string{}
	for _, ctl := range controlsOf(before) {
		was[ctl.node] = strings.TrimSuffix(ctl.line, " (off-screen)")
	}
	seen := map[int]bool{}
	for _, ctl := range controlsOf(now) {
		seen[ctl.node] = true
		if old, ok := was[ctl.node]; ok && old == strings.TrimSuffix(ctl.line, " (off-screen)") {
			c.unchangedControls++
			continue
		}
		c.controls = append(c.controls, ctl.line)
	}
	for _, ctl := range controlsOf(before) {
		if !seen[ctl.node] {
			c.controlsGone = append(c.controlsGone, ctl.node)
		}
	}
	lines := len(strings.Split(now.Text, "\n"))
	small := len(c.added) <= lines/2 && len(c.controls) <= c.unchangedControls
	return c, small
}

// lineChanges answers the lines of now not in before and of before not in
// now, as multisets in their order.
func lineChanges(before, now string) (added, gone []string) {
	count := map[string]int{}
	for _, l := range strings.Split(before, "\n") {
		count[l]++
	}
	for _, l := range strings.Split(now, "\n") {
		if count[l] > 0 {
			count[l]--
			continue
		}
		if strings.TrimSpace(l) != "" {
			added = append(added, l)
		}
	}
	for _, l := range strings.Split(before, "\n") {
		if count[l] > 0 && strings.TrimSpace(l) != "" {
			count[l]--
			gone = append(gone, l)
		}
	}
	return added, gone
}

// writeChanges writes the changes as the page part of a result.
func writeChanges(b *strings.Builder, now *page, c pageChanges) {
	b.WriteString("Page: " + now.Title + "\n" + now.URL + "\n")
	b.WriteString("(The same page as before your action: only what changed is shown. browser_view shows it all.)\n")
	if len(c.added)+len(c.gone)+len(c.controls)+len(c.controlsGone) == 0 {
		b.WriteString("\nNothing on the page changed.\n")
		return
	}
	if len(c.added) > 0 {
		b.WriteString("\nNew text:\n" + strings.Join(c.added, "\n") + "\n")
	}
	if len(c.gone) > 0 {
		b.WriteString("\nText gone:\n" + strings.Join(c.gone, "\n") + "\n")
	}
	if len(c.controls) > 0 {
		b.WriteString("\nControls new or changed (act on one by its number):\n" + strings.Join(c.controls, "\n") + "\n")
	}
	if len(c.controlsGone) > 0 {
		gone := make([]string, len(c.controlsGone))
		for i, n := range c.controlsGone {
			gone[i] = fmt.Sprintf("[%d]", n)
		}
		b.WriteString("\nControls gone: " + strings.Join(gone, " ") + "\n")
	}
	fmt.Fprintf(b, "\n(%d other controls as before.)\n", c.unchangedControls)
}
