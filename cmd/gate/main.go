// Command gate exposes a public MCP endpoint and relays calls to a local WebCodex agent.
package main

import (
	"log"
	"net/http"
)

func main() {
	srv, err := newServer()
	if err != nil {
		log.Fatal(err)
	}

	addr := env("WEBCODEX_ADDR", ":8080")
	log.Printf("gate listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
