package radigo

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yyoshiki41/go-radiko"
)

const (
	// fallback endpoint if station stream XML lookup fails
	timefreePlaylistEndpoint = "https://tf-f-rpaa-radiko.smartstream.ne.jp/tf/playlist.m3u8"
	stationStreamXMLBase     = "https://radiko.jp/v3/station/stream/pc_html5/"
)

type stationStreamData struct {
	URLs []stationStreamURL `xml:"url"`
}

type stationStreamURL struct {
	Arefree           string `xml:"areafree,attr"`
	Timefree          string `xml:"timefree,attr"`
	PlaylistCreateURL string `xml:"playlist_create_url"`
}

// getTimeshiftPlaylistM3U8 returns the media playlist URI for a timeshift program.
// This replaces the broken TimeshiftPlaylistM3U8 in go-radiko, which relied on
// the old radiko.jp/v2/api/ts/playlist.m3u8 endpoint (defunct as of 2026-01-26).
func getTimeshiftPlaylistM3U8(ctx context.Context, client *radiko.Client, stationID string, start time.Time) (string, error) {
	prog, err := client.GetProgramByStartTime(ctx, stationID, start)
	if err != nil {
		return "", err
	}

	endpoint := discoverTimefreeEndpoint(ctx, client, stationID)
	lsid := randomHex(16)
	url := fmt.Sprintf(
		"%s?station_id=%s&start_at=%s&ft=%s&end_at=%s&to=%s&preroll=2&l=15&lsid=%s&type=b",
		endpoint,
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
	req.Header.Set("X-Radiko-AreaId", client.AreaID())
	req.Header.Set("X-Radiko-App", "pc_html5")
	req.Header.Set("X-Radiko-App-Version", "0.0.1")
	req.Header.Set("X-Radiko-User", "test-stream")
	req.Header.Set("X-Radiko-Device", "pc")
	req.Header.Set("Origin", "https://radiko.jp")
	req.Header.Set("Referer", "https://radiko.jp/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("timefree playlist request failed: HTTP %d", resp.StatusCode)
	}

	return parseMasterM3U8URI(resp.Body)
}

// discoverTimefreeEndpoint fetches the station stream XML to find the correct
// timefree playlist creation URL. Falls back to the known default on error.
func discoverTimefreeEndpoint(ctx context.Context, client *radiko.Client, stationID string) string {
	xmlURL := stationStreamXMLBase + stationID + ".xml"
	req, err := http.NewRequestWithContext(ctx, "GET", xmlURL, nil)
	if err != nil {
		return timefreePlaylistEndpoint
	}
	req.Header.Set("X-Radiko-AuthToken", client.AuthToken())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return timefreePlaylistEndpoint
	}
	defer resp.Body.Close()

	var data stationStreamData
	if err := xml.NewDecoder(resp.Body).Decode(&data); err != nil {
		return timefreePlaylistEndpoint
	}

	// Prefer timefree=1, areafree=0 (non-premium area-free)
	for _, u := range data.URLs {
		if u.Timefree == "1" && u.Arefree == "0" && u.PlaylistCreateURL != "" {
			return u.PlaylistCreateURL
		}
	}
	// Fallback to any timefree URL
	for _, u := range data.URLs {
		if u.Timefree == "1" && u.PlaylistCreateURL != "" {
			return u.PlaylistCreateURL
		}
	}
	return timefreePlaylistEndpoint
}

// parseMasterM3U8URI extracts the first stream URI from a master M3U8 playlist.
func parseMasterM3U8URI(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	valid := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !valid {
			if !strings.HasPrefix(line, "#EXTM3U") {
				return "", fmt.Errorf("invalid playlist response: %q", line)
			}
			valid = true
			continue
		}
		if strings.HasPrefix(line, "#") {
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
