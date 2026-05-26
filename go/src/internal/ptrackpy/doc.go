// Package ptrackpy is the shared client for invoking the ptrack_py
// PyInstaller binary as a one-shot subprocess. Every Go package that
// needs a Polars-backed output (CSV reports, GUI stats JSON, …) goes
// through this package so the binary lookup, argument passing, and
// stderr handling live in one place.
package ptrackpy
