package eventstore

import (
	"github.com/apache/arrow/go/v17/arrow"
)

// Schema is the canonical event schema; keep it in sync with the Python side.
// from_start_ms is ms elapsed since the meeting start; session_started is 0.
// The absolute start/end instants live in session_started/session_ended
// metadata under "timestamp_ms" (Unix ms).
var Schema = arrow.NewSchema([]arrow.Field{
	{Name: "meeting_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "from_start_ms", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	{Name: "event_type", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "display_name", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "challenge_id", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "question_id", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "metadata", Type: arrow.BinaryTypes.String, Nullable: true},
}, nil)
