package main

import (
	"image"
	"image/draw"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// A GIF step's caption (gif.go), in the members' own languages (Chinese,
// Vietnamese: a guide in ASCII only was no guide for them, 2026-10-20). It
// is drawn in the system fonts found on the machine (captionFontPaths, in
// order), each character in the first of them that has it; basicfont's
// 7×13 ASCII face is the last resort, and on a machine with none of the
// fonts it is the only one (other characters then read "...").

// captionFontPaths are the system fonts captions may use, in order, by
// operating system: one that covers Chinese first, then ones that cover
// Vietnamese and the rest of Latin.
func captionFontPaths(goos string, getenv func(string) string) []string {
	switch goos {
	case "darwin":
		return []string{"/System/Library/Fonts/PingFang.ttc", "/System/Library/Fonts/Hiragino Sans GB.ttc", "/System/Library/Fonts/STHeiti Medium.ttc",
			"/Library/Fonts/Arial Unicode.ttf", "/System/Library/Fonts/Supplemental/Arial Unicode.ttf", "/System/Library/Fonts/Helvetica.ttc"}
	case "windows":
		dir := getenv("WINDIR")
		if dir == "" {
			dir = `C:\Windows`
		}
		var out []string
		for _, f := range []string{"msyh.ttc", "simsun.ttc", "segoeui.ttf", "arial.ttf"} {
			out = append(out, filepath.Join(dir, "Fonts", f))
		}
		return out
	}
	return []string{"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", "/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/google-noto-cjk/NotoSansCJK-Regular.ttc", "/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
		"/usr/share/fonts/wqy-microhei/wqy-microhei.ttc", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/dejavu/DejaVuSans.ttf"}
}

const (
	// captionFontsMax bounds how many fonts captions keep open.
	captionFontsMax = 4
	// A caption is at most captionLines lines, captionPad pixels from the
	// bar's edges (before its scale).
	captionLines = 2
	captionPad   = 4
)

// captioner draws captions in the fonts it found. Faces are not safe to
// use at once: one caption is drawn at a time.
type captioner struct {
	mu    sync.Mutex
	fonts []*sfnt.Font
	faces map[int][]font.Face // by pixel size: the fonts', basicfont last
}

// captions is the machine's captioner, its fonts looked for once.
var captions = sync.OnceValue(func() *captioner { return newCaptioner(captionFontPaths(runtime.GOOS, os.Getenv)) })

// newCaptioner opens the first captionFontsMax fonts of paths that parse
// (a collection's first font), each file once. A font is read from its
// file as needed, never whole: a CJK font is tens of megabytes.
func newCaptioner(paths []string) *captioner {
	c := &captioner{faces: map[int][]font.Face{}}
	seen := map[string]bool{}
	for _, p := range paths {
		if len(c.fonts) == captionFontsMax {
			break
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		f, err := os.Open(real)
		if err != nil {
			continue
		}
		col, err := sfnt.ParseCollectionReaderAt(f)
		var fnt *sfnt.Font
		if err == nil {
			fnt, err = col.Font(0)
		}
		if err != nil {
			f.Close()
			continue
		}
		c.fonts = append(c.fonts, fnt) // its file stays open for the process
	}
	return c
}

// metrics are a caption's sizes at scale: the font's pixel size (0 for
// basicfont, drawn at its own size and scaled up), the line height and the
// padding.
func (c *captioner) metrics(scale int) (px, line, pad int) {
	if len(c.fonts) == 0 {
		return 0, basicfont.Face7x13.Height, captionPad
	}
	px = 14 * scale
	return px, px * 9 / 7, captionPad * scale
}

// barHeight is the caption bar's height at scale.
func (c *captioner) barHeight(scale int) int {
	_, line, pad := c.metrics(scale)
	if len(c.fonts) == 0 {
		return (2*pad + captionLines*line) * scale
	}
	return 2*pad + captionLines*line
}

// facesAt are the fonts' faces at px pixels, then basicfont.
func (c *captioner) facesAt(px int) []font.Face {
	if faces, ok := c.faces[px]; ok {
		return faces
	}
	var faces []font.Face
	for _, f := range c.fonts {
		if face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(px), DPI: 72, Hinting: font.HintingFull}); err == nil {
			faces = append(faces, face)
		}
	}
	faces = append(faces, basicfont.Face7x13)
	c.faces[px] = faces
	return faces
}

// faceFor is the first of faces that has r (the last, basicfont, else).
func faceFor(faces []font.Face, r rune) font.Face {
	for _, f := range faces {
		if _, ok := f.GlyphAdvance(r); ok {
			return f
		}
	}
	return faces[len(faces)-1]
}

// draw writes caption into the bar at r, at most captionLines lines.
func (c *captioner) draw(dst *image.RGBA, r image.Rectangle, caption string, scale int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	draw.Draw(dst, r, image.NewUniform(gifBar), image.Point{}, draw.Src)
	text := image.NewUniform(gifText)
	px, line, pad := c.metrics(scale)
	if px == 0 {
		// basicfont alone: drawn at its size, then scaled up.
		face := basicfont.Face7x13
		strip := image.NewRGBA(image.Rect(0, 0, r.Dx()/scale, r.Dy()/scale))
		draw.Draw(strip, strip.Bounds(), image.NewUniform(gifBar), image.Point{}, draw.Src)
		d := font.Drawer{Dst: strip, Src: text, Face: face}
		advance := func(rune) fixed.Int26_6 { return fixed.I(face.Advance) }
		for i, l := range wrapCaption(asciiCaption(caption), fixed.I(strip.Bounds().Dx()-2*pad), advance, captionLines, "...") {
			d.Dot = fixed.P(pad, pad+face.Ascent+i*line)
			d.DrawString(l)
		}
		for y := 0; y < r.Dy(); y++ {
			for x := 0; x < r.Dx(); x++ {
				dst.SetRGBA(r.Min.X+x, r.Min.Y+y, strip.RGBAAt(x/scale, y/scale))
			}
		}
		return
	}
	faces := c.facesAt(px)
	advance := func(ch rune) fixed.Int26_6 { a, _ := faceFor(faces, ch).GlyphAdvance(ch); return a }
	ascent := faces[0].Metrics().Ascent.Ceil()
	for i, l := range wrapCaption(caption, fixed.I(r.Dx()-2*pad), advance, captionLines, "…") {
		dot := fixed.P(r.Min.X+pad, r.Min.Y+pad+ascent+i*line+(line-px)/2)
		for _, ch := range l {
			f := faceFor(faces, ch)
			if dr, mask, mp, adv, ok := f.Glyph(dot, ch); ok {
				draw.DrawMask(dst, dr.Intersect(r), text, image.Point{}, mask, mp.Add(dr.Intersect(r).Min.Sub(dr.Min)), draw.Over)
				dot.X += adv
			} else {
				a, _ := f.GlyphAdvance(ch)
				dot.X += a
			}
		}
	}
}

// asciiCaption is s in basicfont's characters, printable ASCII: a run of
// others reads "...", a line break a space.
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

// wideRune is a character a line may break before or after with no space:
// Chinese, Japanese and Korean script and their punctuation.
func wideRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef)
}

// wrapCaption breaks s into lines at most width wide as advance measures
// them: at spaces, and between two characters where either is wide (CJK
// has no spaces); a word wider than a line is cut. Past lines, the last
// ends with ellipsis.
func wrapCaption(s string, width fixed.Int26_6, advance func(rune) fixed.Int26_6, lines int, ellipsis string) []string {
	widthOf := func(rs []rune) fixed.Int26_6 {
		var w fixed.Int26_6
		for _, r := range rs {
			w += advance(r)
		}
		return w
	}
	var out []string
	var line []rune
	cut := -1 // the line may break before line[cut]
	for _, r := range strings.Join(strings.Fields(s), " ") {
		if len(line) == 0 && r == ' ' {
			continue
		}
		if len(line) > 0 && (r == ' ' || wideRune(r) || wideRune(line[len(line)-1])) {
			cut = len(line)
		}
		line = append(line, r)
		for len(line) > 1 && widthOf(line) > width {
			at := cut
			if at <= 0 {
				at = len(line) - 1
			}
			out = append(out, strings.TrimRight(string(line[:at]), " "))
			line, cut = []rune(strings.TrimLeft(string(line[at:]), " ")), -1
		}
	}
	if len(line) > 0 {
		out = append(out, string(line))
	}
	if len(out) > lines {
		out = out[:lines]
		last := []rune(out[lines-1])
		for len(last) > 0 && widthOf(append(last[:len(last):len(last)], []rune(ellipsis)...)) > width {
			last = last[:len(last)-1]
		}
		out[lines-1] = strings.TrimRight(string(last), " ") + ellipsis
	}
	return out
}
