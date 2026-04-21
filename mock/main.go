package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	http.HandleFunc("/testing", handleTesting)

	fmt.Println("Mock server starting on port 5523...")
	log.Fatal(http.ListenAndServe(":5523", nil))
}

func handleTesting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get current time
	currentTime := time.Now().Format("2006-01-02 15:04:05.000000")

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Print request information
	fmt.Printf("\n=== POST /testing received at %s ===\n", currentTime)
	fmt.Printf("Headers:\n")
	for key, values := range r.Header {
		for _, value := range values {
			fmt.Printf("  %s: %s\n", key, value)
		}
	}
	fmt.Printf("Body: %s\n", string(body))
	fmt.Printf("=====================================\n\n")

	// Send response
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Request received at %s", currentTime)))
}
