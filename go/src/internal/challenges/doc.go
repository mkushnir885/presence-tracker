// Package challenges implements the single challenge pipeline: load a
// teacher-prepared YAML question bank, validate it, score student answers,
// and run a poll round (assign one random question per eligible
// participant, dispatch via the messenger, await answers, write events).
//
// There is no producer abstraction. How a YAML bank file came to exist is
// the caller's concern; the pipeline only knows how to consume one. See
// docs/CHALLENGES.md.
package challenges
