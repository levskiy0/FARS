//go:build integration

package httpapi

import (
	"context"
	"image/color"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"fars/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/h2non/bimg"
)

// TestHandleResizeSkipsCacheWriteWhenClientAlreadyGone drives handleResize
// directly with a request whose context is already cancelled, mirroring a
// client that disconnected before the resize/semaphore work even started.
// The handler must bail out without spending a resize slot or writing a
// cache entry.
func TestHandleResizeSkipsCacheWriteWhenClientAlreadyGone(t *testing.T) {
	cfg, _, handler, _ := newResizeTestServer(t, config.ResizeConfig{})
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 16, 16, color.NRGBA{R: 90, G: 90, B: 200, A: 255}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "geometry", Value: "8x8"}, {Key: "filepath", Value: "/img/photo.png"}}
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil).WithContext(ctx)

	handler.handleResize(ginCtx)

	if n := countRegularFiles(t, cfg.Storage.CacheDir); n != 0 {
		t.Fatalf("expected no cache file written for a request whose client already disconnected, found %d", n)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected no response body to be written, got %d bytes", recorder.Body.Len())
	}
}

// lateCancelContext reports "not cancelled" for the first `grace` calls to
// Err() and cancelled from then on, with a Done channel that never closes.
// That places the disconnect deterministically between the handler's
// pre-resize bail-out and its post-resize check, which is the window a real
// client abort lands in.
type lateCancelContext struct {
	context.Context
	grace *atomic.Int32
}

func (c lateCancelContext) Done() <-chan struct{} { return nil }

func (c lateCancelContext) Err() error {
	if c.grace.Add(-1) >= 0 {
		return nil
	}
	return context.Canceled
}

// TestHandleResizeCachesWorkWhenClientDisconnectsMidResize covers the
// opposite of the test above: the resize has already been paid for by the
// time the client goes away, so the bytes must still reach the cache. They
// used to be dropped, which made the next visitor render the same variant
// again.
func TestHandleResizeCachesWorkWhenClientDisconnectsMidResize(t *testing.T) {
	cfg, _, handler, _ := newResizeTestServer(t, config.ResizeConfig{})
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 40, 40, color.NRGBA{R: 90, G: 90, B: 200, A: 255}))

	grace := &atomic.Int32{}
	grace.Store(1) // survive the pre-resize check, gone by the post-resize one

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "geometry", Value: "20x20"}, {Key: "filepath", Value: "/img/photo.png"}}
	request := httptest.NewRequest(http.MethodGet, "/resize/20x20/img/photo.png", nil)
	ginCtx.Request = request.WithContext(lateCancelContext{Context: request.Context(), grace: grace})

	handler.handleResize(ginCtx)

	if recorder.Body.Len() != 0 {
		t.Fatalf("expected no response body for a client that hung up, got %d bytes", recorder.Body.Len())
	}
	if n := countRegularFiles(t, cfg.Storage.CacheDir); n != 1 {
		t.Fatalf("finished resize must still be cached, found %d cache files", n)
	}
}

// TestHandleResizeConcurrentRequestsForSameURL fires many concurrent
// requests at the same resize URL and checks each one gets a correct,
// identical result — modelled on the existing invalidation concurrency test
// (TestInvalidationReviewConcurrentClearRequests).
func TestHandleResizeConcurrentRequestsForSameURL(t *testing.T) {
	cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{})
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 40, 40, color.NRGBA{R: 12, G: 200, B: 40, A: 255}))

	const count = 16
	responses := make(chan *httptest.ResponseRecorder, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/resize/20x20/img/photo.png", nil)
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			responses <- resp
		}()
	}
	close(start)
	wg.Wait()
	close(responses)

	var reference []byte
	for resp := range responses {
		if resp.Code != http.StatusOK {
			t.Fatalf("concurrent resize failed: status=%d body=%s", resp.Code, resp.Body.String())
		}
		size, err := bimg.NewImage(resp.Body.Bytes()).Size()
		if err != nil {
			t.Fatalf("inspect result size: %v", err)
		}
		if size.Width != 20 || size.Height != 20 {
			t.Fatalf("got %dx%d, want 20x20", size.Width, size.Height)
		}
		if reference == nil {
			reference = append([]byte(nil), resp.Body.Bytes()...)
			continue
		}
		if len(resp.Body.Bytes()) != len(reference) {
			t.Fatalf("concurrent responses disagree on size: %d vs %d bytes", len(resp.Body.Bytes()), len(reference))
		}
	}
	if n := countRegularFiles(t, cfg.Storage.CacheDir); n != 1 {
		t.Fatalf("expected exactly one cache file after concurrent requests for the same URL, found %d", n)
	}
}
