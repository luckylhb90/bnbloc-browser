package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// A run's GIF guide (browser_close gif): the run's pictures, step by step,
// as one animated GIF in the agent's workspace, browser-runs/<run>.gif,
// which the bridge publishes as a room file (an image) beside the run's
// replay. It is what a how-to for a support team is made of (2026-10-20:
// a member wanted animated guides of their company's real app): each step
// the run took shows for gifStepDelay with its caption (describeStep, the
// replay's line), the last for gifLastDelay. Only steps with a picture are
// in it, so a sign-in never is (replay.go). Each frame has its own palette
// (median cut), undithered: a page's flat colours stay flat and compress.

const (
	// gifStepDelay and gifLastDelay are how long a step shows, in
	// hundredths of a second (the GIF's unit); the last stays longer.
	gifStepDelay = 150
	gifLastDelay = 300
	// gifPhoneWidth bounds a phone run's GIF width, gifWidth any other's.
	gifPhoneWidth = 480
	gifWidth      = 800
	// gifBudget bounds the file: past it the pictures shrink (to half
	// their width at most), then every other step is dropped.
	gifBudget = 8 << 20
)

// The caption bar's colours, always in a frame's palette.
var (
	gifBar  = color.RGBA{24, 24, 24, 255}
	gifText = color.RGBA{240, 240, 240, 255}
)

// gifFrame is one step's picture and caption.
type gifFrame struct {
	img     image.Image
	caption string
}

// writeGIF writes the run's GIF guide into the workspace and answers its
// workspace path; an error says why there is none.
func (t *tools) writeGIF(r *runState) (string, error) {
	if t.workspace == "" {
		return "", errors.New("no workspace to write it to")
	}
	var frames []gifFrame
	for _, h := range r.History {
		if !h.Frame {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(t.runner.store.framesDir(r.ID), strconv.Itoa(h.Step)+".jpg"))
		if err != nil {
			continue
		}
		jpg, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			continue
		}
		img, err := jpeg.Decode(bytes.NewReader(jpg))
		if err != nil {
			continue
		}
		caption := fmt.Sprintf("%d. %s", len(frames)+1, describeStep(h))
		if h.Text != "" {
			caption += ": " + h.Text
		}
		frames = append(frames, gifFrame{img: img, caption: caption})
	}
	if len(frames) == 0 {
		return "", errors.New("the run has no pictures to make a GIF of")
	}
	widest := gifWidth
	if r.Device == "phone" {
		widest = gifPhoneWidth
	}
	data, err := encodeGIF(frames, widest, gifBudget)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(t.workspace, replayDir)
	tmp := filepath.Join(dir, "."+r.ID+".gif.tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, r.ID+".gif")); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return replayDir + "/" + r.ID + ".gif", nil
}

// gifOf answers the workspace path of the run's GIF guide, "" none.
func (t *tools) gifOf(id string) string {
	if _, err := os.Stat(filepath.Join(t.workspace, replayDir, id+".gif")); err != nil {
		return ""
	}
	return replayDir + "/" + id + ".gif"
}

// encodeGIF is frames as an animated GIF at most widest wide (never wider
// than its widest picture) and budget bytes.
func encodeGIF(frames []gifFrame, widest, budget int) ([]byte, error) {
	w := 0
	for _, f := range frames {
		w = max(w, f.img.Bounds().Dx())
	}
	w = min(w, widest)
	least := w / 2
	for {
		out, err := encodeFrames(frames, w)
		if err != nil {
			return nil, err
		}
		if len(out) <= budget {
			return out, nil
		}
		switch {
		case w*3/4 >= least:
			w = w * 3 / 4
		case len(frames) > 2:
			frames = thinned(frames)
		default:
			return nil, fmt.Errorf("the GIF stays over %d MB", budget>>20)
		}
	}
}

// thinned is every other frame, the last kept.
func thinned(frames []gifFrame) []gifFrame {
	var out []gifFrame
	for i := 0; i < len(frames)-1; i += 2 {
		out = append(out, frames[i])
	}
	return append(out, frames[len(frames)-1])
}

// encodeFrames encodes frames w wide: each picture scaled to w, on a
// canvas as tall as the tallest, its caption in a bar below.
func encodeFrames(frames []gifFrame, w int) ([]byte, error) {
	if w <= 0 {
		return nil, errors.New("the pictures are empty")
	}
	scale := 1
	if w >= 640 {
		scale = 2 // the caption font is 13 pixels high
	}
	bar := (2*captionPad + captionLines*basicfont.Face7x13.Height) * scale
	pics := make([]*image.RGBA, len(frames))
	h := 0
	for i, f := range frames {
		b := f.img.Bounds()
		pics[i] = scaleImage(f.img, w, max(1, b.Dy()*w/b.Dx()))
		h = max(h, pics[i].Bounds().Dy())
	}
	g := &gif.GIF{}
	for i, f := range frames {
		canvas := image.NewRGBA(image.Rect(0, 0, w, h+bar))
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(gifBar), image.Point{}, draw.Src)
		draw.Draw(canvas, pics[i].Bounds(), pics[i], image.Point{}, draw.Src)
		drawCaption(canvas, image.Rect(0, h, w, h+bar), f.caption, scale)
		g.Image = append(g.Image, quantize(canvas))
		delay := gifStepDelay
		if i == len(frames)-1 {
			delay = gifLastDelay
		}
		g.Delay = append(g.Delay, delay)
	}
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, g); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// A caption is at most captionLines lines, captionPad pixels from the
// bar's edges (before its scale).
const (
	captionLines = 2
	captionPad   = 4
)

// drawCaption writes caption into the bar at r, in basicfont's 7×13 face
// drawn scale times its size.
func drawCaption(dst *image.RGBA, r image.Rectangle, caption string, scale int) {
	face := basicfont.Face7x13
	strip := image.NewRGBA(image.Rect(0, 0, r.Dx()/scale, r.Dy()/scale))
	draw.Draw(strip, strip.Bounds(), image.NewUniform(gifBar), image.Point{}, draw.Src)
	d := font.Drawer{Dst: strip, Src: image.NewUniform(gifText), Face: face}
	for i, line := range wrapCaption(asciiCaption(caption), (strip.Bounds().Dx()-2*captionPad)/face.Advance, captionLines) {
		d.Dot = fixed.P(captionPad, captionPad+face.Ascent+i*face.Height)
		d.DrawString(line)
	}
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			dst.SetRGBA(r.Min.X+x, r.Min.Y+y, strip.RGBAAt(x/scale, y/scale))
		}
	}
}

// asciiCaption is s in the caption font's characters, printable ASCII: a
// run of others (a label in Chinese, an emoji) reads "...", a line break
// a space.
func asciiCaption(s string) string {
	var b strings.Builder
	other := false
	for _, c := range s {
		switch {
		case c == '\n' || c == '\t' || c == '\r':
			b.WriteByte(' ')
			other = false
		case c >= 0x20 && c < 0x7f:
			b.WriteRune(c)
			other = false
		case !other:
			b.WriteString("...")
			other = true
		}
	}
	return b.String()
}

// wrapCaption breaks s into lines of at most per characters at spaces (a
// longer word is cut), at most lines of them, the last ending "..." when
// more did not fit.
func wrapCaption(s string, per, lines int) []string {
	if per < 4 {
		return nil
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		for len(word) > per {
			if line != "" {
				out, line = append(out, line), ""
			}
			out, word = append(out, word[:per]), word[per:]
		}
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= per:
			line += " " + word
		default:
			out, line = append(out, line), word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	if len(out) > lines {
		out = out[:lines]
		last := out[lines-1]
		if len(last) > per-3 {
			last = last[:per-3]
		}
		out[lines-1] = last + "..."
	}
	return out
}

// quantize maps img to a palette of its own: the caption bar's two colours
// and a median cut of img's (medianCut), each pixel the nearest, looked up
// once per 15-bit colour.
func quantize(img *image.RGBA) *image.Paletted {
	pal := append(color.Palette{gifBar, gifText}, medianCut(img, 254)...)
	out := image.NewPaletted(img.Bounds(), pal)
	var lookup [1 << 15]int16 // a colour's palette index + 1 (0: not looked up)
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			k := key15(c)
			if lookup[k] == 0 {
				lookup[k] = int16(nearest(pal, c)) + 1
			}
			out.SetColorIndex(x, y, uint8(lookup[k]-1))
		}
	}
	return out
}

// nearest is the index of pal's colour nearest c (pal's are color.RGBA).
func nearest(pal color.Palette, c color.RGBA) int {
	best, at := 1<<30, 0
	for i, p := range pal {
		q := p.(color.RGBA)
		dr, dg, db := int(q.R)-int(c.R), int(q.G)-int(c.G), int(q.B)-int(c.B)
		if d := dr*dr + dg*dg + db*db; d < best {
			best, at = d, i
		}
	}
	return at
}

// key15 is a colour's 15-bit key: five bits a channel.
func key15(c color.RGBA) uint16 {
	return uint16(c.R>>3)<<10 | uint16(c.G>>3)<<5 | uint16(c.B>>3)
}

// medianCut is a palette of at most n colours for img: its 15-bit colour
// histogram cut, box by box (the widest first), at the median pixel of the
// box's widest channel; each box's colour is its pixels' average.
func medianCut(img *image.RGBA, n int) color.Palette {
	var hist [1 << 15]int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			hist[key15(img.RGBAAt(x, y))]++
		}
	}
	type entry struct {
		key   uint16
		count int
	}
	// channel is a key's 5-bit value of channel c (0 red, 1 green, 2 blue).
	channel := func(k uint16, c int) int { return int(k>>(10-5*c)) & 31 }
	type box struct {
		entries []entry
		widest  int // its widest channel
		span    int // that channel's range
	}
	newBox := func(es []entry) box {
		bx := box{entries: es}
		for c := 0; c < 3; c++ {
			lo, hi := 31, 0
			for _, e := range es {
				v := channel(e.key, c)
				lo, hi = min(lo, v), max(hi, v)
			}
			if hi-lo > bx.span || c == 0 {
				bx.widest, bx.span = c, hi-lo
			}
		}
		return bx
	}
	var all []entry
	for k, count := range hist {
		if count > 0 {
			all = append(all, entry{uint16(k), count})
		}
	}
	if len(all) == 0 {
		return nil
	}
	boxes := []box{newBox(all)}
	for len(boxes) < n {
		at := -1
		for i, bx := range boxes {
			if len(bx.entries) > 1 && (at < 0 || bx.span > boxes[at].span) {
				at = i
			}
		}
		if at < 0 || boxes[at].span == 0 {
			break
		}
		es, c := boxes[at].entries, boxes[at].widest
		sort.Slice(es, func(i, j int) bool { return channel(es[i].key, c) < channel(es[j].key, c) })
		total, half, cut := 0, 0, 0
		for _, e := range es {
			total += e.count
		}
		for cut < len(es)-1 {
			half += es[cut].count
			cut++
			if half*2 >= total {
				break
			}
		}
		boxes[at] = newBox(es[:cut])
		boxes = append(boxes, newBox(es[cut:]))
	}
	// eight is a channel's 5-bit value as eight bits.
	eight := func(k uint16, c int) int { v := channel(k, c); return v<<3 | v>>2 }
	pal := make(color.Palette, 0, len(boxes))
	for _, bx := range boxes {
		var r, g, bl, sum int
		for _, e := range bx.entries {
			r += eight(e.key, 0) * e.count
			g += eight(e.key, 1) * e.count
			bl += eight(e.key, 2) * e.count
			sum += e.count
		}
		pal = append(pal, color.RGBA{uint8(r / sum), uint8(g / sum), uint8(bl / sum), 255})
	}
	return pal
}
