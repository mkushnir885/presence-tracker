// Package meet implements the Provider interface for Google Meet using
// the Meet REST API v2. Participant events are detected by polling the
// conferenceRecords/participants resource at a configurable interval.
//
// Limitation: the Google Meet REST API does not expose meeting chat messages.
// Participant pairing codes therefore cannot be detected from Meet; students
// must be pre-registered via another platform or the teacher must pair them
// manually through the GUI.
package meet
