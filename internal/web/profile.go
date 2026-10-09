package web

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"go-git-server/internal/account"
	"go-git-server/internal/render"
	"go-git-server/internal/store"
)

// Settings -> profile: a website link and a profile picture, shown on the
// user's page. Both are public, like the user page itself.

type profileData struct {
	Error, Notice string
	Website       string // the website field
	Profile       store.Profile
}

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request, status int, data profileData) {
	name := currentUser(r).Name
	prof, err := s.store.Profile(name)
	if err != nil {
		log.Printf("profile page: user=%q: %v", name, err)
		s.error(w, r, http.StatusInternalServerError, "Could not load your profile.")
		return
	}
	data.Profile = prof
	if status == http.StatusOK {
		data.Website = prof.Website
	}
	p := s.newPage(r, "Profile", data)
	p.Tab = "profile"
	s.render(w, status, "profile", p)
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	var data profileData
	switch r.URL.Query().Get("saved") {
	case "website":
		data.Notice = "Website saved."
	case "avatar":
		data.Notice = "Profile picture saved."
	case "avatar-removed":
		data.Notice = "Profile picture removed."
	}
	s.profilePage(w, r, http.StatusOK, data)
}

func (s *Server) handleWebsite(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	input := r.PostFormValue("website")
	website, err := account.CleanWebsite(input)
	if err != nil {
		s.profilePage(w, r, http.StatusBadRequest, profileData{Error: capitalize(err.Error()) + ".", Website: input})
		return
	}
	if err := s.store.SetWebsite(name, website); err != nil {
		log.Printf("set website: user=%q: %v", name, err)
		s.profilePage(w, r, http.StatusInternalServerError, profileData{Error: "Could not save the website.", Website: input})
		return
	}
	detail := website
	if detail == "" {
		detail = "removed"
	}
	s.audit(r, "website changed", name, detail)
	http.Redirect(w, r, "/settings/profile?saved=website", http.StatusSeeOther)
}

func (s *Server) handleAvatarUpload(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	f, hdr, err := r.FormFile("avatar")
	if err != nil {
		s.profilePage(w, r, http.StatusBadRequest, profileData{Error: "Choose a picture to upload."})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, render.MaxAvatarUpload+1))
	if err != nil || hdr.Size > render.MaxAvatarUpload || len(data) > render.MaxAvatarUpload {
		s.profilePage(w, r, http.StatusRequestEntityTooLarge, profileData{Error: "The picture is too large; it may be at most 2 MiB."})
		return
	}
	png, err := render.Avatar(data)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, render.ErrAvatarBusy) {
			status = http.StatusServiceUnavailable
		}
		s.profilePage(w, r, status, profileData{Error: capitalize(err.Error()) + "."})
		return
	}
	if err := s.store.SetAvatar(name, png); err != nil {
		log.Printf("set avatar: user=%q: %v", name, err)
		s.profilePage(w, r, http.StatusInternalServerError, profileData{Error: "Could not save the picture."})
		return
	}
	s.audit(r, "profile picture changed", name, "")
	http.Redirect(w, r, "/settings/profile?saved=avatar", http.StatusSeeOther)
}

func (s *Server) handleAvatarDelete(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	if err := s.store.SetAvatar(name, nil); err != nil {
		log.Printf("delete avatar: user=%q: %v", name, err)
		s.profilePage(w, r, http.StatusInternalServerError, profileData{Error: "Could not remove the picture."})
		return
	}
	s.audit(r, "profile picture removed", name, "")
	http.Redirect(w, r, "/settings/profile?saved=avatar-removed", http.StatusSeeOther)
}

// handleAvatar serves a user's profile picture. Pages link to it with
// ?v=HASH, so a browser may keep it until the picture changes.
func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("user")
	if !account.UserNameRe.MatchString(name) {
		s.notFound(w, r)
		return
	}
	png, hash, err := s.store.Avatar(name)
	if err != nil {
		log.Printf("avatar: user=%q: %v", name, err)
	}
	if png == nil {
		s.notFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("ETag", `"`+hash+`"`)
	if r.URL.Query().Get("v") == hash {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(png))
}
