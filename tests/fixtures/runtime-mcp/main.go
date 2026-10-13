// The native harness probes use the real MCP handler with synthetic credentials.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/helmrdotdev/helmr/internal/runtimemcp"
)

func main() {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	token := "Bearer synthetic-" + rand.Text()
	handler := runtimemcp.NewHandler(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("probe cannot invoke Session operations")
	})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/mcp" || request.Header.Get("Authorization") != token {
			http.Error(w, "invalid probe connection", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, request)
	})}
	go func() { _ = server.Serve(listener) }()
	connection, err := json.Marshal(map[string]any{"url": "http://" + listener.Addr().String() + "/mcp", "headers": map[string]string{"Authorization": token}})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(connection))
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = server.Close()
}
