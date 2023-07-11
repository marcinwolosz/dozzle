package web

import (
	"compress/gzip"
	"context"
	"encoding/json"

	"fmt"
	"io"
	"net/http"
	"runtime"

	"time"

	"github.com/amir20/dozzle/docker"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/dustin/go-humanize"
	"github.com/go-chi/chi/v5"

	log "github.com/sirupsen/logrus"
)

func (h *handler) downloadLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	container, err := h.clientFromRequest(r).FindContainer(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now()

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.log.gz", container.Name, now.Format("2006-01-02T15-04-05")))
	w.Header().Set("Content-Type", "application/gzip")
	zw := gzip.NewWriter(w)
	defer zw.Close()
	zw.Name = fmt.Sprintf("%s-%s.log", container.Name, now.Format("2006-01-02T15-04-05"))
	zw.Comment = "Logs generated by Dozzle"
	zw.ModTime = now

	reader, err := h.clientFromRequest(r).ContainerLogsBetweenDates(r.Context(), id, time.Time{}, now, docker.STDALL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if container.Tty {
		io.Copy(zw, reader)
	} else {
		stdcopy.StdCopy(zw, zw, reader)
	}
}

func (h *handler) fetchLogsBetweenDates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/ld+json; charset=UTF-8")

	from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, _ := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	id := chi.URLParam(r, "id")

	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	container, err := h.clientFromRequest(r).FindContainer(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	reader, err := h.clientFromRequest(r).ContainerLogsBetweenDates(r.Context(), container.ID, from, to, stdTypes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	g := docker.NewEventGenerator(reader, container.Tty)

loop:
	for {
		select {
		case event, ok := <-g.Events:
			if !ok {
				break loop
			}
			if err := json.NewEncoder(w).Encode(event); err != nil {
				log.Errorf("json encoding error while streaming %v", err.Error())
			}
		}
	}
}

func (h *handler) streamLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	container, err := h.clientFromRequest(r).FindContainer(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-transform")
	w.Header().Add("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	lastEventId := r.Header.Get("Last-Event-ID")
	if len(r.URL.Query().Get("lastEventId")) > 0 {
		lastEventId = r.URL.Query().Get("lastEventId")
	}

	reader, err := h.clientFromRequest(r).ContainerLogs(r.Context(), container.ID, lastEventId, stdTypes)
	if err != nil {
		if err == io.EOF {
			fmt.Fprintf(w, "event: container-stopped\ndata: end of stream\n\n")
			f.Flush()
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	g := docker.NewEventGenerator(reader, container.Tty)

loop:
	for {
		select {
		case event, ok := <-g.Events:
			if !ok {
				log.WithFields(log.Fields{"id": id}).Debug("stream closed")
				break loop
			}
			if buf, err := json.Marshal(event); err != nil {
				log.Errorf("json encoding error while streaming %v", err.Error())
			} else {
				fmt.Fprintf(w, "data: %s\n", buf)
			}
			if event.Timestamp > 0 {
				fmt.Fprintf(w, "id: %d\n", event.Timestamp)
			}
			fmt.Fprintf(w, "\n")
			f.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ":ping \n\n")
			f.Flush()
		}
	}

	select {
	case err := <-g.Errors:
		if err != nil {
			if err == io.EOF {
				log.Debugf("container stopped: %v", container.ID)
				fmt.Fprintf(w, "event: container-stopped\ndata: end of stream\n\n")
				f.Flush()
			} else if err != context.Canceled {
				log.Errorf("unknown error while streaming %v", err.Error())
			}
		}
	default:
	}

	if log.IsLevelEnabled(log.DebugLevel) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		// For info on each, see: https://golang.org/pkg/runtime/#MemStats
		log.WithFields(log.Fields{
			"allocated":      humanize.Bytes(m.Alloc),
			"totalAllocated": humanize.Bytes(m.TotalAlloc),
			"system":         humanize.Bytes(m.Sys),
			"routines":       runtime.NumGoroutine(),
		}).Debug("runtime mem stats")
	}
}
