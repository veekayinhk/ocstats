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
	version      = "1.2.1"
	prog         = "ocstats"
	modelsDevURL = "https://models.dev/api.json"
)

var (
	dowByWeekday = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	dowOrder     = [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	monthAbbr    = [13]string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
)

// ---------------------------------------------------------------------------
// colors
// ---------------------------------------------------------------------------

var useColor = false

func bold(s string) string   { return esc("1", s) }
func dim(s string) string    { return esc("2", s) }
func yellow(s string) string { return esc("33", s) }
func green(s string) string  { return esc("32", s) }
func blue(s string) string   { return esc("34", s) }
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
	Hour      string
	Dow       string
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
	Since, Until    string
	Model, Provider string
	Agent, Project  string
	Session         string
	lo, hi          int64 // epoch ms
	boundedTime     bool
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

type sessionMeta struct {
	dir     string
	project string
}

func loadSessionMap(db *sql.DB) (map[string]sessionMeta, error) {
	rows, err := db.Query(`SELECT id, COALESCE(directory, '') FROM session_v2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]sessionMeta{}
	for rows.Next() {
		var id, dir string
		if err := rows.Scan(&id, &dir); err != nil {
			return nil, err
		}
		proj := "(none)"
		if dir != "" {
			proj = filepath.Base(dir)
		}
		m[id] = sessionMeta{dir: dir, project: proj}
	}
	return m, rows.Err()
}

func buildMessageWhere(mtype string, flt *Filters, smap map[string]sessionMeta) (string, []any, bool) {
	var conds []string
	var args []any
	useSessIdx := false
	if flt.Session != "" {
		last := flt.Session[len(flt.Session)-1]
		hi := flt.Session[:len(flt.Session)-1] + string(last+1)
		conds = append(conds, "sm.session_id >= ? AND sm.session_id < ?")
		args = append(args, flt.Session, hi)
		useSessIdx = true
	} else if flt.Project != "" && !strings.Contains("(none)", strings.ToLower(flt.Project)) {
		needle := strings.ToLower(flt.Project)
		var ph []string
		for sid, sm := range smap {
			if strings.Contains(strings.ToLower(sm.project), needle) || strings.Contains(strings.ToLower(sm.dir), needle) {
				ph = append(ph, "?")
				args = append(args, sid)
			}
		}
		if len(ph) == 0 {
			return "", nil, true
		}
		conds = append(conds, "sm.session_id IN ("+strings.Join(ph, ",")+")")
		useSessIdx = true
	}
	if useSessIdx {
		conds = append(conds, "sm.type = ?")
		args = append(args, mtype)
		if flt.boundedTime {
			conds = append(conds, "sm.time_created >= ? AND sm.time_created < ?")
			args = append(args, flt.lo, flt.hi)
		}
	} else if flt.boundedTime {
		conds = append(conds, "sm.time_created >= ? AND sm.time_created < ? AND sm.type = ?")
		args = append(args, flt.lo, flt.hi, mtype)
	} else {
		conds = append(conds, "+sm.type = ?")
		args = append(args, mtype)
	}
	return strings.Join(conds, " AND "), args, false
}

func fetchRows(db *sql.DB, flt *Filters) ([]Row, error) {
	smap, err := loadSessionMap(db)
	if err != nil {
		return nil, err
	}
	where, qargs, empty := buildMessageWhere("assistant", flt, smap)
	if empty {
		return nil, nil
	}
	q := `
SELECT sm.session_id,
       sm.time_created,
       COALESCE(json_extract(sm.data, '$.model.providerID'), 'unknown'),
       COALESCE(json_extract(sm.data, '$.model.id'), 'unknown'),
       COALESCE(json_extract(sm.data, '$.agent'), 'unknown'),
       COALESCE(json_extract(sm.data, '$.tokens.input'), 0),
       COALESCE(json_extract(sm.data, '$.tokens.output'), 0),
       COALESCE(json_extract(sm.data, '$.tokens.reasoning'), 0),
       COALESCE(json_extract(sm.data, '$.tokens.cache.read'), 0),
       COALESCE(json_extract(sm.data, '$.tokens.cache.write'), 0),
       COALESCE(json_extract(sm.data, '$.cost'), 0.0)
FROM session_message AS sm
WHERE ` + where + `
ORDER BY sm.time_created`
	rows, err := db.Query(q, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mNeedle := strings.ToLower(flt.Model)
	pNeedle := strings.ToLower(flt.Provider)
	aNeedle := strings.ToLower(flt.Agent)
	projNeedle := strings.ToLower(flt.Project)

	var out []Row
	for rows.Next() {
		var sid, prov, mdl, agt string
		var ts, inT, outT, reasT, crT, cwT int64
		var cost float64
		if err := rows.Scan(&sid, &ts, &prov, &mdl, &agt, &inT, &outT, &reasT, &crT, &cwT, &cost); err != nil {
			return nil, err
		}
		if prov == "" {
			prov = "unknown"
		}
		if mdl == "" {
			mdl = "unknown"
		}
		if agt == "" {
			agt = "unknown"
		}
		if mNeedle != "" && !strings.Contains(strings.ToLower(mdl), mNeedle) {
			continue
		}
		if pNeedle != "" && !strings.Contains(strings.ToLower(prov), pNeedle) {
			continue
		}
		if aNeedle != "" && !strings.Contains(strings.ToLower(agt), aNeedle) {
			continue
		}
		sm, ok := smap[sid]
		if !ok {
			sm = sessionMeta{dir: "", project: "(none)"}
		}
		if projNeedle != "" && !strings.Contains(strings.ToLower(sm.project+"|"+sm.dir), projNeedle) {
			continue
		}
		if flt.Session != "" && !strings.HasPrefix(sid, flt.Session) {
			continue
		}
		t := time.UnixMilli(ts)
		out = append(out, Row{
			TS:        t,
			Day:       t.Format("2006-01-02"),
			Week:      mondayStart(t).Format("2006-01-02"),
			Month:     t.Format("2006-01"),
			Hour:      t.Format("15"),
			Dow:       dowByWeekday[int(t.Weekday())],
			Provider:  prov,
			Model:     mdl,
			Agent:     agt,
			Session:   sid,
			Directory: sm.dir,
			Project:   sm.project,
			In:        inT,
			Out:       outT,
			Reason:    reasT,
			CacheR:    crT,
			CacheW:    cwT,
			Cost:      cost,
		})
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
	smap, err := loadSessionMap(db)
	if err != nil {
		return nil, err
	}
	where, qargs, empty := buildMessageWhere("user", flt, smap)
	if empty {
		return nil, nil
	}
	q := `SELECT sm.session_id, sm.time_created FROM session_message AS sm WHERE ` + where + ` ORDER BY sm.time_created`
	rows, err := db.Query(q, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projNeedle := strings.ToLower(flt.Project)
	var out []Prompt
	for rows.Next() {
		var sid string
		var ts int64
		if err := rows.Scan(&sid, &ts); err != nil {
			return nil, err
		}
		if flt.Session != "" && !strings.HasPrefix(sid, flt.Session) {
			continue
		}
		if projNeedle != "" {
			sm := smap[sid]
			if !strings.Contains(strings.ToLower(sm.project+"|"+sm.dir), projNeedle) {
				continue
			}
		}
		out = append(out, Prompt{TS: time.UnixMilli(ts), Session: sid})
	}
	return out, rows.Err()
}

func attributePrompts(prompts []Prompt, rows []Row, flt *Filters) []Prompt {
	bySession := map[string][]*Row{}
	for i := range rows {
		bySession[rows[i].Session] = append(bySession[rows[i].Session], &rows[i])
	}
	hasMPA := flt != nil && (flt.Model != "" || flt.Provider != "" || flt.Agent != "")
	var kept []Prompt
	for i := range prompts {
		p := prompts[i]
		msgs := bySession[p.Session]
		if len(msgs) == 0 {
			if hasMPA {
				continue
			}
			p.Provider, p.Model, p.Agent, p.Project = "unknown", "unknown", "unknown", "(none)"
			kept = append(kept, p)
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
		kept = append(kept, p)
	}
	return kept
}

// ---------------------------------------------------------------------------
// aggregation
// ---------------------------------------------------------------------------

var dimensions = []string{"model", "provider", "agent", "project", "session", "day", "week", "month", "hour", "dow"}

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
	case "hour":
		return r.Hour
	case "dow":
		return r.Dow
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
			if d == "day" || d == "week" || d == "month" || d == "hour" || d == "dow" {
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

func mondayStart(t time.Time) time.Time {
	return t.AddDate(0, 0, -((int(t.Weekday())+6)%7))
}

func timeDim(t time.Time, dim string) string {
	switch dim {
	case "day":
		return t.Format("2006-01-02")
	case "week":
		return mondayStart(t).Format("2006-01-02")
	case "month":
		return t.Format("2006-01")
	case "hour":
		return t.Format("15")
	case "dow":
		return dowByWeekday[int(t.Weekday())]
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
	s := strconv.FormatInt(n, 10)
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
	period         string
	weeks          int
	split          bool
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
	"monthly": true, "matrix": true, "pivot": true, "heatmap": true,
	"calendar": true, "chart": true, "punchcard": true, "prices": true,
}

func parseArgs(argv []string) (*options, error) {
	o := &options{command: "", limit: 25, rows: "project", cols: "day",
		period: "day", metric: "", monthlyBudget: -1}
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
		case "--period":
			o.period = next()
		case "--weeks":
			v, err := strconv.Atoi(next())
			if err != nil {
				return nil, fmt.Errorf("bad --weeks")
			}
			o.weeks = v
		case "--split":
			o.split = true
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
  heatmap   calendar heatmap (--by model/provider, --split)
  chart     stacked ASCII/Unicode bar chart over time (--period, --by)
  punchcard 7×24 (Mon–Sun × 00–23h) circadian activity grid
  prices    show resolved pricing table (--refresh re-fetches models.dev)

aggregation & chart examples:
  ocstats-bin                                summary of everything
  ocstats-bin heatmap                        calendar heatmap over weeks
  ocstats-bin heatmap --by model             calendar color-coded by dominant model
  ocstats-bin heatmap --by provider --split  stacked mini-calendars per provider
  ocstats-bin chart --period day --by model  stacked daily bar chart by model
  ocstats-bin punchcard --by model           7×24 circadian grid by model
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

dimensions for matrix --by / pivot --rows / --cols / heatmap --by / chart --by:
  model, provider, agent, project, session, day, week, month, hour, dow
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
		boundedTime: o.since != "" || o.until != "" || o.compare != "",
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
	_, _ = db.Exec(`PRAGMA temp_store = MEMORY; PRAGMA cache_size = -64000; PRAGMA mmap_size = 536870912;`)

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
	o.promptRows = attributePrompts(prompts, rows, flt)
	if o.prev != nil {
		pflt := &Filters{Since: o.compare, Until: o.compare, Model: o.model,
			Provider: o.provider, Agent: o.agent, Project: o.project,
			Session: o.session, lo: o.prev.lo, hi: o.prev.hi, boundedTime: true}
		pRows, err := fetchRows(db, pflt)
		if err != nil {
			return err
		}
		pPrompts, err := fetchPrompts(db, pflt)
		if err != nil {
			return err
		}
		o.prev.rows = pRows
		o.prev.prompts = attributePrompts(pPrompts, pRows, pflt)
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
	case "heatmap", "calendar":
		return cmdHeatmap(os.Stdout, rows, flt, p, o)
	case "chart":
		return cmdChart(os.Stdout, rows, flt, p, o)
	case "punchcard":
		return cmdPunchcard(os.Stdout, rows, flt, p, o)
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

// ---------------------------------------------------------------------------
// heatmap / calendar, chart, punchcard
// ---------------------------------------------------------------------------

type seriesStyle struct {
	color func(string) string
	block string
	sym   string
}

var seriesPalette = []seriesStyle{
	{cyan, "█", "@"},
	{magenta, "▓", "#"},
	{yellow, "▒", "+"},
	{green, "░", "*"},
	{blue, "▪", "="},
}

var otherSeriesStyle = seriesStyle{white, "▫", "~"}

func heatBlock(v, mx float64) string {
	if v <= 0 || mx <= 0 {
		return "·"
	}
	q := v / mx
	switch {
	case q > 0.75:
		return "█"
	case q > 0.50:
		return "▓"
	case q > 0.25:
		return "▒"
	default:
		return "░"
	}
}

func colorHeatBlock(v, mx float64, cfn func(string) string, asym string) string {
	if v <= 0 || mx <= 0 {
		return dim("·")
	}
	b := heatBlock(v, mx)
	q := v / mx
	if cfn != nil {
		if !useColor && asym != "" {
			return asym
		}
		if q > 0.75 {
			return bold(cfn(b))
		}
		return cfn(b)
	}
	switch {
	case q > 0.75:
		return bold(cyan("█"))
	case q > 0.50:
		return cyan("▓")
	case q > 0.25:
		return green("▒")
	default:
		return green("░")
	}
}

func validDim(d string) bool {
	for _, v := range dimensions {
		if d == v {
			return true
		}
	}
	return false
}

func cmdHeatmap(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options) error {
	metric := firstNonEmpty(o.metric, "tokens")
	pm, ok := pivotMetrics[metric]
	if !ok {
		return errors.New("--metric must be one of: tokens in out cache msgs prompts cost")
	}
	byDim := strings.TrimSpace(o.by)
	if byDim != "" && !validDim(byDim) {
		return fmt.Errorf("unknown --by dimension %q", byDim)
	}
	fmtv := func(v float64) string {
		if pm.money {
			return money(v)
		}
		return tok(int64(math.Round(v)))
	}

	dayGroups := groupRows(rows, []string{"day"}, p, o.promptRows)
	if len(dayGroups) == 0 {
		return errors.New("no activity matched the current filters")
	}
	dayVal := map[string]float64{}
	var allDays []string
	var grand float64
	for _, g := range dayGroups {
		v := pm.value(g)
		dayVal[g.Key[0]] = v
		allDays = append(allDays, g.Key[0])
		grand += v
	}
	sort.Strings(allDays)

	minDT, _ := time.ParseInLocation("2006-01-02", allDays[0], time.Local)
	maxDT, _ := time.ParseInLocation("2006-01-02", allDays[len(allDays)-1], time.Local)
	firstMon := mondayStart(minDT)
	lastMon := mondayStart(maxDT)
	numWeeks := int(lastMon.Sub(firstMon).Hours()/(24*7)) + 1
	if o.weeks > 0 {
		numWeeks = o.weeks
		firstMon = lastMon.AddDate(0, 0, -(numWeeks-1)*7)
	} else if numWeeks < 16 && o.since == "" {
		numWeeks = 16
		firstMon = lastMon.AddDate(0, 0, -15*7)
	} else if numWeeks > 52 {
		numWeeks = 52
		firstMon = lastMon.AddDate(0, 0, -51*7)
	}

	spanDays := int(maxDT.Sub(minDT).Hours()/24) + 1
	activeDays := 0
	longestStreak, curStreak, run := 0, 0, 0
	peakDay := allDays[0]
	peakVal := -1.0
	dowTotals := map[string]float64{}
	for d := minDT; !d.After(maxDT); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		v := dayVal[ds]
		if v > 0 {
			activeDays++
			run++
			if run > longestStreak {
				longestStreak = run
			}
		} else {
			run = 0
		}
		if v > peakVal {
			peakVal = v
			peakDay = ds
		}
		dowTotals[dowByWeekday[int(d.Weekday())]] += v
	}
	curStreak = run
	busiestDow := "Mon"
	for _, d := range dowOrder {
		if dowTotals[d] > dowTotals[busiestDow] {
			busiestDow = d
		}
	}
	mx := peakVal
	if mx <= 0 {
		mx = 1
	}

	byDaySeries := map[string]map[string]float64{}
	seriesTotals := map[string]float64{}
	var topSeries []string
	styleMap := map[string]seriesStyle{}
	if byDim != "" {
		sg := groupRows(rows, []string{"day", byDim}, p, o.promptRows)
		for _, g := range sg {
			v := pm.value(g)
			dKey, sKey := g.Key[0], g.Key[1]
			if byDaySeries[dKey] == nil {
				byDaySeries[dKey] = map[string]float64{}
			}
			byDaySeries[dKey][sKey] += v
			seriesTotals[sKey] += v
		}
		for s := range seriesTotals {
			topSeries = append(topSeries, s)
		}
		sort.Slice(topSeries, func(i, j int) bool { return seriesTotals[topSeries[i]] > seriesTotals[topSeries[j]] })
		capN := 5
		if o.split {
			if o.topN > 0 {
				capN = o.topN
			} else if o.limit != 25 && o.limit > 0 {
				capN = o.limit
			}
		}
		if len(topSeries) > capN {
			topSeries = topSeries[:capN]
		}
		for i, s := range topSeries {
			styleMap[s] = seriesPalette[i%len(seriesPalette)]
		}
	}

	domFor := func(ds string) string {
		bk := byDaySeries[ds]
		bestS := ""
		bestV := -1.0
		for s, v := range bk {
			if v > bestV && v > 0 {
				bestV = v
				bestS = s
			}
		}
		return bestS
	}

	if o.jsonOut {
		var daysOut []map[string]any
		for _, ds := range allDays {
			t, _ := time.ParseInLocation("2006-01-02", ds, time.Local)
			entry := map[string]any{
				"day": ds, "dow": dowByWeekday[int(t.Weekday())], "value": round6(dayVal[ds]),
			}
			if byDim != "" {
				entry["dominant"] = domFor(ds)
				entry["breakdown"] = byDaySeries[ds]
			}
			daysOut = append(daysOut, entry)
		}
		b, _ := json.MarshalIndent(map[string]any{
			"metric": metric, "by": byDim, "weeks": numWeeks,
			"stats": map[string]any{
				"total": round6(grand), "active_days": activeDays, "span_days": spanDays,
				"longest_streak": longestStreak, "current_streak": curStreak,
				"peak_day": peakDay, "peak_value": round6(peakVal), "busiest_dow": busiestDow,
			},
			"days": daysOut,
		}, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	if o.csvOut {
		hdr := "day,dow,week," + metric
		if byDim != "" {
			hdr += ",dominant_" + byDim
		}
		fmt.Fprintln(w, hdr)
		for _, ds := range allDays {
			t, _ := time.ParseInLocation("2006-01-02", ds, time.Local)
			wk := mondayStart(t).Format("2006-01-02")
			line := fmt.Sprintf("%s,%s,%s,%.6f", ds, dowByWeekday[int(t.Weekday())], wk, dayVal[ds])
			if byDim != "" {
				line += "," + csvQuote(domFor(ds))
			}
			fmt.Fprintln(w, line)
		}
		return nil
	}

	renderCal := func(lookup map[string]float64, maxV float64, fixed *seriesStyle, useDom bool) {
		mChars := make([]rune, numWeeks*2)
		for i := range mChars {
			mChars[i] = ' '
		}
		lastM := -1
		lastPos := -4
		for wi := 0; wi < numWeeks; wi++ {
			wkMon := firstMon.AddDate(0, 0, wi*7)
			mNum := int(wkMon.Month())
			for off := 0; off < 7; off++ {
				cand := wkMon.AddDate(0, 0, off)
				if cand.Day() == 1 && !cand.After(maxDT) {
					mNum = int(cand.Month())
					break
				}
			}
			pos := wi * 2
			if mNum != lastM && pos-lastPos >= 4 {
				for ci, r := range monthAbbr[mNum] {
					if pos+ci < len(mChars) {
						mChars[pos+ci] = r
					}
				}
				lastM = mNum
				lastPos = pos
			}
		}
		fmt.Fprintln(w, "       "+dim(strings.TrimRight(string(mChars), " ")))
		for wdIdx, wdName := range dowOrder {
			var b strings.Builder
			b.WriteString("  " + dim(wdName) + "  ")
			for wi := 0; wi < numWeeks; wi++ {
				curD := firstMon.AddDate(0, 0, wi*7+wdIdx)
				if curD.After(maxDT) {
					b.WriteString("  ")
					continue
				}
				ds := curD.Format("2006-01-02")
				v := lookup[ds]
				if fixed != nil {
					b.WriteString(colorHeatBlock(v, maxV, fixed.color, "") + " ")
				} else if useDom && v > 0 {
					st, ok := styleMap[domFor(ds)]
					if !ok {
						st = otherSeriesStyle
					}
					b.WriteString(colorHeatBlock(v, maxV, st.color, st.sym) + " ")
				} else {
					b.WriteString(colorHeatBlock(v, maxV, nil, "") + " ")
				}
			}
			fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
		}
	}

	pricingHint(p)
	title := fmt.Sprintf("%s · Calendar Heatmap (%dw)", pm.label, numWeeks)
	if byDim != "" {
		if o.split {
			title += " · split by " + byDim
		} else {
			title += " · dominant " + byDim
		}
	}
	fmt.Fprintln(w)
	if mdMode {
		fmt.Fprintf(w, "### %s\n\n```\n\n", title)
	} else {
		fmt.Fprintf(w, "  %s  %s\n\n", bold(cyan(title)), dim("· "+flt.Describe()))
	}

	if byDim != "" && o.split {
		for i, sName := range topSeries {
			st := seriesPalette[i%len(seriesPalette)]
			sDays := map[string]float64{}
			sMax := 0.0
			for ds, bk := range byDaySeries {
				if sv := bk[sName]; sv > 0 {
					sDays[ds] = sv
					if sv > sMax {
						sMax = sv
					}
				}
			}
			if sMax <= 0 {
				sMax = 1
			}
			sw := st.sym
			if useColor {
				sw = st.color(st.block)
			}
			sh := 0.0
			if grand > 0 {
				sh = seriesTotals[sName] / grand
			}
			fmt.Fprintf(w, "  %s %s  %s\n", sw, bold(sName),
				dim(fmt.Sprintf("%s (%s) · %d active days · peak %s", fmtv(seriesTotals[sName]), pct(sh), len(sDays), fmtv(sMax))))
			renderCal(sDays, sMax, &st, false)
			fmt.Fprintln(w)
		}
	} else {
		renderCal(dayVal, mx, nil, byDim != "")
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s %s 0   %s <25%%   %s 25-50%%   %s 50-75%%   %s >75%% (peak %s)\n",
			dim("Legend:"), dim("·"),
			colorHeatBlock(0.15*mx, mx, nil, ""),
			colorHeatBlock(0.40*mx, mx, nil, ""),
			colorHeatBlock(0.65*mx, mx, nil, ""),
			colorHeatBlock(0.90*mx, mx, nil, ""),
			fmtv(peakVal))
		if byDim != "" && len(topSeries) > 0 {
			wins := map[string]int{}
			for _, ds := range allDays {
				if dayVal[ds] > 0 {
					wins[domFor(ds)]++
				}
			}
			var parts []string
			for _, sName := range topSeries {
				st := styleMap[sName]
				sw := st.sym
				if useColor {
					sw = st.color("█")
				}
				sh := 0.0
				if grand > 0 {
					sh = seriesTotals[sName] / grand
				}
				parts = append(parts, fmt.Sprintf("%s %s %s", sw, bold(truncate(sName, 22)), dim(fmt.Sprintf("%s (%dd)", pct(sh), wins[sName]))))
			}
			fmt.Fprintf(w, "  %s %s\n", dim(strings.ToUpper(byDim[:1])+byDim[1:]+":"), strings.Join(parts, "   "))
		}
	}
	shActive := 0.0
	if spanDays > 0 {
		shActive = float64(activeDays) / float64(spanDays)
	}
	fmt.Fprintf(w, "  %s  total %s  ·  active %s (%s)  ·  peak %s (%s)  ·  busiest %s  ·  streak %s (best %dd)\n",
		dim("Stats:"), bold(fmtv(grand)), bold(fmt.Sprintf("%d/%dd", activeDays, spanDays)), pct(shActive),
		bold(peakDay), fmtv(peakVal), bold(busiestDow), bold(fmt.Sprintf("%dd", curStreak)), longestStreak)
	if mdMode {
		fmt.Fprintln(w, "```")
	}
	fmt.Fprintln(w)
	return nil
}

func cmdChart(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options) error {
	period := firstNonEmpty(o.period, "day")
	switch period {
	case "date", "daily":
		period = "day"
	case "weekday":
		period = "dow"
	case "hourly":
		period = "hour"
	}
	if period != "day" && period != "week" && period != "month" && period != "hour" && period != "dow" {
		return errors.New("--period must be one of: day week month hour dow")
	}
	byDim := strings.TrimSpace(o.by)
	if byDim != "" {
		if !validDim(byDim) {
			return fmt.Errorf("unknown --by dimension %q", byDim)
		}
		if byDim == period {
			return errors.New("--period and --by must differ")
		}
	}
	metric := firstNonEmpty(o.metric, "tokens")
	pm, ok := pivotMetrics[metric]
	if !ok {
		return errors.New("--metric must be one of: tokens in out cache msgs prompts cost")
	}
	fmtv := func(v float64) string {
		if pm.money {
			return money(v)
		}
		return tok(int64(math.Round(v)))
	}

	pgroups := groupRows(rows, []string{period}, p, o.promptRows)
	if len(pgroups) == 0 {
		return errors.New("no activity matched the current filters")
	}
	if period == "dow" {
		dowRank := map[string]int{"Mon": 0, "Tue": 1, "Wed": 2, "Thu": 3, "Fri": 4, "Sat": 5, "Sun": 6}
		sort.SliceStable(pgroups, func(i, j int) bool { return dowRank[pgroups[i].Key[0]] < dowRank[pgroups[j].Key[0]] })
	} else if period == "hour" {
		sort.SliceStable(pgroups, func(i, j int) bool { return pgroups[i].Key[0] < pgroups[j].Key[0] })
	} else {
		sortGroups(pgroups, firstNonEmpty(o.sortBy, "date-desc"))
	}
	if o.bottomN > 0 {
		for i, j := 0, len(pgroups)-1; i < j; i, j = i+1, j-1 {
			pgroups[i], pgroups[j] = pgroups[j], pgroups[i]
		}
	}
	grand := 0.0
	for _, g := range pgroups {
		grand += pm.value(g)
	}
	if grand <= 0 {
		grand = 1
	}
	limit := o.limit
	if o.topN > 0 {
		limit = o.topN
	} else if o.bottomN > 0 {
		limit = o.bottomN
	}
	shown := pgroups
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	mx := 0.0
	for _, g := range shown {
		if v := pm.value(g); v > mx {
			mx = v
		}
	}
	if mx <= 0 {
		mx = 1
	}

	byCell := map[string]map[string]float64{}
	seriesTotals := map[string]float64{}
	var topSeries []string
	styleMap := map[string]seriesStyle{}
	if byDim != "" {
		sg := groupRows(rows, []string{period, byDim}, p, o.promptRows)
		for _, g := range sg {
			v := pm.value(g)
			pKey, sKey := g.Key[0], g.Key[1]
			if byCell[pKey] == nil {
				byCell[pKey] = map[string]float64{}
			}
			byCell[pKey][sKey] += v
			seriesTotals[sKey] += v
		}
		for s := range seriesTotals {
			topSeries = append(topSeries, s)
		}
		sort.Slice(topSeries, func(i, j int) bool { return seriesTotals[topSeries[i]] > seriesTotals[topSeries[j]] })
		if len(topSeries) > 5 {
			topSeries = topSeries[:5]
		}
		for i, s := range topSeries {
			styleMap[s] = seriesPalette[i%len(seriesPalette)]
		}
	}

	if o.jsonOut {
		var outRows []map[string]any
		for _, g := range shown {
			pk := g.Key[0]
			v := pm.value(g)
			rec := map[string]any{period: pk, "value": round6(v), "share": round6(v / grand)}
			if byDim != "" {
				rec["breakdown"] = byCell[pk]
			}
			outRows = append(outRows, rec)
		}
		b, _ := json.MarshalIndent(map[string]any{
			"period": period, "by": byDim, "metric": metric,
			"grand_total": round6(grand), "rows": outRows,
		}, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	if o.csvOut {
		fmt.Fprintf(w, "%s,%s,share\n", period, metric)
		for _, g := range shown {
			v := pm.value(g)
			fmt.Fprintf(w, "%s,%.6f,%.6f\n", csvQuote(g.Key[0]), v, v/grand)
		}
		return nil
	}

	pricingHint(p)
	barW := 36
	title := fmt.Sprintf("%s over %s", pm.label, period)
	if byDim != "" {
		title += " · stacked by " + byDim
	}
	headers := []string{strings.ToUpper(period[:1]) + period[1:], "Chart", pm.label, "Share"}
	aligns := "llrr"
	if byDim != "" {
		headers = append(headers, "Top "+byDim)
		aligns += "l"
	}

	var rrows [][]string
	totShown := 0.0
	for _, g := range shown {
		pk := g.Key[0]
		v := pm.value(g)
		totShown += v
		filled := int(math.Round(v / mx * float64(barW)))
		if v > 0 && filled == 0 {
			filled = 1
		}
		if byDim == "" {
			bStr := cyan(strings.Repeat("█", filled)) + dim(strings.Repeat("░", barW-filled))
			rrows = append(rrows, []string{bold(pk), bStr, fmtv(v), pct(v / grand)})
		} else {
			bk := byCell[pk]
			type segItem struct {
				val float64
				st  seriesStyle
			}
			var ordered []segItem
			othV := v
			for _, s := range topSeries {
				sv := bk[s]
				othV -= sv
				if sv > 0 {
					ordered = append(ordered, segItem{sv, styleMap[s]})
				}
			}
			if othV > 0 {
				ordered = append(ordered, segItem{othV, otherSeriesStyle})
			}
			rem := filled
			var sb strings.Builder
			for idx, it := range ordered {
				slen := 0
				if idx == len(ordered)-1 {
					slen = rem
				} else if v > 0 {
					slen = int(math.Round(it.val / v * float64(filled)))
					if it.val > 0 && slen == 0 && rem > 0 {
						slen = 1
					}
					if slen > rem {
						slen = rem
					}
				}
				rem -= slen
				ch := it.st.block
				if useColor {
					ch = "█"
				}
				sb.WriteString(it.st.color(strings.Repeat(ch, slen)))
			}
			sb.WriteString(dim(strings.Repeat("·", barW-filled)))
			domS := "—"
			domV := -1.0
			for s, sv := range bk {
				if sv > domV && sv > 0 {
					domV = sv
					domS = s
				}
			}
			domCell := truncate(domS, 18)
			if v > 0 && domV > 0 {
				domCell += " " + dim(pct(domV/v))
			}
			rrows = append(rrows, []string{bold(pk), sb.String(), fmtv(v), pct(v / grand), domCell})
		}
	}
	foot := []string{bold("TOTAL"), "", bold(white(fmtv(totShown))), bold(pct(totShown / grand))}
	if byDim != "" {
		foot = append(foot, "")
	}
	note := ""
	if byDim == "" {
		note = fmt.Sprintf("metric: %s · peak %s: %s", metric, period, fmtv(mx))
	}
	fmt.Fprintln(w)
	renderTable(w, title, headers, aligns, rrows, [][]string{foot}, note)
	if byDim != "" && len(topSeries) > 0 {
		var leg []string
		for _, sName := range topSeries {
			st := styleMap[sName]
			ch := st.block
			if useColor {
				ch = "█"
			}
			leg = append(leg, fmt.Sprintf("%s %s %s", st.color(strings.Repeat(ch, 2)), bold(sName), dim("("+pct(seriesTotals[sName]/grand)+")")))
		}
		if len(seriesTotals) > len(topSeries) {
			ch := otherSeriesStyle.block
			if useColor {
				ch = "█"
			}
			leg = append(leg, fmt.Sprintf("%s %s", otherSeriesStyle.color(strings.Repeat(ch, 2)), dim(fmt.Sprintf("+%d other", len(seriesTotals)-len(topSeries)))))
		}
		fmt.Fprintf(w, "  %s%s\n", dim("Legend: "), strings.Join(leg, "   "))
	}
	fmt.Fprintln(w)
	return nil
}

func cmdPunchcard(w io.Writer, rows []Row, flt *Filters, p *pricing, o *options) error {
	metric := firstNonEmpty(o.metric, "tokens")
	pm, ok := pivotMetrics[metric]
	if !ok {
		return errors.New("--metric must be one of: tokens in out cache msgs prompts cost")
	}
	byDim := strings.TrimSpace(o.by)
	if byDim != "" && !validDim(byDim) {
		return fmt.Errorf("unknown --by dimension %q", byDim)
	}
	fmtv := func(v float64) string {
		if pm.money {
			return money(v)
		}
		return tok(int64(math.Round(v)))
	}

	hours := make([]string, 24)
	for h := 0; h < 24; h++ {
		hours[h] = fmt.Sprintf("%02d", h)
	}
	groups := groupRows(rows, []string{"dow", "hour"}, p, o.promptRows)
	if len(groups) == 0 {
		return errors.New("no activity matched the current filters")
	}
	cell := map[[2]string]float64{}
	dowTot := map[string]float64{}
	hrTot := map[string]float64{}
	grand := 0.0
	mx := 0.0
	peakSlot := [2]string{"Mon", "00"}
	for _, g := range groups {
		v := pm.value(g)
		d, h := g.Key[0], g.Key[1]
		cell[[2]string{d, h}] = v
		dowTot[d] += v
		hrTot[h] += v
		grand += v
		if v > mx {
			mx = v
			peakSlot = [2]string{d, h}
		}
	}
	if grand <= 0 {
		grand = 1
	}
	if mx <= 0 {
		mx = 1
	}
	peakHr := "00"
	for _, h := range hours {
		if hrTot[h] > hrTot[peakHr] {
			peakHr = h
		}
	}
	busiestDow := "Mon"
	for _, d := range dowOrder {
		if dowTot[d] > dowTot[busiestDow] {
			busiestDow = d
		}
	}

	bySlot := map[[2]string]map[string]float64{}
	seriesTotals := map[string]float64{}
	var topSeries []string
	styleMap := map[string]seriesStyle{}
	if byDim != "" {
		sg := groupRows(rows, []string{"dow", "hour", byDim}, p, o.promptRows)
		for _, g := range sg {
			v := pm.value(g)
			slot := [2]string{g.Key[0], g.Key[1]}
			sKey := g.Key[2]
			if bySlot[slot] == nil {
				bySlot[slot] = map[string]float64{}
			}
			bySlot[slot][sKey] += v
			seriesTotals[sKey] += v
		}
		for s := range seriesTotals {
			topSeries = append(topSeries, s)
		}
		sort.Slice(topSeries, func(i, j int) bool { return seriesTotals[topSeries[i]] > seriesTotals[topSeries[j]] })
		if len(topSeries) > 5 {
			topSeries = topSeries[:5]
		}
		for i, s := range topSeries {
			styleMap[s] = seriesPalette[i%len(seriesPalette)]
		}
	}
	slotDom := func(d, h string) string {
		bk := bySlot[[2]string{d, h}]
		bestS := ""
		bestV := -1.0
		for s, v := range bk {
			if v > bestV && v > 0 {
				bestV = v
				bestS = s
			}
		}
		return bestS
	}

	if o.jsonOut {
		var cellsOut []map[string]any
		for _, d := range dowOrder {
			for _, h := range hours {
				if v := cell[[2]string{d, h}]; v > 0 {
					entry := map[string]any{"dow": d, "hour": h, "value": round6(v)}
					if byDim != "" {
						entry["dominant"] = slotDom(d, h)
					}
					cellsOut = append(cellsOut, entry)
				}
			}
		}
		b, _ := json.MarshalIndent(map[string]any{
			"metric": metric, "by": byDim, "grand_total": round6(grand),
			"peak_hour": peakHr + ":00", "peak_slot": peakSlot[0] + " " + peakSlot[1] + ":00",
			"busiest_dow": busiestDow, "cells": cellsOut,
		}, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	if o.csvOut {
		fmt.Fprintln(w, "dow,"+strings.Join(hours, ",")+",TOTAL")
		for _, d := range dowOrder {
			var parts []string
			parts = append(parts, d)
			for _, h := range hours {
				parts = append(parts, fmt.Sprintf("%.6f", cell[[2]string{d, h}]))
			}
			parts = append(parts, fmt.Sprintf("%.6f", dowTot[d]))
			fmt.Fprintln(w, strings.Join(parts, ","))
		}
		return nil
	}

	pricingHint(p)
	title := pm.label + " · 7×24 Activity Punchcard"
	if byDim != "" {
		title += " · dominant " + byDim
	}
	fmt.Fprintln(w)
	if mdMode {
		fmt.Fprintf(w, "### %s\n\n```\n\n", title)
	} else {
		fmt.Fprintf(w, "  %s  %s\n\n", bold(cyan(title)), dim("· "+flt.Describe()))
	}

	totW := len(fmtv(grand))
	if totW < 10 {
		totW = 10
	}
	hdrHours := strings.Join(hours, " ")
	fmt.Fprintf(w, "       %s │ %*s │ Share\n", bold(hdrHours), totW, "Total")
	sep := "  ─────┼" + strings.Repeat("─", len(hdrHours)+2) + "┼" + strings.Repeat("─", totW+2) + "┼───────"
	fmt.Fprintln(w, sep)
	activeSlots := 0
	for _, d := range dowOrder {
		var rowCells []string
		for _, h := range hours {
			v := cell[[2]string{d, h}]
			if v > 0 {
				activeSlots++
			}
			var ch string
			if byDim != "" && v > 0 {
				st, ok := styleMap[slotDom(d, h)]
				if !ok {
					st = otherSeriesStyle
				}
				ch = colorHeatBlock(v, mx, st.color, st.sym)
			} else {
				ch = colorHeatBlock(v, mx, nil, "")
			}
			rowCells = append(rowCells, ch+" ")
		}
		dt := dowTot[d]
		fmt.Fprintf(w, "  %s  │ %s│ %*s │ %5s\n",
			bold(d), strings.Join(rowCells, " "), totW, fmtv(dt), pct(dt/grand))
	}
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w)
	if byDim != "" && len(topSeries) > 0 {
		var parts []string
		for _, sName := range topSeries {
			st := styleMap[sName]
			sw := st.sym
			if useColor {
				sw = st.color("█")
			}
			parts = append(parts, fmt.Sprintf("%s %s (%s)", sw, bold(truncate(sName, 22)), pct(seriesTotals[sName]/grand)))
		}
		fmt.Fprintf(w, "  %s %s\n", dim(strings.ToUpper(byDim[:1])+byDim[1:]+":"), strings.Join(parts, "   "))
	}
	fmt.Fprintf(w, "  %s  peak hour %s (%s)  ·  peak slot %s (%s)  ·  busiest day %s  ·  active slots %s\n",
		dim("Stats:"), bold(peakHr+":00"), fmtv(hrTot[peakHr]),
		bold(peakSlot[0]+" "+peakSlot[1]+":00"), fmtv(cell[peakSlot]),
		bold(busiestDow), bold(fmt.Sprintf("%d/168", activeSlots)))
	if mdMode {
		fmt.Fprintln(w, "```")
	}
	fmt.Fprintln(w)
	return nil
}
