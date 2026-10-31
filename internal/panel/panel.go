// Package panel 内嵌管理面板的静态资源。
package panel

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html app.js theme.js
var files embed.FS

// Handler 提供面板页面与静态资源（/ 返回 index.html）。
func Handler() http.Handler {
	sub, err := fs.Sub(files, ".")
	if err != nil {
		panic(err) // embed 目录固定，出错说明构建产物坏了
	}
	return http.FileServerFS(sub)
}
