package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

// The first snapshot must arrive immediately on connect and carry the task
// that was just applied — `px watch` diffs phases between frames, so a
// missing first frame would hide the initial phase.
func TestWatchStreamsSnapshot(t *testing.T) {
	srv, _ := newTestServer(t)
	if _, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/watch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("want ndjson content type, got %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() {
		t.Fatalf("no first snapshot: %v", sc.Err())
	}
	var snap struct {
		Tasks []*v1alpha1.Task `json:"tasks"`
	}
	if err := json.Unmarshal(sc.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].Metadata.Name != "t1" {
		t.Fatalf("first snapshot must list t1, got: %s", sc.Text())
	}

	// The stream keeps going (second frame within the timeout proves the
	// ticker is alive and not just a one-shot response).
	if !sc.Scan() {
		t.Fatalf("stream ended after first frame: %v", sc.Err())
	}
}
