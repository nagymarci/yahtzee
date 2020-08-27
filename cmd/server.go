package main

import (
	"math/rand"
	"net/http"
	"os"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/akarasz/yahtzee/pkg/game"
	"github.com/akarasz/yahtzee/pkg/handler"
	"github.com/akarasz/yahtzee/pkg/store"
)

const listenAddress = ":8000"

func main() {
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)

	rand.Seed(time.Now().UnixNano())

	h := handler.New(
		store.NewInMemory(),
		&handler.GameHandler{
			Controller: game.New(),
		})

	log.Infoln("starting server on", listenAddress)
	err := http.ListenAndServe(listenAddress, h)
	if err != nil {
		log.Errorln("listen and serve:", err)
		panic(err)
	}
}
