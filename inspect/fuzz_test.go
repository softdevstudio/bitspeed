package inspect

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Fuzz the bounded import boundary and both pure report paths together.
func FuzzRecordingReports(f *testing.F) {
	seed, _ := json.Marshal(timedRecording())
	f.Add(seed)
	f.Add([]byte(`{"schema_version":1,"schema_version":1}`))
	f.Add([]byte(`null`))
	f.Add([]byte(strings.Repeat("[", 65) + strings.Repeat("]", 65)))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256<<10 {
			t.Skip()
		}
		r, err := Decode(bytes.NewReader(data))
		if err != nil {
			return
		}
		report, err := Explain(r)
		if err != nil {
			t.Fatal("accepted recording cannot be explained", err)
		}
		var explained bytes.Buffer
		if err := WriteReportJSON(&explained, report); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(explained.Bytes()) {
			t.Fatal("invalid report JSON")
		}
		compared, err := Compare(r, r)
		if err != nil {
			t.Fatal(err)
		}
		var comparison bytes.Buffer
		if err := WriteComparisonJSON(&comparison, compared); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(comparison.Bytes()) {
			t.Fatal("invalid comparison JSON")
		}
	})
}
