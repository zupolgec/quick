// healthcheck è un mini client HTTP statico per gli HEALTHCHECK di immagini
// distroless (che non hanno shell/wget/curl). Uso: `healthcheck <url>`.
// Exit 0 se la risposta è 2xx, altrimenti 1. Solo stdlib → binario statico.
package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(os.Args[1])
	if err != nil {
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		os.Exit(1)
	}
}
