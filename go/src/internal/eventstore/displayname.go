package eventstore

import "fmt"

// UpdateDisplayName rewrites all events in parquetPath, setting display_name
// for every event whose participant_id matches the given participantID.
//
// This is called by the GUI when the teacher changes a student's display name
// in the meeting analysis view.
//
// TODO: implement full Parquet rewrite once the GUI is available.
func UpdateDisplayName(parquetPath, participantID, displayName string) error {
	return fmt.Errorf("eventstore: UpdateDisplayName: not yet implemented")
}
