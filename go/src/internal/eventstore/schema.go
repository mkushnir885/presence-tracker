package eventstore

import (
	"github.com/apache/arrow/go/v17/arrow"
)

// Schema is the canonical event schema; keep it in sync with the Python side.
// timestamp encoding: session_started holds absolute Unix ms, every other row a ms offset from it.
var Schema = arrow.NewSchema([]arrow.Field{
	{Name: "meeting_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "timestamp", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	{Name: "event_type", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "display_name", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "challenge_id", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "question_id", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "metadata", Type: arrow.BinaryTypes.String, Nullable: true},
}, nil)
