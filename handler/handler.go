package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/akarasz/yahtzee"
	"github.com/akarasz/yahtzee/event"
	"github.com/akarasz/yahtzee/store"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

type handler struct {
	store      store.Store
	emitter    event.Emitter
	subscriber event.Subscriber
}

func New(s store.Store, e event.Emitter, sub event.Subscriber) http.Handler {
	h := &handler{s, e, sub}

	r := mux.NewRouter()
	r.Use(corsMiddleware)
	r.HandleFunc("/", h.Create).
		Methods("POST", "OPTIONS")
	r.HandleFunc("/features", h.Features).
		Methods("GET", "OPTIONS")
	r.HandleFunc("/{gameID}", h.Get).
		Methods("GET", "OPTIONS")
	r.HandleFunc("/{gameID}/hints", h.HintsForGame).
		Methods("GET", "OPTIONS")
	r.HandleFunc("/{gameID}/join", h.AddPlayer).
		Methods("POST", "OPTIONS")
	r.HandleFunc("/{gameID}/roll", h.Roll).
		Methods("POST", "OPTIONS")
	r.HandleFunc("/{gameID}/lock/{dice}", h.Lock).
		Methods("POST", "OPTIONS")
	r.HandleFunc("/{gameID}/score", h.Score).
		Methods("POST", "OPTIONS")
	r.HandleFunc("/{gameID}/ws", h.WS)
	return r
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Location")

		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func generateID() string {
	const (
		idCharset = "abcdefghijklmnopqrstvwxyz0123456789"
		length    = 4
	)

	b := make([]byte, length)
	for i := range b {
		b[i] = idCharset[rand.Intn(len(idCharset))]
	}
	return string(b)
}

func (h *handler) Create(w http.ResponseWriter, r *http.Request) {
	gameID := generateID()
	features := []yahtzee.Feature{}
	if r.Body != nil {
		err := json.NewDecoder(r.Body).Decode(&features)
		if err != nil && err != io.EOF {
			writeError(w, r, err, "create game", http.StatusBadRequest)
			return
		}
	}
	if err := h.store.Save(gameID, *yahtzee.NewGame(features...)); err != nil {
		writeError(w, r, err, "create game", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/%s", gameID))
	w.WriteHeader(http.StatusCreated)

	log.Print("game created")
}

func (h *handler) HintsForGame(w http.ResponseWriter, r *http.Request) {
	gameID, ok := readGameID(w, r)
	if !ok {
		return
	}

	unlocker, err := h.store.Lock(gameID)
	if err != nil {
		writeError(w, r, err, "locking issue", http.StatusInternalServerError)
		return
	}
	defer unlocker()

	g, err := h.store.Load(gameID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	res, err := hints(&g)
	if err != nil {
		writeError(w, r, err, "", http.StatusInternalServerError)
		return
	}

	if ok := writeJSON(w, r, res); !ok {
		return
	}

	log.Print("hints for game returned")
}

func hints(game *yahtzee.Game) (map[yahtzee.Category]int, error) {
	res := map[yahtzee.Category]int{}
	for c, scorer := range game.Scorer.ScoreActions {
		res[c] = scorer(game)
		if game.HasFeature(yahtzee.Ordered) && game.Round < len(yahtzee.Categories()) && yahtzee.Categories()[game.Round] != c {
			res[c] = 0
		}
	}

	return res, nil
}

func (h *handler) Get(w http.ResponseWriter, r *http.Request) {
	gameID, ok := readGameID(w, r)
	if !ok {
		return
	}

	unlocker, err := h.store.Lock(gameID)
	if err != nil {
		writeError(w, r, err, "locking issue", http.StatusInternalServerError)
		return
	}
	defer unlocker()

	g, err := h.store.Load(gameID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	if ok := writeJSON(w, r, g); !ok {
		return
	}

	log.Print("game returned")
}

type AddPlayerResponse struct {
	Players []*yahtzee.Player
}

func (h *handler) AddPlayer(w http.ResponseWriter, r *http.Request) {
	user, ok := readUser(w, r)
	if !ok {
		return
	}
	gameID, ok := readGameID(w, r)
	if !ok {
		return
	}

	unlocker, err := h.store.Lock(gameID)
	if err != nil {
		writeError(w, r, err, "locking issue", http.StatusInternalServerError)
		return
	}
	defer unlocker()

	g, err := h.store.Load(gameID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	if g.CurrentPlayer > 0 || g.Round > 0 {
		writeError(w, r, nil, "game already started", http.StatusBadRequest)
		return
	}
	for _, p := range g.Players {
		if p.User == user {
			writeError(w, r, nil, "already joined", http.StatusConflict)
			return
		}
	}

	g.Players = append(g.Players, yahtzee.NewPlayer(user))

	if err := h.store.Save(gameID, g); err != nil {
		writeStoreError(w, r, err)
		return
	}

	changes := &AddPlayerResponse{
		Players: g.Players,
	}

	h.emitter.Emit(gameID, &user, event.AddPlayer, changes)

	w.WriteHeader(http.StatusCreated)
	if ok := writeJSON(w, r, changes); !ok {
		return
	}

	log.Print("player added")
}

type RollResponse struct {
	Dices     []*yahtzee.Dice
	RollCount int
}

func (h *handler) Roll(w http.ResponseWriter, r *http.Request) {
	user, ok := readUser(w, r)
	if !ok {
		return
	}
	gameID, ok := readGameID(w, r)
	if !ok {
		return
	}

	unlocker, err := h.store.Lock(gameID)
	if err != nil {
		writeError(w, r, err, "locking issue", http.StatusInternalServerError)
		return
	}
	defer unlocker()

	g, err := h.store.Load(gameID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	if len(g.Players) == 0 {
		writeError(w, r, nil, "no players joined", http.StatusBadRequest)
		return
	}
	currentPlayer := g.Players[g.CurrentPlayer]
	if user != currentPlayer.User {
		writeError(w, r, nil, "another players turn", http.StatusBadRequest)
		return
	}
	if g.Round >= 13 {
		writeError(w, r, nil, "game is over", http.StatusBadRequest)
		return
	}
	if g.RollCount >= 3 {
		writeError(w, r, nil, "no more rolls", http.StatusBadRequest)
		return
	}

	for _, d := range g.Dices {
		if d.Locked {
			continue
		}

		d.Value = rand.Intn(6) + 1
	}

	g.RollCount++

	if err := h.store.Save(gameID, g); err != nil {
		writeStoreError(w, r, err)
		return
	}

	changes := &RollResponse{
		Dices:     g.Dices,
		RollCount: g.RollCount,
	}

	h.emitter.Emit(gameID, &user, event.Roll, changes)

	if ok := writeJSON(w, r, changes); !ok {
		return
	}

	log.Print("rolled dices")
}

type LockResponse struct {
	Dices []*yahtzee.Dice
}

func (h *handler) Lock(w http.ResponseWriter, r *http.Request) {
	user, ok := readUser(w, r)
	if !ok {
		return
	}
	gameID, ok := readGameID(w, r)
	if !ok {
		return
	}

	unlocker, err := h.store.Lock(gameID)
	if err != nil {
		writeError(w, r, err, "locking issue", http.StatusInternalServerError)
		return
	}
	defer unlocker()

	g, err := h.store.Load(gameID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	diceIndex, ok := readDiceIndex(w, r, len(g.Dices))
	if !ok {
		return
	}

	if len(g.Players) == 0 {
		writeError(w, r, nil, "no players joined", http.StatusBadRequest)
		return
	}
	currentPlayer := g.Players[g.CurrentPlayer]
	if user != currentPlayer.User {
		writeError(w, r, nil, "another players turn", http.StatusBadRequest)
		return
	}
	if g.Round >= 13 {
		writeError(w, r, nil, "game is over", http.StatusBadRequest)
		return
	}
	if g.RollCount == 0 {
		writeError(w, r, nil, "roll first", http.StatusBadRequest)
		return
	}
	if g.RollCount >= 3 {
		writeError(w, r, nil, "no more rolls", http.StatusBadRequest)
		return
	}

	g.Dices[diceIndex].Locked = !g.Dices[diceIndex].Locked

	if err := h.store.Save(gameID, g); err != nil {
		writeStoreError(w, r, err)
		return
	}

	changes := &LockResponse{
		Dices: g.Dices,
	}

	h.emitter.Emit(gameID, &user, event.Lock, changes)

	if ok := writeJSON(w, r, changes); !ok {
		return
	}

	log.Print("toggled dice")
}

func (h *handler) Score(w http.ResponseWriter, r *http.Request) {
	user, ok := readUser(w, r)
	if !ok {
		return
	}
	gameID, ok := readGameID(w, r)
	if !ok {
		return
	}
	category, ok := readCategory(w, r)
	if !ok {
		return
	}

	unlocker, err := h.store.Lock(gameID)
	if err != nil {
		writeError(w, r, err, "locking issue", http.StatusInternalServerError)
		return
	}
	defer unlocker()

	g, err := h.store.Load(gameID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	if len(g.Players) == 0 {
		writeError(w, r, nil, "no players joined", http.StatusBadRequest)
		return
	}
	currentPlayer := g.Players[g.CurrentPlayer]
	if user != currentPlayer.User {
		writeError(w, r, nil, "another players turn", http.StatusBadRequest)
		return
	}
	if g.Round >= 13 {
		writeError(w, r, nil, "game is over", http.StatusBadRequest)
		return
	}
	if g.RollCount == 0 {
		writeError(w, r, nil, "roll first", http.StatusBadRequest)
		return
	}
	if _, ok := currentPlayer.ScoreSheet[category]; ok {
		writeError(w, r, nil, "category is already used", http.StatusBadRequest)
		return
	}

	if g.HasFeature(yahtzee.Ordered) && yahtzee.Categories()[g.Round] != category {
		writeError(w, r, nil, "invalid category", http.StatusBadRequest)
		return
	}

	var scorer func(game *yahtzee.Game) int
	if scorer, ok = g.Scorer.ScoreActions[category]; !ok {
		writeError(w, r, nil, "invalid category", http.StatusBadRequest)
		return
	}

	dices := make([]int, len(g.Dices))
	for i, d := range g.Dices {
		dices[i] = d.Value
	}

	//prescore actions
	for _, action := range g.Scorer.PreScoreActions {
		action(&g)
	}

	currentPlayer.ScoreSheet[category] = scorer(&g)

	//postscore actions
	for _, action := range g.Scorer.PostScoreActions {
		action(&g)
	}

	for _, d := range g.Dices {
		d.Locked = false
	}

	g.RollCount = 0
	g.CurrentPlayer = (g.CurrentPlayer + 1) % len(g.Players)
	if g.CurrentPlayer == 0 {
		g.Round++
	}

	if g.Round >= 13 { //End of game, running postgame actions
		for _, action := range g.Scorer.PostGameActions {
			action(&g)
		}
	}

	if err := h.store.Save(gameID, g); err != nil {
		writeStoreError(w, r, err)
		return
	}

	h.emitter.Emit(gameID, &user, event.Score, &g)

	if ok := writeJSON(w, r, &g); !ok {
		return
	}

	log.Print("scored")
}

const (
	wsPongWait   = 30 * time.Second
	wsPingPeriod = (wsPongWait * 8) / 10
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsWriter(ws *websocket.Conn, events <-chan *event.Event, s event.Subscriber, gameID string) {
	pingTicker := time.NewTicker(wsPingPeriod)
	defer func() {
		s.Unsubscribe(gameID, ws)
		pingTicker.Stop()
		ws.Close()
	}()

	for {
		select {
		case e := <-events:
			if err := ws.WriteJSON(e); err != nil {
				return
			}
		case <-pingTicker.C:
			if err := ws.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

func wsReader(ws *websocket.Conn, s event.Subscriber, gameID string) {
	defer func() {
		s.Unsubscribe(gameID, ws)
		ws.Close()
	}()
	ws.SetReadLimit(512)
	ws.SetReadDeadline(time.Now().Add(wsPongWait))
	ws.SetPongHandler(func(string) error { ws.SetReadDeadline(time.Now().Add(wsPongWait)); return nil })
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break
		}
	}
}

func (h *handler) WS(w http.ResponseWriter, r *http.Request) {
	gameID, ok := readGameID(w, r)
	if !ok {
		return
	}

	unlock, err := h.store.Lock(gameID)
	if err != nil {
		writeError(w, r, err, "locking issue", http.StatusInternalServerError)
		return
	}
	_, err = h.store.Load(gameID)
	unlock()
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if _, ok := err.(websocket.HandshakeError); !ok {
			writeError(w, r, err, "unknown error", http.StatusInternalServerError)
		}
		return
	}

	eventChannel, err := h.subscriber.Subscribe(gameID, ws)
	if err != nil {
		writeError(w, r, err, "unable to subscribe", http.StatusInternalServerError)
		return
	}

	go wsWriter(ws, eventChannel, h.subscriber, gameID)
	wsReader(ws, h.subscriber, gameID)
}

func (h *handler) Features(w http.ResponseWriter, r *http.Request) {
	features := yahtzee.Features()
	if ok := writeJSON(w, r, &features); !ok {
		return
	}
}

func readDiceIndex(w http.ResponseWriter, r *http.Request, diceNum int) (int, bool) {
	raw, ok := mux.Vars(r)["dice"]
	if !ok {
		writeError(w, r, nil, "no dice index in request", http.StatusInternalServerError)
		return 0, false
	}
	index, err := strconv.Atoi(raw)
	if err != nil || index < 0 || index > diceNum-1 {
		writeError(w, r, err, "invalid dice index", http.StatusBadRequest)
		return index, false
	}
	return index, true
}

func readFeatures(w http.ResponseWriter, r *http.Request) ([]yahtzee.Feature, bool) {
	raw := r.URL.Query().Get("features")
	rawFeatures := strings.Split(raw, ",")

	var features []yahtzee.Feature
	for _, f := range rawFeatures {
		features = append(features, yahtzee.Feature(f))
	}
	return features, true
}

func readCategory(w http.ResponseWriter, r *http.Request) (yahtzee.Category, bool) {
	if r.Body == nil {
		writeError(w, r, nil, "no category", http.StatusBadRequest)
		return "", false
	}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		writeError(w, r, err, "extract category from body", http.StatusInternalServerError)
		return "", false
	}
	return yahtzee.Category(body), true
}

func readGameID(w http.ResponseWriter, r *http.Request) (string, bool) {
	gameID, ok := mux.Vars(r)["gameID"]
	if !ok {
		err := errors.New("no gameID")
		writeError(w, r, err, "no gameID in request", http.StatusInternalServerError)
		return "", false
	}
	return gameID, true
}

func readUser(w http.ResponseWriter, r *http.Request) (yahtzee.User, bool) {
	user, _, ok := r.BasicAuth()
	if !ok {
		err := errors.New("no user")
		writeError(w, r, err, "no user in request", http.StatusUnauthorized)
		return "", false
	}
	return yahtzee.User(user), true
}

func writeJSON(w http.ResponseWriter, r *http.Request, body interface{}) bool {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		writeError(w, r, err, "response json encode", http.StatusInternalServerError)
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, r *http.Request, err error, msg string, status int) {
	log.Printf("%s: %v", msg, err)
	http.Error(w, "", status)
}

func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.As(err, &store.ErrNotExists) {
		writeError(w, r, err, "not exists", http.StatusNotFound)
	} else {
		writeError(w, r, err, "unknown error", http.StatusInternalServerError)
	}
}
