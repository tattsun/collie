package server

import (
	"net/http"
	"time"

	"github.com/tattsun/collie/internal/session"
)

// Schedules: each user sees and manages only their own.
func (s *Server) scheduleRoutes() {
	mx := s.mux
	mx.HandleFunc("GET /api/schedules", s.listSchedules)
	mx.HandleFunc("POST /api/schedules", s.createSchedule)
	mx.HandleFunc("GET /api/schedules/times", s.scheduleTimes)
	mx.HandleFunc("PATCH /api/schedules/{id}", s.patchSchedule)
	mx.HandleFunc("DELETE /api/schedules/{id}", s.deleteSchedule)
	mx.HandleFunc("POST /api/schedules/{id}/run", s.runSchedule)
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.m.Schedules(actor(r).ID))
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req session.ScheduleReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	sc, err := s.m.CreateSchedule(actor(r), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, sc)
}

func (s *Server) patchSchedule(w http.ResponseWriter, r *http.Request) {
	var req session.ScheduleReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	sc, err := s.m.PatchSchedule(actor(r), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, sc)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteSchedule(actor(r), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request) {
	run, err := s.m.RunScheduleNow(actor(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, run)
}

// scheduleTimes previews when a cron expression runs (?cron=…&tz=…).
func (s *Server) scheduleTimes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	times, err := session.ScheduleTimes(q.Get("cron"), q.Get("tz"), time.Now(), 3)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]int64, len(times))
	for i, t := range times {
		out[i] = t.UnixMilli()
	}
	writeJSON(w, map[string]any{"times": out})
}
