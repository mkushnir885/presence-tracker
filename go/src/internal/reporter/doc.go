// Package reporter shells out to the ptrack_py binary to produce CSV
// presence reports for the /report HTTP endpoint and the `ptrack report`
// CLI. It is a thin wrapper over internal/ptrackpy: with one file it
// invokes the report subcommand, with more than one the aggregate
// subcommand.
package reporter
