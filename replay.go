package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A run's replay (PRD §2.8a): after each call the run's steps, each with
// the page after it, are written to browser-runs/<run>.html in the agent's
// workspace, which the bridge publishes as a room file like any other the
// agent made. A sign-in is never pictured; frames are kept newest first
// within the room's inline size.

const (
	// replayDir is the workspace directory replays are written to.
	replayDir = "browser-runs"
	// replayFrameBudget bounds a replay's pictures (base64 bytes): a page
	// with a script stays inline in the room up to 2 MiB.
	replayFrameBudget = 1_600_000
	// replayQuality is a frame's JPEG quality.
	replayQuality = 45
)

// framesDir is where a run's frames are kept, beside its state.
func (s runStore) framesDir(id string) string { return filepath.Join(s.dir, id+".frames") }

// keepFrame pictures the page after the run's last step as its frame,
// unless the page is a sign-in.
func (t *tools) keepFrame(ctx context.Context, r *runState, tb tabHandle) {
	pg, err := observe(ctx, tb)
	if err != nil || len(r.History) == 0 || signInPage(pg) || hasSecret(pg) {
		return
	}
	shot, _, err := capture(ctx, tb, replayQuality)
	if err != nil || shot == "" {
		return
	}
	dir := t.runner.store.framesDir(r.ID)
	last := &r.History[len(r.History)-1]
	if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(filepath.Join(dir, strconv.Itoa(last.Step)+".jpg"), []byte(shot), 0o600) != nil {
		return
	}
	last.Frame = true
}

// replayStep is one step as the replay page shows it.
type replayStep struct {
	Step  int    `json:"step"`
	What  string `json:"what"`
	Text  string `json:"text,omitempty"`
	URL   string `json:"url,omitempty"`
	At    string `json:"at"`
	Frame string `json:"frame,omitempty"`
	Note  string `json:"note,omitempty"`
}

// writeReplay writes the run's replay into the workspace ("" writes none).
// It never fails the call: a replay is a record, not the work.
func (t *tools) writeReplay(r *runState) {
	if t.workspace == "" || len(r.History) == 0 {
		return
	}
	steps := make([]replayStep, len(r.History))
	budget := replayFrameBudget
	for i := len(r.History) - 1; i >= 0; i-- {
		h := r.History[i]
		s := replayStep{Step: h.Step, What: describeStep(h), Text: h.Text, URL: h.URL, At: time.UnixMilli(h.AtMs).Format("15:04:05")}
		if h.Frame {
			raw, err := os.ReadFile(filepath.Join(t.runner.store.framesDir(r.ID), strconv.Itoa(h.Step)+".jpg"))
			switch {
			case err != nil:
			case len(raw) > budget:
				s.Note = "picture left out to keep the replay small"
			default:
				budget -= len(raw)
				s.Frame = string(raw)
			}
		}
		steps[i] = s
	}
	data, err := json.Marshal(map[string]any{"run": r.ID, "sites": r.Sites, "started": time.UnixMilli(r.StartedAtMs).Format("2006-01-02 15:04"), "steps": steps})
	if err != nil {
		return
	}
	page := strings.Replace(replayPage, "{{DATA}}", string(data), 1)
	dir := filepath.Join(t.workspace, replayDir)
	tmp := filepath.Join(dir, "."+r.ID+".tmp")
	if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(tmp, []byte(page), 0o644) != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, r.ID+".html")); err != nil {
		_ = os.Remove(tmp)
	}
}

// describeStep is a step's one line.
func describeStep(h historyRecord) string {
	switch h.Operation {
	case "OPEN":
		return "Opened " + h.Action
	case "PERSON":
		return h.Action
	case "TYPE":
		return fmt.Sprintf("Typed into %q", h.Action)
	case "SELECT":
		return fmt.Sprintf("Chose in %q", h.Action)
	case "PRESS":
		return "Pressed " + h.Action
	case "SCROLL", "BACK":
		return h.Action
	case "UPLOAD":
		return fmt.Sprintf("Uploaded to %q", h.Action)
	}
	return fmt.Sprintf("Clicked %q", h.Action)
}

// replayPage is the replay: the steps are JSON data (json.Marshal escapes
// <, > and &, so page text cannot close the script), shown with textContent
// only, never as markup.
const replayPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Browser run replay</title>
<style>
body{margin:0;font:14px system-ui,sans-serif;background:#111;color:#eee;display:flex;flex-direction:column;height:100vh}
header{padding:8px 12px;border-bottom:1px solid #333;display:flex;gap:8px;align-items:center;flex-wrap:wrap}
header .meta{color:#999;flex:1 1 200px}
button{background:#2a2a2a;color:#eee;border:1px solid #444;border-radius:6px;padding:4px 10px;cursor:pointer}
main{flex:1;display:flex;min-height:0}
#stage{flex:1;display:flex;flex-direction:column;min-width:0}
#shot{flex:1;min-height:0;display:flex;align-items:center;justify-content:center;background:#000}
#shot img{max-width:100%;max-height:100%;object-fit:contain}
#shot .none{color:#888;padding:24px;text-align:center}
#caption{padding:8px 12px;border-top:1px solid #333}
#caption .url{color:#888;font-size:12px;word-break:break-all}
ol{margin:0;padding:0;list-style:none;width:280px;overflow:auto;border-left:1px solid #333}
li{padding:6px 10px;border-bottom:1px solid #222;cursor:pointer}
li.on{background:#1d3a5a}
li .at{color:#888;font-size:12px;margin-right:6px}
@media (max-width:640px){main{flex-direction:column}ol{width:auto;max-height:35vh;border-left:0;border-top:1px solid #333}}
</style></head><body>
<header><strong>Browser run replay</strong><span class="meta" id="meta"></span>
<button id="prev">&#9664;</button><button id="play">Play</button><button id="next">&#9654;</button></header>
<main><div id="stage"><div id="shot"></div><div id="caption"><div id="what"></div><div class="url" id="url"></div></div></div><ol id="list"></ol></main>
<script id="data" type="application/json">{{DATA}}</script>
<script>
const d=JSON.parse(document.getElementById('data').textContent), steps=d.steps;
const $=id=>document.getElementById(id);
$('meta').textContent=d.run+' · '+(d.sites||[]).join(', ')+' · '+d.started+' · '+steps.length+' steps';
let at=steps.length-1, timer=null;
steps.forEach((s,i)=>{const li=document.createElement('li');const t=document.createElement('span');t.className='at';t.textContent=s.at;
  li.append(t,document.createTextNode(s.step+'. '+s.what));li.onclick=()=>{stop();show(i)};$('list').append(li)});
function show(i){if(i<0||i>=steps.length)return;at=i;const s=steps[i],shot=$('shot');shot.replaceChildren();
  if(s.frame){const img=new Image();img.alt='The page after step '+s.step;img.src='data:image/jpeg;base64,'+s.frame;shot.append(img)}
  else{const p=document.createElement('div');p.className='none';p.textContent=s.note||'No picture for this step';shot.append(p)}
  $('what').textContent=s.step+'. '+s.what+(s.text?' — '+s.text:'');$('url').textContent=s.url||'';
  [...$('list').children].forEach((li,j)=>li.classList.toggle('on',j===i));$('list').children[i]?.scrollIntoView({block:'nearest'})}
function stop(){clearInterval(timer);timer=null;$('play').textContent='Play'}
$('prev').onclick=()=>{stop();show(at-1)};$('next').onclick=()=>{stop();show(at+1)};
$('play').onclick=()=>{if(timer){stop();return}if(at>=steps.length-1)show(0);$('play').textContent='Pause';
  timer=setInterval(()=>{if(at>=steps.length-1){stop();return}show(at+1)},1500)};
addEventListener('keydown',e=>{if(e.key==='ArrowLeft')$('prev').click();if(e.key==='ArrowRight')$('next').click()});
show(at);
</script></body></html>
`
