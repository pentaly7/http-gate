package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	var targetURL, listenAddr string
	flag.StringVar(&targetURL, "target", "http://localhost:5523", "Target app base URL")
	flag.StringVar(&listenAddr, "listen", ":9090", "Address to listen on")
	flag.Parse()

	fmt.Println("┌─────────────────────────────────────┐")
	fmt.Println("│         HTTP Gate — Race Repro      │")
	fmt.Printf("│  Listen : %-26s│\n", listenAddr)
	fmt.Printf("│  Target : %-26s│\n", targetURL)
	fmt.Println("├─────────────────────────────────────┤")
	fmt.Println("│  ENTER → release all held requests  │")
	fmt.Println("│  q     → quit                       │")
	fmt.Println("└─────────────────────────────────────┘")
	fmt.Println()

	gate := NewGate(targetURL)

	go func() {
		if err := http.ListenAndServe(listenAddr, gate); err != nil {
			log.Fatalf("listen error: %v", err)
		}
	}()

	keyboardLoop(gate)
}

func keyboardLoop(gate *Gate) {
	if err := setRawMode(); err != nil {
		log.Println("[WARN] No TTY — using line-buffered input (ENTER to release, q to quit)")
		lineBufferedLoop(gate)
		return
	}
	rawLoop(gate)
}

func lineBufferedLoop(gate *Gate) {
	var line string
	for {
		fmt.Scan(&line)
		switch line {
		case "q", "Q":
			fmt.Println("Goodbye.")
			os.Exit(0)
		default:
			n := gate.ReleaseAll()
			if n == 0 {
				fmt.Println("[GATE] Nothing held — send some requests first.")
			}
		}
	}
}
