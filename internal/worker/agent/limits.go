package agent

import (
	"cmp"
	"fmt"
	"slices"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// outputSlack is kept free of list entries when an output is truncated, for
// the message framing around them.
const outputSlack = 64

// limitOutput returns out unchanged if its encoding fits in maxBytes.
// Otherwise it returns a truncated copy and a warning saying what was left
// out:
//
//   - restic_snapshots keeps the newest snapshots (by time) that fit, in
//     their original order;
//   - restic_forget keeps the first removed snapshot IDs that fit;
//   - any other output is dropped.
//
// out itself is never modified.
func limitOutput(out *syncrv1.JobOutput, maxBytes int) (*syncrv1.JobOutput, string) {
	size := proto.Size(out)
	if out == nil || size <= maxBytes {
		return out, ""
	}
	budget := maxBytes - outputSlack
	switch o := out.GetOutput().(type) {
	case *syncrv1.JobOutput_ResticSnapshots:
		snaps := o.ResticSnapshots.GetSnapshots()
		newest := slices.Clone(snaps)
		slices.SortStableFunc(newest, func(a, b *syncrv1.ResticSnapshot) int {
			return -cmp.Compare(a.GetTime().AsTime().UnixNano(), b.GetTime().AsTime().UnixNano())
		})
		keep := make(map[*syncrv1.ResticSnapshot]bool)
		used := 0
		for _, sn := range newest {
			n := entrySize(proto.Size(sn))
			if used+n > budget {
				break
			}
			used += n
			keep[sn] = true
		}
		kept := make([]*syncrv1.ResticSnapshot, 0, len(keep))
		for _, sn := range snaps {
			if keep[sn] {
				kept = append(kept, sn)
			}
		}
		return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticSnapshots{ResticSnapshots: &syncrv1.ResticSnapshotsOutput{Snapshots: kept}}},
			fmt.Sprintf("output truncated: listing the newest %d of %d snapshots (the full list exceeds %d bytes)", len(kept), len(snaps), maxBytes)
	case *syncrv1.JobOutput_ResticForget:
		ids := o.ResticForget.GetRemovedSnapshotIds()
		used, n := 0, 0
		for _, id := range ids {
			sz := entrySize(len(id))
			if used+sz > budget {
				break
			}
			used += sz
			n++
		}
		return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: &syncrv1.ResticForgetOutput{RemovedSnapshotIds: slices.Clone(ids[:n])}}},
			fmt.Sprintf("output truncated: listing %d of %d removed snapshots (the full list exceeds %d bytes)", n, len(ids), maxBytes)
	default:
		return nil, fmt.Sprintf("output dropped: %d bytes exceed the limit of %d bytes", size, maxBytes)
	}
}

// entrySize is the encoded size of a length-delimited repeated field entry
// whose payload has n bytes (field numbers below 16 take one tag byte).
func entrySize(n int) int { return 1 + protowire.SizeBytes(n) }

// clip cuts s to at most n bytes on a UTF-8 boundary, marking the cut with
// "...".
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := max(n-3, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
