// ocstats — descriptive statistics & cost analytics for OpenCode v2 usage.
//
// Go implementation of the ocstats CLI.  Same data source, pricing cache and
// overrides as the Python reference (./ocstats) and Bash (./ocstats.sh):
//
//	database   $OCSTATS_DB or ~/.local/share/opencode/opencode.db  (read-only)
//	pricing    ~/.cache/ocstats/api.json       (models.dev, manual refresh)
//	overrides  ~/.config/ocstats/pricing.json
//
// Build:  go build -o ocstats-bin ./ocstats-go
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	version      = "1.0.0"
	prog         = "ocstats"
	modelsDevURL = "https://models.dev/api.json"
)

// ---------------------------------------------------------------------------
// colors
// ---------------------------------------------------------------------------

var useColor = false

func bold(s string) string   { return esc("1", s) }
func dim(s string) string    { return esc("2", s) }
func yellow(s string) string { return esc("33", s) }
func green(s string) string  { return esc("32", s) }
func cyan(s string) string   { return esc("36", s) }
func white(s string) string  { return esc("37", s) }

func esc(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

// ---------------------------------------------------------------------------
// data model
// ---------------------------------------------------------------------------

type Row struct {
	TS        time.Time
	Day       string
	Week      string
	Month     string
	Provider  string
	Model     string
	Agent     string
	Session   string
	Directory string
	Project   string
	In        int64
	Out       int64
	Reason    int64
	CacheR    int64
	CacheW    int64
	Cost      float64 // reported
}

type Prompt struct {
	TS       time.Time
	Session  string
	Provider string
	Model    string
	Agent    string
	Project  string
}

func (r *Row) Tokens() int64 { return r.In + r.Out + r.Reason + r.CacheR + r.CacheW }

type Group struct {
	Key        []string
	N          int
	Prompts    int
	Sessions   map[string]bool
	In, Out    int64
	Reason     int64
	CR, CW     int64
	Rep, Est   float64
	Eff        float64
	Tokens     []int64
	FirstTS    time.Time
	LastTS     time.Time
}

func (g *Group) CacheHit() float64 {
	d := g.CR + g.In + g.CW
	if d == 0 {
		return 0
	}
	return float64(g.CR) / float64(d)
}

func (g *Group) Mean() float64 {
	if len(g.Tokens) == 0 {
		return 0
	}
	var s int64
	for _, t := range g.Tokens {
		s += t
	}
	return float64(s) / float64(len(g.Tokens))
}

func (g *Group) Median() float64 { return percentile(g.Tokens, 50) }
func (g *Group) P90() float64    { return percentile(g.Tokens, 90) }

func percentile(vals []int64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := make([]int64, len(vals))
	copy(s, vals)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	k := float64(len(s)-1) * p / 100
	f := math.Floor(k)
	c := math.Ceil(k)
	if f == c {
		return float64(s[int(k)])
	}
	return float64(s[int(f)])*(c-k) + float64(s[int(c)])*(k-f)
}

// ---------------------------------------------------------------------------
// filters
// ---------------------------------------------------------------------------

type Filters struct {
	Since, Until         string
	Model, Provider      string
	Agent, Project       string
	Session              string
	lo, hi               int64 // epoch ms
}

func (f *Filters) Describe() string {
	var bits []string
	add := func(k, v string) {
		if v != "" {
			bits = append(bits, k+" "+v)
		}
	}
	add("since", f.Since)
	add("until", f.Until)
	add("model~", f.Model)
	add("provider~", f.Provider)
	add("agent~", f.Agent)
	add("project~", f.Project)
	add("session", f.Session)
	if len(bits) == 0 {
		return "all time"
	}
	return strings.Join(bits, ", ")
}

var relDateRe = regexp.MustCompile(`^(\d+)([dwm])$`)

func parseWhen(s string, edge string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if m := relDateRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "w":
			n *= 7
		case "m":
			n *= 30
		}
		return now.AddDate(0, 0, -n), nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if edge == "hi" {
				switch layout {
				case "2006-01-02":
					return t.AddDate(0, 0, 1), nil
				case "2006-01":
					return t.AddDate(0, 1, 0), nil
				default:
					return t.AddDate(1, 0, 0), nil
				}
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date %q (use YYYY-MM-DD, YYYY-MM, YYYY or e.g. 30d)", s)
}

// ---------------------------------------------------------------------------
// database
// ---------------------------------------------------------------------------

func dbPath() string {
	if p := os.Getenv("OCSTATS_DB"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

const fetchRowsQuery = `
SELECT sm.session_id, sm.time_created, sm.data, sv.directory
FROM session_message AS sm
LEFT JOIN session_v2 AS sv ON sv.id = sm.session_id
WHERE sm.type = 'assistant' AND sm.time_created >= ? AND sm.time_created < ?
ORDER BY sm.time_created`

func fetchRows(db *sql.DB, flt *Filters) ([]Row, error) {
	rows, err := db.Query(fetchRowsQuery, flt.lo, flt.hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var sid, data string
		var ts int64
		var dir sql.NullString
		if err := rows.Scan(&sid, &ts, &data, &dir); err != nil {
			return nil, err
		}
		var d struct {
			Agent string `json:"agent"`
			Model struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
				Variant    string `json:"variant"`
			} `json:"model"`
			Tokens struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
			Cost float64 `json:"cost"`
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			continue
		}
		t := time.UnixMilli(ts)
		r := Row{
			TS:        t,
			Day:       t.Format("2006-01-02"),
			Week:      t.AddDate(0, 0, -int(t.Weekday())).Format("2006-01-02"),
			Month:     t.Format("2006-01"),
			Provider:  d.Model.ProviderID,
			Model:     d.Model.ID,
			Agent:     d.Agent,
			Session:   sid,
			Directory: dir.String,
			In:        d.Tokens.Input,
			Out:       d.Tokens.Output,
			Reason:    d.Tokens.Reasoning,
			CacheR:    d.Tokens.Cache.Read,
			CacheW:    d.Tokens.Cache.Write,
			Cost:      d.Cost,
		}
		if r.Provider == "" {
			r.Provider = "unknown"
		}
		if r.Model == "" {
			r.Model = "unknown"
		}
		if r.Agent == "" {
			r.Agent = "unknown"
		}
		r.Project = "(none)"
		if dir.Valid && dir.String != "" {
			r.Project = filepath.Base(dir.String)
		}
		// substring filters
		if flt.Model != "" && !strings.Contains(strings.ToLower(r.Model), strings.ToLower(flt.Model)) {
			continue
		}
		if flt.Provider != "" && !strings.Contains(strings.ToLower(r.Provider), strings.ToLower(flt.Provider)) {
			continue
		}
		if flt.Agent != "" && !strings.Contains(strings.ToLower(r.Agent), strings.ToLower(flt.Agent)) {
			continue
		}
		if flt.Project != "" && !strings.Contains(strings.ToLower(r.Project+ "|" +r.Directory), strings.ToLower(flt.Project)) {
			continue
		}
		if flt.Session != "" && !strings.HasPrefix(r.Session, flt.Session) {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// pricing
// ---------------------------------------------------------------------------

type rates struct {
	In, Out, CacheR, CacheW float64
	Source                  string
}

type pricing struct {
	doc       map[string]provider
	overrides map[string]map[string]float64
	resolved  map[string]rates
}

type provider struct {
	Models map[string]struct {
		Cost *struct {
			Input     *float64 `json:"input"`
			Output    *float64 `json:"output"`
			CacheRead *float64 `json:"cache_read"`
			CacheWrite *float64 `json:"cache_write"`
		} `json:"cost"`
	} `json:"models"`
}

func cacheFile() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "ocstats", "api.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "ocstats", "api.json")
}

func overridesFile() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "ocstats", "pricing.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ocstats", "pricing.json")
}

func loadPricing(refresh bool) (*pricing, error) {
	p := &pricing{
		doc:       map[string]provider{},
		overrides: map[string]map[string]float64{},
		resolved:  map[string]rates{},
	}
	// overrides
	if b, err := os.ReadFile(overridesFile()); err == nil {
		var m map[string]map[string]float64
		if json.Unmarshal(b, &m) == nil {
			p.overrides = m
		}
	}
	// models.dev cache
	path := cacheFile()
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &p.doc)
	}
	if len(p.doc) == 0 || refresh {
		if err := p.fetch(); err != nil {
			if refresh {
				return nil, err
			}
			// keep whatever we have; estimates just unavailable
		}
	}
	return p, nil
}

func (p *pricing) fetch() error {
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", modelsDevURL, nil)
	req.Header.Set("User-Agent", prog+"/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	doc := map[string]provider{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	p.doc = doc
	_ = os.MkdirAll(filepath.Dir(cacheFile()), 0o755)
	return os.WriteFile(cacheFile(), body, 0o644)
}

func (p *pricing) available() bool { return len(p.doc) > 0 || len(p.overrides) > 0 }

func mdevRates(entry *struct {
	Cost *struct {
		Input     *float64 `json:"input"`
		Output    *float64 `json:"output"`
		CacheRead *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	} `json:"cost"`
}) rates {
	var r rates
	if entry == nil || entry.Cost == nil {
		return r
	}
	f := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v
	}
	r.In, r.Out, r.CacheR, r.CacheW = f(entry.Cost.Input), f(entry.Cost.Output),
		f(entry.Cost.CacheRead), f(entry.Cost.CacheWrite)
	return r
}

// Rates resolves rates with fuzzy provider/model matching (override > exact >
// variant-stripped > leaf > any-provider).
func (p *pricing) Rates(provider, model string) rates {
	key := provider + "/" + model
	if r, ok := p.resolved[key]; ok {
		return r
	}
	result := rates{Source: "none"}
	base := strings.SplitN(model, "@", 2)[0]
	leaf := base
	if i := strings.LastIndex(base, "/"); i >= 0 {
		leaf = base[i+1:]
	}
	for _, m := range []string{model, base, leaf} {
		if ov, ok := p.overrides[provider+"/"+m]; ok {
			r := rates{Source: "override"}
			r.In = ov["input"]
			r.Out = ov["output"]
			r.CacheR = ov["cache_read"]
			r.CacheW = ov["cache_write"]
			result = r
			break
		}
		if pv, ok := p.doc[provider]; ok {
			if entry, ok := pv.Models[m]; ok {
				r := mdevRates(&entry)
				r.Source = "models.dev"
				result = r
				break
			}
		}
	}
	if result.Source == "none" && len(p.doc) > 0 {
		ids := make([]string, 0, len(p.doc))
		for id := range p.doc {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			models := p.doc[id].Models
			for _, m := range []string{model, leaf} {
				if entry, ok := models[m]; ok {
					r := mdevRates(&entry)
					if entry.Cost != nil {
						r.Source = "models.dev~"
						result = r
					}
				}
			}
			if result.Source != "none" {
				break
			}
		}
	}
	p.resolved[key] = result
	return result
}

func (p *pricing) Estimate(r *Row) float64 {
	rt := p.Rates(r.Provider, r.Model)
	return (float64(r.In)*rt.In + float64(r.Out)*rt.Out +
		float64(r.CacheR)*rt.CacheR + float64(r.CacheW)*rt.CacheW) / 1e6
}

// ---------------------------------------------------------------------------
// prompts (user messages, attributed to the assistant message that answers)
// ---------------------------------------------------------------------------

func fetchPrompts(db *sql.DB, flt *Filters) ([]Prompt, error) {
	q := `
SELECT sm.session_id, sm.time_created, COALESCE(sv.directory, '')
FROM session_message AS sm
LEFT JOIN session_v2 AS sv ON sv.id = sm.session_id
WHERE sm.type = 'user' AND sm.time_created >= ? AND sm.time_created < ?
ORDER BY sm.time_created`
	rows, err := db.Query(q, flt.lo, flt.hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Prompt
	for rows.Next() {
		var sid, dir string
		var ts int64
		if err := rows.Scan(&sid, &ts, &dir); err != nil {
			return nil, err
		}
		if flt.Session != "" && !strings.HasPrefix(sid, flt.Session) {
			continue
		}
		if flt.Project != "" && !strings.Contains(strings.ToLower(dir), strings.ToLower(flt.Project)) {
			continue
		}
		out = append(out, Prompt{TS: time.UnixMilli(ts), Session: sid})
	}
	return out, rows.Err()
}

func attributePrompts(prompts []Prompt, rows []Row) {
	bySession := map[string][]*Row{}
	for i := range rows {
		bySession[rows[i].Session] = append(bySession[rows[i].Session], &rows[i])
	}
	for i := range prompts {
		p := &prompts[i]
		msgs := bySession[p.Session]
		if len(msgs) == 0 {
			p.Provider, p.Model, p.Agent, p.Project = "unknown", "unknown", "unknown", "(none)"
			continue
		}
		ref := msgs[len(msgs)-1]
		for _, m := range msgs {
			if !m.TS.Before(p.TS) {
				ref = m
				break
			}
		}
		p.Provider, p.Model, p.Agent, p.Project = ref.Provider, ref.Model, ref.Agent, ref.Project
	}
}

// ---------------------------------------------------------------------------
// aggregation
// ---------------------------------------------------------------------------

var dimensions = []string{"model", "provider", "agent", "project", "session", "day", "week", "month"}

func dimValue(r *Row, dim string) string {
	switch dim {
	case "model":
		return r.Model
	case "provider":
		return r.Provider
	case "agent":
		return r.Agent
	case "project":
		return r.Project
	case "session":
		return r.Session
	case "day":
		return r.Day
	case "week":
		return r.Week
	case "month":
		return r.Month
	}
	return "?"
}

func promptValue(p *Prompt, dim string) string {
	switch dim {
	case "model":
		return p.Model
	case "provider":
		return p.Provider
	case "agent":
		return p.Agent
	case "project":
		return p.Project
	case "session":
		return p.Session
	}
	return "?"
}

func groupRows(rows []Row, dims []string, p *pricing, prompts []Prompt) []*Group {
	byKey := map[string]*Group{}
	var order []*Group
	promptCounts := map[string]int{}
	for i := range prompts {
		parts := make([]string, len(dims))
		for j, d := range dims {
			if d == "day" || d == "week" || d == "month" {
				parts[j] = timeDim(prompts[i].TS, d)
			} else {
				parts[j] = promptValue(&prompts[i], d)
			}
		}
		promptCounts[strings.Join(parts, "\x1f")]++
	}
	for i := range rows {
		r := &rows[i]
		keyParts := make([]string, len(dims))
		for i, d := range dims {
			keyParts[i] = dimValue(r, d)
		}
		key := strings.Join(keyParts, "\x1f")
		g, ok := byKey[key]
		if !ok {
			g = &Group{Key: keyParts, Sessions: map[string]bool{}, FirstTS: r.TS, LastTS: r.TS}
			byKey[key] = g
			order = append(order, g)
		}
		g.N++
		g.Sessions[r.Session] = true
		g.In += r.In
		g.Out += r.Out
		g.Reason += r.Reason
		g.CR += r.CacheR
		g.CW += r.CacheW
		g.Rep += r.Cost
		est := p.Estimate(r)
		g.Est += est
		if r.Cost > 0 {
			g.Eff += r.Cost
		} else {
			g.Eff += est
		}
		g.Tokens = append(g.Tokens, r.Tokens())
		if r.TS.Before(g.FirstTS) {
			g.FirstTS = r.TS
		}
		if r.TS.After(g.LastTS) {
			g.LastTS = r.TS
		}
	}
	for k, g := range byKey {
		g.Prompts = promptCounts[k]
	}
	return order
}

func timeDim(t time.Time, dim string) string {
	switch dim {
	case "day":
		return t.Format("2006-01-02")
	case "week":
		return t.AddDate(0, 0, -int(t.Weekday())).Format("2006-01-02")
	case "month":
		return t.Format("2006-01")
	}
	return "?"
}

func sortGroups(gs []*Group, by string) {
	less := func(a, b *Group) bool { return false }
	switch by {
	case "rep":
		less = func(a, b *Group) bool { return a.Rep > b.Rep }
	case "est":
		less = func(a, b *Group) bool { return a.Est > b.Est }
	case "tokens":
		less = func(a, b *Group) bool { return tokenSum(a) > tokenSum(b) }
	case "msgs":
		less = func(a, b *Group) bool { return a.N > b.N }
	case "sessions":
		less = func(a, b *Group) bool { return len(a.Sessions) > len(b.Sessions) }
	case "name":
		less = func(a, b *Group) bool { return strings.Join(a.Key, "/") < strings.Join(b.Key, "/") }
	case "date":
		less = func(a, b *Group) bool { return a.FirstTS.Before(b.FirstTS) }
	case "date-desc":
		less = func(a, b *Group) bool { return a.FirstTS.After(b.FirstTS) }
	default: // cost
		less = func(a, b *Group) bool { return a.Eff > b.Eff }
	}
	sort.SliceStable(gs, func(i, j int) bool { return less(gs[i], gs[j]) })
}

func tokenSum(g *Group) int64 {
	var s int64
	for _, t := range g.Tokens {
		s += t
	}
	return s
}

// ---------------------------------------------------------------------------
// formatting
// ---------------------------------------------------------------------------

func money(v float64) string {
	switch {
	case v == 0:
		return "$0"
	case math.Abs(v) >= 100:
		return fmt.Sprintf("$%.2f", v)
	case math.Abs(v) >= 1:
		return fmt.Sprintf("$%.3f", v)
	default:
		return fmt.Sprintf("$%.4f", v)
	}
}

func tok(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 0)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		return "-" + out
	}
	return out
}

func pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func bar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * float64(width)))
	return cyan(strings.Repeat("█", filled)) + dim(strings.Repeat("░", width-filled))
}

var sparkChars = []rune("▁▂▃▄▅▆▇█")

func spark(vals []float64) string {
	mx := 0.0
	for _, v := range vals {
		if v > mx {
			mx = v
		}
	}
	if mx == 0 {
		mx = 1
	}
	var b strings.Builder
	for _, v := range vals {
		lvl := int(v / mx * 8)
		if lvl > 7 {
			lvl = 7
		}
		if lvl < 0 {
			lvl = 0
		}
		b.WriteRune(sparkChars[lvl])
	}
	return b.String()
}

// table renders a boxed table (or a markdown pipe table when mdMode is set).
// aligns is one char per column: l/r/c.
func renderTable(w io.Writer, title string, headers []string, aligns string, rows [][]string, footer [][]string, note string) {
	ncols := len(headers)

	if mdMode {
		renderMDTable(w, title, headers, aligns, rows, footer, note)
		return
	}

	widths := make([]int, ncols)
	for i, h := range headers {
		widths[i] = len(plain(h))
	}
	all := append(append([][]string{}, rows...), footer...)
	for _, row := range all {
		for i := 0; i < ncols && i < len(row); i++ {
			if l := len(plain(row[i])); l > widths[i] {
				widths[i] = l
			}
		}
	}
	hline := func(l, m, r, fill string) string {
		var parts []string
		for i := 0; i < ncols; i++ {
			parts = append(parts, strings.Repeat(fill, widths[i]+2))
		}
		return l + strings.Join(parts, m) + r
	}
	fmtRow := func(row []string, deco func(string) string) string {
		cells := make([]string, ncols)
		for i := 0; i < ncols; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			pad := widths[i] - len(plain(cell))
			switch {
			case i < len(aligns) && aligns[i] == 'r':
				cell = strings.Repeat(" ", pad) + cell
			case i < len(aligns) && aligns[i] == 'c':
				l := pad / 2
				cell = strings.Repeat(" ", l) + cell + strings.Repeat(" ", pad-l)
			default:
				cell = cell + strings.Repeat(" ", pad)
			}
			cells[i] = " " + cell + " "
		}
		if deco != nil {
			for i := range cells {
				cells[i] = deco(cells[i])
			}
		}
		return "│" + strings.Join(cells, "│") + "│"
	}

	if title != "" {
		fmt.Fprintln(w, bold(cyan(title)))
	}
	fmt.Fprintln(w, hline("╭", "┬", "╮", "─"))
	fmt.Fprintln(w, bold(fmtRow(headers, nil)))
	fmt.Fprintln(w, hline("├", "┼", "┤", "─"))
	for _, row := range rows {
		fmt.Fprintln(w, fmtRow(row, nil))
	}
	if len(footer) > 0 {
		fmt.Fprintln(w, hline("╞", "╪", "╡", "═"))
		for _, row := range footer {
			fmt.Fprintln(w, bold(fmtRow(row, nil)))
		}
	}
	fmt.Fprintln(w, hline("╰", "┴", "╯", "─"))
	if note != "" {
		fmt.Fprintln(w, dim("  "+note))
	}
}

func renderMDTable(w io.Writer, title string, headers []string, aligns string, rows [][]string, footer [][]string, note string) {
	join := func(row []string, boldRow bool) string {
		cells := make([]string, len(headers))
		for i := range headers {
			cell := ""
			if i < len(row) && row[i] != "" {
				cell = plain(row[i])
				cell = strings.ReplaceAll(cell, "|", "\\|")
				cell = strings.ReplaceAll(cell, "\n", " ")
				if boldRow {
					cell = "**" + cell + "**"
				}
			}
			cells[i] = cell
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	if title != "" {
		fmt.Fprintln(w, "### "+plain(title))
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, join(headers, false))
	marks := make([]string, len(headers))
	for i := range headers {
		switch {
		case i < len(aligns) && aligns[i] == 'r':
			marks[i] = "---:"
		case i < len(aligns) && aligns[i] == 'c':
			marks[i] = ":---:"
		default:
			marks[i] = ":---"
		}
	}
	fmt.Fprintln(w, "|" + strings.Join(marks, "|") + "|")
	for _, row := range rows {
		fmt.Fprintln(w, join(row, false))
	}
	for _, row := range footer {
		fmt.Fprintln(w, join(row, true))
	}
	if note != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "*"+plain(note)+"*")
	}
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

type options struct {
	command        string
	since          string
	until          string
	model          string
	provider       string
	agent          string
	project        string
	session        string
	limit          int
	sortBy         string
	jsonOut        bool
	csvOut         bool
	mdOut          bool
	details        bool
	topN           int
	bottomN        int
	minShare       float64
	monthlyBudget  float64
	compare        string
	by             string
	rows           string
	cols           string
	metric         string
	refresh        bool
	promptRows     []Prompt
	prev           *prevData
}

type prevData struct {
	rows    []Row
	prompts []Prompt
	lo, hi  int64
}

var mdMode = false // render tables as markdown

var commands = map[string]bool{
	"summary": true, "models": true, "providers": true, "agents": true,
	"projects": true, "sessions": true, "daily": true, "weekly": true,
	"monthly": true, "matrix": true, "pivot": true, "prices": true,
}

func parseArgs(argv []string) (*options, error) {
	o := &options{command: "", limit: 25, rows: "project", cols: "day",
		metric: "", monthlyBudget: -1}
	var rest []string
	for _, a := range argv {
		if commands[a] && o.command == "" {
			o.command = a
		} else {
			rest = append(rest, a)
		}
	}
	if o.command == "" {
		o.command = "summary"
	}
	i := 0
	for i < len(rest) {
		a := rest[i]
		next := func() string {
			i++
			if i >= len(rest) {
				return ""
			}
			return rest[i]
		}
		switch a {
		case "--since":
			o.since = next()
		case "--until":
			o.until = next()
		case "--model":
			o.model = next()
		case "--provider":
			o.provider = next()
		case "--agent":
			o.agent = next()
		case "--project":
			o.project = next()
		case "--session":
			o.session = next()
		case "--limit":
			v, err := strconv.Atoi(next())
			if err != nil {
				return nil, fmt.Errorf("bad --limit")
			}
			o.limit = v
		case "--sort":
			o.sortBy = next()
		case "--by":
			o.by = next()
		case "--rows":
			o.rows = next()
		case "--cols":
			o.cols = next()
		case "--metric":
			o.metric = next()
		case "--json":
			o.jsonOut = true
		case "--csv":
			o.csvOut = true
		case "--md":
			o.mdOut = true
		case "--details":
			o.details = true
		case "--top":
			v, err := strconv.Atoi(next())
			if err != nil {
				return nil, fmt.Errorf("bad --top")
			}
			o.topN = v
		case "--bottom":
			v, err := strconv.Atoi(next())
			if err != nil {
				return nil, fmt.Errorf("bad --bottom")
			}
			o.bottomN = v
		case "--min-share":
			v, err := strconv.ParseFloat(next(), 64)
			if err != nil {
				return nil, fmt.Errorf("bad --min-share")
			}
			o.minShare = v
		case "--monthly-budget":
			v, err := strconv.ParseFloat(next(), 64)
			if err != nil || v <= 0 {
				return nil, fmt.Errorf("--monthly-budget must be a number > 0")
			}
			o.monthlyBudget = v
		case "--compare":
			o.compare = next()
		case "--refresh":
			o.refresh = true
		case "--no-color":
			// handled globally
		case "--width":
			next() // accepted for parity
		case "--version":
			fmt.Printf("%s %s\n", prog, version)
			os.Exit(0)
		case "-h", "--help":
			usage()
			os.Exit(0)
		default:
			return nil, fmt.Errorf("unknown argument %q", a)
		}
		i++
	}
	if o.topN > 0 && o.bottomN > 0 {
		return nil, fmt.Errorf("--top and --bottom are mutually exclusive")
	}
	return o, nil
}

func usage() {
	fmt.Printf(`%s — descriptive statistics & cost analytics for OpenCode v2 usage.
Aggregate tokens, prompts, sessions and cost by model, provider, agent,
project, day, week, month, session — or any combination of these.

commands:
  summary   dashboard overview of everything (default command)
  models    aggregate usage & cost per provider × model
  providers aggregate usage & cost per provider
  agents    aggregate usage & cost per agent
  projects  aggregate usage & cost per project directory
  sessions  top sessions by effective cost
  daily     aggregate per day (time series)
  weekly    aggregate per week (time series)
  monthly   aggregate per month (time series)
  matrix    aggregate by ANY dimension combination (--by provider,month, …)
  pivot     cross-tab heat-map grid (--rows project --cols day)
  prices    show resolved pricing table (--refresh re-fetches models.dev)

aggregation examples:
  ocstats-bin                                summary of everything
  ocstats-bin models                         per provider × model
  ocstats-bin providers --since 30d          last 30 days per provider
  ocstats-bin daily                          per-day time series
  ocstats-bin projects --details             per project + prompts/reasoning/p90
  ocstats-bin matrix --by provider,month     per provider per month
  ocstats-bin matrix --by project,day --metric tokens   tokens per project per day
  ocstats-bin pivot --rows project --cols day           projects-per-day grid
  ocstats-bin agents --compare 7d/7d         this week vs last, per agent
  ocstats-bin sessions --top 5               5 most expensive sessions
  ocstats-bin summary --monthly-budget 50    spend vs $50 monthly budget

dimensions for matrix --by / pivot --rows / --cols:
  model, provider, agent, project, session, day, week, month
  (comma-separate any combination, e.g. --by project,day,provider)

filters: --since --until (YYYY-MM-DD | 30d)  --model --provider --agent --project --session
output:  --json --csv --md --details --limit N --top N --bottom N --min-share P
         --sort cost|rep|est|tokens|msgs|sessions|name|date|date-desc --no-color --width N
extra:   --monthly-budget AMOUNT --compare CUR/PREV (e.g. 7d/7d) --refresh
`, prog)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, prog+": "+err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	o, err := parseArgs(argv)
	if err != nil {
		return err
	}
	noColor := false
	for _, a := range argv {
		if a == "--no-color" {
			noColor = true
		}
	}
	mdMode = o.mdOut && !o.jsonOut && !o.csvOut
	useColor = !noColor && os.Getenv("NO_COLOR") == "" &&
		!o.jsonOut && !o.csvOut && !o.mdOut && isTTY(os.Stdout)

	now := time.Now()
	flt := &Filters{
		Since: o.since, Until: o.until, Model: o.model, Provider: o.provider,
		Agent: o.agent, Project: o.project, Session: o.session,
		lo: 0, hi: now.AddDate(1, 0, 0).UnixMilli(),
	}
	if o.since != "" {
		t, err := parseWhen(o.since, "lo", now)
		if err != nil {
			return err
		}
		flt.lo = t.UnixMilli()
	}
	if o.until != "" {
		t, err := parseWhen(o.until, "hi", now)
		if err != nil {
			return err
		}
		flt.hi = t.UnixMilli()
	}
	if o.compare != "" {
		cur := o.compare
		prev := o.compare
		if i := strings.Index(o.compare, "/"); i >= 0 {
			cur, prev = o.compare[:i], o.compare[i+1:]
		}
		clo, chi, err := compareWindow(cur, now, -1)
		if err != nil {
			return fmt.Errorf("bad --compare %q: %w", o.compare, err)
		}
		flt.lo, flt.hi = clo, chi
		plo, phi, err := compareWindow(prev, now, clo)
		if err != nil {
			return fmt.Errorf("bad --compare %q: %w", o.compare, err)
		}
		o.prev = &prevData{}
		o.prev.rows, o.prev.prompts = nil, nil
		o.prev.lo, o.prev.hi = plo, phi
	}

	path := dbPath()
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("database not found: %s (set OCSTATS_DB to override)", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()

	p, err := loadPricing(o.refresh && o.command == "prices")
	if err != nil {
		return fmt.Errorf("refresh failed: %w", err)
	}

	rows, err := fetchRows(db, flt)
	if err != nil {
		return err
	}
	if len(rows) == 0 && o.command != "prices" {
		return errors.New("no messages matched the current filters")
	}
	prompts, err := fetchPrompts(db, flt)
	if err != nil {
		return err
	}
	attributePrompts(prompts, rows)
	o.promptRows = prompts
	if o.prev != nil {
		pflt := &Filters{Since: o.compare, Until: o.compare, Model: o.model,
			Provider: o.provider, Agent: o.agent, Project: o.project,
			Session: o.session, lo: o.prev.lo, hi: o.prev.hi}
		pRows, err := fetchRows(db, pflt)
		if err != nil {
			return err
		}
		pPrompts, err := fetchPrompts(db, pflt)
		if err != nil {
			return err
		}
		attributePrompts(pPrompts, pRows)
		o.prev.rows, o.prev.prompts = pRows, pPrompts
	}

	switch o.command {
	case "summary":
		return cmdSummary(os.Stdout, rows, flt, p, o)
	case "models":
		return groupReport(os.Stdout, rows, flt, p, o, []string{"provider", "model"}, "Usage by model")
	case "providers":
		return groupReport(os.Stdout, rows, flt, p, o, []string{"provider"}, "Usage by provider")
	case "agents":
		return groupReport(os.Stdout, rows, flt, p, o, []string{"agent"}, "Usage by agent")
	case "projects":
		return groupReport(os.Stdout, rows, flt, p, o, []string{"project"}, "Usage by project")
	case "sessions":
		return cmdSessions(os.Stdout, rows, flt, p, o)
	case "daily":
		return groupReport(os.Stdout, rows, flt, p, o, []string{"day"}, "Usage by day")
	case "weekly":
		return groupReport(os.Stdout, rows, flt, p, o, []string{"week"}, "Usage by week")
	case "monthly":
		return groupReport(os.Stdout, rows, flt, p, o, []string{"month"}, "Usage by month")
	case "pivot":
		return cmdPivot(os.Stdout, rows, flt, p, o)
	case "matrix":
		dims := []string{}
		for _, d := range strings.Split(o.by, ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			ok := false
			for _, valid := range dimensions {
				if d == valid {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("unknown dimension %q (choose from: %s)", d, strings.Join(dimensions, " "))
			}
			dims = append(dims, d)
		}
		if len(dims) == 0 {
			return errors.New("matrix requires --by provider,model,…")
		}
		title := "Matrix by " + strings.Join(dims, " × ")
		return groupReport(os.Stdout, rows, flt, p, o, dims, title)
	case "prices":
		return cmdPrices(os.Stdout, rows, flt, p, o)
	}
	return nil
}

// compareWindow resolves one --compare window spec to (lo_ms, hi_ms).
// Relative specs end at `now`, or at anchorHi when chaining the previous
// window directly before the current one.
func compareWindow(spec string, now time.Time, anchorHi int64) (int64, int64, error) {
	if m := relDateRe.FindStringSubmatch(spec); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "w":
			n *= 7
		case "m":
			n *= 30
		}
		hi := now
		if anchorHi >= 0 {
			hi = time.UnixMilli(anchorHi)
		}
		return hi.AddDate(0, 0, -n).UnixMilli(), hi.UnixMilli(), nil
	}
	lo, err := parseWhen(spec, "lo", now)
	if err != nil {
		return 0, 0, err
	}
	hi, err := parseWhen(spec, "hi", now)
	if err != nil {
		return 0, 0, err
	}
	return lo.UnixMilli(), hi.UnixMilli(), nil
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ---------------------------------------------------------------------------
// commands
// ---------------------------------------------------------------------------

func pricingHint(p *pricing) {
	if !p.available() {
		fmt.Fprintln(os.Stderr, "note: no pricing data — run `ocstats prices --refresh` to enable cost estimates")
	}
}

func effective(rows []Row, p *pricing) (rep, est, eff float64) {
	for i := range rows {
		r := &rows[i]
		rep += r.Cost
		e := p.Estimate(r)
		est += e
		if r.Cost > 0 {
			eff += r.Cost
		} else {
			eff += e
		}
	}
	return
}

func monthToDate(rows []Row, p *pricing) float64 {
	month := time.Now().Format("2006-01")
	eff := 0.0
	for i := range rows {
		if rows[i].Month == month {
			if rows[i].Cost > 0 {
				eff += rows[i].Cost
			} else {
				eff += p.Estimate(&rows[i])
			}
		}
	}
	return eff
}

func budgetBlock(w io.Writer, spent, budget float64) {
	now := time.Now()
	dim := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -1).Day()
	projected := 0.0
	if now.Day() > 0 {
		projected = spent / float64(now.Day()) * float64(dim)
	}
	used := 0.0
	if budget > 0 {
		used = spent / budget
	}
	verdict, vcol := "UNDER PACE", green
	if projected > budget {
		verdict, vcol = "OVER PACE", red
	}
	remainCol := green
	if budget-spent < 0 {
		remainCol = red
	}
	renderTable(w, "Budget",
		[]string{"Budget", "Spent (MTD)", "Projected", "Remaining", "Used", "Pace"},
		"rrrrrl",
		[][]string{{money(budget), white(money(spent)), yellow(money(projected)),
			remainCol(money(budget - spent)), pct(used),
			bar(used, 12) + "  " + vcol(verdict)}},
		nil, "")
	fmt.Fprintln(w)
}

func cmdSummary(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options) error {
	prompts := o.promptRows
	rep, est, eff := effective(rows, p)
	var in, out, reason, cr, cw int64
	sessions := map[string]bool{}
	days := map[string]bool{}
	toks := make([]int64, len(rows))
	for i := range rows {
		r := &rows[i]
		in += r.In
		out += r.Out
		reason += r.Reason
		cr += r.CacheR
		cw += r.CacheW
		sessions[r.Session] = true
		days[r.Day] = true
		toks[i] = r.Tokens()
	}
	var first, last time.Time
	for i := range rows {
		if first.IsZero() || rows[i].TS.Before(first) {
			first = rows[i].TS
		}
		if rows[i].TS.After(last) {
			last = rows[i].TS
		}
	}
	byDay := map[string]float64{}
	for i := range rows {
		r := &rows[i]
		if r.Cost > 0 {
			byDay[r.Day] += r.Cost
		} else {
			byDay[r.Day] += p.Estimate(r)
		}
	}
	dayKeys := make([]string, 0, len(byDay))
	for d := range byDay {
		dayKeys = append(dayKeys, d)
	}
	sort.Strings(dayKeys)
	totalTokens := in + out + reason + cr + cw

	if o.jsonOut {
		outM := map[string]any{
			"totals": map[string]any{
				"prompts": len(prompts), "messages": len(rows),
				"sessions": len(sessions), "active_days": len(days),
				"tokens": map[string]int64{"input": in, "output": out,
					"reasoning": reason, "cache_read": cr, "cache_write": cw,
					"total": totalTokens},
				"cache_ratio": cacheRatio(cr, in, cw),
				"cost":        map[string]float64{"reported": rep, "estimated": est, "effective": eff},
				"period":      map[string]string{"from": first.Format(time.RFC3339), "to": last.Format(time.RFC3339)},
			},
		}
		if o.monthlyBudget > 0 {
			outM["budget"] = budgetJSON(monthToDate(rows, p), o.monthlyBudget)
		}
		b, _ := json.MarshalIndent(outM, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	if o.csvOut {
		fmt.Fprintln(w, "prompts,messages,sessions,input,output,reasoning,cache_read,cache_write,tokens_total,cost_reported,cost_estimated,cost_effective")
		fmt.Fprintf(w, "%d,%d,%d,%d,%d,%d,%d,%d,%d,%.6f,%.6f,%.6f\n",
			len(prompts), len(rows), len(sessions), in, out, reason, cr, cw,
			totalTokens, rep, est, eff)
		return nil
	}

	pricingHint(p)
	label := "OpenCode Usage Summary"
	if o.prev != nil {
		label += "  (period comparison)"
	}
	fmt.Println()
	fmt.Printf("  %s  %s  %s\n", bold(cyan(label)),
		dim(first.Format("2006-01-02")+" → "+last.Format("2006-01-02")),
		dim("· "+flt.Describe()))
	fmt.Println()
	renderTable(w, "",
		[]string{"Prompts", "Messages", "Sessions", "Active days", "Tok input",
			"Tok output", "Reasoning", "Cache hits", "Cache writes", "Tok total",
			"Cache hit"},
		"rrrrrrrrrrr",
		[][]string{{tok(int64(len(prompts))), tok(int64(len(rows))),
			tok(int64(len(sessions))), tok(int64(len(days))), tok(in), tok(out),
			tok(reason), tok(cr), tok(cw), tok(totalTokens),
			pct(cacheRatio(cr, in, cw))}},
		nil, "")
	fmt.Println()
	renderTable(w, "Cost",
		[]string{"Reported", "Estimated", "Effective Σ", "Median tok/msg", "p90 tok/msg"},
		"rrrrr",
		[][]string{{yellow(money(rep)), green(money(est)), bold(white(money(eff))),
			tok(int64(math.Round(percentile(toks, 50)))),
			tok(int64(math.Round(percentile(toks, 90))))}},
		nil, "Σ effective = reported where reported > 0, else estimated")
	fmt.Println()

	if o.monthlyBudget > 0 {
		budgetBlock(w, monthToDate(rows, p), o.monthlyBudget)
	}

	if o.prev != nil {
		_, _, pEff := effective(o.prev.rows, p)
		var pTok int64
		for i := range o.prev.rows {
			pTok += o.prev.rows[i].Tokens()
		}
		renderTable(w, "Period comparison",
			[]string{"Period", "Msgs", "Tokens", "Σ $", "Δ msgs", "Δ tokens", "Δ Σ $"},
			"lrrrrrr",
			[][]string{
				{dim("previous"), tok(int64(len(o.prev.rows))), tok(pTok), dim(money(pEff)), "", "", ""},
				{bold("current"), tok(int64(len(rows))), tok(totalTokens), bold(white(money(eff))),
					deltaStr(int64(len(rows)) - int64(len(o.prev.rows))),
					deltaStr(totalTokens - pTok),
					moneyDelta(eff - pEff)},
			}, nil, "")
		fmt.Println()
	}

	gs := groupRows(rows, []string{"provider", "model"}, p, prompts)
	sortGroups(gs, "cost")
	var mrows [][]string
	for i, g := range gs {
		if i >= 3 {
			break
		}
		mrows = append(mrows, []string{g.Key[0], g.Key[1], tok(int64(g.N)),
			tok(tokenSum(g)), money(g.Rep), money(g.Est), money(g.Eff)})
	}
	renderTable(w, "Top models", []string{"Provider", "Model", "Msgs", "Tokens", "Rep $", "Est $", "Σ $"},
		"llrrrrr", mrows, nil, "")
	fmt.Println()

	if len(dayKeys) > 0 {
		vals := make([]float64, len(dayKeys))
		for i, d := range dayKeys {
			vals[i] = byDay[d]
		}
		fmt.Printf("  %s  %s\n", bold("Daily cost trend"),
			dim(fmt.Sprintf("(%s → %s, %d active days)", dayKeys[0], dayKeys[len(dayKeys)-1], len(dayKeys))))
		fmt.Printf("  %s  %s %s\n", spark(vals), bold(white(money(maxF(vals)))), dim("peak day"))
	}
	fmt.Println()
	return nil
}

func deltaStr(d int64) string {
	var s string
	if d >= 0 {
		s = "+" + tok(d)
	} else {
		s = tok(d)
	}
	if d >= 0 {
		return green(s)
	}
	return red(s)
}

func moneyDelta(d float64) string {
	var s string
	if d >= 0 {
		s = "+" + money(d)
	} else {
		s = money(d)
	}
	if d >= 0 {
		return green(s)
	}
	return red(s)
}

func budgetJSON(spent, budget float64) map[string]any {
	now := time.Now()
	dim := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -1).Day()
	projected := 0.0
	if now.Day() > 0 {
		projected = spent / float64(now.Day()) * float64(dim)
	}
	usedPct := 0.0
	if budget > 0 {
		usedPct = spent / budget * 100
	}
	return map[string]any{
		"budget": budget, "spent_mtd": spent, "projected_month_end": projected,
		"remaining": budget - spent, "used_pct": usedPct,
		"on_pace": projected <= budget,
	}
}

func cacheRatio(cr, in, cw int64) float64 {
	d := cr + in + cw
	if d == 0 {
		return 0
	}
	return float64(cr) / float64(d)
}

func maxF(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

type jrow map[string]any

func groupReport(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options, dims []string, title string) error {
	sortBy := o.sortBy
	if sortBy == "" {
		sortBy = "cost"
	}
	gs := groupRows(rows, dims, p, o.promptRows)
	sortGroups(gs, sortBy)
	if o.bottomN > 0 {
		for i, j := 0, len(gs)-1; i < j; i, j = i+1, j-1 {
			gs[i], gs[j] = gs[j], gs[i]
		}
	}
	totalEff := 0.0
	for _, g := range gs {
		totalEff += g.Eff
	}
	totalSessions := map[string]bool{}
	for i := range rows {
		totalSessions[rows[i].Session] = true
	}

	hidden := 0
	if o.minShare > 0 && totalEff > 0 {
		kept := gs[:0]
		for _, g := range gs {
			if g.Eff/totalEff*100 >= o.minShare {
				kept = append(kept, g)
			} else {
				hidden++
			}
		}
		gs = kept
	}
	limit := o.limit
	if o.topN > 0 {
		limit = o.topN
	}
	if o.bottomN > 0 {
		limit = o.bottomN
	}
	limited := gs
	restN := 0
	if limit > 0 && len(gs) > limit {
		restN = len(gs) - limit
		limited = gs[:limit]
	}

	if o.jsonOut {
		var out []jrow
		for _, g := range gs {
			r := jrow{}
			for i, d := range dims {
				r[d] = g.Key[i]
			}
			r["n"] = g.N
			r["prompts"] = g.Prompts
			r["sessions"] = len(g.Sessions)
			r["in"] = g.In
			r["out"] = g.Out
			r["reason"] = g.Reason
			r["cr"] = g.CR
			r["cw"] = g.CW
			r["tok_total"] = tokenSum(g)
			r["cost_rep"] = round6(g.Rep)
			r["cost_est"] = round6(g.Est)
			r["cost_eff"] = round6(g.Eff)
			r["tok_mean"] = round6(g.Mean())
			r["tok_median"] = round6(g.Median())
			r["tok_p90"] = round6(g.P90())
			out = append(out, r)
		}
		b, _ := json.MarshalIndent(map[string]any{"rows": out}, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	if o.csvOut {
		fmt.Fprint(w, strings.Join(dims, ","))
		fmt.Fprintln(w, ",n,prompts,sessions,in,out,reason,cr,cw,cost_rep,cost_est,cost_eff,first_ts,last_ts,tok_mean,tok_median,tok_p90,tok_total")
		for _, g := range gs {
			vals := []string{}
			for _, gk := range g.Key {
				vals = append(vals, csvQuote(gk))
			}
			vals = append(vals,
				strconv.Itoa(g.N), strconv.Itoa(g.Prompts), strconv.Itoa(len(g.Sessions)),
				strconv.FormatInt(g.In, 10), strconv.FormatInt(g.Out, 10),
				strconv.FormatInt(g.Reason, 10), strconv.FormatInt(g.CR, 10),
				strconv.FormatInt(g.CW, 10),
				fmt.Sprintf("%.6f", g.Rep), fmt.Sprintf("%.6f", g.Est), fmt.Sprintf("%.6f", g.Eff),
				strconv.FormatInt(g.FirstTS.UnixMilli(), 10), strconv.FormatInt(g.LastTS.UnixMilli(), 10),
				fmt.Sprintf("%.1f", g.Mean()), fmt.Sprintf("%.1f", g.Median()),
				fmt.Sprintf("%.1f", g.P90()), strconv.FormatInt(tokenSum(g), 10))
			fmt.Fprintln(w, strings.Join(vals, ","))
		}
		return nil
	}

	pricingHint(p)
	focus := o.metric != "" && o.command == "matrix"
	headers := make([]string, 0, len(dims)+20)
	for _, d := range dims {
		headers = append(headers, strings.ToUpper(d[:1])+d[1:])
	}
	var frow func(*Group) string
	if focus {
		switch o.metric {
		case "cost":
			headers = append(headers, "Σ $")
			frow = func(g *Group) string { return money(g.Eff) }
		case "msgs":
			headers = append(headers, "Msgs")
			frow = func(g *Group) string { return tok(int64(g.N)) }
		case "prompts":
			headers = append(headers, "Prompts")
			frow = func(g *Group) string { return tok(int64(g.Prompts)) }
		case "in":
			headers = append(headers, "Tok in")
			frow = func(g *Group) string { return tok(g.In) }
		case "out":
			headers = append(headers, "Tok out")
			frow = func(g *Group) string { return tok(g.Out) }
		case "cache":
			headers = append(headers, "Cache hits")
			frow = func(g *Group) string { return tok(g.CR) }
		default:
			headers = append(headers, "Tokens")
			frow = func(g *Group) string { return tok(tokenSum(g)) }
		}
	} else {
		headers = append(headers, "Msgs", "Sessions", "Tok in", "Tok out", "Cache R",
			"Cache hit", "Rep $", "Est $", "Σ $", "Share")
		if o.details {
			headers = append(headers, "Prompts", "Reasoning", "Cache W", "Mean",
				"Median", "p90", "First seen", "Last seen")
		}
	}
	aligns := strings.Repeat("l", len(dims)) + "r"
	if !focus {
		aligns += "rrrrrrrrr"
		if o.details {
			aligns += "rrrrrrrr"
		}
	}

	prevMap := map[string]*Group{}
	if o.prev != nil {
		for _, pg := range groupRows(o.prev.rows, dims, p, o.prev.prompts) {
			prevMap[strings.Join(pg.Key, "\x1f")] = pg
		}
		headers = append(headers, "Prev Σ $", "Δ Σ $", "Δ msgs")
		aligns += "rrr"
	}

	var rrows [][]string
	for _, g := range limited {
		row := append([]string{}, g.Key...)
		if focus {
			row = append(row, frow(g))
		} else {
			repC := yellow(money(g.Rep))
			if g.Rep == 0 {
				repC = dim(repC)
			}
			estC := green(money(g.Est))
			if g.Est == 0 && g.Rep == 0 && tokenSum(g) > 0 {
				estC = dim("—")
			}
			share := 0.0
			if totalEff > 0 {
				share = g.Eff / totalEff
			}
			row = append(row, tok(int64(g.N)), tok(int64(len(g.Sessions))), tok(g.In),
				tok(g.Out), tok(g.CR), pct(g.CacheHit()), repC, estC,
				bold(white(money(g.Eff))), pct(share))
			if o.details {
				row = append(row, tok(int64(g.Prompts)), tok(g.Reason), tok(g.CW),
					tok(int64(math.Round(g.Mean()))), tok(int64(math.Round(g.Median()))),
					tok(int64(math.Round(g.P90()))),
					g.FirstTS.Format("01-02 15:04"), g.LastTS.Format("01-02 15:04"))
			}
		}
		if len(prevMap) > 0 {
			if pg, ok := prevMap[strings.Join(g.Key, "\x1f")]; ok {
				row = append(row, dim(money(pg.Eff)), moneyDelta(g.Eff-pg.Eff),
					deltaStr(int64(g.N-pg.N)))
			} else {
				row = append(row, dim("—"), dim("new"), dim("+"))
			}
		}
		rrows = append(rrows, row)
	}
	if restN+hidden > 0 {
		label := fmt.Sprintf("+ %d more", restN+hidden)
		if hidden > 0 {
			label += fmt.Sprintf(" (%d below %g%% share)", hidden, o.minShare)
		}
		rrows = append(rrows, append([]string{dim(label)},
			make([]string, len(headers)-1)...))
	}
	var tIn, tOut, tCR, tTok int64
	var tRep, tEst, tEff float64
	var tN, tP int
	for _, g := range gs {
		tN += g.N
		tP += g.Prompts
		tIn += g.In
		tOut += g.Out
		tCR += g.CR
		tTok += tokenSum(g)
		tRep += g.Rep
		tEst += g.Est
		tEff += g.Eff
	}
	var footerCells []string
	if focus {
		fallback := tok(tTok)
		switch o.metric {
		case "cost":
			fallback = money(tEff)
		case "msgs":
			fallback = tok(int64(tN))
		case "prompts":
			fallback = tok(int64(tP))
		case "in":
			fallback = tok(tIn)
		case "out":
			fallback = tok(tOut)
		case "cache":
			fallback = tok(tCR)
		}
		footerCells = append(append([]string{"TOTAL"}, make([]string, len(dims)-1)...),
			bold(white(fallback)))
	} else {
		footerCells = append(append([]string{"TOTAL"}, make([]string, len(dims)-1)...),
			tok(int64(tN)), tok(int64(len(totalSessions))), tok(tIn), tok(tOut), tok(tCR),
			pct(cacheRatio(tCR, tIn, 0)), bold(yellow(money(tRep))), bold(green(money(tEst))),
			bold(white(money(tEff))), bold("100%"))
		if o.details {
			footerCells = append(footerCells, "", "", "", "", "", "", "", "")
		}
		if len(prevMap) > 0 {
			footerCells = append(footerCells, "", "", "")
		}
	}
	note := "Σ = reported where > 0, else estimated · — = no pricing data available"
	if o.minShare > 0 {
		note += fmt.Sprintf(" · %d group(s) below %g%% share hidden", hidden, o.minShare)
	}
	renderTable(w, title, headers, aligns, rrows,
		[][]string{footerCells}, note)
	fmt.Println()
	return nil
}

func csvQuote(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func cmdSessions(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options) error {
	gs := groupRows(rows, []string{"session"}, p, o.promptRows)
	sortGroups(gs, firstNonEmpty(o.sortBy, "cost"))
	totalEff := 0.0
	for _, g := range gs {
		totalEff += g.Eff
	}
	if o.limit > 0 && len(gs) > o.limit {
		gs = gs[:o.limit]
	}
	project := map[string]string{}
	for i := range rows {
		if _, ok := project[rows[i].Session]; !ok {
			project[rows[i].Session] = rows[i].Project
		}
	}
	if o.jsonOut {
		var out []jrow
		for _, g := range gs {
			r := jrow{"session": g.Key[0], "project": project[g.Key[0]], "n": g.N,
				"prompts": g.Prompts, "sessions": len(g.Sessions),
				"tokens":   tokenSum(g), "cost_rep": round6(g.Rep),
				"cost_est": round6(g.Est), "cost_eff": round6(g.Eff)}
			out = append(out, r)
		}
		b, _ := json.MarshalIndent(map[string]any{"rows": out}, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	if o.csvOut {
		fmt.Fprintln(w, "session,project,n,prompts,sessions,tokens,cost_rep,cost_est,cost_eff")
		for _, g := range gs {
			fmt.Fprintf(w, "%s,%s,%d,%d,%d,%d,%.6f,%.6f,%.6f\n",
				csvQuote(g.Key[0]), csvQuote(project[g.Key[0]]), g.N, g.Prompts,
				len(g.Sessions), tokenSum(g), g.Rep, g.Est, g.Eff)
		}
		return nil
	}
	var rrows [][]string
	for _, g := range gs {
		sid := strings.TrimPrefix(g.Key[0], "ses_")
		if len(sid) > 14 {
			sid = sid[:14]
		}
		repC := yellow(money(g.Rep))
		if g.Rep == 0 {
			repC = dim(repC)
		}
		estC := green(money(g.Est))
		share := 0.0
		if totalEff > 0 {
			share = g.Eff / totalEff
		}
		rrows = append(rrows, []string{dim(sid), truncate(project[g.Key[0]], 36),
			tok(int64(g.N)), tok(int64(g.Prompts)), tok(tokenSum(g)), repC, estC,
			bold(white(money(g.Eff))), pct(share)})
	}
	var tRep, tEff float64
	var tN, tP int
	var tToks int64
	for _, g := range gs {
		tN += g.N
		tP += g.Prompts
		tToks += tokenSum(g)
		tRep += g.Rep
		tEff += g.Eff
	}
	renderTable(w, "Sessions",
		[]string{"Session", "Project", "Msgs", "Prompts", "Tokens",
			"Rep $", "Est $", "Σ $", "Share"}, "llrrrrrrr",
		rrows, [][]string{{"TOTAL", "", tok(int64(tN)), tok(int64(tP)), tok(tToks),
			bold(yellow(money(tRep))), "", bold(white(money(tEff))), ""}},
		fmt.Sprintf("top %d sessions · Σ = reported where > 0, else estimated", len(gs)))
	fmt.Println()
	return nil
}

// ---------------------------------------------------------------------------
// pivot
// ---------------------------------------------------------------------------

type pivotMetric struct {
	label string
	value func(*Group) float64
	money bool
}

var pivotMetrics = map[string]pivotMetric{
	"tokens":  {"Tokens", func(g *Group) float64 { return float64(tokenSum(g)) }, false},
	"in":      {"Tok in", func(g *Group) float64 { return float64(g.In) }, false},
	"out":     {"Tok out", func(g *Group) float64 { return float64(g.Out) }, false},
	"cache":   {"Cache hits", func(g *Group) float64 { return float64(g.CR) }, false},
	"msgs":    {"Msgs", func(g *Group) float64 { return float64(g.N) }, false},
	"prompts": {"Prompts", func(g *Group) float64 { return float64(g.Prompts) }, false},
	"cost":    {"Σ $", func(g *Group) float64 { return g.Eff }, true},
}

func cmdPivot(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options) error {
	rowdim, coldim := o.rows, o.cols
	valid := func(d string) bool {
		for _, v := range dimensions {
			if d == v {
				return true
			}
		}
		return false
	}
	if !valid(rowdim) || !valid(coldim) {
		return fmt.Errorf("--rows/--cols must be one of: %s", strings.Join(dimensions, " "))
	}
	if rowdim == coldim {
		return errors.New("--rows and --cols must differ")
	}
	if o.metric == "" {
		o.metric = "tokens"
	}
	pm, ok := pivotMetrics[o.metric]
	if !ok {
		return errors.New("--metric must be one of: tokens in out cache msgs prompts cost")
	}

	// auto-bucket wide day/week column ranges
	effColdim := coldim
	countDistinct := func(dim string) int {
		seen := map[string]bool{}
		for i := range rows {
			seen[dimValue(&rows[i], dim)] = true
		}
		return len(seen)
	}
	if coldim == "day" && countDistinct("day") > 14 {
		effColdim = "week"
		if countDistinct("week") > 14 {
			effColdim = "month"
		}
	} else if coldim == "week" && countDistinct("week") > 14 {
		effColdim = "month"
	}

	groups := groupRows(rows, []string{rowdim, effColdim}, p, o.promptRows)
	cell := map[[2]string]float64{}
	rowTotals := map[string]float64{}
	colTotals := map[string]float64{}
	var grand float64
	for _, g := range groups {
		v := pm.value(g)
		cell[[2]string{g.Key[0], g.Key[1]}] = v
		rowTotals[g.Key[0]] += v
		colTotals[g.Key[1]] += v
		grand += v
	}
	mx := 0.0
	for _, v := range cell {
		if v > mx {
			mx = v
		}
	}
	colKeys := make([]string, 0, len(colTotals))
	for k := range colTotals {
		colKeys = append(colKeys, k)
	}
	sort.Strings(colKeys)
	rowKeys := make([]string, 0, len(rowTotals))
	for k := range rowTotals {
		rowKeys = append(rowKeys, k)
	}
	sort.Slice(rowKeys, func(i, j int) bool { return rowTotals[rowKeys[i]] > rowTotals[rowKeys[j]] })

	colLabel := func(k string) string {
		if (effColdim == "day" || effColdim == "week") && len(k) == 10 {
			return k[5:]
		}
		if (effColdim == "week" || effColdim == "month") && len(k) == 7 {
			return k[2:]
		}
		return k
	}
	fmtv := func(v float64, heat bool) string {
		s := tok(int64(math.Round(v)))
		if pm.money {
			s = money(v)
		}
		if heat && useColor && v > 0 && mx > 0 {
			q := v / mx
			switch {
			case q >= 0.75:
				s = bold(white(s))
			case q >= 0.5:
				s = white(s)
			case q >= 0.25:
				s = green(s)
			default:
				s = dim(s)
			}
		}
		return s
	}

	if o.jsonOut {
		type cellj struct {
			Row   string  `json:"row"`
			Col   string  `json:"col"`
			Value float64 `json:"value"`
			Share float64 `json:"share"`
		}
		var cells []cellj
		for _, rk := range rowKeys {
			for _, ck := range colKeys {
				if v, ok := cell[[2]string{rk, ck}]; ok && v != 0 {
					share := 0.0
					if grand > 0 {
						share = v / grand
					}
					cells = append(cells, cellj{rk, ck, round6(v), share})
				}
			}
		}
		b, _ := json.MarshalIndent(map[string]any{
			"rows": cells, "row_totals": rowTotals, "col_totals": colTotals,
			"grand_total": round6(grand)}, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	if o.csvOut {
		fmt.Printf("%s,%s,%s\n", rowdim, effColdim, o.metric)
		for _, g := range groups {
			fmt.Printf("%s,%s,%.6f\n", csvQuote(g.Key[0]), csvQuote(g.Key[1]), pm.value(g))
		}
		return nil
	}

	pricingHint(p)
	fmt.Println()
	title := fmt.Sprintf("%s — %s × %s", pm.label, rowdim, effColdim)
	if effColdim != coldim {
		title += fmt.Sprintf(" (%s bucketed → %s)", coldim, effColdim)
	}
	if mdMode {
		fmt.Printf("### %s\n\n", title)
	} else {
		fmt.Printf("  %s  %s\n", bold(cyan(title)), dim("· "+flt.Describe()))
	}

	headers := []string{strings.ToUpper(rowdim[:1]) + rowdim[1:]}
	for _, k := range colKeys {
		headers = append(headers, colLabel(k))
	}
	headers = append(headers, "Total")
	var rrows [][]string
	for _, rk := range rowKeys {
		row := []string{truncate(rk, 28)}
		for _, ck := range colKeys {
			row = append(row, fmtv(cell[[2]string{rk, ck}], true))
		}
		row = append(row, bold(fmtv(rowTotals[rk], false)))
		rrows = append(rrows, row)
	}
	footer := []string{bold("TOTAL")}
	for _, ck := range colKeys {
		footer = append(footer, bold(fmtv(colTotals[ck], false)))
	}
	footer = append(footer, bold(white(fmtv(grand, false))))
	renderTable(w, "", headers, "l"+strings.Repeat("r", len(colKeys)+1), rrows,
		[][]string{footer},
		fmt.Sprintf("heat: dim <25%% <50%% <75%% ≤max of %s · metric: %s",
			strings.ToLower(pm.label), o.metric))
	fmt.Println()
	return nil
}

func cmdPrices(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options) error {
	type combo struct{ prov, model string }
	var combos []combo
	seen := map[combo]bool{}
	for i := range rows {
		c := combo{rows[i].Provider, rows[i].Model}
		if !seen[c] {
			seen[c] = true
			combos = append(combos, c)
		}
	}
	sort.Slice(combos, func(i, j int) bool {
		if combos[i].prov != combos[j].prov {
			return combos[i].prov < combos[j].prov
		}
		return combos[i].model < combos[j].model
	})
	if o.csvOut {
		fmt.Fprintln(w, "provider,model,input,output,cache_read,cache_write,source")
		for _, c := range combos {
			r := p.Rates(c.prov, c.model)
			fmt.Fprintf(w, "%s,%s,%g,%g,%g,%g,%s\n",
				csvQuote(c.prov), csvQuote(c.model), r.In, r.Out, r.CacheR, r.CacheW, r.Source)
		}
		return nil
	}
	var rrows [][]string
	for _, c := range combos {
		r := p.Rates(c.prov, c.model)
		src := r.Source
		switch src {
		case "override":
			src = magenta(src)
		case "models.dev":
			src = green(src)
		case "models.dev~":
			src = cyan(src)
		default:
			src = red(src)
		}
		rrows = append(rrows, []string{truncate(c.prov, 22), truncate(c.model, 40),
			fmt.Sprintf("%g", r.In), fmt.Sprintf("%g", r.Out),
			fmt.Sprintf("%g", r.CacheR), fmt.Sprintf("%g", r.CacheW), src})
	}
	renderTable(w, "Pricing table  ·  $ per 1M tokens",
		[]string{"Provider", "Model", "In", "Out", "Cache R", "Cache W", "Source"},
		"llrrrrl", rrows, nil,
		"refresh with: "+prog+" prices --refresh · overrides: "+overridesFile())
	fmt.Println()
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func red(s string) string     { return esc("31", s) }
func magenta(s string) string { return esc("35", s) }
