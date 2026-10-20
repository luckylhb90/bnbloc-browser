package main

import (
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// captionLit draws caption with c in a bar w wide and counts its light
// pixels.
func captionLit(c *captioner, caption string, w int) int {
	r := image.Rect(0, 0, w, c.barHeight(1))
	dst := image.NewRGBA(r)
	c.draw(dst, r, caption, 1)
	return lit(dst, r)
}

// With a system font found, Chinese and Vietnamese are drawn in it, each
// character in a font that has it, never basicfont's stand-in.
func TestCaptionDrawsChineseAndVietnamese(t *testing.T) {
	c := captions()
	if len(c.fonts) == 0 {
		t.Skip("none of the caption fonts is on this machine: " + strings.Join(captionFontPaths(runtime.GOOS, os.Getenv), ", "))
	}
	px, _, _ := c.metrics(1)
	faces := c.facesAt(px)
	for _, r := range "提交订单Bấm Đặt hàng ở trường ư ơ ộ ữ" {
		if r != ' ' && faceFor(faces, r) == basicfont.Face7x13 {
			t.Fatalf("no caption font has %q", r)
		}
	}
	for _, s := range []string{"提交订单", "Đặt hàng", "3. Clicked \"提交订单\" then Bấm"} {
		if captionLit(c, s, 300) == 0 {
			t.Fatalf("%q drew nothing", s)
		}
	}
	if captionLit(c, "", 300) != 0 {
		t.Fatal("an empty caption drew something")
	}
}

// Lines wrap by measured width: at spaces, and between any two CJK
// characters (no spaces there); past the lines, the last ends in an
// ellipsis.
func TestWrapCaption(t *testing.T) {
	ten := func(rune) fixed.Int26_6 { return fixed.I(10) }
	long := strings.Repeat("提交订单", 6) // 24 characters, 240 px
	got := wrapCaption(long, fixed.I(100), ten, 5, "…")
	if len(got) != 3 || strings.Join(got, "") != long {
		t.Fatalf("a long CJK line = %q", got)
	}
	for _, l := range got {
		if len([]rune(l))*10 > 100 {
			t.Fatalf("line %q is wider than 100", l)
		}
	}
	if got := wrapCaption("Clicked 提交订单 now please", fixed.I(100), ten, 5, "…"); strings.Join(got, "|") != "Clicked 提交|订单 now|please" {
		t.Fatalf("mixed = %q", got)
	}
	if got := wrapCaption("Typed into Amount: 120.50", fixed.I(200), ten, 2, "…"); strings.Join(got, "|") != "Typed into Amount:|120.50" {
		t.Fatalf("words = %q", got)
	}
	got = wrapCaption(long, fixed.I(100), ten, 2, "…")
	if len(got) != 2 || !strings.HasSuffix(got[1], "…") || len([]rune(got[1]))*10 > 100 {
		t.Fatalf("past two lines = %q", got)
	}
}

// With no font file found (or none that parses), captions fall back to
// basicfont: ASCII drawn, other characters as "...".
func TestCaptionFallsBackToBasicfont(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "junk.ttc")
	writeFile(t, junk, "not a font")
	c := newCaptioner([]string{filepath.Join(t.TempDir(), "missing.ttc"), junk})
	if len(c.fonts) != 0 || c.barHeight(1) != 34 || c.barHeight(2) != 68 {
		t.Fatalf("fonts %d, bars %d %d", len(c.fonts), c.barHeight(1), c.barHeight(2))
	}
	if captionLit(c, "1. Clicked Submit", 300) == 0 || captionLit(c, "提交", 300) == 0 {
		t.Fatal("basicfont drew nothing")
	}
	if got := asciiCaption("Clicked \"提交订单\"\nnow"); got != `Clicked "..." now` {
		t.Fatalf("asciiCaption = %q", got)
	}
}
