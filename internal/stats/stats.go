// Package stats computes aggregate counts for the Statistics tab.
//
// Source of truth is the SQLite sightings table — one row per saved
// picture, surviving the 30-day file retention sweep. The detection
// log (separate sqlite db) is per-tick and would massively
// double-count, so it isn't used here.
package stats

import (
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linda/linda_cam/internal/sightings"
)

const yearTrendSpecies = 5

// Bundle is everything the Statistics tab needs in one round trip.
type Bundle struct {
	Totals      Totals         `json:"totals"`
	Top7d       []SpeciesCount `json:"top_7d"`
	YearTrend   YearTrend      `json:"year_trend"`
	HourOfDay   []int          `json:"hour_of_day"` // length 24
	GeneratedAt time.Time      `json:"generated_at"`
}

type Totals struct {
	Pictures       int   `json:"pictures"`
	SightingsToday int   `json:"sightings_today"`
	Sightings7d    int   `json:"sightings_7d"`
	Species30d     int   `json:"species_30d"`
	DiskBytes      int64 `json:"disk_bytes"` // total bytes on disk under the pictures dir (-1 = unknown)
}

type SpeciesCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// YearTrend is the top-N species over the last 12 months bucketed by
// ISO week. Series is species × weeks; both axes are aligned to
// Species and Weeks respectively.
type YearTrend struct {
	Species []string `json:"species"`
	Weeks   []string `json:"weeks"` // YYYY-MM-DD of each week's Monday, oldest first
	Series  [][]int  `json:"series"`
}

// Compute pulls aggregate counts from the sightings store.
//
// Totals come from cheap COUNT queries; the per-week / per-hour
// breakdowns require the actual rows over the 12-month window, which
// the store returns and we bucket in Go. picturesDir is shelled out
// to `du -sb` for the disk-usage KPI.
func Compute(store *sightings.Store, picturesDir string) (Bundle, error) {
	now := time.Now()
	bundle := Bundle{
		HourOfDay:   make([]int, 24),
		GeneratedAt: now,
		Top7d:       []SpeciesCount{},
		YearTrend:   YearTrend{Species: []string{}, Weeks: []string{}, Series: [][]int{}},
	}

	totals, err := store.Totals(now)
	if err != nil {
		return bundle, err
	}
	bundle.Totals = Totals{
		Pictures:       totals.Pictures,
		SightingsToday: totals.SightingsToday,
		Sightings7d:    totals.Sightings7d,
		Species30d:     totals.Species30d,
		DiskBytes:      diskBytesOf(picturesDir),
	}

	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cutoff7d := startOfToday.AddDate(0, 0, -6)
	cutoff12mo := startOfToday.AddDate(-1, 0, 0)

	rows, err := store.StatRowsSince(cutoff12mo)
	if err != nil {
		return bundle, err
	}

	count7d := map[string]int{}
	count12mo := map[string]int{}
	for _, r := range rows {
		if r.When.Hour() >= 0 && r.When.Hour() < 24 {
			bundle.HourOfDay[r.When.Hour()]++
		}
		if !r.When.Before(cutoff7d) {
			count7d[r.Species]++
		}
		count12mo[r.Species]++
	}
	bundle.Top7d = topN(count7d, 10)

	topSpecies := topN(count12mo, yearTrendSpecies)
	speciesNames := make([]string, len(topSpecies))
	speciesIdx := map[string]int{}
	for i, s := range topSpecies {
		speciesNames[i] = s.Name
		speciesIdx[s.Name] = i
	}
	weekStarts := buildWeekStarts(cutoff12mo, startOfToday)
	weekKeys := make([]string, len(weekStarts))
	weekIdx := map[string]int{}
	for i, t := range weekStarts {
		key := t.Format("2006-01-02")
		weekKeys[i] = key
		weekIdx[key] = i
	}
	series := make([][]int, len(speciesNames))
	for i := range series {
		series[i] = make([]int, len(weekKeys))
	}
	for _, r := range rows {
		si, ok := speciesIdx[r.Species]
		if !ok {
			continue
		}
		wk := weekStartOf(r.When).Format("2006-01-02")
		wi, ok := weekIdx[wk]
		if !ok {
			continue
		}
		series[si][wi]++
	}
	bundle.YearTrend = YearTrend{
		Species: speciesNames,
		Weeks:   weekKeys,
		Series:  series,
	}
	return bundle, nil
}

func topN(counts map[string]int, n int) []SpeciesCount {
	out := make([]SpeciesCount, 0, len(counts))
	for name, c := range counts {
		out = append(out, SpeciesCount{Name: name, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// weekStartOf returns the Monday at 00:00 in the local timezone of
// the week containing t. ISO weeks (Mon-start) keep year boundaries
// stable.
func weekStartOf(t time.Time) time.Time {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, -(wd - 1))
}

// buildWeekStarts returns a chronological list of Monday-aligned week
// starts covering [from, throughInclusive].
func buildWeekStarts(from, throughInclusive time.Time) []time.Time {
	first := weekStartOf(from)
	last := weekStartOf(throughInclusive)
	var out []time.Time
	for w := first; !w.After(last); w = w.AddDate(0, 0, 7) {
		out = append(out, w)
	}
	return out
}

// diskBytesOf shells out to `du -sb` for the pictures dir and returns
// the byte count. Returns -1 on any failure (missing dir, non-GNU du,
// parse error) — the frontend renders -1 as "unknown".
func diskBytesOf(dir string) int64 {
	if dir == "" {
		return -1
	}
	out, err := exec.Command("du", "-sb", dir).Output()
	if err != nil {
		return -1
	}
	// `du -sb /foo/pictures` outputs: "1234567\t/foo/pictures\n"
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return -1
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

