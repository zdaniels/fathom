package ui

import (
	"fmt"
	"io"
	"strings"
)

// icebergGlyph is the small ASCII iceberg rendered above the wordmark.
// Echoes the brand logo: faceted peak above a water line, inverted
// underwater mass below. Cobalt + ice-white gradient matches the logo's
// blue-on-navy palette.
var icebergGlyph = []string{
	"        ▲        ",
	"       ╱█╲       ",
	"      ╱███╲      ",
	"  ─────────────  ",
	"      ╲███╱      ",
	"       ╲█╱       ",
	"        ▼        ",
}

// Per-row color for the iceberg. Bright ice-white at the peak, deepening
// to cobalt below the waterline.
var icebergGradient = []string{
	"\x1b[38;5;195m", // ice white
	"\x1b[38;5;117m", // bright cyan
	"\x1b[38;5;75m",  // sky blue
	"\x1b[38;5;253m", // pale grey waterline
	"\x1b[38;5;67m",  // cobalt
	"\x1b[38;5;60m",  // deep cobalt
	"\x1b[38;5;60m",  // deepest
}

// fathomBanner is the Fathom wordmark, rendered with an amber-to-cobalt gradient.
var fathomBanner = []string{
	"███████╗ █████╗ ████████╗██╗  ██╗ ██████╗ ███╗   ███╗",
	"██╔════╝██╔══██╗╚══██╔══╝██║  ██║██╔═══██╗████╗ ████║",
	"█████╗  ███████║   ██║   ███████║██║   ██║██╔████╔██║",
	"██╔══╝  ██╔══██║   ██║   ██╔══██║██║   ██║██║╚██╔╝██║",
	"██║     ██║  ██║   ██║   ██║  ██║╚██████╔╝██║ ╚═╝ ██║",
	"╚═╝     ╚═╝  ╚═╝   ╚═╝   ╚═╝  ╚═╝ ╚═════╝ ╚═╝     ╚═╝",
}

// brandSubtitle is the quiet attribution tucked under the FATHOM wordmark,
// right-aligned to the wordmark's edge and rendered in muted grey.
const brandSubtitle = "local AI agent"

// Per-row ANSI color. Top rows brighter amber, deeper rows shading toward
// cobalt — light hitting the surface, dimming as it sinks. Reads as depth.
var bannerGradient = []string{
	"\x1b[38;5;215m", // bright amber
	"\x1b[38;5;215m",
	"\x1b[38;5;209m", // muted amber
	"\x1b[38;5;67m",  // cobalt
	"\x1b[38;5;67m",
	"\x1b[38;5;60m", // deep cobalt
}

// BannerBlock renders the FATHOM wordmark with the amber→cobalt depth gradient
// and the muted "by fathom" subtitle right-aligned beneath it. Returned as a
// string — one trailing newline per line, no surrounding blank lines, so
// callers own their own spacing. width drives the fallback: terminals too
// narrow for the wordmark get the compact one-line "fathom" mark instead. Color
// is applied only when enabled (NO_COLOR / non-TTY yields plain text).
//
// This is the single source of truth for the wordmark — the chat, join, and
// gateway splashes all render through it, so they can't drift apart again.
func BannerBlock(width int) string {
	var b strings.Builder
	bw := runeLen(fathomBanner[0])
	if width >= bw+4 {
		for i, row := range fathomBanner {
			b.WriteString("  " + wrap(row, bannerGradient[i%len(bannerGradient)]) + "\n")
		}
		// "by fathom" tucked under the wordmark, right edge aligned.
		pad := 2 + bw - runeLen(brandSubtitle)
		if pad < 2 {
			pad = 2
		}
		b.WriteString(strings.Repeat(" ", pad) + Mute(brandSubtitle) + "\n")
	} else {
		b.WriteString("  " + Brand(Bold("fathom")) + "\n")
	}
	return b.String()
}

// Welcome prints the startup splash for the gateway (`fathom start`): iceberg
// glyph, FATHOM wordmark + attribution, tagline, then session info. Single
// render — not repainted.
//
//	        ▲
//	       ╱█╲
//	      ╱███╲
//	  ─────────────
//	      ╲███╱
//	       ╲█╱
//	        ▼
//
//	████████╗ █████╗ ███████╗
//	╚══██╔══╝██╔══██╗╚══███╔╝
//	   ██║   ███████║  ███╔╝
//	   ██║   ██╔══██║ ███╔╝
//	   ██║   ██║  ██║███████╗
//	   ╚═╝   ╚═╝  ╚═╝╚══════╝
//	         by fathom
//
//	Local agent · github.com/zdaniels/fathom
func Welcome(out io.Writer, agent, workspace string) {
	fmt.Fprintln(out)
	bw := runeLen(fathomBanner[0])
	if enabled && Width() >= bw+4 {
		// Iceberg glyph centered above the wordmark. Pad on the left so
		// it lines up with the wordmark's center rather than the indent.
		icebergWidth := runeLen(icebergGlyph[0])
		pad := strings.Repeat(" ", 2+(bw-icebergWidth)/2)
		for i, row := range icebergGlyph {
			color := icebergGradient[i%len(icebergGradient)]
			fmt.Fprintln(out, pad+color+row+reset)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprint(out, BannerBlock(Width()))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+Body("Local agent")+"  "+Mute("·")+"  "+Mute("github.com/zdaniels/fathom"))
	fmt.Fprintln(out)
	KV(out, "agent", agent)
	if workspace != "" {
		KV(out, "workspace", workspace)
	}
	KV(out, "commands", "/quit  /help")
	fmt.Fprintln(out)
}
