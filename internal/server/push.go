package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/push"
)

// Web Push: browsers register here to hear about the user's sessions while
// Collie is closed (internal/push sends, web/public/sw.js shows them).

func (s *Server) pushRoutes() {
	mx := s.mux
	mx.HandleFunc("GET /api/push", s.pushInfo)
	mx.HandleFunc("POST /api/push/subscriptions", s.pushSubscribe)
	mx.HandleFunc("POST /api/push/unsubscribe", s.pushUnsubscribe)
	mx.HandleFunc("POST /api/push/test", s.pushTest)
}

const errNoPush = "notifications are not available on this server"

// pushPayload is all a notification carries: no conversation content, no
// tool input.
type pushPayload struct {
	SID     string `json:"sid,omitempty"`
	Project string `json:"project,omitempty"`
	Title   string `json:"title,omitempty"`
	// Status: approval | question | done | error, or test.
	Status string `json:"status"`
	Lang   string `json:"lang,omitempty"`
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func pushLang(l string) string {
	if l == "ja" {
		return "ja"
	}
	return "en"
}

// pushContact is the VAPID subject: the address Collie is opened at, if
// it is an https: one ("" lets the push package use a mailto: stand-in).
func (s *Server) pushContact(r *http.Request) string {
	if u, err := url.Parse(s.opt.PublicURL); err == nil && u.Scheme == "https" && u.Host != "" {
		return "https://" + u.Host
	}
	// Already checked to be this server's own origin (sameOrigin).
	if o := r.Header.Get("Origin"); strings.HasPrefix(o, "https://") {
		return o
	}
	return ""
}

func (s *Server) pushInfo(w http.ResponseWriter, r *http.Request) {
	if s.opt.Push == nil {
		httpError(w, http.StatusNotFound, errNoPush)
		return
	}
	writeJSON(w, map[string]any{"key": s.opt.Push.PublicKey()})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	ps := s.opt.Push
	if ps == nil {
		httpError(w, http.StatusNotFound, errNoPush)
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Lang string `json:"lang"`
		// Replaces is the endpoint the browser had before renewing its
		// subscription (sent by the service worker, which knows no language).
		Replaces string `json:"replaces"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	u := current(r)
	sessHash, _ := r.Context().Value(sessKey).(string)
	old, renewed := push.Subscription{}, false
	if req.Replaces != "" && req.Replaces != req.Endpoint {
		old, renewed = ps.Get(u.ID, req.Replaces)
	}
	if req.Lang == "" && renewed {
		req.Lang = old.Lang
	}
	sub, err := ps.Add(push.Subscription{UserID: u.ID, Session: sessHash, Endpoint: req.Endpoint,
		P256dh: req.Keys.P256dh, Auth: req.Keys.Auth, Contact: s.pushContact(r), Lang: pushLang(req.Lang)})
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if renewed {
		ps.Remove(u.ID, old.Endpoint)
	}
	writeJSON(w, map[string]any{"id": sub.ID})
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.opt.Push == nil {
		httpError(w, http.StatusNotFound, errNoPush)
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.opt.Push.Remove(current(r).ID, req.Endpoint)
	ok(w)
}

// pushTest sends a test notification to the calling browser, and reports
// what the push service said.
func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	ps := s.opt.Push
	if ps == nil {
		httpError(w, http.StatusNotFound, errNoPush)
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, found := ps.Get(current(r).ID, req.Endpoint)
	if !found {
		httpError(w, http.StatusNotFound, "this device is not registered for notifications")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := ps.Send(ctx, sub, push.Message{Payload: mustJSON(pushPayload{Status: "test", Lang: sub.Lang}),
		TTL: 10 * time.Minute, Urgency: "high"})
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	ok(w)
}

func sessionPayload(sess model.Session, project, event, lang string) []byte {
	return mustJSON(pushPayload{SID: sess.ID, Project: clipRunes(project, 80), Title: clipRunes(sess.Title, 120),
		Status: event, Lang: lang})
}

// notifySession tells a session's owner about it on each of their
// browsers, except where Collie is open in front of them (that page shows
// the notification itself) and browsers whose sign-in has ended.
func (s *Server) notifySession(sess model.Session, event string) {
	ps := s.opt.Push
	if ps == nil || sess.Owner == "" {
		return
	}
	subs := ps.ForUser(sess.Owner)
	if len(subs) == 0 {
		return
	}
	u, found := s.users.User(sess.Owner)
	if !found {
		ps.RemoveUser(sess.Owner)
		return
	}
	if u.Disabled || !s.m.SessionVisible(u.ID, sess) {
		return
	}
	p, _ := s.m.Store().Project(sess.ProjectID)
	msg := push.Message{TTL: 24 * time.Hour, Urgency: "normal", Topic: sess.ID}
	if event == "approval" || event == "question" {
		msg.Urgency = "high"
	}
	for _, sub := range subs {
		if sub.Session != "" && !s.opt.NoAuth && !s.users.SessionValid(sub.Session) {
			ps.Remove(sub.UserID, sub.Endpoint) // signed out (or expired) on that browser
			continue
		}
		if s.m.Hub().Foreground(sub.UserID, sub.ID) {
			continue
		}
		m := msg
		m.Payload = sessionPayload(sess, p.Name, event, sub.Lang)
		go func(sub push.Subscription) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if err := ps.Send(ctx, sub, m); err != nil && !errors.Is(err, push.ErrGone) {
				log.Printf("web push: %v", err)
			}
		}(sub)
	}
}
