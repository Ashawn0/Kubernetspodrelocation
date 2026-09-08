package disrupt

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const HeaderPodUID = "X-Pod-Uid"

// Sample is one observed HTTP response under load.
type Sample struct {
	At     time.Time
	UID    string
	Status int
	Path   string // "clusterip" | "podip"
	Err    string
}

// Client is the frozen headline client: Connection: close, no keep-alive.
func HeadlineClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			MaxIdleConns:      0,
		},
	}
}

// KeepAliveClient is the sensitivity cell (not headline).
func KeepAliveClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: false,
		},
	}
}

// GetUID issues one GET and returns status + X-Pod-Uid.
func GetUID(ctx context.Context, client *http.Client, url string) (status int, uid string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Connection", "close")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get(HeaderPodUID), nil
}

// LoadGenerator fires concurrent Connection:close GETs until cancel.
type LoadGenerator struct {
	Client  *http.Client
	URL     string
	Path    string
	Workers int
	mu      sync.Mutex
	Samples []Sample
}

func (g *LoadGenerator) Run(ctx context.Context) {
	if g.Workers < 1 {
		g.Workers = 8
	}
	var wg sync.WaitGroup
	for i := 0; i < g.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				st, uid, err := GetUID(ctx, g.Client, g.URL)
				s := Sample{At: time.Now().UTC(), UID: uid, Status: st, Path: g.Path}
				if err != nil {
					s.Err = err.Error()
				}
				g.mu.Lock()
				g.Samples = append(g.Samples, s)
				g.mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func (g *LoadGenerator) Snapshot() []Sample {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Sample, len(g.Samples))
	copy(out, g.Samples)
	return out
}

// FirstSuccessUID returns the first sample with status 200 and matching uid after t0.
func FirstSuccessUID(samples []Sample, wantUID string, after time.Time) (Sample, bool) {
	for _, s := range samples {
		if s.At.Before(after) {
			continue
		}
		if s.Status == 200 && s.UID == wantUID && s.Err == "" {
			return s, true
		}
	}
	return Sample{}, false
}

// FirstAny200 returns first status-200 after t0, any UID.
func FirstAny200(samples []Sample, after time.Time) (Sample, bool) {
	for _, s := range samples {
		if s.At.Before(after) {
			continue
		}
		if s.Status == 200 && s.Err == "" {
			return s, true
		}
	}
	return Sample{}, false
}

// CountUID counts successful samples with given UID in [start, end).
func CountUID(samples []Sample, uid string, start, end time.Time) int {
	n := 0
	for _, s := range samples {
		if s.Status != 200 || s.Err != "" || s.UID != uid {
			continue
		}
		if !s.At.Before(start) && s.At.Before(end) {
			n++
		}
	}
	return n
}

// FormatDurationMS returns milliseconds as float for JSONL.
func FormatDurationMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// WaitFirstUID polls url until X-Pod-Uid == want or timeout.
func WaitFirstUID(ctx context.Context, client *http.Client, url, want string, path string) (Sample, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Minute)
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Sample{}, ctx.Err()
		default:
		}
		st, uid, err := GetUID(ctx, client, url)
		s := Sample{At: time.Now().UTC(), UID: uid, Status: st, Path: path}
		if err != nil {
			s.Err = err.Error()
		} else if st == 200 && uid == want {
			return s, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return Sample{}, fmt.Errorf("timeout waiting for uid %s on %s", want, url)
}
