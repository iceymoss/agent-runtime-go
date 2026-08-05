// Package durable provides persistence-neutral durable execution contracts and
// a thread-safe in-memory reference implementation. It does not start workers
// or background reconciliation; callers explicitly drive every operation.
package durable
