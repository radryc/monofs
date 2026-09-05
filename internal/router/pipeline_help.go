package router

import (
	_ "embed"
	"net/http"
)

//go:embed help_pipelines.md
var pipelineHelpContent []byte

func (r *Router) handlePipelineHelp(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(pipelineHelpContent)
}
