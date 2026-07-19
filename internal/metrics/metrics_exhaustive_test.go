package metrics

// metrics_exhaustive_test.go adds a real Prometheus text-exposition grammar
// validator (a from-scratch parser, since the package depends on nothing) and
// drives it against edge-case recorder states. It complements — and never
// duplicates — the lighter structural checks in metrics_test.go.
//
// The validator asserts the exposition-format contract a Prometheus scraper
// enforces:
//
//   - every non-comment line is `name` or `name{labels}` followed by a single
//     space and a numeric value;
//   - the metric NAME token matches [a-zA-Z_:][a-zA-Z0-9_:]* and can never be
//     injected by a hostile label value (brace-matching respects quoting);
//   - label names match [a-zA-Z_][a-zA-Z0-9_]* and values are correctly
//     escaped (\\ \" \n) with no raw newline or unescaped quote surviving;
//   - # HELP and # TYPE precede every series of their family, and each family
//     carries exactly one TYPE;
//   - for histograms: _bucket/_sum/_count all exist per label-set, the le
//     values are sorted ascending with +Inf last, bucket counts are cumulative
//     and monotonic non-decreasing, and the +Inf bucket equals _count;
//   - series within a family are emitted in stable sorted order.

import (
	"bufio"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// parsedSeries is one fully-parsed exposition series line.
type parsedSeries struct {
	name   string
	labels []label // ordered as emitted
	value  string
	raw    string
}

// labelVal returns the value of label name, or "" and false.
func (p parsedSeries) labelVal(name string) (string, bool) {
	for _, l := range p.labels {
		if l.name == name {
			return l.value, true
		}
	}
	return "", false
}

// labelsExcluding renders the parsed labels (order preserved) minus the named
// one, as a canonical key for grouping histogram series by their base set.
func (p parsedSeries) labelsExcluding(drop string) string {
	var b strings.Builder
	for _, l := range p.labels {
		if l.name == drop {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.name)
		b.WriteByte('=')
		b.WriteString(l.value)
	}
	return b.String()
}

// findClosingBrace returns the index of the '}' that closes the '{' at open,
// treating quoted label values (with \\ and \" escapes) as opaque so a '}'
// inside a value never terminates the label set. Returns -1 if unbalanced.
func findClosingBrace(s string, open int) int {
	inQuote := false
	for i := open + 1; i < len(s); i++ {
		c := s[i]
		if inQuote {
			switch c {
			case '\\':
				i++ // skip the escaped char
			case '"':
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case '}':
			return i
		}
	}
	return -1
}

// parseLabels parses the inner text of a `{...}` label set with full escape
// handling, rejecting anything a scraper would reject.
func parseLabels(s string) ([]label, error) {
	var out []label
	i, n := 0, len(s)
	for i < n {
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, fmt.Errorf("label without '=': %q", s[i:])
		}
		name := s[i : i+eq]
		if !labelNameRE.MatchString(name) {
			return nil, fmt.Errorf("invalid label name %q", name)
		}
		j := i + eq + 1
		if j >= n || s[j] != '"' {
			return nil, fmt.Errorf("label value not double-quoted at %q", s[i:])
		}
		j++ // consume opening quote
		var val strings.Builder
		closed := false
		for j < n {
			c := s[j]
			switch c {
			case '\\':
				if j+1 >= n {
					return nil, fmt.Errorf("dangling escape in label value")
				}
				switch s[j+1] {
				case '\\':
					val.WriteByte('\\')
				case '"':
					val.WriteByte('"')
				case 'n':
					val.WriteByte('\n')
				default:
					return nil, fmt.Errorf("illegal escape \\%c", s[j+1])
				}
				j += 2
				continue
			case '"':
				closed = true
				j++
			case '\n':
				return nil, fmt.Errorf("raw newline inside label value")
			default:
				val.WriteByte(c)
				j++
			}
			if closed {
				break
			}
		}
		if !closed {
			return nil, fmt.Errorf("unterminated label value")
		}
		out = append(out, label{name: name, value: val.String()})
		i = j
		if i < n {
			if s[i] != ',' {
				return nil, fmt.Errorf("expected ',' between labels, got %q", s[i:])
			}
			i++
			if i >= n {
				return nil, fmt.Errorf("trailing comma in label set")
			}
		}
	}
	return out, nil
}

// parseSeriesLine parses one `name{labels} value` (or `name value`) line.
func parseSeriesLine(line string) (parsedSeries, error) {
	ps := parsedSeries{raw: line}
	brace := strings.IndexByte(line, '{')
	space := strings.IndexByte(line, ' ')

	if brace >= 0 && (space < 0 || brace < space) {
		ps.name = line[:brace]
		close := findClosingBrace(line, brace)
		if close < 0 {
			return ps, fmt.Errorf("unbalanced braces: %q", line)
		}
		labels, err := parseLabels(line[brace+1 : close])
		if err != nil {
			return ps, err
		}
		ps.labels = labels
		rest := line[close+1:]
		if !strings.HasPrefix(rest, " ") {
			return ps, fmt.Errorf("no space before value: %q", line)
		}
		ps.value = rest[1:]
	} else {
		if space < 0 {
			return ps, fmt.Errorf("series line has no value: %q", line)
		}
		ps.name = line[:space]
		ps.value = line[space+1:]
	}

	if !metricNameRE.MatchString(ps.name) {
		return ps, fmt.Errorf("invalid metric name %q (possible label injection) in %q", ps.name, line)
	}
	if strings.ContainsAny(ps.value, " {}") {
		return ps, fmt.Errorf("value %q contains stray metric syntax in %q", ps.value, line)
	}
	if _, err := strconv.ParseFloat(ps.value, 64); err != nil {
		return ps, fmt.Errorf("value %q not a float in %q: %v", ps.value, line, err)
	}
	return ps, nil
}

// validateExposition fully validates out as Prometheus text exposition and
// returns the parsed series (in emission order). Any violation fails t.
func validateExposition(t *testing.T, out string) []parsedSeries {
	t.Helper()

	types := map[string]string{}    // family -> declared type
	helped := map[string]bool{}     // family -> saw # HELP
	typeSeen := map[string]bool{}   // family -> saw # TYPE (before its series)
	familyOrder := map[string]int{} // family -> first-series index, for sort check
	var all []parsedSeries

	// familyOf resolves a series name to its declared family, understanding the
	// histogram _bucket/_sum/_count suffixes.
	familyOf := func(name string) (string, string, bool) {
		if typ, ok := types[name]; ok {
			return name, typ, true
		}
		for _, suf := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(name, suf) {
				base := strings.TrimSuffix(name, suf)
				if typ, ok := types[base]; ok && typ == "histogram" {
					return base, typ, true
				}
			}
		}
		return "", "", false
	}

	// First pass: collect all TYPE declarations so suffix resolution works even
	// though (per format) TYPE always precedes the series anyway.
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "# TYPE ") {
			f := strings.Fields(line)
			if len(f) != 4 {
				t.Fatalf("malformed # TYPE line: %q", line)
			}
			if prev, dup := types[f[2]]; dup {
				t.Errorf("duplicate # TYPE for %q (was %q)", f[2], prev)
			}
			types[f[2]] = f[3]
		}
	}

	seriesIdx := 0
	sc = bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			f := strings.SplitN(line, " ", 4)
			if len(f) < 3 {
				t.Fatalf("malformed # HELP line: %q", line)
			}
			helped[f[2]] = true
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			typeSeen[f[2]] = true
			if !helped[f[2]] {
				t.Errorf("# TYPE for %q appeared before its # HELP", f[2])
			}
		case strings.HasPrefix(line, "#"):
			// foreign comment — tolerated, ignored
		default:
			ps, err := parseSeriesLine(line)
			if err != nil {
				t.Fatalf("grammar error: %v", err)
			}
			fam, _, ok := familyOf(ps.name)
			if !ok {
				t.Fatalf("series %q belongs to no declared family (missing TYPE)", ps.name)
			}
			if !typeSeen[fam] {
				t.Errorf("series %q emitted before its family's # TYPE", ps.name)
			}
			if !helped[fam] {
				t.Errorf("series %q emitted without a # HELP for family %q", ps.name, fam)
			}
			if _, seen := familyOrder[fam]; !seen {
				familyOrder[fam] = seriesIdx
			}
			all = append(all, ps)
			seriesIdx++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}

	validateHistograms(t, all)
	return all
}

// validateHistograms checks the cumulative/monotonic/le-sorted/+Inf==count
// contract for every histogram label-set present in the parsed series.
func validateHistograms(t *testing.T, all []parsedSeries) {
	t.Helper()

	type bucket struct {
		le    string
		count uint64
	}
	// key: histogramBase "\x00" baseLabelSet
	buckets := map[string][]bucket{}
	counts := map[string]uint64{}
	sums := map[string]bool{}

	base := func(name, suf string) (string, bool) {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf), true
		}
		return "", false
	}

	for _, ps := range all {
		if b, ok := base(ps.name, "_bucket"); ok {
			le, has := ps.labelVal("le")
			if !has {
				t.Errorf("_bucket series %q missing le label", ps.raw)
				continue
			}
			v, err := strconv.ParseUint(ps.value, 10, 64)
			if err != nil {
				t.Errorf("_bucket value %q not a uint: %v", ps.value, err)
				continue
			}
			key := b + "\x00" + ps.labelsExcluding("le")
			buckets[key] = append(buckets[key], bucket{le: le, count: v})
			continue
		}
		if b, ok := base(ps.name, "_count"); ok {
			v, err := strconv.ParseUint(ps.value, 10, 64)
			if err != nil {
				t.Errorf("_count value %q not a uint: %v", ps.value, err)
				continue
			}
			counts[b+"\x00"+ps.labelsExcluding("le")] = v
			continue
		}
		if b, ok := base(ps.name, "_sum"); ok {
			if _, err := strconv.ParseFloat(ps.value, 64); err != nil {
				t.Errorf("_sum value %q not a float: %v", ps.value, err)
			}
			sums[b+"\x00"+ps.labelsExcluding("le")] = true
		}
	}

	leValue := func(le string) float64 {
		if le == "+Inf" {
			return math.Inf(1)
		}
		f, err := strconv.ParseFloat(le, 64)
		if err != nil {
			t.Errorf("le=%q is not a float", le)
			return math.NaN()
		}
		return f
	}

	for key, bs := range buckets {
		// Every histogram series must have matching _sum and _count.
		if _, ok := counts[key]; !ok {
			t.Errorf("histogram %q has _bucket but no _count", key)
		}
		if !sums[key] {
			t.Errorf("histogram %q has _bucket but no _sum", key)
		}
		// le values must be strictly ascending, +Inf last and present exactly once.
		infSeen := false
		for i, b := range bs {
			if b.le == "+Inf" {
				infSeen = true
				if i != len(bs)-1 {
					t.Errorf("histogram %q: +Inf bucket not last", key)
				}
			}
			if i > 0 {
				if !(leValue(bs[i-1].le) < leValue(b.le)) {
					t.Errorf("histogram %q: le not strictly ascending at %q after %q", key, b.le, bs[i-1].le)
				}
			}
		}
		if !infSeen {
			t.Errorf("histogram %q: missing +Inf bucket", key)
		}
		// Bucket counts must be cumulative & monotonic non-decreasing.
		for i := 1; i < len(bs); i++ {
			if bs[i].count < bs[i-1].count {
				t.Errorf("histogram %q: bucket count decreased %d -> %d at le=%q",
					key, bs[i-1].count, bs[i].count, bs[i].le)
			}
		}
		// +Inf bucket must equal _count, and be >= every finite bucket.
		if len(bs) > 0 {
			last := bs[len(bs)-1]
			if last.le == "+Inf" && last.count != counts[key] {
				t.Errorf("histogram %q: +Inf bucket %d != _count %d", key, last.count, counts[key])
			}
		}
	}
}

// TestExposition_GrammarValid runs the full validator over a populated recorder.
func TestExposition_GrammarValid(t *testing.T) {
	r := New()
	r.RecordRequest("GET", "/v1/messages", 200, 3*time.Millisecond)
	r.RecordRequest("POST", "/v1/messages", 500, 800*time.Millisecond)
	r.RecordRequest("POST", "/v1/messages", 500, 3*time.Second)
	r.RecordRequest("DELETE", "/v1/keys", 204, 12*time.Second) // beyond top bucket -> +Inf only
	r.RecordUpstream("openai", "gpt-4o")
	r.RecordTokens("openai", "gpt-4o", 100, 250)
	r.RecordCache("exact", true)
	r.RecordCache("semantic", false)
	r.IncInFlight()

	validateExposition(t, scrape(r))
}

// TestExposition_EmptyRecorderValid proves a freshly-constructed recorder still
// emits valid exposition: HELP/TYPE for every family and the always-present
// in-flight gauge at 0, with no orphan series.
func TestExposition_EmptyRecorderValid(t *testing.T) {
	r := New()
	out := scrape(r)
	series := validateExposition(t, out)

	// The only series an empty recorder emits is the gauge (counters/histograms
	// have no observations yet).
	if len(series) != 1 {
		t.Fatalf("empty recorder emitted %d series, want 1 (the gauge)\n%s", len(series), out)
	}
	if series[0].name != "ccr_http_inflight_requests" || series[0].value != "0" {
		t.Errorf("empty gauge = %q=%q, want ccr_http_inflight_requests=0", series[0].name, series[0].value)
	}
}

// TestExposition_HistogramBeyondTopBucket confirms an observation larger than
// the top finite bucket lands only in +Inf, keeping cumulative == count there.
func TestExposition_HistogramBeyondTopBucket(t *testing.T) {
	r := New()
	r.RecordRequest("GET", "/slow", 200, 30*time.Second) // > 10s top bucket
	out := scrape(r)
	validateExposition(t, out)

	if v, ok := findSeries(t, out, `ccr_http_request_duration_seconds_bucket{method="GET",path="/slow",le="10"}`); !ok || v != "0" {
		t.Errorf("le=10 bucket = %q (ok=%v), want 0 (obs is beyond it)", v, ok)
	}
	if v, ok := findSeries(t, out, `ccr_http_request_duration_seconds_bucket{method="GET",path="/slow",le="+Inf"}`); !ok || v != "1" {
		t.Errorf("+Inf bucket = %q (ok=%v), want 1", v, ok)
	}
}

// TestExposition_NoLabelInjection feeds a value engineered to break out of the
// label set and forge a fake `ccr_evil_total` series. The escaping + the
// quote-aware brace matcher must keep it confined to one label value, so the
// parsed metric name stays the real family and no ccr_evil_total appears.
func TestExposition_NoLabelInjection(t *testing.T) {
	r := New()
	inject := `"} ccr_evil_total{x="1"} 999
ccr_evil_total 999`
	r.RecordRequest("GET", inject, 200, time.Millisecond)

	out := scrape(r)
	series := validateExposition(t, out)

	for _, s := range series {
		if strings.HasPrefix(s.name, "ccr_evil_total") {
			t.Fatalf("label injection succeeded: forged series %q", s.name)
		}
	}
	// Exactly one http_requests_total series, and its path label round-trips the
	// hostile string verbatim.
	var got parsedSeries
	found := 0
	for _, s := range series {
		if s.name == "ccr_http_requests_total" {
			found++
			got = s
		}
	}
	if found != 1 {
		t.Fatalf("want exactly 1 ccr_http_requests_total series, got %d", found)
	}
	if pv, _ := got.labelVal("path"); pv != inject {
		t.Errorf("path label did not round-trip:\n got %q\nwant %q", pv, inject)
	}
}

// TestExposition_SpecialCharLabels checks values containing '{', '}', '=' (none
// of which are escaped, per spec) plus unicode all parse and round-trip.
func TestExposition_SpecialCharLabels(t *testing.T) {
	r := New()
	paths := []string{
		`/a{b}=c`,
		`/café/naïve/日本語/🚀`,
		`/mix="x"\y` + "\n" + `z`,
		`/eq=al=s`,
	}
	for _, p := range paths {
		r.RecordRequest("GET", p, 200, time.Millisecond)
	}
	out := scrape(r)
	series := validateExposition(t, out)

	got := map[string]bool{}
	for _, s := range series {
		if s.name != "ccr_http_requests_total" {
			continue
		}
		if pv, ok := s.labelVal("path"); ok {
			got[pv] = true
		}
	}
	for _, p := range paths {
		if !got[p] {
			t.Errorf("path %q did not round-trip through exposition\n%s", p, out)
		}
	}
}

// TestExposition_StatusCodeSpread records the full 1xx–5xx family and confirms
// each status renders as its own valid, correctly-counted series.
func TestExposition_StatusCodeSpread(t *testing.T) {
	r := New()
	statuses := []int{100, 101, 200, 204, 301, 302, 400, 404, 429, 500, 503}
	for _, s := range statuses {
		r.RecordRequest("GET", "/v1/messages", s, time.Millisecond)
	}
	out := scrape(r)
	validateExposition(t, out)

	for _, s := range statuses {
		key := fmt.Sprintf(`ccr_http_requests_total{method="GET",path="/v1/messages",status="%d"}`, s)
		if v, ok := findSeries(t, out, key); !ok || v != "1" {
			t.Errorf("status %d series = %q (ok=%v), want 1", s, v, ok)
		}
	}
}

// TestExposition_VeryLargeCounter accumulates a near-uint64 token total and
// checks it renders exactly (no float rounding, no overflow to a wrong value).
func TestExposition_VeryLargeCounter(t *testing.T) {
	r := New()
	const big = math.MaxInt32 // 2147483647, the max a single RecordTokens call takes
	const reps = 1000
	for i := 0; i < reps; i++ {
		r.RecordTokens("p", "m", big, 0)
	}
	want := strconv.FormatUint(uint64(big)*reps, 10) // 2147483647000
	out := scrape(r)
	validateExposition(t, out)

	if v, ok := findSeries(t, out, `ccr_gen_ai_input_tokens_total{provider="p",model="m"}`); !ok || v != want {
		t.Errorf("large input tokens = %q (ok=%v), want %s", v, ok, want)
	}
}

// TestExposition_DeterministicBytes proves the renderer is a pure function of
// recorder state: the same observations yield byte-identical output, both when
// re-scraped and when replayed into a second recorder.
func TestExposition_DeterministicBytes(t *testing.T) {
	build := func() *Recorder {
		r := New()
		// Deliberately record label-sets out of sorted order to exercise the
		// sort in the renderer.
		r.RecordRequest("POST", "/zeta", 500, 2*time.Second)
		r.RecordRequest("GET", "/alpha", 200, time.Millisecond)
		r.RecordRequest("GET", "/alpha", 200, time.Millisecond)
		r.RecordRequest("DELETE", "/mid", 204, 40*time.Millisecond)
		r.RecordUpstream("zzz", "m")
		r.RecordUpstream("aaa", "m")
		r.RecordTokens("aaa", "m", 5, 6)
		r.RecordCache("semantic", false)
		r.RecordCache("exact", true)
		return r
	}

	r1 := build()
	first := scrape(r1)
	if again := scrape(r1); first != again {
		t.Fatalf("re-scrape differs from first scrape:\n---first---\n%s\n---again---\n%s", first, again)
	}
	if other := scrape(build()); first != other {
		t.Fatalf("identical observations produced different bytes across recorders")
	}
}

// TestExposition_SeriesSortedWithinFamily verifies the per-family series are
// emitted in ascending label-key order (the stable-ordering guarantee).
func TestExposition_SeriesSortedWithinFamily(t *testing.T) {
	r := New()
	for _, p := range []string{"/c", "/a", "/b", "/aa", "/a/b"} {
		r.RecordRequest("GET", p, 200, time.Millisecond)
	}
	out := scrape(r)
	series := validateExposition(t, out)

	var keys []string
	for _, s := range series {
		if s.name != "ccr_http_requests_total" {
			continue
		}
		// Reconstruct the emitted inner label key for comparison.
		var b strings.Builder
		for i, l := range s.labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.name)
			b.WriteString(`="`)
			b.WriteString(escapeLabelValue(l.value))
			b.WriteByte('"')
		}
		keys = append(keys, b.String())
	}
	if !sort.StringsAreSorted(keys) {
		t.Errorf("ccr_http_requests_total series not in sorted order: %v", keys)
	}
}
