package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// A run's device (browser_open device): a run shows its pages as a desktop
// browser (the default), a phone or a tablet does, so a site serves the
// layout its phone users see (2026-10-20: a member wanted how-to GIFs of
// their company's app as a phone shows it). The device is the run's
// (runState.Device) and is emulated on each session that drives or shows
// its tab (attachTab): Chrome keeps a session's emulation only while it is
// attached, and when a session that emulates detaches it clears the
// emulation of the page for every session (verified on Chrome 146). So a
// call and its keeper emulate, one at a time, and a look that only asks
// whether the tab is still there never attaches (host.url).
//
// Input stays the mouse: under touch emulation a mouse click still reaches
// a page's click handlers, while a tap (Input.dispatchTouchEvent) never
// answers on a page that is not shown (a background tab). Pictures stay one
// image pixel per CSS pixel (capture): a phone's three device pixels a CSS
// pixel are scaled back, so its pictures are phone-shaped, 390 wide.

// device is one emulated device: its screen in CSS pixels, its pixel
// density, and whether its user agent is a phone's (a tablet's says no
// Mobile, which is how sites tell the two apart).
type device struct {
	Width, Height int
	DPR           float64
	Phone         bool
}

// devices are the emulated devices by name; desktop is none (the window as
// it is).
var devices = map[string]device{
	"phone":  {Width: 390, Height: 844, DPR: 3, Phone: true},
	"tablet": {Width: 820, Height: 1180, DPR: 2},
}

// parseDevice is browser_open's device: "" (the run's own), desktop, phone
// or tablet.
func parseDevice(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if _, ok := devices[name]; ok || name == "" || name == "desktop" {
		return name, nil
	}
	return "", errors.New("device is desktop, phone or tablet")
}

// chromeMajor finds the browser's major version in its user agent.
var chromeMajor = regexp.MustCompile(`Chrome/(\d+)`)

// emulate shows the tab as d on this session: its size, density and
// touch, and the user agent (with its client hints) of Chrome on an
// Android device of that kind, the browser's own engine and version, so a
// site serves what that device's Chrome gets. The agent takes effect on
// the next load.
func (t *tab) emulate(ctx context.Context, d device) error {
	if err := t.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": d.Width, "height": d.Height, "deviceScaleFactor": d.DPR, "mobile": true,
		"screenOrientation": map[string]any{"type": "portraitPrimary", "angle": 0}}, nil); err != nil {
		return err
	}
	if err := t.call(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true, "maxTouchPoints": 5}, nil); err != nil {
		return err
	}
	var v struct {
		UserAgent string `json:"userAgent"`
	}
	_ = t.conn.call(ctx, "", "Browser.getVersion", nil, &v)
	major := "146"
	if m := chromeMajor.FindStringSubmatch(v.UserAgent); m != nil {
		major = m[1]
	}
	mobile := ""
	if d.Phone {
		mobile = "Mobile "
	}
	return t.call(ctx, "Emulation.setUserAgentOverride", map[string]any{
		"userAgent": "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 " + mobile + "Safari/537.36",
		"platform":  "Linux armv8l",
		"userAgentMetadata": map[string]any{
			"brands":          []any{map[string]any{"brand": "Chromium", "version": major}, map[string]any{"brand": "Not.A/Brand", "version": "99"}},
			"platform":        "Android",
			"platformVersion": "10",
			"architecture":    "",
			"model":           "",
			"mobile":          d.Phone,
		},
	}, nil)
}
