package websocket

import (
	"path/filepath"

	"rtc-media-server/internal/log"
)

func init() {
	path := filepath.Join("testdata", "websocket.log")
	if err := log.Init(log.Options{Level: "debug", Format: "text", File: path}); err != nil {
		panic(err)
	}
}
