package internal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var frame = []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0}

func TestDownloadRejectsBadResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
	}{
		{"503", 503, []byte("unavailable")}, {"403", 403, []byte("forbidden")},
		{"empty", 200, nil}, {"HTML", 200, []byte("<html>error</html>")}, {"truncated", 200, frame[:7]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = old })
			var calls int32
			http.DefaultClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(bytes.NewReader(tc.body)), Request: r}, nil
			})}
			dir := t.TempDir()
			if err := BulkDownload([]string{"https://audio.invalid/a.aac"}, dir); err == nil {
				t.Fatal("invalid audio accepted")
			}
			if calls != maxAttempts {
				t.Fatalf("calls=%d", calls)
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 0 {
				t.Fatal("invalid audio saved")
			}
		})
	}
}

func TestDownloadRetriesAndPreservesOrder(t *testing.T) {
	old := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = old })
	var calls int32
	http.DefaultClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		code := 200
		body := append([]byte(nil), frame...)
		if atomic.AddInt32(&calls, 1) == 1 {
			code = 503
		} else {
			body[7] = byte(len(r.URL.Path))
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})}
	links := []string{"https://audio.invalid/z/a.aac", "https://audio.invalid/long/a.aac", "https://audio.invalid/z/a.aac"}
	dir := t.TempDir()
	if err := BulkDownload(links, dir); err != nil {
		t.Fatal(err)
	}
	for i, want := range []byte{8, 11, 8} {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%09d.aac", i)))
		if err != nil {
			t.Fatal(err)
		}
		if data[7] != want {
			t.Fatalf("segment %d got %d want %d", i, data[7], want)
		}
	}
	if calls != 4 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestDownloadCanceledAndEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := BulkDownloadContext(ctx, []string{"https://audio.invalid/a"}, t.TempDir()); err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
	if err := BulkDownload(nil, t.TempDir()); err == nil {
		t.Fatal("empty list accepted")
	}
}

func TestAACValidation(t *testing.T) {
	withID3 := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}, frame...)
	if err := validateAAC(withID3); err != nil {
		t.Fatal(err)
	}
	if err := validateAAC(append(append([]byte(nil), frame...), frame...)); err != nil {
		t.Fatal(err)
	}
	if err := validateAAC(withID3[:10]); err == nil {
		t.Fatal("ID3 without audio accepted")
	}
}
