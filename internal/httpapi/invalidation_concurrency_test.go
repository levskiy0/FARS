package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"fars/internal/config"
	"github.com/gin-gonic/gin"
)

func TestInvalidationReviewConcurrentClearRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg, manager, router := clearTestRouter(t, "secret")
	rel := "img/photo.jpg"
	writeHTTPTestFile(t, cfg.Storage.BaseDir, rel, []byte("original"))
	var variants []string
	for _, path := range []string{"200x200/" + rel, "400x400/" + rel + ".webp", "x100/" + rel + ".avif"} {
		variants = append(variants, writeHTTPTestFile(t, cfg.Storage.CacheDir, path, []byte("resize")))
	}
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/other.jpg", []byte("unrelated original"))
	unrelated := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/other.jpg", []byte("keep"))

	cfg.Cache.CheckOriginalsInterval = config.Duration{Duration: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	manager.StartBackground(ctx)
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := manager.WaitBackground(stopCtx); err != nil {
			t.Errorf("shutdown monitor: %v", err)
		}
	})

	const count = 12
	responses := make(chan *httptest.ResponseRecorder, count)
	start := make(chan struct{})
	var requests sync.WaitGroup
	requestCtx, stopRequests := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopRequests()
	for range count {
		requests.Add(1)
		go func() {
			defer requests.Done()
			<-start
			request := httptest.NewRequest(http.MethodPost, "/cclear/"+rel, nil).WithContext(requestCtx)
			request.Header.Set("Authorization", "Bearer secret")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			responses <- response
		}()
	}
	close(start)
	requests.Wait()
	close(responses)

	removed := 0
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent clear failed: status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Invalidated []string `json:"invalidated"`
			Removed     int      `json:"variants_removed"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Invalidated) != 1 || body.Invalidated[0] != rel {
			t.Fatalf("unexpected invalidated paths: %v", body.Invalidated)
		}
		removed += body.Removed
	}
	if removed != len(variants) {
		t.Fatalf("files double-counted or not removed: got %d want %d", removed, len(variants))
	}
	for _, path := range variants {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("variant survived concurrent requests: %s, %v", path, err)
		}
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "keep" {
		t.Fatalf("unrelated resize changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(cfg.Storage.BaseDir + "/" + rel); err != nil || string(data) != "original" {
		t.Fatalf("original changed: %q, %v", data, err)
	}
}
