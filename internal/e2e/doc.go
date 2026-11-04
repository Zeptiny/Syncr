// Package e2e holds end-to-end tests of the worker link: a controller
// (memstore-backed services, the dispatcher and its active and passive
// transports) and workers (the real agent, link core and transports) in one
// process, talking over real HTTP and TLS. It contains no production code.
//
// It is the only package that may import both controller and worker code;
// it uses the in-memory adapters, never Postgres.
package e2e
