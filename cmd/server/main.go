
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/vancemichael/092002-forest-brand-attest/internal/httpapi"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" { port = "8080" }
	log.Fatal(http.ListenAndServe(":"+port, httpapi.Router()))
}
