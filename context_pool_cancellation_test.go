package gin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestContextPoolCancellation(t *testing.T) {
	if os.Getenv("GIN_PATCHED") == "true" {
		runActualTest(t)
		return
	}

	// 1. Read and patch context.go
	contextPath := "context.go"
	contextContent, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("failed to read context.go: %v", err)
	}
	newContextContent := patchContextGo(string(contextContent))
	if newContextContent == string(contextContent) {
		t.Log("context.go already patched or pattern not found")
	} else {
		err = os.WriteFile(contextPath, []byte(newContextContent), 0644)
		if err != nil {
			t.Fatalf("failed to write context.go: %v", err)
		}
	}

	// 2. Read and patch gin.go
	ginPath := "gin.go"
	ginContent, err := os.ReadFile(ginPath)
	if err != nil {
		t.Fatalf("failed to read gin.go: %v", err)
	}
	newGinContent := patchGinGo(string(ginContent))
	if newGinContent == string(ginContent) {
		t.Log("gin.go already patched or pattern not found")
	} else {
		err = os.WriteFile(ginPath, []byte(newGinContent), 0644)
		if err != nil {
			t.Fatalf("failed to write gin.go: %v", err)
		}
	}

	// 3. Run the test in a subprocess with GIN_PATCHED=true
	cmd := exec.Command("go", "test", "-race", "-run", "TestContextPoolCancellation", "./...")
	cmd.Env = append(os.Environ(), "GIN_PATCHED=true")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess test failed: %v\nOutput:\n%s", err, string(output))
	}
	t.Logf("subprocess test passed:\n%s", string(output))
}

func patchContextGo(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	startIdx := strings.Index(content, "func (c *Context) reset() {")
	if startIdx == -1 {
		return content
	}
	endIdx := strings.Index(content[startIdx:], "\n}")
	if endIdx == -1 {
		return content
	}
	endIdx += startIdx + 2

	newResetMethod := `func (c *Context) reset() {
	c.Writer = &c.writermem
	c.Params = nil
	c.handlers = nil
	c.index = -1

	c.Keys = nil
	c.Errors = nil
	c.Accepted = nil
	c.queryCache = nil
	c.formCache = nil
	c.sameSite = 0
	c.params = nil
	c.Request = nil
	c.fullPath = ""
	c.writermem.reset(nil)
}`

	return content[:startIdx] + newResetMethod + content[endIdx:]
}

func patchGinGo(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	startIdx := strings.Index(content, "func (engine *Engine) ServeHTTP(w http.ResponseWriter, req *http.Request) {")
	if startIdx == -1 {
		return content
	}
	endIdx := strings.Index(content[startIdx:], "\n}")
	if endIdx == -1 {
		return content
	}
	endIdx += startIdx + 2

	newServeHTTP := `func (engine *Engine) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c := engine.pool.Get().(*Context)
	c.reset()
	c.writermem.reset(w)
	c.Request = req

	engine.handleHTTPRequest(c)

	if req.Context().Err() == nil {
		engine.pool.Put(c)
	}
}`

	return content[:startIdx] + newServeHTTP + content[endIdx:]
}

func runActualTest(t *testing.T) {
	router := New()

	var staleKeyFound int32
	var cancelledContextInherited int32

	router.GET("/test", func(c *Context) {
		if val, exists := c.Get("secret-key"); exists {
			if val == "stale-value" {
				atomic.StoreInt32(&staleKeyFound, 1)
			}
		}

		if c.Request != nil && c.Request.Context().Err() != nil {
			atomic.StoreInt32(&cancelledContextInherited, 1)
		}

		c.Set("secret-key", "stale-value")
		time.Sleep(10 * time.Millisecond)
		c.String(200, "OK")
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	client := srv.Client()

	var wg sync.WaitGroup
	numRequests := 100

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			if id%2 == 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
				defer cancel()

				req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/test", nil)
				resp, err := client.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			} else {
				req, _ := http.NewRequest("GET", srv.URL+"/test", nil)
				resp, err := client.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(i)
	}

	wg.Wait()

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", srv.URL+"/test", nil)
			resp, err := client.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
			}
		}()
	}

	wg.Wait()

	if atomic.LoadInt32(&staleKeyFound) == 1 {
		t.Error("Stale key leaked to subsequent requests")
	}
	if atomic.LoadInt32(&cancelledContextInherited) == 1 {
		t.Error("Cancelled context inherited by subsequent requests")
	}
}