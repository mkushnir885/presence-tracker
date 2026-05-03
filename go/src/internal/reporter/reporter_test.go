package reporter

import (
	"testing"
)

func TestParse(t *testing.T) {
	header := "display_name,presence_ratio,challenges_issued,challenges_correct\n"

	tests := []struct {
		name    string
		input   string
		want    []Row
		wantErr bool
	}{
		{
			name:  "two participants",
			input: header + "Alice,0.9167,3,2\nBob,0.75,2,1\n",
			want: []Row{
				{"Alice", 0.9167, 3, 2},
				{"Bob", 0.75, 2, 1},
			},
		},
		{
			name:  "header only",
			input: header,
			want:  nil,
		},
		{
			name:  "empty input",
			input: "",
			want:  nil,
		},
		{
			name:  "zero challenges",
			input: header + "Charlie,0.5,0,0\n",
			want:  []Row{{"Charlie", 0.5, 0, 0}},
		},
		{
			name:  "name with comma",
			input: header + `"Smith, John",1.0,1,1` + "\n",
			want:  []Row{{"Smith, John", 1.0, 1, 1}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.input)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Parse() error = %v, wantErr %v", err, tc.wantErr)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Parse() returned %d rows, want %d", len(got), len(tc.want))
			}
			for i, row := range got {
				w := tc.want[i]
				if row.DisplayName != w.DisplayName ||
					row.PresenceRatio != w.PresenceRatio ||
					row.ChallengesIssued != w.ChallengesIssued ||
					row.ChallengesCorrect != w.ChallengesCorrect {
					t.Errorf("row[%d] = %+v, want %+v", i, row, w)
				}
			}
		})
	}
}

func TestParseAggregate(t *testing.T) {
	header := "display_name,meeting,presence_ratio,challenges_issued,challenges_correct\n"

	tests := []struct {
		name  string
		input string
		want  []AggregateRow
	}{
		{
			name: "two participants two meetings",
			input: header +
				"Alice,2026-04-21T10:00:00Z,0.9167,3,2\n" +
				"Alice,2026-04-23T10:00:00Z,1.0,2,2\n" +
				"Bob,2026-04-21T10:00:00Z,0.75,2,1\n",
			want: []AggregateRow{
				{"Alice", "2026-04-21T10:00:00Z", 0.9167, 3, 2},
				{"Alice", "2026-04-23T10:00:00Z", 1.0, 2, 2},
				{"Bob", "2026-04-21T10:00:00Z", 0.75, 2, 1},
			},
		},
		{
			name:  "header only",
			input: header,
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAggregate(tc.input)
			if err != nil {
				t.Fatalf("ParseAggregate() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseAggregate() returned %d rows, want %d", len(got), len(tc.want))
			}
			for i, row := range got {
				w := tc.want[i]
				if row != w {
					t.Errorf("row[%d] = %+v, want %+v", i, row, w)
				}
			}
		})
	}
}

func TestContainsGlob(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"meetings/*.parquet", true},
		{"meeting.parquet", false},
		{"meetings/2026-??.parquet", true},
		{"meetings/[abc].parquet", true},
		{"/abs/path/to/file.parquet", false},
	}
	for _, tc := range tests {
		if got := containsGlob(tc.input); got != tc.want {
			t.Errorf("containsGlob(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}
