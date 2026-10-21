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
// is the pointer cursor. The snapshot lists each once, as a button, and a
// click on one routes the page; a card keeps its own button instead, a
// pointer span inside a button is that button's, and a label over a hidden
// checkbox stands for it.
func TestScriptClickableControlsOnChrome(t *testing.T) {
	c := startTestChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>console</title><style>
.menu-item,.submenu-title,.card,.tick{cursor:pointer}
.tick input{opacity:0;position:absolute;width:0;height:0}
</style>
<ul class="menu">
  <li class="submenu"><div class="submenu-title"><span class="text">SQLQuery</span> <span class="badge">6</span></div>
    <ul><li class="menu-item" data-to="/select/query"><span class="text">Query</span></li>
        <li class="menu-item" data-to="/select/query_new"><span class="text">Query_new</span></li></ul></li>
  <li class="menu-item" style="cursor:not-allowed"><span>Locked</span></li>
</ul>
<div class="card"><h3>Pending reviews</h3><button type="button" id="open">Open <span style="cursor:pointer">reviews</span></button></div>
<label class="tick"><input type="checkbox" id="mine"> Only mine</label>
<div onclick="document.title='plain'">Plain handler</div>
<div class="card" aria-label=""><span></span></div>
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
		if _, dup := labels[a.Label]; dup {
			t.Fatalf("%q listed twice: %+v", a.Label, pg.Actions)
		}
		labels[a.Label] = a
		got = append(got, a.Role+" "+a.Kind+" "+strings.Join(strings.Fields(a.Label), " "))
	}
	// Native controls first, as before; then the script's, in page order.
	want := []string{
		"button click Open reviews",
		"button click SQLQuery 6",
		"button click Query",
		"button click Query_new",
		"checkbox click Only mine",
		"button click Plain handler",
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
