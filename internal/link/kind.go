package link

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// operationOneof is the JobSpec oneof whose set field names the job kind.
const operationOneof protoreflect.Name = "operation"

// KindOf returns the job kind of spec: the name of the JobSpec.operation field
// that is set (e.g. "rclone_transfer", "restic_backup"). It returns "" for a
// nil spec or a spec without an operation.
//
// Kinds are derived from the wire contract rather than declared separately,
// so adding a JobSpec operation automatically defines its kind; the
// controller's job kinds and the worker's runners use the same strings.
func KindOf(spec *syncrv1.JobSpec) string {
	if spec == nil {
		return ""
	}
	m := spec.ProtoReflect()
	fd := m.WhichOneof(m.Descriptor().Oneofs().ByName(operationOneof))
	if fd == nil {
		return ""
	}
	return string(fd.Name())
}

// Kinds returns every job kind the wire contract defines, in field-number
// order.
func Kinds() []string {
	fields := (&syncrv1.JobSpec{}).ProtoReflect().Descriptor().Oneofs().ByName(operationOneof).Fields()
	kinds := make([]string, 0, fields.Len())
	for i := range fields.Len() {
		kinds = append(kinds, string(fields.Get(i).Name()))
	}
	return kinds
}

// IsKind reports whether kind is defined by the wire contract.
func IsKind(kind string) bool {
	if kind == "" {
		return false
	}
	fd := (&syncrv1.JobSpec{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(kind))
	return fd != nil && fd.ContainingOneof() != nil && fd.ContainingOneof().Name() == operationOneof
}
