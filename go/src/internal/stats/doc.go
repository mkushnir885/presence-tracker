// Package stats fetches the GUI stats JSON from the ptrack_py binary
// and caches it on disk. The cache key is the sorted set of input file
// paths; an entry is invalidated when any of those inputs' mtime is
// newer than the cache file's mtime. Callers (the /stats handler)
// receive a typed Document parsed from that JSON.
package stats
