// Package promtext writes metrics in the Prometheus text exposition format,
// version 0.0.4 (https://prometheus.io/docs/instrumenting/exposition_formats/),
// which every Prometheus-compatible scraper reads (Prometheus, VictoriaMetrics,
// Grafana Alloy, the OpenTelemetry Collector's prometheus receiver).
//
// It only formats: the caller collects the values and passes the families in
// the order they should appear. Output is deterministic for a given input.
//
// This package is a leaf: it imports only the standard library.
package promtext

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ContentType is the Content-Type of the format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Type is a metric family's type.
type Type string

// Family types.
const (
	Counter   Type = "counter"
	Gauge     Type = "gauge"
	Histogram Type = "histogram"
)

// Label is one label of a sample.
type Label struct {
	Name, Value string
}

// L returns the label name=value.
func L(name, value string) Label { return Label{Name: name, Value: value} }

// Sample is one value of a counter or gauge family.
type Sample struct {
	Labels []Label
	Value  float64
}

// Bucket is one bucket of a histogram: Count observations were at most
// UpperBound (cumulative, as Prometheus expects).
type Bucket struct {
	UpperBound float64
	Count      uint64
}

// HistogramSample is one histogram of a histogram family. Buckets are
// sorted by UpperBound, cumulative, and without the +Inf bucket, which
// Write adds with Count.
type HistogramSample struct {
	Labels  []Label
	Buckets []Bucket
	Count   uint64
	Sum     float64
}

// Family is a metric family: a name, its help text and type, and its
// samples (Histograms for a Histogram family, Samples otherwise).
type Family struct {
	Name       string
	Help       string
	Type       Type
	Samples    []Sample
	Histograms []HistogramSample
}

// Add appends a sample with labels to f and returns f, for building a
// family in a loop.
func (f *Family) Add(value float64, labels ...Label) *Family {
	f.Samples = append(f.Samples, Sample{Labels: labels, Value: value})
	return f
}

// Empty reports whether f has no samples (Write leaves it out).
func (f *Family) Empty() bool { return len(f.Samples) == 0 && len(f.Histograms) == 0 }

// Write writes fams to w in order. A family without samples is left out.
// It fails, writing nothing, if a name is not a valid metric or label name,
// a family name repeats, a sample repeats a label name, a counter or
// gauge family has histograms or a histogram family plain samples, or a
// histogram's buckets are not sorted or not cumulative.
func Write(w io.Writer, fams []Family) error {
	if err := validate(fams); err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	for i := range fams {
		f := &fams[i]
		if f.Empty() {
			continue
		}
		bw.WriteString("# HELP " + f.Name + " " + escapeHelp(f.Help) + "\n")
		bw.WriteString("# TYPE " + f.Name + " " + string(f.Type) + "\n")
		for _, s := range f.Samples {
			writeSample(bw, f.Name, s.Labels, nil, formatFloat(s.Value))
		}
		for _, h := range f.Histograms {
			for _, b := range h.Buckets {
				le := L("le", formatFloat(b.UpperBound))
				writeSample(bw, f.Name+"_bucket", h.Labels, &le, strconv.FormatUint(b.Count, 10))
			}
			inf := L("le", "+Inf")
			writeSample(bw, f.Name+"_bucket", h.Labels, &inf, strconv.FormatUint(h.Count, 10))
			writeSample(bw, f.Name+"_sum", h.Labels, nil, formatFloat(h.Sum))
			writeSample(bw, f.Name+"_count", h.Labels, nil, strconv.FormatUint(h.Count, 10))
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write metrics: %w", err)
	}
	return nil
}

func writeSample(w *bufio.Writer, name string, labels []Label, extra *Label, value string) {
	w.WriteString(name)
	if len(labels) > 0 || extra != nil {
		w.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				w.WriteByte(',')
			}
			writeLabel(w, l)
		}
		if extra != nil {
			if len(labels) > 0 {
				w.WriteByte(',')
			}
			writeLabel(w, *extra)
		}
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(value)
	w.WriteByte('\n')
}

func writeLabel(w *bufio.Writer, l Label) {
	w.WriteString(l.Name)
	w.WriteString(`="`)
	w.WriteString(escapeLabelValue(l.Value))
	w.WriteByte('"')
}

// formatFloat formats v as Prometheus parses it: the shortest
// representation, +Inf, -Inf or NaN.
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeHelp escapes a help text: backslash and line feed.
func escapeHelp(s string) string { return escape(s, false) }

// escapeLabelValue escapes a label value: backslash, line feed and double
// quote.
func escapeLabelValue(s string) string { return escape(s, true) }

func escape(s string, quote bool) string {
	if !strings.ContainsAny(s, "\\\n\"") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, c := range s {
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '"' && quote:
			b.WriteString(`\"`)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// validate checks fams (see Write).
func validate(fams []Family) error {
	seen := make(map[string]bool, len(fams))
	for i := range fams {
		f := &fams[i]
		if !validMetricName(f.Name) {
			return fmt.Errorf("metric family %q: invalid name", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("metric family %q: repeated", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case Counter, Gauge:
			if len(f.Histograms) > 0 {
				return fmt.Errorf("metric family %q: a %s has no histograms", f.Name, f.Type)
			}
			for _, s := range f.Samples {
				if err := validateLabels(s.Labels, false); err != nil {
					return fmt.Errorf("metric family %q: %w", f.Name, err)
				}
			}
		case Histogram:
			if len(f.Samples) > 0 {
				return fmt.Errorf("metric family %q: a histogram has only histogram samples", f.Name)
			}
			for _, h := range f.Histograms {
				if err := validateLabels(h.Labels, true); err != nil {
					return fmt.Errorf("metric family %q: %w", f.Name, err)
				}
				if err := validateBuckets(h); err != nil {
					return fmt.Errorf("metric family %q: %w", f.Name, err)
				}
			}
		default:
			return fmt.Errorf("metric family %q: unknown type %q", f.Name, f.Type)
		}
	}
	return nil
}

func validateLabels(labels []Label, histogram bool) error {
	for i, l := range labels {
		if !validLabelName(l.Name) {
			return fmt.Errorf("invalid label name %q", l.Name)
		}
		if histogram && l.Name == "le" {
			return errors.New(`a histogram's labels may not include "le"`)
		}
		for _, o := range labels[:i] {
			if o.Name == l.Name {
				return fmt.Errorf("label %q repeated", l.Name)
			}
		}
	}
	return nil
}

func validateBuckets(h HistogramSample) error {
	for i, b := range h.Buckets {
		if math.IsNaN(b.UpperBound) || math.IsInf(b.UpperBound, 1) {
			return fmt.Errorf("bucket bound %v: not a finite bound (+Inf is added)", b.UpperBound)
		}
		if i > 0 && (b.UpperBound <= h.Buckets[i-1].UpperBound || b.Count < h.Buckets[i-1].Count) {
			return errors.New("buckets are not sorted and cumulative")
		}
	}
	if n := len(h.Buckets); n > 0 && h.Buckets[n-1].Count > h.Count {
		return errors.New("a bucket counts more than the histogram")
	}
	return nil
}

// validMetricName reports whether s matches [a-zA-Z_:][a-zA-Z0-9_:]*.
func validMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c == ':' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// validLabelName reports whether s matches [a-zA-Z_][a-zA-Z0-9_]* and does
// not start with "__" (reserved for Prometheus).
func validLabelName(s string) bool {
	if s == "" || strings.HasPrefix(s, "__") {
		return false
	}
	for i, c := range s {
		if !(c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}
