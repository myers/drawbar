// Adapted from gitea.com/gitea/act pkg/artifactcache/handler.go
// Original: Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
//
// Modified to use SQLite WAL instead of BoltDB. SQLite WAL allows concurrent
// readers + single writer without exclusive file locking, enabling rolling
// updates and multi-replica scaling.

package cache

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/nektos/act/pkg/common"
)

const urlBase = "/_apis/artifactcache"

// Handler implements the GitHub Actions cache protocol backed by SQLite + filesystem.
type Handler struct {
	dir        string
	db         *sql.DB
	storage    *Storage
	router     *httprouter.Router
	listener   net.Listener
	server     *http.Server
	outboundIP string
	gcing      atomic.Bool
	gcAt       time.Time
}

// StartHandler creates and starts a cache server on the given port.
func StartHandler(dir string, outboundIP string, port uint16) (*Handler, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".cache", "actcache")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	db, err := OpenDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		return nil, err
	}

	storage, err := NewStorage(filepath.Join(dir, "cache"))
	if err != nil {
		db.Close()
		return nil, err
	}

	if outboundIP == "" {
		if ip := common.GetOutboundIP(); ip != nil {
			outboundIP = ip.String()
		} else {
			db.Close()
			return nil, fmt.Errorf("unable to determine outbound IP address")
		}
	}

	h := &Handler{
		dir:        dir,
		db:         db,
		storage:    storage,
		outboundIP: outboundIP,
	}

	router := httprouter.New()
	router.GET(urlBase+"/cache", h.middleware(h.find))
	router.POST(urlBase+"/caches", h.middleware(h.reserve))
	router.PATCH(urlBase+"/caches/:id", h.middleware(h.upload))
	router.POST(urlBase+"/caches/:id", h.middleware(h.commit))
	router.GET(urlBase+"/artifacts/:id", h.middleware(h.get))
	router.POST(urlBase+"/clean", h.middleware(h.clean))
	h.router = router

	h.gcCache()

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		db.Close()
		return nil, err
	}
	server := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           router,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("cache server error", "error", err)
		}
	}()
	h.listener = listener
	h.server = server

	return h, nil
}

// ExternalURL returns the URL that job pods use to reach this cache server.
func (h *Handler) ExternalURL() string {
	return fmt.Sprintf("http://%s:%d",
		h.outboundIP,
		h.listener.Addr().(*net.TCPAddr).Port)
}

// Close shuts down the server and closes the database.
func (h *Handler) Close() error {
	if h == nil {
		return nil
	}
	var retErr error
	if h.server != nil {
		if err := h.server.Close(); err != nil {
			retErr = err
		}
	}
	if h.listener != nil {
		if err := h.listener.Close(); !errors.Is(err, net.ErrClosed) && err != nil {
			retErr = err
		}
	}
	if h.db != nil {
		if err := h.db.Close(); err != nil {
			retErr = err
		}
	}
	return retErr
}

// GET /_apis/artifactcache/cache
func (h *Handler) find(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	keys := strings.Split(r.URL.Query().Get("keys"), ",")
	for i, key := range keys {
		keys[i] = strings.ToLower(key)
	}
	version := r.URL.Query().Get("version")

	cache, err := FindCache(h.db, keys, version)
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	if cache == nil {
		h.responseJSON(w, r, 204)
		return
	}

	if ok, err := h.storage.Exist(uint64(cache.ID)); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	} else if !ok {
		_ = DeleteCache(h.db, cache.ID)
		h.responseJSON(w, r, 204)
		return
	}

	h.responseJSON(w, r, 200, map[string]any{
		"result":          "hit",
		"archiveLocation": fmt.Sprintf("%s%s/artifacts/%d", h.ExternalURL(), urlBase, cache.ID),
		"cacheKey":        cache.Key,
	})
}

// POST /_apis/artifactcache/caches
func (h *Handler) reserve(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var req struct {
		Key     string `json:"key"`
		Version string `json:"version"`
		Size    int64  `json:"cacheSize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	req.Key = strings.ToLower(req.Key)

	cache := &Cache{
		Key:     req.Key,
		Version: req.Version,
		Size:    req.Size,
	}
	if cache.Size == 0 {
		cache.Size = -1 // old actions/cache@v2 doesn't send size
	}

	if err := InsertCache(h.db, cache); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	h.responseJSON(w, r, 200, map[string]any{
		"cacheId": cache.ID,
	})
}

// PATCH /_apis/artifactcache/caches/:id
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}

	cache, err := GetCache(h.db, id)
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	if cache == nil {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %d: not reserved", id))
		return
	}
	if cache.Complete {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %v %q: already complete", cache.ID, cache.Key))
		return
	}

	start, _, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	if err := h.storage.Write(uint64(cache.ID), start, r.Body); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	TouchCache(h.db, id)
	h.responseJSON(w, r, 200)
}

// POST /_apis/artifactcache/caches/:id
func (h *Handler) commit(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}

	cache, err := GetCache(h.db, id)
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	if cache == nil {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %d: not reserved", id))
		return
	}
	if cache.Complete {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %v %q: already complete", cache.ID, cache.Key))
		return
	}

	size, err := h.storage.Commit(uint64(cache.ID), cache.Size)
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}

	if err := CompleteCache(h.db, id, size); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	h.responseJSON(w, r, 200)
}

// GET /_apis/artifactcache/artifacts/:id
func (h *Handler) get(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	TouchCache(h.db, id)
	h.storage.Serve(w, r, uint64(id))
}

// POST /_apis/artifactcache/clean
func (h *Handler) clean(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	h.responseJSON(w, nil, 200)
}

func (h *Handler) middleware(handler httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		slog.Debug("cache request", "method", r.Method, "uri", r.RequestURI)
		handler(w, r, params)
		go h.gcCache()
	}
}

const (
	keepUsed   = 30 * 24 * time.Hour
	keepUnused = 7 * 24 * time.Hour
	keepTemp   = 5 * time.Minute
	keepOld    = 5 * time.Minute
)

func (h *Handler) gcCache() {
	if !h.gcing.CompareAndSwap(false, true) {
		return
	}
	defer h.gcing.Store(false)

	if time.Since(h.gcAt) < time.Hour {
		return
	}
	h.gcAt = time.Now()

	// Remove incomplete caches older than keepTemp.
	caches, _ := FindExpired(h.db,
		"used_at < ? AND complete = 0", time.Now().Add(-keepTemp).Unix())
	for _, c := range caches {
		h.storage.Remove(uint64(c.ID))
		_ = DeleteCache(h.db, c.ID)
	}

	// Remove unused caches older than keepUnused.
	caches, _ = FindExpired(h.db,
		"used_at < ?", time.Now().Add(-keepUnused).Unix())
	for _, c := range caches {
		h.storage.Remove(uint64(c.ID))
		_ = DeleteCache(h.db, c.ID)
	}

	// Remove all caches older than keepUsed.
	caches, _ = FindExpired(h.db,
		"created_at < ?", time.Now().Add(-keepUsed).Unix())
	for _, c := range caches {
		h.storage.Remove(uint64(c.ID))
		_ = DeleteCache(h.db, c.ID)
	}

	// Remove duplicate caches (same key+version), keep the newest.
	caches, _ = FindDuplicates(h.db, time.Now().Add(-keepOld).Unix())
	for _, c := range caches {
		h.storage.Remove(uint64(c.ID))
		_ = DeleteCache(h.db, c.ID)
	}
}

func (h *Handler) responseJSON(w http.ResponseWriter, r *http.Request, code int, v ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	var data []byte
	if len(v) == 0 || v[0] == nil {
		data, _ = json.Marshal(struct{}{})
	} else if err, ok := v[0].(error); ok {
		if r != nil {
			slog.Error("cache error", "method", r.Method, "uri", r.RequestURI, "error", err)
		}
		data, _ = json.Marshal(map[string]any{"error": err.Error()})
	} else {
		data, _ = json.Marshal(v[0])
	}
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func parseContentRange(s string) (int64, int64, error) {
	s, _, _ = strings.Cut(strings.TrimPrefix(s, "bytes "), "/")
	s1, s2, _ := strings.Cut(s, "-")
	start, err := strconv.ParseInt(s1, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %q: %w", s, err)
	}
	stop, err := strconv.ParseInt(s2, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %q: %w", s, err)
	}
	return start, stop, nil
}
