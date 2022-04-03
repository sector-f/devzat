package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/gliderlabs/ssh"
	terminal "github.com/quackduck/term"
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
	devbot      string // TODO: can we get rid of this entirely? Or maybe keep it as a package-scoped var?
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
	s.bans.save()
	s.logfile.Close()
}

func (s *server) universeBroadcast(msg string) {
	for _, r := range s.rooms {
		r.broadcast(s.devbot, msg) // Hardcoding devbot as the sender is probably fine since this is for system messages
	}
}

func (s *server) newUser(sess ssh.Session) *user {
	term := terminal.NewTerminal(sess, "> ")
	_ = term.SetSize(10000, 10000) // disable any formatting done by term
	pty, winChan, _ := sess.Pty()
	w := pty.Window
	host, _, _ := net.SplitHostPort(sess.RemoteAddr().String()) // definitely should not give an err

	toHash := ""

	pubkey := sess.PublicKey()
	if pubkey != nil {
		toHash = string(pubkey.Marshal())
	} else { // If we can't get the public key fall back to the IP.
		toHash = host
	}

	now := time.Now()
	u := &user{
		name:          "",
		pronouns:      []string{"unset"},
		session:       sess,
		term:          term,
		bell:          true,
		colorBG:       "bg-off", // the FG will be set randomly
		id:            shasum(toHash),
		addr:          host,
		win:           w,
		lastTimestamp: now,
		joinTime:      now,
		room:          s.mainRoom}

	go func() {
		for u.win = range winChan {
		}
	}()

	s.l.Println("Connected " + u.name + " [" + u.id + "]")

	if s.bans.contains(u.addr, u.id) {
		s.l.Println("Rejected " + u.name + " [" + host + "]")
		u.writeln(s.devbot, "**You are banned**. If you feel this was a mistake, please reach out at github.com/quackduck/devzat/issues or email igoel.mail@gmail.com. Please include the following information: [ID "+u.id+"]")
		u.closeQuietly()
		return nil
	}

	idsInMinToTimes[u.id]++
	time.AfterFunc(60*time.Second, func() {
		idsInMinToTimes[u.id]--
	})
	if idsInMinToTimes[u.id] > 6 {
		bans = append(bans, ban{u.addr, u.id})
		mainRoom.broadcast(devbot, "`"+sess.User()+"` has been banned automatically. ID: "+u.id)
		return nil
	}

	clearCMD("", u) // always clear the screen on connect

	if len(backlog) > 0 {
		lastStamp := s.backlog[0].timestamp
		u.rWriteln(printPrettyDuration(u.joinTime.Sub(lastStamp)) + " earlier")
		for _, msg := range s.backlog {
			if msg.timestamp.Sub(lastStamp) > time.Minute {
				lastStamp = msg.timestamp
				u.rWriteln(printPrettyDuration(u.joinTime.Sub(lastStamp)) + " earlier")
			}
			u.writeln(msg.senderName, msg.text)
		}
	}

	if err := u.pickUsernameQuietly(sess.User()); err != nil { // user exited or had some error
		l.Println(err)
		sess.Close()
		return nil
	}

	// TODO: this should probably be a method of room
	s.mainRoom.usersMutex.Lock()
	s.mainRoom.users = append(mainRoom.users, u)
	s.mainRoom.usersMutex.Unlock()

	u.term.SetBracketedPasteMode(true) // experimental paste bracketing support
	term.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		return autocompleteCallback(u, line, pos, key)
	}

	switch len(s.mainRoom.users) - 1 {
	case 0:
		u.writeln("", blue.Paint("Welcome to the chat. There are no more users"))
	case 1:
		u.writeln("", yellow.Paint("Welcome to the chat. There is one more user"))
	default:
		u.writeln("", green.Paint("Welcome to the chat. There are", strconv.Itoa(len(mainRoom.users)-1), "more users"))
	}
	s.mainRoom.broadcast(s.devbot, u.name+" has joined the chat")
	return u
}

func (s *server) closeUser(user *user, msg string) {
	user.closeOnce.Do(func() {
		user.closeQuietly()
		if time.Since(user.joinTime) > time.Minute/2 {
			msg += ". They were online for " + printPrettyDuration(time.Since(user.joinTime))
		}
		user.room.broadcast(devbot, msg)
		user.room.users = remove(user.room.users, user)
		cleanupRoom(user.room)
	})
}
