package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pictureJPEG is a w×h picture of c as a kept frame is (base64 JPEG), with
// noise below its top 40 pixels when noisy.
func pictureJPEG(t *testing.T, w, h int, c color.RGBA, noisy bool) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	if noisy {
		rnd := rand.New(rand.NewSource(int64(c.R) + int64(c.G)<<8 + int64(c.B)<<16))
		for y := 40; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetRGBA(x, y, color.RGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), 255})
			}
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

// lit counts the light pixels of img in r (a caption's text on its dark
// bar).
func lit(img image.Image, r image.Rectangle) int {
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if cr, cg, cb, _ := img.At(x, y).RGBA(); cr>>8 > 150 && cg>>8 > 150 && cb>>8 > 150 {
				n++
			}
		}
	}
	return n
}

// near reports whether a palette colour is about c.
func near(got color.Color, c color.RGBA) bool {
	r, g, b, _ := got.RGBA()
	d := func(x uint32, y uint8) bool { v := int(x>>8) - int(y); return v < 40 && v > -40 }
	return d(r, c.R) && d(g, c.G) && d(b, c.B)
}

var (
	red    = color.RGBA{220, 30, 30, 255}
	green  = color.RGBA{30, 200, 60, 255}
	blue   = color.RGBA{40, 60, 220, 255}
	yellow = color.RGBA{230, 220, 40, 255}
)

// A run's GIF shows its pictured steps in order, each 1.5 s and the last
// 3 s, captioned; a step without a picture (a sign-in, one never
// pictured) is not in it. A phone run's GIF is at most 480 wide and never
// wider than its pictures; a desktop run's at most 800. The replay names it.
func TestGIFGuideShowsThePicturedSteps(t *testing.T) {
	for _, c := range []struct {
		device       string
		picW, picH   int
		wantW, wantH int
		bar          int
	}{
		{"phone", 390, 844, 390, 844 + captions().barHeight(1), captions().barHeight(1)},
		{"", 1024, 640, 800, 500 + captions().barHeight(2), captions().barHeight(2)},
	} {
		store := runStore{dir: t.TempDir()}
		tl := &tools{runner: &runner{store: store}, workspace: t.TempDir()}
		r := &runState{ID: "brun_gif", Device: c.device, History: []historyRecord{
			{Step: 1, Operation: "OPEN", Action: "https://shop.example.com/", Frame: true},
			{Step: 2, Operation: "PERSON", Action: "A person signs in here", Kind: "person"},
			{Step: 3, Operation: "CLICK", Action: "Track order", Frame: true},
			{Step: 4, Operation: "SCROLL", Action: "Scroll down"},
			{Step: 5, Operation: "TYPE", Action: "Order number", Text: "A-1042", Frame: true},
		}}
		_ = os.MkdirAll(store.framesDir(r.ID), 0o700)
		for step, col := range map[int]color.RGBA{1: red, 3: green, 5: blue} {
			writeFile(t, filepath.Join(store.framesDir(r.ID), strconv.Itoa(step)+".jpg"), pictureJPEG(t, c.picW, c.picH, col, false))
		}
		path, err := tl.writeGIF(r)
		if err != nil || path != "browser-runs/brun_gif.gif" {
			t.Fatalf("writeGIF = %q, %v", path, err)
		}
		raw, err := os.ReadFile(filepath.Join(tl.workspace, path))
		if err != nil {
			t.Fatal(err)
		}
		g, err := gif.DecodeAll(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Image) != 3 || g.Delay[0] != 150 || g.Delay[1] != 150 || g.Delay[2] != 300 || g.LoopCount != 0 {
			t.Fatalf("%s: %d frames, delays %v, loop %d", c.device, len(g.Image), g.Delay, g.LoopCount)
		}
		if g.Config.Width != c.wantW || g.Config.Height != c.wantH {
			t.Fatalf("%s: the GIF is %dx%d; want %dx%d", c.device, g.Config.Width, g.Config.Height, c.wantW, c.wantH)
		}
		for i, col := range []color.RGBA{red, green, blue} {
			if px := g.Image[i].At(10, 10); !near(px, col) {
				t.Fatalf("%s: frame %d shows %v; want %v", c.device, i, px, col)
			}
			if lit(g.Image[i], image.Rect(0, c.wantH-c.bar, c.wantW, c.wantH)) == 0 {
				t.Fatalf("%s: frame %d has no caption", c.device, i)
			}
		}
		tl.writeReplay(r)
		if _, page := replayOf(t, tl.workspace, r.ID); !strings.Contains(page, `"gif":"browser-runs/brun_gif.gif"`) {
			t.Fatalf("the replay does not name the GIF")
		}
	}
}

// A GIF past its budget shrinks, then drops the lower ranked half of its
// pictures (the replay's order), keeping the first and the last.
func TestGIFStaysWithinItsBudget(t *testing.T) {
	cols := []color.RGBA{red, green, blue, yellow, {0, 200, 200, 255}, {200, 0, 200, 255}}
	ranks := []int{6, 1, 3, 2, 4, 5}
	var frames []gifFrame
	for i, col := range cols {
		raw, _ := base64.StdEncoding.DecodeString(pictureJPEG(t, 120, 90, col, true))
		img, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, gifFrame{img: img, caption: strconv.Itoa(i+1) + ". step", rank: ranks[i]})
	}
	whole, err := encodeGIF(frames, 120, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	budget := len(whole) / 6
	out, err := encodeGIF(frames, 120, budget)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > budget || len(g.Image) >= len(frames) || g.Config.Width >= 120 {
		t.Fatalf("%d bytes (budget %d), %d frames, %d wide", len(out), budget, len(g.Image), g.Config.Width)
	}
	if !near(g.Image[0].At(5, 5), cols[0]) || !near(g.Image[len(g.Image)-1].At(5, 5), cols[len(cols)-1]) || g.Delay[len(g.Delay)-1] != gifLastDelay {
		t.Fatalf("the first or the last step was dropped")
	}
	if _, err := encodeGIF(frames, 120, 100); err == nil {
		t.Fatal("a GIF over any budget was made")
	}
}

// fewer keeps the higher ranked half of the pictures, in order, a
// picture's frames together; one picture is as few as it gets.
func TestFewer(t *testing.T) {
	for ranks, want := range map[string]string{"1,2": "2", "3,1,2": "3,2", "4,1,3,2": "4,3", "2,2,1": "2,2", "5,5": "5,5"} {
		var frames []gifFrame
		for _, r := range strings.Split(ranks, ",") {
			n, _ := strconv.Atoi(r)
			frames = append(frames, gifFrame{rank: n})
		}
		var got []string
		for _, f := range fewer(frames) {
			got = append(got, strconv.Itoa(f.rank))
		}
		if strings.Join(got, ",") != want {
			t.Fatalf("fewer(%s) = %v; want %s", ranks, got, want)
		}
	}
}

// browser_close gif makes the run's GIF before the run goes, and the
// replay names it; a run without pictures says so and still closes.
func TestCloseMakesTheGIF(t *testing.T) {
	p, _, _ := expensePortal()
	h := newHarness(t, p, func(q decReq) (string, string) { return "", "" })
	h.tools.workspace = t.TempDir()
	head, _ := h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new", "device": "phone"})
	run := head["run"].(string)
	h.call("browser_act", map[string]any{"run": run, "action": "type", "element": 1, "text": "120.50"})
	r, err := h.tools.runner.store.load(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.History {
		if s.Frame { // the fake's pictures are no JPEGs
			writeFile(t, filepath.Join(h.tools.runner.store.framesDir(run), strconv.Itoa(s.Step)+".jpg"), pictureJPEG(t, 390, 844, green, false))
		}
	}
	head, _ = h.call("browser_close", map[string]any{"run": run, "gif": true})
	gifs, _ := head["gif"].([]any)
	if len(gifs) != 1 || gifs[0] != "browser-runs/"+run+".gif" || !strings.Contains(head["note"].(string), "GIF guide") {
		t.Fatalf("close = %v", head)
	}
	if _, err := os.Stat(filepath.Join(h.tools.workspace, replayDir, run+".gif")); err != nil {
		t.Fatal(err)
	}
	if _, page := replayOf(t, h.tools.workspace, run); !strings.Contains(page, run+".gif") {
		t.Fatal("the replay does not name the GIF")
	}
	if _, err := os.Stat(h.tools.runner.store.framesDir(run)); err == nil {
		t.Fatal("the run's frames outlived it")
	}

	head, _ = h.call("browser_open", map[string]any{"url": "https://portal.example.com/expenses/new"})
	other := head["run"].(string)
	_ = os.RemoveAll(h.tools.runner.store.framesDir(other))
	head, _ = h.call("browser_close", map[string]any{"run": other, "gif": true})
	if head["gif"] != nil || len(head["noGif"].([]any)) != 1 || len(head["stopped"].([]any)) != 1 {
		t.Fatalf("close of a run without pictures = %v", head)
	}
}
