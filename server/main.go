package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	config, err := LoadConfig("config.toml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf(`Loaded Configuration
	Port: %d
	AllowedOrigins: %v
	MaxViewersPerNode: %d
`, config.Port, config.AllowedOrigins, config.ViewerPolicy.MaxViewersPerNode)

	hub := NewHub(config)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.Handle("/ws", webSocketHandler(hub, config))

	address := fmt.Sprintf(":%d", config.Port)
	log.Printf("Signaling service listening on %s", address)
	log.Fatal(http.ListenAndServe(address, mux))
}
