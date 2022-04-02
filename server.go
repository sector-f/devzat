package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"runtime/debug"
	"time"

	"github.com/gliderlabs/ssh"
)

type server struct {
	port        int
	scrollback  int
	profilePort int

	mainRoom         *room
	rooms            map[string]*room
	backlog          []backlogMessage
	bans             *banlist
	idsInMinToTimes  map[string]int
	antispamMessages map[string]int

	logfile     *os.File
	l           *log.Logger
	devbot      string // TODO: can we get rid of this entirely?
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

	bans, err := banlistFromFile(banFilename)
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
		// We don't need to return early just because the ban file doesn't exist
	default:
		return nil, err
	}

	// Not being able to _save_ the bans file is worth returning early for, though
	err = bans.save()
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

func (s *server) run() error {
	// TODO: see if we can create a concrete instance here rather than relying on
	// package-scoped vars, like ssh.DefaultHandler here
	ssh.Handle(func(sess ssh.Session) {
		u := newUser(sess)
		if u == nil {
			sess.Close()
			return
		}
		defer func() { // crash protection
			if i := recover(); i != nil {
				mainRoom.broadcast(s.devbot, "Slap the developers in the face for me, the server almost crashed, also tell them this: "+fmt.Sprint(i)+", stack: "+string(debug.Stack()))
			}
		}()
		u.repl()
	})

	// TODO: decide if this functionality is actually necessary/desired
	if s.port == 22 {
		fmt.Println("Also starting chat server on port 443")
		go func() {
			err := ssh.ListenAndServe(":443", nil, ssh.HostKeyFile(os.Getenv("HOME")+"/.ssh/id_rsa"))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
	}

	return ssh.ListenAndServe(
		fmt.Sprintf(":%d", s.port),
		nil,
		ssh.HostKeyFile(os.Getenv("HOME")+"/.ssh/id_rsa"),
		ssh.PublicKeyAuth(
			func(ctx ssh.Context, key ssh.PublicKey) bool {
				return true // allow all keys, this lets us hash pubkeys later
			},
		),
	)
}

func (s *server) shutdown() {
	s.logfile.Close()
}

func (s *server) universeBroadcast(msg string) {
	for _, r := range s.rooms {
		r.broadcast(s.devbot, msg) // Hardcoding devbot as the sender is probably fine since this is for system messages
	}
}
