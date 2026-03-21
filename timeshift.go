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

	// needsAreaFree is true when the client area was overridden (premium area-free)
	needsAreaFree := client.AreaID() != "" && client.AreaID() != currentAreaID

	// type=b: in-region, type=c: area-free (premium)
	streamType := "b"
	if needsAreaFree {
		streamType = "c"
	}

	// For area-free, the auth token is bound to the auto-detected area (e.g. JP13),
	// not the overridden area. Send the actual detected area to match the token.
	areaID := client.AreaID()
	if needsAreaFree {
		areaID = currentAreaID
	}

	endpoint := discoverTimefreeEndpoint(ctx, client, stationID)
	lsid := randomHex(16)
	url := fmt.Sprintf(
		"%s?station_id=%s&start_at=%s&ft=%s&end_at=%s&to=%s&preroll=2&l=15&lsid=%s&type=%s",
		endpoint,
		stationID,
		prog.Ft, prog.Ft,
		prog.To, prog.To,
		lsid,
		streamType,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Radiko-AuthToken", client.AuthToken())
	req.Header.Set("X-Radiko-AreaId", areaID)
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
// timefree playlist creation URL. Always prefers areafree=0 URLs; area-free
// access for premium members is indicated by type=c in the request, not by
// the URL. Falls back to the known default on error.
func discoverTimefreeEndpoint(ctx context.Context, client *radiko.Client, stationID string) string {
	xmlURL := stationStreamXMLBase + stationID + ".xml"
	req, err := http.NewRequestWithContext(ctx, "GET", xmlURL, nil)
	if err != nil {
		return timefreePlaylistEndpoint
	}
	req.Header.Set("X-Radiko-AuthToken", client.AuthToken())
	req.Header.Set("X-Radiko-AreaId", client.AreaID())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return timefreePlaylistEndpoint
	}
	defer resp.Body.Close()

	var data stationStreamData
	if err := xml.NewDecoder(resp.Body).Decode(&data); err != nil {
		return timefreePlaylistEndpoint
	}

	// Always prefer areafree=0 URLs; type=c in the request signals area-free to the CDN
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

// getTimeshiftChunklist collects all HLS segment URLs for a full timefree program
// by iterating through seek windows from program start to end.
// It replaces the single-call approach (getTimeshiftPlaylistM3U8 + GetChunklistFromM3U8)
// which only returned the first ~windowSeconds of content.
func getTimeshiftChunklist(ctx context.Context, client *radiko.Client, stationID string, start time.Time) ([]string, error) {
	prog, err := client.GetProgramByStartTime(ctx, stationID, start)
	if err != nil {
		return nil, err
	}

	needsAreaFree := client.AreaID() != "" && client.AreaID() != currentAreaID
	streamType := "b"
	if needsAreaFree {
		streamType = "c"
	}
	areaID := client.AreaID()
	if needsAreaFree {
		areaID = currentAreaID
	}

	endpoint := discoverTimefreeEndpoint(ctx, client, stationID)

	ftTime, err := time.ParseInLocation(datetimeLayout, prog.Ft, location)
	if err != nil {
		return nil, fmt.Errorf("parse ft %q: %w", prog.Ft, err)
	}
	toTime, err := time.ParseInLocation(datetimeLayout, prog.To, location)
	if err != nil {
		return nil, fmt.Errorf("parse to %q: %w", prog.To, err)
	}

	const windowSecs = 15 * 60 // 15-minute seek windows

	seen := make(map[string]bool)
	var chunklist []string

	for seek := ftTime; seek.Before(toTime); seek = seek.Add(windowSecs * time.Second) {
		seekStr := seek.Format(datetimeLayout)
		masterURL := fmt.Sprintf(
			"%s?station_id=%s&start_at=%s&ft=%s&end_at=%s&to=%s&l=%d&lsid=%s&type=%s",
			endpoint, stationID,
			prog.Ft, seekStr,
			prog.To, prog.To,
			windowSecs,
			randomHex(16),
			streamType,
		)

		req, err := http.NewRequestWithContext(ctx, "GET", masterURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Radiko-AuthToken", client.AuthToken())
		req.Header.Set("X-Radiko-AreaId", areaID)
		req.Header.Set("X-Radiko-App", "pc_html5")
		req.Header.Set("X-Radiko-App-Version", "0.0.1")
		req.Header.Set("X-Radiko-User", "test-stream")
		req.Header.Set("X-Radiko-Device", "pc")
		req.Header.Set("Origin", "https://radiko.jp")
		req.Header.Set("Referer", "https://radiko.jp/")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		mediaURI, parseErr := parseMasterM3U8URI(resp.Body)
		resp.Body.Close()
		if parseErr != nil {
			return nil, fmt.Errorf("window ft=%s: %w", seekStr, parseErr)
		}

		segments, err := radiko.GetChunklistFromM3U8(mediaURI)
		if err != nil {
			return nil, fmt.Errorf("chunklist ft=%s: %w", seekStr, err)
		}

		for _, seg := range segments {
			if !seen[seg] {
				seen[seg] = true
				chunklist = append(chunklist, seg)
			}
		}
	}

	if len(chunklist) == 0 {
		return nil, fmt.Errorf("no segments found for %s %s-%s", stationID, prog.Ft, prog.To)
	}
	return chunklist, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
