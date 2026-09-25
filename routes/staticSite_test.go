package routes

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ginframework "github.com/gin-gonic/gin"
	ginadapter "github.com/goravel/gin"
)

func TestWriteFileRendersCompleteResponse(t *testing.T) {
	file := filepath.Join(t.TempDir(), "index.html")
	want := []byte("<html>admin</html>")
	if err := os.WriteFile(file, want, 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := ginframework.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest("GET", "/admin", nil)

	if !writeFile(ginadapter.NewContext(ginContext), file) {
		t.Fatal("writeFile returned false")
	}
	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Body.Bytes(); string(got) != string(want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
}
