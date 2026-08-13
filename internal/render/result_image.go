// Package render generates presentation assets (currently PNG images) for
// Concept2 rowing/skiing/biking results. It has no DB or service
// dependencies — callers (e.g. internal/app) own delivery.
package render

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/fogleman/gg"

	"github.com/softsrv/rowbot/internal/concept2"
)

// canvasWidth is the fixed pixel width of every rendered result image.
// Height is computed dynamically from content (header, hero stats, and —
// if present — the splits/intervals table row count, plus the footer).
const canvasWidth = 650

const (
	marginX = 32

	headerHeight = 100

	// heroStatsHeight is the height of the band of big pull-out numbers
	// (time, avg pace, avg watts, calories) drawn between the header and
	// the splits table.
	heroStatsHeight = 84

	// tableTopPad is the breathing room between the hero stats band and
	// the table's column-header row.
	tableTopPad       = 20
	tableHeaderHeight = 34
	tableRowHeight    = 30
	tableBottomPad    = 24

	// restRowHeight is the height of the annotation row drawn after an
	// interval piece that reports a rest time — shorter than a full piece
	// row since it carries a single muted value rather than the whole
	// column set.
	restRowHeight = 24

	// footerHeight is the height of the closing brand/date strip at the
	// bottom of the card.
	footerHeight = 40

	// cardRadius is the corner radius applied to the whole image, giving
	// it a card silhouette (the four corners render fully transparent)
	// rather than a hard rectangle.
	cardRadius = 16

	// maxTableRows caps the number of split/interval rows drawn before
	// falling back to a "+N more" line, keeping canvas height bounded
	// regardless of workout length.
	maxTableRows = 22
)

// The "Regatta Ledger" palette: a deep navy ground with parchment ink for
// nearly everything, so brass reads as a genuine highlight rather than
// competing with a second accent color. The rule colors below aren't
// arbitrary greys — they're parchment blended down to near-transparency
// over the navy paper color (the design's dark-mode ".rule"/".rule-soft"/
// ".ink-faint" washes), which is what keeps hairlines and dividers reading
// as muted rather than adding a second, competing highlight color.
var (
	colorPaper    = rgb{20, 34, 56}
	colorInk      = rgb{239, 231, 210}
	colorInkSoft  = rgb{156, 156, 151}
	colorInkFaint = rgb{103, 109, 115}
	colorRule     = rgb{64, 73, 87}
	colorRuleSoft = rgb{42, 54, 71}
	colorBrass    = rgb{204, 159, 82}
)

type rgb struct {
	r, g, b int
}

// RenderResultPNG draws a Concept2 workout result as a PNG "card" styled
// like an engraved boathouse scoreboard plaque: an ivory ground, a masthead
// with a tracked eyebrow and a serif small-caps sport label, a row of hero
// stats pulled out of the summary (time, avg pace, avg watts, calories), a
// ruled table whose remaining rows — if the workout has splits or intervals
// — are the per-piece breakdown, and a closing brand/date footer. The
// single fastest piece (by Pace()) is called out in bold text, its PACE
// underline brass like every other row. The whole card has rounded corners
// (transparent PNG corners), a thin outer border, and an inset rule
// mimicking a double-ruled plaque edge. It returns the PNG-encoded bytes.
func RenderResultPNG(result concept2.Result) ([]byte, error) {
	title, _ := sportLabelAndColor(result.Type)
	subtitle := result.WorkoutType
	if subtitle != "" {
		subtitle = humanizeWorkoutType(subtitle) + " - " + workoutSubtitleMetric(result)
	}

	pieces := result.Workout.Pieces()

	// The summary row is a synthetic Split built from the result's own
	// totals/averages. Its Pace()/Watts() use the exact same formula as
	// Split's, computed from these same Time/Distance fields, so this
	// renders identically to calling Result.AveragePace()/Watts() directly
	// — letting the summary row (and the hero stats band) reuse every
	// value function rather than needing a parallel set for
	// concept2.Result.
	summary := concept2.Split{
		Time:       result.Time,
		Distance:   result.Distance,
		StrokeRate: result.StrokeRate,
		Calories:   result.Calories,
		HeartRate:  result.HeartRate,
	}

	hasHR := result.HeartRate != nil
	for _, p := range pieces {
		if p.HeartRate != nil {
			hasHR = true
			break
		}
	}

	hasPieces := len(pieces) > 0

	rowCount := len(pieces)
	truncated := false
	if rowCount > maxTableRows {
		rowCount = maxTableRows
		truncated = true
	}

	// header + hero stats + footer, plus — only when there are splits or
	// intervals to show — breathing room, the column-header row, the table
	// body (per-piece rows, a rest-time annotation row after any piece that
	// reports one, and a "+N more" line if truncated), and bottom padding.
	// A result with no pieces (e.g. JustRow) has no table at all, so its
	// card ends right after the hero stats band.
	height := headerHeight + heroStatsHeight
	if hasPieces {
		height += tableTopPad + tableHeaderHeight + tableBodyHeight(pieces, rowCount, truncated)
		height += tableBottomPad
	}
	height += footerHeight

	dc := gg.NewContext(canvasWidth, height)

	// Clip everything that follows to a rounded rect. The image is
	// initialized fully transparent, so the four corners outside that
	// rect are left untouched — the PNG renders as a rounded card however
	// it's composited downstream, rather than a hard white rectangle.
	dc.DrawRoundedRectangle(0, 0, float64(canvasWidth), float64(height), cardRadius)
	dc.Clip()

	setColor(dc, colorPaper)
	dc.DrawRectangle(0, 0, float64(canvasWidth), float64(height))
	dc.Fill()

	// A thin inset rule just inside the card edge, echoing the
	// double-ruled border of an engraved plaque.
	const insetMargin = 10.0
	setColor(dc, colorRuleSoft)
	dc.DrawRectangle(insetMargin, insetMargin, float64(canvasWidth)-2*insetMargin, float64(height)-2*insetMargin)
	dc.SetLineWidth(1)
	dc.Stroke()

	drawHeader(dc, title, subtitle)
	drawHeroStats(dc, float64(headerHeight), []heroStat{
		{"Time", formatDuration(summary.Time)},
		{"Avg pace", paceString(summary)},
		{"Avg watts", wattsString(summary)},
		{"Calories", strconv.Itoa(summary.Calories)},
	})

	if hasPieces {
		tableTop := float64(headerHeight+heroStatsHeight) + tableTopPad
		drawSplitsTable(dc, pieces, hasHR, rowCount, truncated, tableTop)
	}

	drawFooter(dc, float64(height-footerHeight), formatResultDate(result.Date))

	// The border is drawn outside the clip so it isn't itself clipped away
	// at the very edge.
	dc.ResetClip()
	setColor(dc, colorRule)
	dc.DrawRoundedRectangle(0.75, 0.75, float64(canvasWidth)-1.5, float64(height)-1.5, cardRadius-1)
	dc.SetLineWidth(1.5)
	dc.Stroke()

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, fmt.Errorf("render result png: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// workoutSubtitleMetric returns the single headline metric appended after
// the workout type in the header subtitle: total time for time-based
// workout types (those whose WorkoutType contains "Time", e.g.
// "FixedTimeSplits"), otherwise total distance — which also covers types
// that don't fit Concept2's Fixed<Distance|Time><Splits|Interval> naming
// convention (e.g. "JustRow", "VariableInterval"), where distance is the
// more meaningful default.
func workoutSubtitleMetric(result concept2.Result) string {
	if strings.Contains(result.WorkoutType, "Time") {
		return formatDuration(result.Time)
	}
	return formatDistance(result.Distance)
}

// humanizeWorkoutType inserts a space before each interior capital letter
// of a Concept2 WorkoutType value (PascalCase, e.g. "FixedDistanceSplits"),
// so it displays as "Fixed Distance Splits". Values with no interior
// capital letters (e.g. "unknown", one of Concept2's documented
// workout_type values) pass through unchanged — there's nothing to split.
func humanizeWorkoutType(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// maxHeaderTextWidth is the pixel budget for the header band's title/
// subtitle text: full width minus the left and right margins.
const maxHeaderTextWidth = canvasWidth - 2*marginX

// titleSmallCapsRatio sizes the "lower-case" glyphs in the header's faux
// small-caps title relative to the "upper-case" ones (see
// drawSmallCapsTracked) — gg can't apply an OpenType smcp feature, so a
// second, smaller pass at the same baseline is what stands in for it.
const titleSmallCapsRatio = 0.74

// drawHeader draws the plaque's masthead: a small tracked brass eyebrow,
// the sport label set as faux small caps in the vendored Crimson Text Bold
// (the nearest open serif match, given gg's plain glyph rendering, to the
// Georgia small-caps the design calls for), and — if present — a muted
// monospace subtitle line below.
func drawHeader(dc *gg.Context, title, subtitle string) {
	textX := float64(marginX)

	eyebrowFace := loadFace(fontMonoBold, 10)
	dc.SetFontFace(eyebrowFace)
	setColor(dc, colorBrass)
	drawTracked(dc, "CONCEPT2 RESULT", textX, float64(headerHeight)*0.26, 0.5, 2.2)

	titleSize := 26.0
	if smallCapsTrackedWidth(dc, title, 1.2, titleSize) > maxHeaderTextWidth {
		// Text is too wide for the default size — try a smaller size
		// before resorting to a plain truncated fallback.
		titleSize = 22.0
	}
	setColor(dc, colorInk)
	if smallCapsTrackedWidth(dc, title, 1.2, titleSize) > maxHeaderTextWidth {
		dc.SetFontFace(loadFace(fontSerifBold, titleSize))
		display := truncateToWidth(dc, strings.ToUpper(title), maxHeaderTextWidth)
		drawTracked(dc, display, textX, float64(headerHeight)*0.56, 0.5, 1.2)
	} else if subtitle == "" {
		drawSmallCapsTracked(dc, title, textX, float64(headerHeight)*0.62, 1.2, titleSize)
	} else {
		drawSmallCapsTracked(dc, title, textX, float64(headerHeight)*0.56, 1.2, titleSize)
	}

	if subtitle == "" {
		return
	}

	subFace := loadFace(fontMono, 13)
	dc.SetFontFace(subFace)
	setColor(dc, colorInkSoft)
	displaySubtitle := subtitle
	if w, _ := dc.MeasureString(displaySubtitle); w > maxHeaderTextWidth {
		displaySubtitle = truncateToWidth(dc, displaySubtitle, maxHeaderTextWidth)
	}
	dc.DrawStringAnchored(displaySubtitle, textX, float64(headerHeight)*0.84, 0, 0.5)
}

// truncateToWidth shortens s (using the font face currently set on dc) to
// fit within maxWidth pixels, appending an ellipsis. s is assumed to be
// ASCII (true for the sport labels and Concept2 WorkoutType values this is
// used with), so byte-wise slicing is safe.
func truncateToWidth(dc *gg.Context, s string, maxWidth float64) string {
	const ellipsis = "…"
	for i := len(s); i > 0; i-- {
		candidate := s[:i] + ellipsis
		if w, _ := dc.MeasureString(candidate); w <= maxWidth {
			return candidate
		}
	}
	return ellipsis
}

// drawTracked draws s left-to-right starting at (x, y), inserting extra
// letter-spacing (tracking, in pixels) after each rune; ay is the vertical
// anchor (0 top .. 1 bottom, matching gg.DrawStringAnchored's ay). gg has no
// built-in letter-spacing, and a touch of tracking on upper-case labels is
// what makes the masthead and table headers read as engraved rather than
// merely capitalized.
func drawTracked(dc *gg.Context, s string, x, y, ay, tracking float64) {
	cx := x
	for _, r := range s {
		ch := string(r)
		dc.DrawStringAnchored(ch, cx, y, 0, ay)
		w, _ := dc.MeasureString(ch)
		cx += w + tracking
	}
}

// trackedWidth returns the total pixel width drawTracked would consume for
// s at the given tracking, using the font face currently set on dc.
func trackedWidth(dc *gg.Context, s string, tracking float64) float64 {
	w := 0.0
	n := 0
	for _, r := range s {
		cw, _ := dc.MeasureString(string(r))
		w += cw
		n++
	}
	if n > 1 {
		w += tracking * float64(n-1)
	}
	return w
}

// drawTrackedRight draws s tracked (see drawTracked) so that it ends at
// rightX — the right-aligned equivalent used for the table's column
// headers.
func drawTrackedRight(dc *gg.Context, s string, rightX, y, ay, tracking float64) {
	drawTracked(dc, s, rightX-trackedWidth(dc, s, tracking), y, ay, tracking)
}

// drawTrackedCentered draws s tracked (see drawTracked) centered on
// centerX — used for the hero stats band's caption labels.
func drawTrackedCentered(dc *gg.Context, s string, centerX, y, ay, tracking float64) {
	drawTracked(dc, s, centerX-trackedWidth(dc, s, tracking)/2, y, ay, tracking)
}

// drawSmallCapsTracked draws s as faux small caps at (x, y) (vertically
// centered, left-aligned): runes that are already upper-case (or aren't
// letters at all) are drawn in fontSerifBold at fullSize; lower-case runes
// are upper-cased and drawn at fullSize*titleSmallCapsRatio. Both passes
// share the same baseline anchor, so e.g. "BikeErg" reads as "BIKE" in full
// caps with a smaller "ERG" beside it, the classic small-caps treatment for
// a mixed-case brand name.
func drawSmallCapsTracked(dc *gg.Context, s string, x, y, tracking, fullSize float64) {
	fullFace := loadFace(fontSerifBold, fullSize)
	smallFace := loadFace(fontSerifBold, fullSize*titleSmallCapsRatio)

	cx := x
	for _, r := range s {
		face := fullFace
		ch := r
		if unicode.IsLower(r) {
			face = smallFace
			ch = unicode.ToUpper(r)
		}
		dc.SetFontFace(face)
		str := string(ch)
		dc.DrawStringAnchored(str, cx, y, 0, 0.5)
		w, _ := dc.MeasureString(str)
		cx += w + tracking
	}
}

// smallCapsTrackedWidth returns the total width drawSmallCapsTracked would
// consume for s, for the overflow check that picks the title's font size.
func smallCapsTrackedWidth(dc *gg.Context, s string, tracking, fullSize float64) float64 {
	fullFace := loadFace(fontSerifBold, fullSize)
	smallFace := loadFace(fontSerifBold, fullSize*titleSmallCapsRatio)

	w := 0.0
	n := 0
	for _, r := range s {
		face := fullFace
		ch := r
		if unicode.IsLower(r) {
			face = smallFace
			ch = unicode.ToUpper(r)
		}
		dc.SetFontFace(face)
		cw, _ := dc.MeasureString(string(ch))
		w += cw
		n++
	}
	if n > 1 {
		w += tracking * float64(n-1)
	}
	return w
}

// heroStat is one label/value pair drawn in the hero stats band.
type heroStat struct {
	label string
	value string
}

// drawHeroStats draws a row of evenly-spaced stat gauges (muted tracked
// monospace caption above, large bold monospace value below), separated by
// hairline dividers and bounded top and bottom by rules — a ruled ledger
// section rather than a shaded band. This pulls the headline numbers (time,
// avg pace, avg watts, calories) out of the table below so they're readable
// at a glance instead of buried in a bolded row of six columns.
func drawHeroStats(dc *gg.Context, top float64, stats []heroStat) {
	if len(stats) == 0 {
		return
	}

	contentWidth := float64(canvasWidth - 2*marginX)
	cardWidth := contentWidth / float64(len(stats))

	setColor(dc, colorRule)
	dc.DrawLine(float64(marginX), top, float64(canvasWidth-marginX), top)
	dc.SetLineWidth(1)
	dc.Stroke()

	labelFace := loadFace(fontMonoBold, 10)
	valueFace := loadFace(fontMonoBold, 22)

	for i, s := range stats {
		cardX := float64(marginX) + float64(i)*cardWidth
		if i > 0 {
			setColor(dc, colorRule)
			dc.DrawLine(cardX, top+14, cardX, top+heroStatsHeight-14)
			dc.SetLineWidth(1)
			dc.Stroke()
		}
		centerX := cardX + cardWidth/2

		dc.SetFontFace(labelFace)
		setColor(dc, colorInkFaint)
		drawTrackedCentered(dc, strings.ToUpper(s.label), centerX, top+28, 0.5, 1.3)

		dc.SetFontFace(valueFace)
		setColor(dc, colorInk)
		dc.DrawStringAnchored(s.value, centerX, top+56, 0.5, 0.5)
	}

	setColor(dc, colorRule)
	dc.DrawLine(float64(marginX), top+heroStatsHeight, float64(canvasWidth-marginX), top+heroStatsHeight)
	dc.SetLineWidth(1)
	dc.Stroke()
}

// paceString formats a split's pace the way the table and hero stats band
// both want it: "m:ss.t" per 500m, or an em dash when the split has no
// distance to compute a pace from.
func paceString(p concept2.Split) string {
	if pace := p.Pace(); pace > 0 {
		return formatDuration(pace)
	}
	return "—"
}

// wattsString formats a split's watts, or an em dash when unavailable.
func wattsString(p concept2.Split) string {
	if watts := p.Watts(); watts > 0 {
		return strconv.Itoa(int(math.Round(watts)))
	}
	return "—"
}

// splitsColumn describes one column of the results table: its header label,
// a relative width weight (used to divide the fixed content width), and a
// per-row value function. Per-piece rows are concept2.Split values (see
// RenderResultPNG), so one column set renders all of them.
type splitsColumn struct {
	label  string
	weight float64
	value  func(p concept2.Split) string
}

// drawSplitsTable draws the column-header row (starting at headerTop), then
// up to rowCount per-piece rows (plus a "+N more" line if truncated).
// Callers must not invoke this with an empty pieces slice (e.g. a JustRow
// result with no split/interval breakdown) — RenderResultPNG skips the
// table, including its column headings, entirely in that case. Rows are
// separated by hairline rules rather than zebra shading; the single fastest
// piece (by Pace()) gets bold text, same as every other row's brass PACE
// underline.
func drawSplitsTable(dc *gg.Context, pieces []concept2.Split, hasHR bool, rowCount int, truncated bool, headerTop float64) {
	columns := []splitsColumn{
		{"TIME", 1.05, func(p concept2.Split) string { return formatDuration(p.Time) }},
		{"DIST", 1.05, func(p concept2.Split) string { return formatDistance(p.Distance) }},
		{"PACE", 1.05, paceString},
		{"WATTS", 0.85, wattsString},
		{"CAL", 0.8, func(p concept2.Split) string { return strconv.Itoa(p.Calories) }},
		{"S/M", 0.8, func(p concept2.Split) string { return strconv.Itoa(p.StrokeRate) }},
	}
	if hasHR {
		columns = append(columns, splitsColumn{"HR", 0.85, func(p concept2.Split) string {
			if p.HeartRate != nil {
				if v := p.HeartRate.Value(); v != 0 {
					return strconv.Itoa(v)
				}
			}
			return "—"
		}})
	}

	paceColIdx := -1
	timeColIdx := -1
	for i, c := range columns {
		switch c.label {
		case "PACE":
			paceColIdx = i
		case "TIME":
			timeColIdx = i
		}
	}

	contentWidth := float64(canvasWidth - 2*marginX)
	totalWeight := 0.0
	for _, c := range columns {
		totalWeight += c.weight
	}

	colWidths := make([]float64, len(columns))
	colX := make([]float64, len(columns))
	x := float64(marginX)
	for i, c := range columns {
		colWidths[i] = c.weight / totalWeight * contentWidth
		colX[i] = x
		x += colWidths[i]
	}

	headerFace := loadFace(fontMonoBold, 10)
	dc.SetFontFace(headerFace)
	setColor(dc, colorBrass)
	headerCenterY := headerTop + tableHeaderHeight/2
	for i, c := range columns {
		cx := colX[i] + colWidths[i] - 8
		drawTrackedRight(dc, c.label, cx, headerCenterY, 0.5, 1.2)
	}

	setColor(dc, colorBrass)
	dc.DrawLine(float64(marginX), headerTop+tableHeaderHeight, float64(canvasWidth-marginX), headerTop+tableHeaderHeight)
	dc.SetLineWidth(1)
	dc.Stroke()

	rowFace := loadFace(fontMono, 14)
	rowFaceBold := loadFace(fontMonoBold, 14)

	rowsTop := headerTop + tableHeaderHeight

	// Pace range across the drawn rows, used to size each row's pace
	// underline and to identify the single fastest piece. minPace tracks
	// the fastest (smallest, since pace is time-per-500m) split; maxPace
	// the slowest.
	var minPace, maxPace int64
	if paceColIdx >= 0 {
		for r := 0; r < rowCount; r++ {
			pace := pieces[r].Pace()
			if pace <= 0 {
				continue
			}
			if minPace == 0 || pace < minPace {
				minPace = pace
			}
			if pace > maxPace {
				maxPace = pace
			}
		}
	}

	cursorY := rowsTop
	for r := 0; r < rowCount; r++ {
		p := pieces[r]
		rowY := cursorY
		rowCenterY := rowY + tableRowHeight/2

		if r > 0 {
			setColor(dc, colorRuleSoft)
			dc.DrawLine(float64(marginX), rowY, float64(canvasWidth-marginX), rowY)
			dc.SetLineWidth(1)
			dc.Stroke()
		}

		pace := p.Pace()
		frac := 0.0
		isFastest := false
		if paceColIdx >= 0 && pace > 0 {
			if maxPace > minPace {
				frac = float64(maxPace-pace) / float64(maxPace-minPace)
				isFastest = pace == minPace
			} else {
				// Every drawn split has the same pace — there's no single
				// standout piece to flag.
				frac = 1.0
			}
		}

		if isFastest {
			dc.SetFontFace(rowFaceBold)
		} else {
			dc.SetFontFace(rowFace)
		}

		for i, c := range columns {
			val := c.value(p)
			cx := colX[i] + colWidths[i] - 8

			setColor(dc, colorInk)
			dc.DrawStringAnchored(val, cx, rowCenterY, 1, 0.5)

			if i == paceColIdx && pace > 0 {
				textW, _ := dc.MeasureString(val)
				drawPaceUnderline(dc, cx, textW, rowCenterY, frac)
			}
		}

		cursorY += tableRowHeight

		if p.RestTime > 0 {
			setColor(dc, colorRuleSoft)
			dc.DrawLine(float64(marginX), cursorY, float64(canvasWidth-marginX), cursorY)
			dc.SetLineWidth(1)
			dc.Stroke()

			restCenterY := cursorY + restRowHeight/2
			timeColRightX := colX[timeColIdx] + colWidths[timeColIdx] - 8
			drawRestRow(dc, timeColRightX, restCenterY, p.RestTime)

			cursorY += restRowHeight
		}
	}

	if truncated {
		moreY := cursorY
		setColor(dc, colorInkSoft)
		moreFace := loadFace(fontMono, 13)
		dc.SetFontFace(moreFace)
		more := fmt.Sprintf("+ %d more", len(pieces)-rowCount)
		dc.DrawStringAnchored(more, float64(marginX), moreY+tableRowHeight/2, 0, 0.5)
	}
}

// tableBodyHeight returns the pixel height of the splits table body: one
// tableRowHeight per drawn piece (rowCount of them), plus one
// restRowHeight for each of those pieces that reports a rest time (see
// drawRestRow), plus one more tableRowHeight if the workout has more pieces
// than fit (the "+N more" line). RenderResultPNG uses this to size the
// canvas before drawSplitsTable lays out the same rows.
func tableBodyHeight(pieces []concept2.Split, rowCount int, truncated bool) int {
	height := rowCount * tableRowHeight
	for _, p := range pieces[:rowCount] {
		if p.RestTime > 0 {
			height += restRowHeight
		}
	}
	if truncated {
		height += tableRowHeight
	}
	return height
}

// drawRestRow draws a single-line annotation for an interval's rest time,
// right-aligned at the TIME column's right edge like the piece row above
// it, prefixed with "r" (e.g. "r3:00.0") and set smaller and muted so it
// reads as a note rather than another scored piece.
func drawRestRow(dc *gg.Context, timeColRightX, centerY float64, restTenths int64) {
	dc.SetFontFace(loadFace(fontMono, 12))
	setColor(dc, colorInkSoft)
	dc.DrawStringAnchored("r"+formatDuration(restTenths), timeColRightX, centerY, 1, 0.5)
}

// drawPaceUnderline draws a brass underline beneath a row's PACE value,
// right-aligned under the text's own right edge (rightX) and sized by frac
// (0 slowest drawn split .. 1 fastest): a full-width underline under the
// fastest split's pace, tapering to a shorter stub for the slowest — so a
// glance down the column still shows the pacing trend, without a separate
// column of its own competing for width.
func drawPaceUnderline(dc *gg.Context, rightX, textW, textCenterY, frac float64) {
	const minFillFrac = 0.35
	const lineH = 2.0
	const gapBelowText = 11.0

	lineW := textW * (minFillFrac + frac*(1-minFillFrac))
	lineY := textCenterY + gapBelowText

	setColor(dc, colorBrass)
	dc.DrawRectangle(rightX-lineW, lineY, lineW, lineH)
	dc.Fill()
}

// drawFooter draws the closing brand/date strip: a hairline divider, the
// bold "RowBot" brand mark on the left, and the formatted result date (if
// any) on the right.
func drawFooter(dc *gg.Context, top float64, dateLabel string) {
	setColor(dc, colorRule)
	dc.DrawLine(float64(marginX), top, float64(canvasWidth-marginX), top)
	dc.SetLineWidth(1)
	dc.Stroke()

	centerY := top + footerHeight/2

	dc.SetFontFace(loadFace(fontMonoBold, 12))
	setColor(dc, colorInk)
	dc.DrawStringAnchored("RowBot", float64(marginX), centerY, 0, 0.5)

	if dateLabel != "" {
		dc.SetFontFace(loadFace(fontMono, 12))
		setColor(dc, colorInkFaint)
		dc.DrawStringAnchored(dateLabel, float64(canvasWidth-marginX), centerY, 1, 0.5)
	}
}

// formatResultDate parses a Concept2 result's Date field (observed as
// "2006-01-02 15:04:05", but RFC3339 and a bare date are also accepted
// defensively) into a short human-readable form, e.g. "Jul 24, 2026".
// Returns "" if raw is empty or doesn't match any known layout, so callers
// can simply skip drawing it.
func formatResultDate(raw string) string {
	if raw == "" {
		return ""
	}
	layouts := []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Format("Jan 2, 2006")
		}
	}
	return ""
}

func setColor(dc *gg.Context, c rgb) {
	dc.SetRGB255(c.r, c.g, c.b)
}

// sportLabelAndColor returns the short display label and brand color (as a
// packed 0xRRGGBB int) for a given Concept2 activity type. It covers all 10
// values Concept2's Add Result API documents for concept2.Result.Type
// (rower, dynamic, slides, paddle, water, snow, rollerski, bike, multierg)
// plus the empty string, grouped into a small brand palette by activity
// family so the rendered card reads as one coherent design rather than ten
// unrelated hues:
//
//   - Rowing family (default blue 0x4A90D9): "rower", "dynamic", "slides",
//     "water". These are rowing-erg variants or outdoor-rowing log entries —
//     Concept2's own docs group them together too, e.g. the weight_class
//     field is documented as "Required if type is rower, dynamic or
//     slides".
//   - Ski family (light blue 0x5B9BD5): "skierg", "snow", "rollerski" —
//     indoor SkiErg plus its outdoor skiing analogues.
//   - Bike (orange 0xED7D31): "bike". Note the API's documented value is
//     "bike", not "bikeerg" — an earlier version of this switch checked for
//     "bikeerg", which never matched a real API response and silently fell
//     through to the generic default branch instead of showing the Cycling
//     branding.
//   - Paddle (teal 0x2FA89A) and MultiErg (purple 0x8B5FBF) are distinct
//     enough product categories to get their own accent colors rather than
//     being folded into another family.
//
// The color is currently unused by RenderResultPNG — the "Regatta Ledger"
// design deliberately stays to a fixed navy/brass palette rather than
// tinting each card by sport — but the mapping is kept (and tested) since
// it's part of this function's documented contract and may still be
// useful to callers outside this package (e.g. a Discord embed's side
// color).
//
// Any other non-empty, undocumented type falls back to the type itself with
// its first letter uppercased, using the default rowing blue; an empty type
// falls back to the generic label "Result".
func sportLabelAndColor(sportType string) (string, int) {
	const (
		defaultColor = 0x4A90D9
		skiColor     = 0x5B9BD5
		bikeColor    = 0xED7D31
		paddleColor  = 0x2FA89A
		multiColor   = 0x8B5FBF
	)

	switch sportType {
	case "rower":
		return "Rowing", defaultColor
	case "dynamic":
		return "Dynamic Rower", defaultColor
	case "slides":
		return "Slides", defaultColor
	case "water":
		return "Water", defaultColor
	case "skierg":
		return "Skiing", skiColor
	case "snow":
		return "Snow", skiColor
	case "rollerski":
		return "Roller Ski", skiColor
	case "bike":
		return "Cycling", bikeColor
	case "paddle":
		return "Paddling", paddleColor
	case "multierg":
		return "MultiErg", multiColor
	case "":
		return "Result", defaultColor
	default:
		return titleCase(sportType), defaultColor
	}
}

// titleCase uppercases the first byte of s and lowercases the rest. It's
// ASCII-only, which is sufficient for the Concept2 activity type strings.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// formatDuration converts tenths of a second to "m:ss.t" format.
// For example, 13477 → "22:27.7".
func formatDuration(tenths int64) string {
	if tenths <= 0 {
		return "0:00.0"
	}
	t := tenths % 10
	totalSeconds := tenths / 10
	seconds := totalSeconds % 60
	minutes := totalSeconds / 60
	return fmt.Sprintf("%d:%02d.%d", minutes, seconds, t)
}

// formatDistance formats a distance in metres as a human-readable string.
// Values under 10 km are shown as whole metres with a thousands separator;
// values 10 km and over are shown in kilometres with one decimal place.
func formatDistance(metres float64) string {
	if metres >= 10000 {
		return fmt.Sprintf("%.1f km", metres/1000)
	}
	// Format with comma thousands separator for values >= 1000.
	m := int64(metres)
	if m >= 1000 {
		thousands := m / 1000
		remainder := m % 1000
		return fmt.Sprintf("%d,%03d m", thousands, remainder)
	}
	return fmt.Sprintf("%d m", m)
}
