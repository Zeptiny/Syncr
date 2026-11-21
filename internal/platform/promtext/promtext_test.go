package promtext_test

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/platform/promtext"
)

func TestWrite(t *testing.T) {
	up := promtext.Family{Name: "syncr_worker_up", Help: "Whether the worker is online.", Type: promtext.Gauge}
	up.Add(1, promtext.L("worker", "nas")).Add(0, promtext.L("worker", `odd "name"`+"\n"+`back\slash`))
	fams := []promtext.Family{
		{Name: "syncr_build_info", Help: "Build of the controller.\nA second line with a \\.", Type: promtext.Gauge,
			Samples: []promtext.Sample{{Labels: []promtext.Label{promtext.L("version", "v2.1.0")}, Value: 1}}},
		{Name: "syncr_empty", Help: "Left out.", Type: promtext.Counter},
		up,
		{Name: "syncr_jobs_finished_total", Help: "Jobs.", Type: promtext.Counter,
			Samples: []promtext.Sample{{Value: 3}, {Labels: []promtext.Label{promtext.L("x", "y")}, Value: 1.5e10}}},
		{Name: "syncr_special", Help: "Special values.", Type: promtext.Gauge,
			Samples: []promtext.Sample{
				{Labels: []promtext.Label{promtext.L("v", "inf")}, Value: math.Inf(1)},
				{Labels: []promtext.Label{promtext.L("v", "-inf")}, Value: math.Inf(-1)},
				{Labels: []promtext.Label{promtext.L("v", "nan")}, Value: math.NaN()},
				{Labels: []promtext.Label{promtext.L("v", "small")}, Value: 0.25},
			}},
		{Name: "syncr_job_duration_seconds", Help: "Run time.", Type: promtext.Histogram,
			Histograms: []promtext.HistogramSample{
				{Labels: []promtext.Label{promtext.L("kind", "rclone_transfer")},
					Buckets: []promtext.Bucket{{UpperBound: 1, Count: 1}, {UpperBound: 60, Count: 3}}, Count: 4, Sum: 3700.5},
				{Buckets: []promtext.Bucket{{UpperBound: 1, Count: 0}}, Count: 0, Sum: 0},
			}},
	}
	var buf bytes.Buffer
	if err := promtext.Write(&buf, fams); err != nil {
		t.Fatal(err)
	}
	want := `# HELP syncr_build_info Build of the controller.\nA second line with a \\.
# TYPE syncr_build_info gauge
syncr_build_info{version="v2.1.0"} 1
# HELP syncr_worker_up Whether the worker is online.
# TYPE syncr_worker_up gauge
syncr_worker_up{worker="nas"} 1
syncr_worker_up{worker="odd \"name\"\nback\\slash"} 0
# HELP syncr_jobs_finished_total Jobs.
# TYPE syncr_jobs_finished_total counter
syncr_jobs_finished_total 3
syncr_jobs_finished_total{x="y"} 1.5e+10
# HELP syncr_special Special values.
# TYPE syncr_special gauge
syncr_special{v="inf"} +Inf
syncr_special{v="-inf"} -Inf
syncr_special{v="nan"} NaN
syncr_special{v="small"} 0.25
# HELP syncr_job_duration_seconds Run time.
# TYPE syncr_job_duration_seconds histogram
syncr_job_duration_seconds_bucket{kind="rclone_transfer",le="1"} 1
syncr_job_duration_seconds_bucket{kind="rclone_transfer",le="60"} 3
syncr_job_duration_seconds_bucket{kind="rclone_transfer",le="+Inf"} 4
syncr_job_duration_seconds_sum{kind="rclone_transfer"} 3700.5
syncr_job_duration_seconds_count{kind="rclone_transfer"} 4
syncr_job_duration_seconds_bucket{le="1"} 0
syncr_job_duration_seconds_bucket{le="+Inf"} 0
syncr_job_duration_seconds_sum 0
syncr_job_duration_seconds_count 0
`
	if got := buf.String(); got != want {
		t.Errorf("Write =\n%s\nwant\n%s", got, want)
	}
}

func TestWriteRefuses(t *testing.T) {
	gauge := func(name string, labels ...promtext.Label) promtext.Family {
		return promtext.Family{Name: name, Type: promtext.Gauge, Samples: []promtext.Sample{{Labels: labels, Value: 1}}}
	}
	hist := func(labels []promtext.Label, count uint64, buckets ...promtext.Bucket) promtext.Family {
		return promtext.Family{Name: "h", Type: promtext.Histogram,
			Histograms: []promtext.HistogramSample{{Labels: labels, Buckets: buckets, Count: count}}}
	}
	tests := []struct {
		name string
		fams []promtext.Family
		want string
	}{
		{"empty name", []promtext.Family{gauge("")}, "invalid name"},
		{"name with a dash", []promtext.Family{gauge("a-b")}, "invalid name"},
		{"name starting with a digit", []promtext.Family{gauge("1a")}, "invalid name"},
		{"repeated family", []promtext.Family{gauge("a"), gauge("a")}, "repeated"},
		{"bad label name", []promtext.Family{gauge("a", promtext.L("a.b", "x"))}, "invalid label name"},
		{"reserved label name", []promtext.Family{gauge("a", promtext.L("__name__", "x"))}, "invalid label name"},
		{"repeated label", []promtext.Family{gauge("a", promtext.L("x", "1"), promtext.L("x", "2"))}, "repeated"},
		{"unknown type", []promtext.Family{{Name: "a", Type: "summary", Samples: []promtext.Sample{{Value: 1}}}}, "unknown type"},
		{"gauge with histograms", []promtext.Family{{Name: "a", Type: promtext.Gauge, Histograms: []promtext.HistogramSample{{}}}}, "no histograms"},
		{"histogram with samples", []promtext.Family{{Name: "a", Type: promtext.Histogram, Samples: []promtext.Sample{{}}}}, "only histogram"},
		{"le label", []promtext.Family{hist([]promtext.Label{promtext.L("le", "1")}, 0)}, `"le"`},
		{"unsorted buckets", []promtext.Family{hist(nil, 2, promtext.Bucket{UpperBound: 2, Count: 1}, promtext.Bucket{UpperBound: 1, Count: 2})}, "sorted"},
		{"not cumulative", []promtext.Family{hist(nil, 2, promtext.Bucket{UpperBound: 1, Count: 2}, promtext.Bucket{UpperBound: 2, Count: 1})}, "cumulative"},
		{"bucket above count", []promtext.Family{hist(nil, 1, promtext.Bucket{UpperBound: 1, Count: 2})}, "more than"},
		{"infinite bound", []promtext.Family{hist(nil, 1, promtext.Bucket{UpperBound: math.Inf(1), Count: 1})}, "finite"},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		err := promtext.Write(&buf, tt.fams)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: Write = %v, want an error containing %q", tt.name, err, tt.want)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: wrote %q", tt.name, buf.String())
		}
	}
}
