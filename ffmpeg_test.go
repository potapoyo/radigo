package radigo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestConcatFailureKeepsInput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "invalid.aac")
	if err := os.WriteFile(input, []byte("not audio"), 0600); err != nil {
		t.Fatal(err)
	}
	err := ConcatAACFiles(context.Background(), []string{input}, dir, filepath.Join(dir, "result.aac"))
	if err == nil {
		t.Fatal("invalid audio accepted")
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatal("input removed after failed concat")
	}
}

func TestConcatAcrossBatchBoundary(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	seed := filepath.Join(t.TempDir(), "seed.aac")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.1", "-c:a", "aac", seed)
	if log, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, log)
	}
	data, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%09d.aac", i)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	output, err := ConcatAACFilesFromList(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 101*len(data) {
		t.Fatalf("lost frames across batch boundary: got %d bytes want %d", len(got), 101*len(data))
	}
}
