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

	// timeshiftWindowSec is the window size per CDN request.
	// The CDN only accepts l=15; larger values result in HTTP 400.
	timeshiftWindowSec = 15
)

type stationStreamData struct {
	URLs []stationStreamURL `xml:"url"`
}

type stationStreamURL struct {
	Arefree           string `xml:"areafree,attr"`
	Timefree          string `xml:"timefree,attr"`
	PlaylistCreateURL string `xml:"playlist_create_url"`
}

// getTimeshiftChunklist returns all segment URLs for a timeshifted program.
// It fetches the program in chunks of maxChunkSec seconds to stay within CDN limits.
func getTimeshiftChunklist(ctx context.Context, client *radiko.Client, stationID string, start time.Time) ([]string, error) {
	prog, err := client.GetProgramByStartTime(ctx, stationID, start)
	if err != nil {
		return nil, err
	}

	ft, err := time.ParseInLocation(datetimeLayout, prog.Ft, location)
	if err != nil {
		return nil, fmt.Errorf("failed to parse program ft: %w", err)
	}
	to, err := time.ParseInLocation(datetimeLayout, prog.To, location)
	if err != nil {
		return nil, fmt.Errorf("failed to parse program to: %w", err)
	}

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

	var allSegments []string
	seen := make(map[string]bool)

	for chunkStart := ft; chunkStart.Before(to); chunkStart = chunkStart.Add(timeshiftWindowSec * time.Second) {
		lsid := randomHex(16)
		url := fmt.Sprintf(
			"%s?station_id=%s&start_at=%s&ft=%s&end_at=%s&to=%s&preroll=2&l=%d&lsid=%s&type=%s",
			endpoint,
			stationID,
			chunkStart.Format(datetimeLayout), prog.Ft,
			prog.To, prog.To,
			timeshiftWindowSec,
			lsid,
			streamType,
		)

		segments, err := fetchTimeshiftWindow(ctx, client, url, areaID)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch chunk starting %s: %w", chunkStart.Format(datetimeLayout), err)
		}

		for _, seg := range segments {
			if !seen[seg] {
				seen[seg] = true
				allSegments = append(allSegments, seg)
			}
		}
	}

	return allSegments, nil
}

// fetchTimeshiftWindow fetches one chunk window's segment URLs from the CDN.
func fetchTimeshiftWindow(ctx context.Context, client *radiko.Client, url, areaID string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
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
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("timefree playlist request failed: HTTP %d", resp.StatusCode)
	}

	mediaURI, err := parseMasterM3U8URI(resp.Body)
	if err != nil {
		return nil, err
	}

	return radiko.GetChunklistFromM3U8(mediaURI)
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

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
