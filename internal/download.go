package internal

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxAttempts    = 4
	maxConcurrents = 8
)

func BulkDownload(list []string, output string) error {
	return BulkDownloadContext(context.Background(), list, output)
}

// Stable numbered names preserve playlist order, including repeated URLs.
func BulkDownloadContext(ctx context.Context, list []string, output string) error {
	if len(list) == 0 {
		return fmt.Errorf("empty audio list")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for n := 0; n < maxConcurrents; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				var err error
				for attempt := 0; attempt < maxAttempts; attempt++ {
					if ctx.Err() != nil {
						return
					}
					err = download(ctx, list[i], filepath.Join(output, fmt.Sprintf("%09d.aac", i)))
					if err == nil {
						break
					}
					if attempt+1 < maxAttempts {
						timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
						select {
						case <-ctx.Done():
							timer.Stop()
							return
						case <-timer.C:
						}
					}
				}
				if err != nil {
					once.Do(func() { firstErr = fmt.Errorf("audio segment %d: %w", i, err); cancel() })
					return
				}
			}
		}()
	}
dispatch:
	for i := range list {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func download(ctx context.Context, link, destination string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxSegmentSize = 8 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxSegmentSize {
		return fmt.Errorf("audio segment exceeds size limit")
	}
	if err := validateAAC(data); err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0600)
}

// Reject error pages, empty responses and truncated ADTS frames even on HTTP 200.
func validateAAC(data []byte) error {
	frames := 0
	for len(data) > 0 {
		if len(data) >= 10 && string(data[:3]) == "ID3" {
			length := 10
			for _, b := range data[6:10] {
				if b&0x80 != 0 {
					return fmt.Errorf("invalid ID3 size")
				}
			}
			length += int(data[6])<<21 | int(data[7])<<14 | int(data[8])<<7 | int(data[9])
			if data[5]&0x10 != 0 {
				length += 10
			}
			if length > len(data) {
				return fmt.Errorf("truncated ID3 tag")
			}
			data = data[length:]
			continue
		}
		if len(data) < 7 || data[0] != 0xff || data[1]&0xf6 != 0xf0 {
			return fmt.Errorf("invalid AAC frame")
		}
		length := int(data[3]&3)<<11 | int(data[4])<<3 | int(data[5]>>5)
		header := 7
		if data[1]&1 == 0 {
			header = 9
		}
		if length < header || length > len(data) {
			return fmt.Errorf("truncated AAC frame")
		}
		frames++
		data = data[length:]
	}
	if frames == 0 {
		return fmt.Errorf("empty audio segment")
	}
	return nil
}
