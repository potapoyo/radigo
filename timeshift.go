package radigo

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/grafov/m3u8"
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
// It follows actual segment timestamps within the CDN's 15-second request limit.
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

	lsid := randomHex(16)
	// These are successive seeks through one recording, not new playback starts.
	// Do not request a fresh preroll for each seek.
	return collectTimeshiftSegments(ctx, ft, to, func(chunkStart time.Time) ([]timedSegment, error) {
		link := fmt.Sprintf(
			"%s?station_id=%s&start_at=%s&ft=%s&end_at=%s&to=%s&preroll=0&l=%d&lsid=%s&type=%s",
			endpoint,
			stationID,
			chunkStart.Format(datetimeLayout), prog.Ft,
			prog.To, prog.To,
			timeshiftWindowSec,
			lsid,
			streamType,
		)

		return fetchTimedTimeshiftWindow(ctx, client, link, areaID)
	})
}

type timedSegment struct {
	URI      string
	Start    time.Time
	Duration time.Duration
}

const segmentTimeTolerance = 10 * time.Millisecond

// Program schedule boundaries need not align with the first audio timestamp.
// LFR, for example, starts 26ms after the scheduled second. Only the first
// segment gets this allowance; gaps within the recording remain strict.
const programStartTolerance = 100 * time.Millisecond

// Advance only through contiguous audio, never by the requested window length.
// A repeated URI may represent audio at another time (e.g. inserted content).
func collectTimeshiftSegments(ctx context.Context, start, end time.Time, fetch func(time.Time) ([]timedSegment, error)) ([]string, error) {
	if !end.After(start) {
		return nil, fmt.Errorf("invalid program time range")
	}
	cursor := start
	var result []string
	stalls := 0
	for cursor.Before(end.Add(-segmentTimeTolerance)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		segments, err := fetch(cursor)
		previous := cursor
		if err == nil {
			sort.SliceStable(segments, func(i, j int) bool { return segments[i].Start.Before(segments[j].Start) })
			for _, s := range segments {
				if s.URI == "" || s.Start.IsZero() || s.Duration <= 0 {
					return nil, fmt.Errorf("invalid segment timing")
				}
				if !s.Start.Before(end) {
					break
				}
				if len(result) == 0 {
					// Keep a segment covering the program boundary, or starting
					// just after it, then follow actual audio timestamps.
					if !s.Start.Add(s.Duration).After(start) {
						continue
					}
					if s.Start.After(start.Add(programStartTolerance)) {
						break
					}
					result = append(result, s.URI)
					cursor = s.Start.Add(s.Duration)
					continue
				}
				if s.Start.Before(cursor.Add(-segmentTimeTolerance)) {
					continue
				}
				if s.Start.After(cursor.Add(segmentTimeTolerance)) {
					break
				}
				result = append(result, s.URI)
				cursor = s.Start.Add(s.Duration)
			}
		}
		if cursor.Equal(previous) {
			stalls++
			if stalls >= 8 {
				if err != nil {
					return nil, fmt.Errorf("playlist retrieval failed at %s: %w", cursor.Format(time.RFC3339Nano), err)
				}
				return nil, fmt.Errorf("missing audio at %s after %d attempts", cursor.Format(time.RFC3339Nano), stalls)
			}
			timer := time.NewTimer(time.Duration(stalls) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		} else {
			stalls = 0
		}
	}
	return result, nil
}

// Keep HTTP errors, timing and relative URIs instead of discarding HLS metadata.
func fetchTimedTimeshiftWindow(ctx context.Context, client *radiko.Client, link, areaID string) ([]timedSegment, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
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

	reference, err := url.Parse(mediaURI)
	if err != nil {
		return nil, err
	}
	mediaURL := resp.Request.URL.ResolveReference(reference)
	mediaReq, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL.String(), nil)
	if err != nil {
		return nil, err
	}
	mediaResp, err := http.DefaultClient.Do(mediaReq)
	if err != nil {
		return nil, err
	}
	defer mediaResp.Body.Close()
	if mediaResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media playlist request failed: HTTP %d", mediaResp.StatusCode)
	}
	return parseTimedPlaylist(mediaResp.Body, mediaResp.Request.URL)
}

func parseTimedPlaylist(body io.Reader, base *url.URL) ([]timedSegment, error) {
	p, kind, err := m3u8.DecodeFrom(body, true)
	if err != nil {
		return nil, err
	}
	if kind != m3u8.MEDIA {
		return nil, fmt.Errorf("expected media playlist")
	}
	var result []timedSegment
	var next time.Time
	for _, s := range p.(*m3u8.MediaPlaylist).Segments {
		if s == nil {
			continue
		}
		if s.Discontinuity {
			next = time.Time{}
		}
		if !s.ProgramDateTime.IsZero() {
			next = s.ProgramDateTime
		}
		if next.IsZero() || s.Duration <= 0 {
			return nil, fmt.Errorf("missing or invalid segment time")
		}
		if s.Key != nil && s.Key.Method != "NONE" {
			return nil, fmt.Errorf("encrypted audio is not supported")
		}
		if s.Map != nil || s.Limit != 0 {
			return nil, fmt.Errorf("unsupported audio segment format")
		}
		u, err := url.Parse(s.URI)
		if err != nil {
			return nil, err
		}
		duration := time.Duration(s.Duration * float64(time.Second))
		result = append(result, timedSegment{base.ResolveReference(u).String(), next, duration})
		next = next.Add(duration)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty media playlist")
	}
	return result, nil
}

// discoverTimefreeEndpoint fetches the station stream XML to find the correct
// timefree playlist creation URL. Always prefers areafree=0 URLs; area-free
// access for premium members is indicated by type=c in the request, not by
// the URL. Falls back to the known default on error.
func discoverTimefreeEndpoint(ctx context.Context, client *radiko.Client, stationID string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
	if resp.StatusCode != http.StatusOK {
		return timefreePlaylistEndpoint
	}

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
