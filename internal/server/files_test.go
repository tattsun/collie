package server

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/session"
)

// Attached files keep their name and come back only as downloads.
func TestAttachedFileUploadAndDownload(t *testing.T) {
	s := newTestServer(t, Options{})
	secret, err := s.users.NewSession(s.owner, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	as := cookie(sessionCookie, secret)
	upload := func(name string, data []byte) *http.Response {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(data)
		mw.Close()
		w := s.doBody("POST", "/api/files", &body, mw.FormDataContentType(), as)
		return w.Result()
	}

	page := []byte("<!doctype html><script>alert(document.cookie)</script>")
	res := upload("依頼書 (draft).html", page)
	var f model.File
	if err := json.NewDecoder(res.Body).Decode(&f); err != nil || res.StatusCode != 200 {
		t.Fatalf("upload: %d %v", res.StatusCode, err)
	}
	if f.Name != "依頼書 (draft).html" || f.Size != int64(len(page)) || len(f.ID) != 32 {
		t.Fatalf("uploaded: %+v", f)
	}

	w := s.do("GET", "/api/files/"+f.ID, "", as)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), page) {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}
	h := w.Result().Header
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("served as something a browser could render: %v", h)
	}
	disp, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil || disp != "attachment" || params["filename"] != f.Name {
		t.Errorf("Content-Disposition %q: %v %v", h.Get("Content-Disposition"), params, err)
	}

	if w := s.do("GET", "/api/files/"+f.ID, ""); w.Code == 200 {
		t.Error("downloaded without signing in")
	}
	for _, id := range []string{strings.Repeat("0", 32), "not-an-id", f.ID[:16], strings.ToUpper(f.ID)} {
		if w := s.do("GET", "/api/files/"+id, "", as); w.Code != 404 {
			t.Errorf("GET %s: %d", id, w.Code)
		}
	}
	if res := upload("big.bin", make([]byte, session.MaxFile+1)); res.StatusCode != 413 {
		t.Errorf("a file over the limit: %d", res.StatusCode)
	}
}
