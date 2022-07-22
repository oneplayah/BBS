package main

import (
	"context"
	"flag"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh/terminal"
)

var addr = flag.String("addr", "bbs-server.mattiselin.repl.co", "bbs service address")

func main() {
	flag.Parse()

	log.SetLevel(log.TraceLevel)
	log.SetFormatter(&log.TextFormatter{
		DisableColors: false,
		FullTimestamp: true,
	})

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	url := url.URL{Scheme: "wss", Host: *addr, Path: "/ws"}
	log.Infof("connecting to %s", url.String())

	// REPL_OWNER is the user clicking "run" in spotlight
	// It's verified by the existence of a pre-shared key (to make it harder to spoof)
	headers := make(http.Header)
	headers.Add("X-BBS-PSK", os.Getenv("PSK"))
	if os.Getenv("PSK") == "" {
		// No PSK!!
		// Secrets aren't in "Guest Forks" (i.e. spotlight / cover apge runs).
		// So we use a hacky backup strategy: the Repl DB URL is signed and verifiable (http 200 = OK)
		// The JWT in it includes the user/slug in the claims, so the server can use the username without a PSK.
		// A better solution is on the way.....
		headers.Add("X-BBS-DBAuth", os.Getenv("REPLIT_DB_URL"))
	}
	headers.Add("X-BBS-Username", os.Getenv("REPL_OWNER"))

	c, _, err := websocket.DefaultDialer.Dial(url.String(), headers)
	if err != nil {
		log.WithError(err).Fatal("dial")
	}
	defer c.Close()

	done := make(chan struct{})

	stdin := make(chan string)
	stdinContext, cancelStdin := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	wg.Add(2)

	// main connection loop, reads websocket payloads and writes to the terminal
	go func() {
		defer cancelStdin()
		defer close(done)
		defer wg.Done()

		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				return
			}

			os.Stdout.Write(message)
		}
	}()

	// main stdin loop, reads anything and everything from stdin and sends to remote
	go func() {
		defer wg.Done()

		termState, err := terminal.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			log.WithError(err).Fatal("failed to set raw mode for stdin")
			return
		}

		defer terminal.Restore(int(os.Stdin.Fd()), termState)

		buffer := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buffer)
			if err != nil {
				log.WithError(err).Error("failed to read stdin")
				continue
			}

			select {
			case <-stdinContext.Done():
				close(stdin)
				return
			default:
				stdin <- string(buffer[:n])
			}
		}
	}()

	log.Info("starting main loop...")

L:
	for {
		select {
		case <-done:
			return
		case buf := <-stdin:
			err := c.WriteMessage(websocket.TextMessage, []byte(buf))
			if err != nil {
				log.WithError(err).Error("failed to write to websocket")
			}
		case <-interrupt:
			log.Info("received interrupt, terminating cleanly...")

			err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.WithError(err).Error("failed to close websocket")
				break L
			}
			return
		}
	}

	wg.Wait()
}
