package fixture

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFixtureBehaviors(t *testing.T) {
	s := httptest.NewServer(New(Config{}))
	defer s.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	for _, path := range []string{"/headers", "/body", "/redirect", "/empty", "/disconnect", "/missing"} {
		t.Run(path, func(t *testing.T) {
			res, err := client.Get(s.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, readErr := io.ReadAll(res.Body)
			switch path {
			case "/headers":
				if string(body) != "headers delayed\n" || readErr != nil {
					t.Fatalf("%q %v", body, readErr)
				}
			case "/body":
				if string(body) != "chunk 0\nchunk 1\nchunk 2\nchunk 3\n" || readErr != nil {
					t.Fatalf("%q %v", body, readErr)
				}
			case "/empty", "/redirect":
				if res.StatusCode != 204 || len(body) != 0 || readErr != nil {
					t.Fatal("expected empty 204")
				}
			case "/disconnect":
				if !errors.Is(readErr, io.ErrUnexpectedEOF) || string(body) != "short" {
					t.Fatalf("%q %v", body, readErr)
				}
			case "/missing":
				if res.StatusCode != 404 {
					t.Fatal("expected 404")
				}
			}
		})
	}
}

func TestDelayedHeadersAndCancellation(t *testing.T) {
	s := httptest.NewServer(New(Config{HeaderDelay: time.Hour}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL+"/headers", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Client().Do(req)
	if res != nil {
		res.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline: %v", err)
	}
}

func TestBodyFlushesBeforeDelayedChunks(t *testing.T) {
	s := httptest.NewServer(New(Config{ChunkDelay: time.Hour}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/body", nil)
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal("headers were held behind body delay:", err)
	}
	cancel()
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Fatal("expected interrupted body")
	}
}

func TestTimeoutEndpoint(t *testing.T) {
	s := httptest.NewServer(New(Config{}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/timeout", nil)
	res, err := s.Client().Do(req)
	if res != nil {
		res.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline: %v", err)
	}
}
