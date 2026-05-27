// Package challenger is the in-process auto-generation pipeline.
//
// One audio segment per poll interval is captured by the browser via
// MediaRecorder and POSTed to the daemon as a discrete Opus/WebM blob.
// Service.Generate transcribes the segment via an OpenAI-compatible ASR
// endpoint, accumulates the transcript across intervals until there is
// enough text to ask the LLM for at least one question, and either
// dispatches the resulting bank in-process through challenges.Pipeline
// (auto_submit = true) or writes it to a user-visible review directory
// (auto_submit = false). The package owns no model weights — the
// configured ASR and LLM backends do.
package challenger
