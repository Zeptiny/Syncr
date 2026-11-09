package link

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Link features a worker reports in Hello.features (Milestone 5). The
// controller sends a worker only what its features allow: probes to a worker
// with FeatureProbe, restic jobs to one with FeatureResticV2 (and, for
// restic_delete_data, FeatureResticDelete). A worker that reports no
// features is a Milestone 4 worker: it ignores the Milestone 5 restic spec
// fields and acks probe commands without running them.
const (
	// FeatureProbe: the worker runs probes (Command.run_probe in active
	// mode, WorkerLinkService.Probe in passive mode).
	FeatureProbe = "probe"
	// FeatureResticV2: the worker honours the Milestone 5 restic spec
	// fields (restore overwrite, subfolder and paths, forget keep_within)
	// and reports the Milestone 5 outputs.
	FeatureResticV2 = "restic.v2"
	// FeatureResticDelete: the worker runs the restic_delete_data job kind.
	FeatureResticDelete = "restic.delete"
)

// Features returns every link feature this build defines, in a fixed order.
// An up-to-date worker reports all of them.
func Features() []string {
	return []string{FeatureProbe, FeatureResticV2, FeatureResticDelete}
}

// probeOneof is the ProbeSpec oneof whose set field names the probe op.
const probeOneof protoreflect.Name = "op"

// ProbeOpOf returns the probe op of spec: the name of the ProbeSpec.op field
// that is set (e.g. "list_local", "restic_ls"). It returns "" for a nil spec
// or a spec without an op.
func ProbeOpOf(spec *syncrv1.ProbeSpec) string {
	if spec == nil {
		return ""
	}
	m := spec.ProtoReflect()
	fd := m.WhichOneof(m.Descriptor().Oneofs().ByName(probeOneof))
	if fd == nil {
		return ""
	}
	return string(fd.Name())
}

// ProbeOps returns every probe op the wire contract defines, in
// field-number order.
func ProbeOps() []string {
	fields := (&syncrv1.ProbeSpec{}).ProtoReflect().Descriptor().Oneofs().ByName(probeOneof).Fields()
	ops := make([]string, 0, fields.Len())
	for i := range fields.Len() {
		ops = append(ops, string(fields.Get(i).Name()))
	}
	return ops
}

// IsProbeOp reports whether op is defined by the wire contract.
func IsProbeOp(op string) bool {
	if op == "" {
		return false
	}
	fd := (&syncrv1.ProbeSpec{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(op))
	return fd != nil && fd.ContainingOneof() != nil && fd.ContainingOneof().Name() == probeOneof
}
