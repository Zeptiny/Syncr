package link

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Link features a worker reports in Hello.features (Milestone 5). The
// controller sends a worker only what its features allow: probes to a worker
// with FeatureProbe, restic jobs to one with FeatureResticV2 (and, for
// restic_delete_data, FeatureResticDelete), a job with a time limit to one
// with FeatureJobTimeLimit, a job with hooks to one with FeatureJobHooks, a
// two-way sync to one with FeatureRcloneBisync. A worker that reports no
// features is a Milestone 4 worker: it ignores the Milestone 5 restic spec
// fields and acks probe commands without running them.
const (
	// FeatureProbe: the worker runs probes (Command.run_probe in active
	// mode, WorkerLinkService.Probe in passive mode).
	FeatureProbe = "probe"
	// FeatureResticV2: the worker honors the Milestone 5 restic spec
	// fields (restore overwrite, subfolder and paths, forget keep_within)
	// and reports the Milestone 5 outputs.
	FeatureResticV2 = "restic.v2"
	// FeatureResticDelete: the worker runs the restic_delete_data job kind.
	FeatureResticDelete = "restic.delete"
	// FeatureJobTimeLimit (Milestone 6): the worker enforces
	// JobSpec.max_runtime, stopping a job at its time limit with
	// JOB_ERROR_CODE_TIME_LIMIT. A worker without it would ignore the field
	// and let the job run on, so the controller does not send it a job with
	// a limit.
	FeatureJobTimeLimit = "job.time_limit"
	// FeatureJobHooks: the worker runs JobSpec.hooks, the hooks its
	// operator defined (Hello.hooks), around a job's operation and reports
	// them in JobResult.hook_runs. A worker without it would ignore the
	// field and run the operation alone, so the controller does not send it
	// a job with hooks.
	FeatureJobHooks = "job.hooks"
	// FeatureRcloneBisync: the worker runs the rclone_bisync job kind
	// (two-way sync, docs/adr/0018-two-way-sync.md): it keeps each pair's
	// state in its data directory under RcloneBisync.state_key, resyncs a
	// pair it has no state for, and reports RESYNC_REQUIRED,
	// SAFETY_ABORT and SIDE_UNAVAILABLE. The kind requires it, so a job of
	// it fails with WORKER_OUTDATED on a worker without it.
	FeatureRcloneBisync = "rclone.bisync"
	// FeatureResticRestoreSubfolderPaths (Milestone 7): the worker restores
	// selected items of a subfolder (ResticRestore.subfolder together with
	// paths): it runs "<snapshot>:<subfolder>" with the paths as includes
	// relative to the subfolder, so the items land in the target as they
	// were in the subfolder ("restore to the original location"). An older
	// worker refuses that combination as an invalid spec, so the controller
	// sends it only to a worker with this feature (jobkind
	// DefinitionFeatures) and fails the job with WORKER_OUTDATED otherwise.
	FeatureResticRestoreSubfolderPaths = "restic.restore_subfolder_paths"
	// FeatureResticRemoteSource: the worker backs up a folder of a remote
	// (ResticBackup.remote_source) by mirroring it into its data directory
	// or mounting it with rclone mount, and reports which of the two it can
	// use in Hello.remote_sources. It reports such a backup's phases in
	// Progress.phase (ProgressPhaseMirror, ProgressPhaseBackup), how it read
	// the folder in ResticBackupOutput.remote_source, and an empty folder
	// as SOURCE_EMPTY. An older worker would drop the field and see a
	// backup without paths, so the backup kind requires the feature for a
	// definition with a remote source (jobkind DefinitionFeatures) and the
	// job fails with WORKER_OUTDATED on a worker without it.
	FeatureResticRemoteSource = "restic.remote_source"
)

// Features returns every link feature this build defines, in a fixed order.
// An up-to-date worker reports all of them.
func Features() []string {
	return []string{
		FeatureProbe, FeatureResticV2, FeatureResticDelete, FeatureJobTimeLimit, FeatureJobHooks,
		FeatureRcloneBisync, FeatureResticRestoreSubfolderPaths, FeatureResticRemoteSource,
	}
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
