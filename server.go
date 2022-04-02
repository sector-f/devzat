package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"time"
)

type server struct {
	port        int
	scrollback  int
	profilePort int

	mainRoom         *room
	rooms            map[string]*room
	backlog          []backlogMessage
	bans             []ban
	idsInMinToTimes  map[string]int
	antispamMessages map[string]int

	logfile     *os.File
	l           *log.Logger
	devbot      string
	startupTime time.Time
}

func newServer() (*server, error) {
	// TODO: replace with config type
	var (
		port        = 22
		scrollback  = 16
		profilePort = 5555

		logFilename = "log.txt"
		banFilename = "bans.json"
	)

	// Do the thing(s) that can fail as early as possible
	logfile, err := os.OpenFile(logFilename, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return nil, err
	}

	// TODO: move this into separate method to allow hot reloading?
	bans := []ban{}
	banfile, err := os.Open(banFilename)
	if err != nil {
		return nil, err
	}
	defer banfile.Close()

	err = json.NewDecoder(banfile).Decode(&bans)
	if err != nil {
		return nil, err
	}

	s := server{
		port:        port,
		scrollback:  scrollback,
		profilePort: profilePort,

		mainRoom:         &room{name: "#main"},
		rooms:            make(map[string]*room),
		backlog:          make([]backlogMessage, 0, scrollback),
		bans:             bans,
		idsInMinToTimes:  make(map[string]int, 10),
		antispamMessages: make(map[string]int),

		logfile:     logfile,
		l:           log.New(io.MultiWriter(logfile, os.Stdout), "", log.Ldate|log.Ltime|log.Lshortfile),
		devbot:      green.Paint("devbot"),
		startupTime: time.Now(),
	}

	return &s, nil
}

func (s *server) shutdown() {
	s.logfile.Close()
}
