// Command uidserver serves HTTP for Stage 0 / campaign disruption measurement.
// Responds 200 with X-Pod-Uid from Downward API env POD_UID (metadata.uid).
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	uid := os.Getenv("POD_UID")
	if uid == "" {
		log.Fatal("POD_UID env required (Downward API fieldRef metadata.uid)")
	}
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Pod-Uid", uid)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "ok uid=%s\n", uid)
	})
	log.Printf("uidserver listening on %s uid=%s", addr, uid)
	log.Fatal(http.ListenAndServe(addr, mux))
}
