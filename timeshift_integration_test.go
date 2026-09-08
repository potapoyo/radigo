package radigo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yyoshiki41/radigo/internal"
)

// Explicitly opt in: this verifies a real recording, requiring network and ffmpeg.
func TestTBSRecordingIntegration(t *testing.T) {
	verifyRecordingIntegration(t, "TBS", "20260908010000")
}

func TestLFRRecordingIntegration(t *testing.T) {
	verifyRecordingIntegration(t, "LFR", "20260909010000")
}

func verifyRecordingIntegration(t *testing.T, stationID, startTime string) {
	t.Helper()
	dir := os.Getenv("RADIGO_VERIFY_AUDIO_DIR")
	if dir == "" {
		t.Skip("set RADIGO_VERIFY_AUDIO_DIR to verify the recording")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	client, err := getClient(ctx, "JP13")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AuthorizeToken(ctx); err != nil {
		t.Fatal(err)
	}
	start, _ := time.ParseInLocation(datetimeLayout, startTime, location)
	links, err := getTimeshiftChunklist(ctx, client, stationID, start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("complete playlist: %d ordered segments", len(links))
	segments, err := os.MkdirTemp(dir, "segments-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(segments)
	if err := internal.BulkDownloadContext(ctx, links, segments); err != nil {
		t.Fatal(err)
	}
	t.Log("all segments downloaded and ADTS frames validated")
	output, err := ConcatAACFilesFromList(ctx, segments)
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, startTime+"-"+stationID+".aac")
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatal("verification output already exists")
	}
	if err := os.Rename(output, final); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-xerror", "-i", final, "-progress", "pipe:1", "-f", "null", "-")
	log, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("decode: %v: %s", err, log)
	}
	var micros int64
	for _, line := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(line, "out_time_us=") {
			micros, _ = strconv.ParseInt(strings.TrimPrefix(line, "out_time_us="), 10, 64)
		}
	}
	seconds := float64(micros) / 1e6
	if seconds < 7199.9 || seconds > 7205.1 {
		t.Fatalf("decoded duration %.6f, expected approximately 7200", seconds)
	}
	t.Logf("VERIFIED decoded duration=%.6f seconds, file=%s", seconds, final)
}
