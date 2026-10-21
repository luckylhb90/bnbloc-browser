package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A page built the way component-library admin pages are (QA 2026-10-21: a
// database console's side menu was text the agent could not click): menu
// items are li/div elements whose click a script binds and whose only sign
// is the pointer cursor. The snapshot lists each once, in page order, as a
// button with its state from the library's classes, and a click on one
// routes the page. A card keeps its own button instead, a pointer span
// inside a button is that button's, a label over a hidden checkbox stands
// for it, an icon is named by its icon, a row's "Edit" carries its column
// and row, and a control the library disabled by class is left out.
func TestScriptClickableControlsOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>console</title><style>
.x-menu-item,.x-submenu-title,.card,.tick,.x-icon-wrap,.edit,.x-page-item{cursor:pointer}
.tick input{opacity:0;position:absolute;width:0;height:0}
.x-icon-wrap{display:inline-block;width:16px;height:16px}
</style>
<ul class="menu">
  <li class="x-menu-item x-menu-item-selected"><span>Home</span></li>
  <li class="x-submenu x-menu-opened"><div class="x-submenu-title"><span class="text">SQLQuery</span> <span class="badge">6</span></div>
    <ul><li class="x-menu-item" data-to="/select/query"><span class="text">Query</span></li>
        <li class="x-menu-item" data-to="/select/query_new"><span class="text">Query_new</span></li></ul></li>
  <li class="x-menu-item" style="cursor:not-allowed"><span>Locked</span></li>
</ul>
<span class="x-icon-wrap"><i class="x-icon x-icon-ios-refresh"></i></span>
<div class="card"><h3>Pending reviews</h3><button type="button" id="open">Open <span style="cursor:pointer">reviews</span></button></div>
<label class="tick"><input type="checkbox" id="mine"> Only mine</label>
<div onclick="document.title='plain'">Plain handler</div>
<div class="card" aria-label=""><span></span></div>
<table><thead><tr><th>Name</th><th>Action</th></tr></thead>
<tbody><tr><td>alpha-db</td><td><span class="edit">Edit</span></td></tr>
<tr><td>beta-db</td><td><span class="edit">Edit</span></td></tr></tbody></table>
<ul class="x-page"><li class="x-page-prev x-page-disabled"><a><i class="x-icon x-icon-ios-arrow-back"></i></a></li>
<li class="x-page-item">2</li></ul>
<main id="view">Home</main>
<script>
for (const li of document.querySelectorAll('[data-to]')) li.addEventListener('click', () => {
  history.pushState({}, '', li.dataset.to);
  document.getElementById('view').textContent = 'Page ' + li.dataset.to;
});
</script>`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tb, err := openTab(ctx, c, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := navigate(ctx, tb, srv.URL+"/home"); err != nil {
		t.Fatal(err)
	}
	pg, err := observe(ctx, tb)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]action{}
	var got []string
	for _, a := range pg.Actions {
		if a.Node == nil {
			continue
		}
		label := strings.Join(strings.Fields(a.Label), " ")
		if _, dup := labels[label]; dup {
			t.Fatalf("%q listed twice: %+v", label, pg.Actions)
		}
		labels[label] = a
		line := a.Role + " " + label
		if a.Expanded != "" {
			line += " expanded=" + a.Expanded
		}
		if a.Selected != "" {
			line += " selected=" + a.Selected
		}
		got = append(got, line)
	}
	want := []string{
		"button Home selected=true",
		"button SQLQuery 6 expanded=true",
		"button Query",
		"button Query_new",
		"button refresh (icon)",
		"button Open reviews",
		"checkbox Only mine",
		"button Plain handler",
		"button Edit (Action; alpha-db)",
		"button Edit (Action; beta-db)",
		"button 2",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("controls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if labels["Only mine"].Checked != "false" {
		t.Fatalf("the hidden checkbox's label = %+v", labels["Only mine"])
	}

	if err := act(ctx, tb, pg, labels["Query_new"], ""); err != nil {
		t.Fatalf("click the menu item: %v", err)
	}
	pg2, err := observe(ctx, tb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pg2.URL, "/select/query_new") || !strings.Contains(pg2.Text, "Page /select/query_new") {
		t.Fatalf("after the click: %s %q", pg2.URL, pg2.Text)
	}
	var mine *action
	for i, a := range pg2.Actions {
		if a.Label == "Only mine" {
			mine = &pg2.Actions[i]
		}
	}
	if mine == nil {
		t.Fatalf("no label after routing: %+v", pg2.Actions)
	}
	if err := act(ctx, tb, pg2, *mine, ""); err != nil {
		t.Fatalf("click the label: %v", err)
	}
	var checked bool
	if err := evaluate(ctx, tb, "document.getElementById('mine').checked", &checked); err != nil || !checked {
		t.Fatalf("the hidden checkbox after its label's click = %v %v", checked, err)
	}
}
