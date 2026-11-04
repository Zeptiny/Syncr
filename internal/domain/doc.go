// Package domain holds Syncr's core vocabulary: entity identifiers, value
// objects, the job status state machine and domain errors.
//
// It is the innermost layer of the ports-and-adapters architecture (see
// docs/adr/0001-architecture.md): it imports only the standard library, and
// nothing in it knows about storage, transport or processes. Every other
// layer may depend on it. Add types here only once their shape is settled by
// a use case; speculative entities belong in the milestone that needs them.
package domain
