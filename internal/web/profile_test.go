package web

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"go-git-server/internal/render"
)

// upload posts a file as the "avatar" field, like the profile form does.
func (e *testEnv) upload(c *http.Client, path, csrf string, file []byte) (int, string) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if csrf != "" {
		mw.WriteField("csrf", csrf)
	}
	fw, _ := mw.CreateFormFile("avatar", "me.png")
	fw.Write(file)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func testPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestProfile(t *testing.T) {
	e := newTestEnv(t)
	alice := e.login("alice")
	if code, body := e.get(alice, "/settings"); code != 200 || !strings.Contains(body, `name="website"`) {
		t.Fatalf("/settings does not lead to the profile: %d", code)
	}
	_, body := e.get(alice, "/settings/profile")
	csrf := csrfToken(t, body)

	// Website: checked, stored with a scheme, shown on the user page.
	for _, bad := range []string{"javascript:alert(1)", "https://u:p@example.com", "ftp://example.com"} {
		if code, body := e.post(alice, "/settings/profile", url.Values{"csrf": {csrf}, "website": {bad}}, ""); code != 400 || !strings.Contains(body, "http or https") {
			t.Errorf("website %q: %d", bad, code)
		}
	}
	if code, _ := e.post(alice, "/settings/profile", url.Values{"website": {"example.com"}}, ""); code != 403 {
		t.Errorf("website without CSRF token: %d", code)
	}
	if code, body := e.post(alice, "/settings/profile", url.Values{"csrf": {csrf}, "website": {"alice.example/blog"}}, ""); code != 200 || !strings.Contains(body, "Website saved") {
		t.Fatalf("save website: %d", code)
	}
	_, page := e.get(http.DefaultClient, "/~alice") // anyone can see it
	if !strings.Contains(page, `<a href="https://alice.example/blog" rel="me nofollow ugc noopener">alice.example/blog</a>`) {
		t.Fatalf("website not on the user page:\n%s", page)
	}
	if strings.Contains(page, `class="avatar"`) {
		t.Fatal("picture shown before one was uploaded")
	}

	// Picture: re-encoded to a square PNG, served with its own headers.
	if code, body := e.upload(alice, "/settings/profile/avatar", csrf, []byte("<svg onload=alert(1)>")); code != 400 || !strings.Contains(body, "PNG, JPEG or GIF") {
		t.Fatalf("SVG accepted: %d", code)
	}
	if code, _ := e.upload(alice, "/settings/profile/avatar", "", testPNG(10, 10)); code != 403 {
		t.Fatalf("upload without CSRF token: %d", code)
	}
	if code, _ := e.upload(alice, "/settings/profile/avatar", csrf, make([]byte, render.MaxAvatarUpload+20<<10)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", code)
	}
	upload := testPNG(600, 300)
	if code, body := e.upload(alice, "/settings/profile/avatar", csrf, upload); code != 200 || !strings.Contains(body, "Profile picture saved") {
		t.Fatalf("upload: %d\n%s", code, body)
	}
	_, page = e.get(http.DefaultClient, "/~alice")
	m := regexp.MustCompile(`<img class="avatar" src="(/avatars/alice\?v=[0-9a-f]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("picture not on the user page:\n%s", page)
	}
	resp, err := http.Get(e.srv.URL + m[1])
	if err != nil {
		t.Fatal(err)
	}
	served, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") ||
		!strings.Contains(resp.Header.Get("Cache-Control"), "immutable") || bytes.Equal(served, upload) {
		t.Fatalf("avatar: %d %v", resp.StatusCode, resp.Header)
	}
	img, err := png.Decode(bytes.NewReader(served))
	if err != nil || img.Bounds() != image.Rect(0, 0, render.AvatarSize, render.AvatarSize) {
		t.Fatalf("avatar is not a %d px square PNG: %v %v", render.AvatarSize, err, img)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+m[1], nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match: %v %v", resp.Status, err)
	}
	if _, page := e.get(alice, "/~bob"); strings.Contains(page, "avatars/alice") || strings.Contains(page, "alice.example") {
		t.Fatal("alice's profile on bob's page")
	}

	// Removing it.
	if code, body := e.post(alice, "/settings/profile/avatar/delete", url.Values{"csrf": {csrf}}, ""); code != 200 || !strings.Contains(body, "Profile picture removed") {
		t.Fatalf("remove: %d", code)
	}
	if code, _ := e.get(http.DefaultClient, "/avatars/alice"); code != 404 {
		t.Fatalf("removed avatar still served: %d", code)
	}
	for _, path := range []string{"/avatars/bob", "/avatars/nobody", "/avatars/..%2Fx"} {
		if code, _ := e.get(http.DefaultClient, path); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	e.post(alice, "/settings/profile", url.Values{"csrf": {csrf}, "website": {""}}, "")
	if _, page := e.get(alice, "/~alice"); strings.Contains(page, "alice.example") {
		t.Fatal("website not removed")
	}

	l, _ := e.s.store.AuditLog("alice", 10)
	var actions []string
	for _, a := range l {
		actions = append(actions, a.Action+":"+a.Detail)
	}
	if got := strings.Join(actions, ","); got != "website changed:removed,profile picture removed:,profile picture changed:,website changed:https://alice.example/blog" {
		t.Fatalf("audit: %s", got)
	}
}
