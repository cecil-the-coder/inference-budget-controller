/*
Copyright 2024 eh-ops.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed webui/*
var webUIFS embed.FS

// WebUIHandler serves the llama.cpp web UI embedded in the operator.
// This provides a chat interface that uses the operator's OpenAI-compatible API.
type WebUIHandler struct {
	fileServer http.Handler
}

// NewWebUIHandler creates a new web UI handler.
func NewWebUIHandler() *WebUIHandler {
	// Get the webui subdirectory from the embedded FS
	subFS, err := fs.Sub(webUIFS, "webui")
	if err != nil {
		panic(err)
	}

	return &WebUIHandler{
		fileServer: http.FileServer(http.FS(subFS)),
	}
}

// HandleIndex serves the web UI index page.
func (h *WebUIHandler) HandleIndex(c *gin.Context) {
	// Open index.html from embedded FS
	file, err := webUIFS.Open("webui/index.html")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load UI"})
		return
	}
	defer func() {
		_ = file.Close()
	}()

	stat, err := file.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read file info"})
		return
	}

	c.DataFromReader(http.StatusOK, stat.Size(), "text/html; charset=utf-8", file, nil)
}

// HandleStatic serves static files (bundle.js, bundle.css, etc.).
func (h *WebUIHandler) HandleStatic(c *gin.Context) {
	// Get the filepath from the URL parameter
	filepath := c.Param("filepath")

	// Strip leading slash if present
	filepath = strings.TrimPrefix(filepath, "/")

	// Sanitize the path to prevent directory traversal
	filepath = path.Base(filepath)
	if filepath == "." || filepath == ".." {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid path"})
		return
	}

	// Open file from embedded FS
	file, err := webUIFS.Open("webui/" + filepath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}
	defer func() {
		_ = file.Close()
	}()

	// Get file info for content type
	stat, err := file.Stat()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read file"})
		return
	}

	// Determine content type
	contentType := "application/octet-stream"
	switch path.Ext(filepath) {
	case ".js":
		contentType = "application/javascript"
	case ".css":
		contentType = "text/css"
	case ".html":
		contentType = "text/html; charset=utf-8"
	case ".json":
		contentType = "application/json"
	case ".png":
		contentType = "image/png"
	case ".svg":
		contentType = "image/svg+xml"
	case ".ico":
		contentType = "image/x-icon"
	}

	// Set cache headers for static assets
	c.Header("Cache-Control", "public, max-age=31536000") // 1 year for versioned assets

	// Serve the file
	c.DataFromReader(http.StatusOK, stat.Size(), contentType, file, nil)
}
