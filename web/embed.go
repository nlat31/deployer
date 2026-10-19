package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist
var assets embed.FS
func Handler()http.Handler{root,_:=fs.Sub(assets,"dist");return http.FileServer(http.FS(root))}
