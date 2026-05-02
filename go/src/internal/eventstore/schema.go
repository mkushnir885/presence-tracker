package eventstore

import (
	"github.com/apache/arrow/go/v17/arrow"
)

// Schema is the canonical Arrow schema for meeting event Parquet files.
// It must stay in sync with docs/EVENT_SCHEMA.md and
// py/src/ptrack_analytics/schema.py.
//
// The metadata column stores a JSON-encoded map[string]string.
// TODO: migrate to Arrow map<string,string> for native analytics support.
var Schema = arrow.NewSchema([]arrow.Field{
	{Name: "event_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "meeting_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "timestamp", Type: &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "UTC"}, Nullable: false},
	{Name: "source", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "event_type", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "participant_id", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "platform_handle", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "display_name", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "metadata", Type: arrow.BinaryTypes.String, Nullable: true}, // JSON-encoded map
}, func() *arrow.Metadata {
	m := arrow.MetadataFrom(map[string]string{"schema_version": "1"})
	return &m
}())
