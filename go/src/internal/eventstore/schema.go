package eventstore

import (
	"github.com/apache/arrow/go/v17/arrow"
)

// Schema is the canonical Arrow schema for meeting event Parquet files.
// It must stay in sync with docs/EVENT_SCHEMA.md and
// py/src/ptrack_analytics/schema.py.
//
// display_name is the participant identity: it is the canonical name the
// student registered with, recorded on every per-participant event and
// used as the cross-meeting join key.
//
// timestamp semantics:
//   - meeting_started row: absolute Unix timestamp in milliseconds.
//   - all other rows: milliseconds elapsed since the meeting_started timestamp.
//
// The metadata column stores a JSON-encoded map[string]string.
// TODO: migrate to Arrow map<string,string> for native analytics support.
var Schema = arrow.NewSchema([]arrow.Field{
	{Name: "meeting_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "timestamp", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	{Name: "event_type", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "display_name", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "challenge_id", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "question_id", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "metadata", Type: arrow.BinaryTypes.String, Nullable: true}, // JSON-encoded map
}, nil)
