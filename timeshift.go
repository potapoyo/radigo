package radigo

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yyoshiki41/go-radiko"
)

const timefreePlaylistEndpoint = "https://tf-rpaa.smartstream.ne.jp/tf/playlist.m3u8"

// getTimeshiftPlaylistM3U8 returns the media playlist URI for a timeshift program.
// This replaces the broken TimeshiftPlaylistM3U8 in go-radiko, which relied on
// the old radiko.jp/v2/api/ts/playlist.m3u8 endpoint (defunct as of 2026-01-26).
func getTimeshiftPlaylistM3U8(ctx context.Context, client *radiko.Client, stationID string, start time.Time) (string, error) {
	prog, err := client.GetProgramByStartTime(ctx, stationID, start)
	if err != nil {
		return "", err
	}

	lsid := randomHex(16)
	url := fmt.Sprintf(
		"%s?station_id=%s&start_at=%s&ft=%s&end_at=%s&to=%s&l=15&lsid=%s&type=b",
		timefreePlaylistEndpoint,
		stationID,
		prog.Ft, prog.Ft,
		prog.To, prog.To,
		lsid,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Radiko-AuthToken", client.AuthToken())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	return parseMasterM3U8URI(resp.Body)
}

// parseMasterM3U8URI extracts the first stream URI from a master M3U8 playlist.
func parseMasterM3U8URI(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no URI found in master playlist")
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
