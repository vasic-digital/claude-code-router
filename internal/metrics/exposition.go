package metrics

import (
	"io"
	"sort"
	"strconv"
	"strings"
)

// exposition.go hand-renders the Prometheus text-exposition format
// (https://prometheus.io/docs/instrumenting/exposition_formats/). Keeping this
// ~150 lines of standard-library code is why the package needs no
// github.com/prometheus/client_golang dependency.

// label is one name="value" pair. Values are escaped at render time.
type label struct {
	name  string
	value string
}

// labelKey renders an ordered label set into the canonical, escaped inner form
// used both as a map key and directly in the exposition line, e.g.
// `method="GET",path="/v1/messages",status="200"`. The order of the arguments
// is preserved, which (because every call site passes labels in a fixed order)
// makes the emitted series deterministic. An empty set renders to "".
func labelKey(labels ...label) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l.value))
		b.WriteByte('"')
	}
	return b.String()
}

// escapeLabelValue escapes a label value per the exposition spec: backslash,
// double-quote and newline are the only characters with special meaning inside
// a "..."-quoted label value.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// escapeHelp escapes HELP text: only backslash and newline are special there
// (double-quotes are NOT escaped in HELP lines, per the spec).
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// itoa is a tiny strconv.Itoa alias kept in this package so metrics.go does not
// import strconv directly.
func itoa(n int) string { return strconv.Itoa(n) }

// WriteExposition writes the full metric snapshot to w in Prometheus text
// format, with each family preceded by its # HELP and # TYPE lines and every
// series emitted in a stable (label-key sorted) order. It takes the Recorder's
// lock for the duration so the snapshot is internally consistent.
func (r *Recorder) WriteExposition(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	writeCounter(w, r.httpRequests)
	writeHistogram(w, r.httpDuration)
	writeGauge(w, "ccr_http_inflight_requests",
		"In-flight HTTP requests currently being served.", r.inFlight.Load())
	writeCounter(w, r.upstreamReqs)
	writeCounter(w, r.inputTokens)
	writeCounter(w, r.outputTokens)
	writeCounter(w, r.cacheLookups)
}

func writeHelpType(w io.Writer, name, help, typ string) {
	io.WriteString(w, "# HELP "+name+" "+escapeHelp(help)+"\n")
	io.WriteString(w, "# TYPE "+name+" "+typ+"\n")
}

// series renders one `name{labels} value` line, omitting the braces when the
// label set is empty.
func series(w io.Writer, name, labelKey, value string) {
	if labelKey == "" {
		io.WriteString(w, name+" "+value+"\n")
		return
	}
	io.WriteString(w, name+"{"+labelKey+"} "+value+"\n")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeCounter(w io.Writer, c *counter) {
	writeHelpType(w, c.name, c.help, "counter")
	for _, k := range sortedKeys(c.values) {
		series(w, c.name, k, strconv.FormatUint(c.values[k], 10))
	}
}

func writeGauge(w io.Writer, name, help string, value int64) {
	writeHelpType(w, name, help, "gauge")
	series(w, name, "", strconv.FormatInt(value, 10))
}

func writeHistogram(w io.Writer, h *histogram) {
	writeHelpType(w, h.name, h.help, "histogram")
	for _, k := range sortedKeys(h.series) {
		s := h.series[k]
		var running uint64
		for i, b := range defaultBuckets {
			running += s.buckets[i]
			series(w, h.name+"_bucket", withLE(k, formatFloat(b)), strconv.FormatUint(running, 10))
		}
		// The +Inf bucket always equals the total count.
		series(w, h.name+"_bucket", withLE(k, "+Inf"), strconv.FormatUint(s.count, 10))
		series(w, h.name+"_sum", k, formatFloat(s.sum))
		series(w, h.name+"_count", k, strconv.FormatUint(s.count, 10))
	}
}

// withLE appends the le="..." bucket label to an existing (possibly empty)
// label key, keeping le last as Prometheus histograms conventionally do.
func withLE(labelKey, le string) string {
	leLabel := `le="` + le + `"`
	if labelKey == "" {
		return leLabel
	}
	return labelKey + "," + leLabel
}

// formatFloat renders a float in the shortest round-trippable form Prometheus
// accepts (e.g. "0.005", "10", "1.5").
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
