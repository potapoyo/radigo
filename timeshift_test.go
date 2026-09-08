package radigo

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCollectTimeshiftRepairsGapAndKeepsTemporalOrder(t *testing.T) {
	start := time.Date(2026, 9, 8, 1, 0, 0, 0, location)
	n := 0
	got, err := collectTimeshiftSegments(context.Background(), start, start.Add(20*time.Second), func(cursor time.Time) ([]timedSegment, error) {
		n++
		if n == 1 {
			return []timedSegment{{"z", start, 5 * time.Second}, {"later", start.Add(15 * time.Second), 5 * time.Second}}, nil
		}
		if n == 2 {
			return []timedSegment{{"stale", start, 5 * time.Second}}, nil
		}
		if !cursor.Equal(start.Add(5 * time.Second)) {
			t.Fatalf("skipped gap: %s", cursor)
		}
		return []timedSegment{{"a", start.Add(15 * time.Second), 5 * time.Second}, {"same", start.Add(5 * time.Second), 5 * time.Second}, {"same", start.Add(10 * time.Second), 5 * time.Second}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"z", "same", "same", "a"}) {
		t.Fatalf("got=%v", got)
	}
}

func TestCollectTimeshiftFailsOnPersistentGap(t *testing.T) {
	start := time.Now()
	n := 0
	_, err := collectTimeshiftSegments(context.Background(), start, start.Add(time.Minute), func(time.Time) ([]timedSegment, error) { n++; return nil, nil })
	if err == nil || n != 8 {
		t.Fatalf("err=%v calls=%d", err, n)
	}
}

func TestCollectTimeshiftCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := collectTimeshiftSegments(ctx, time.Now(), time.Now().Add(time.Hour), func(time.Time) ([]timedSegment, error) { t.Fatal("fetch called"); return nil, nil })
	if err != context.Canceled {
		t.Fatal(err)
	}
}

func TestParseTimedPlaylist(t *testing.T) {
	base, _ := url.Parse("https://audio.invalid/dir/list")
	input := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-PROGRAM-DATE-TIME:2026-09-08T01:00:00.005+09:00\n#EXTINF:5.035,\nz.aac\n#EXTINF:5.035,\n../a.aac\n#EXT-X-ENDLIST\n"
	got, err := parseTimedPlaylist(strings.NewReader(input), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].URI != "https://audio.invalid/a.aac" || got[1].Start.Sub(got[0].Start) != 5035*time.Millisecond {
		t.Fatalf("got=%+v", got)
	}
	for _, bad := range []string{"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-ENDLIST\n", strings.ReplaceAll(input, "#EXT-X-PROGRAM-DATE-TIME:2026-09-08T01:00:00.005+09:00\n", ""), strings.Replace(input, "#EXTINF:5.035,", "#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXTINF:5.035,", 1)} {
		if _, err := parseTimedPlaylist(strings.NewReader(bad), base); err == nil {
			t.Fatal(fmt.Sprintf("accepted %q", bad))
		}
	}
}
