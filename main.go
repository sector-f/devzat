package main

import (
	_ "embed"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/acarl005/stripansi"
	"github.com/gliderlabs/ssh"
	terminal "github.com/quackduck/term"
)

var (
	scrollback = 16

	mainRoom         = &room{"#main", make([]*user, 0, 10), sync.Mutex{}}
	rooms            = map[string]*room{mainRoom.name: mainRoom}
	backlog          = make([]backlogMessage, 0, scrollback)
	bans             = make([]ban, 0, 10)
	idsInMinToTimes  = make(map[string]int, 10) // TODO: maybe add some IP-based factor to disallow rapid key-gen attempts
	antispamMessages = make(map[string]int)

	logfile, _  = os.OpenFile("log.txt", os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0666)
	l           = log.New(io.MultiWriter(logfile, os.Stdout), "", log.Ldate|log.Ltime|log.Lshortfile)
	devbot      = "" // initialized in main
	startupTime = time.Now()
)

const (
	maxMsgLen = 5120
)

type room struct {
	name       string
	users      []*user
	usersMutex sync.Mutex
}

type backlogMessage struct {
	timestamp  time.Time
	senderName string
	text       string
}

func main() {
	// TODO: have a web dashboard that shows logs
	/*
		go func() {
			err := http.ListenAndServe(fmt.Sprintf(":%d", profilePort), nil)
			if err != nil {
				l.Println(err)
			}
		}()
	*/

	server, err := newServer()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	rand.Seed(time.Now().Unix())

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-c
		fmt.Println("Shutting down...")
		server.shutdown()
		logfile.Close()
		time.AfterFunc(time.Second, func() {
			l.Println("Broadcast taking too long, exiting server early.")
			os.Exit(4)
		})
		server.universeBroadcast("Server going down! This is probably because it is being updated. Try joining back immediately.  \n" +
			"If you still can't join, try joining back in 2 minutes. If you _still_ can't join, make an issue at github.com/quackduck/devzat/issues")
		os.Exit(0)
	}()

	err = server.run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (r *room) broadcast(senderName, msg string) {
	if msg == "" {
		return
	}
	msg = strings.ReplaceAll(msg, "@everyone", green.Paint("everyone\a"))
	r.usersMutex.Lock()

	/*
		for i := range r.users {
			msg = strings.ReplaceAll(msg, "@"+stripansi.Strip(r.users[i].name), r.users[i].name)
			msg = strings.ReplaceAll(msg, `\`+r.users[i].name, "@"+stripansi.Strip(r.users[i].name)) // allow escaping
		}

		for i := range r.users {
			r.users[i].writeln(senderName, msg)
		}
	*/

	for _, user := range r.users {
		msg = strings.ReplaceAll(msg, "@"+stripansi.Strip(user.name), user.name)
		msg = strings.ReplaceAll(msg, `\`+user.name, "@"+stripansi.Strip(user.name)) // allow escaping
	}

	for _, user := range r.users {
		user.writeln(senderName, msg)
	}

	r.usersMutex.Unlock()
	if r == mainRoom {
		backlog = append(backlog, backlogMessage{time.Now(), senderName, msg + "\n"})
		if len(backlog) > scrollback {
			backlog = backlog[len(backlog)-scrollback:]
		}
	}
}

func autocompleteCallback(u *user, line string, pos int, key rune) (string, int, bool) {
	if key == '\t' {
		// Autocomplete a username

		// Split the input string to look for @<name>
		words := strings.Fields(line)

		toAdd := userMentionAutocomplete(u, words)
		if toAdd != "" {
			return line + toAdd, pos + len(toAdd), true
		}
		toAdd = roomAutocomplete(u, words)
		if toAdd != "" {
			return line + toAdd, pos + len(toAdd), true
		}
		//return line + toAdd + " ", pos + len(toAdd) + 1, true

	}
	return "", pos, false
}

func userMentionAutocomplete(u *user, words []string) string {
	if len(words) < 1 {
		return ""
	}
	// Check the last word and see if it's trying to refer to a user
	if words[len(words)-1][0] == '@' || (len(words)-1 == 0 && words[0][0] == '=') { // mentioning someone or dm-ing someone
		inputWord := words[len(words)-1][1:] // slice the @ or = off
		for i := range u.room.users {
			strippedName := stripansi.Strip(u.room.users[i].name)
			toAdd := strings.TrimPrefix(strippedName, inputWord)
			if toAdd != strippedName { // there was a match, and some text got trimmed!
				return toAdd + " "
			}
		}
	}
	return ""
}

func roomAutocomplete(u *user, words []string) string {
	if len(words) < 1 {
		return ""
	}

	// trying to refer to a room?
	if words[len(words)-1][0] == '#' {
		// don't slice the # off, since the room name includes it
		for name := range rooms {
			toAdd := strings.TrimPrefix(name, words[len(words)-1])
			if toAdd != name { // there was a match, and some text got trimmed!
				return toAdd + " "
			}
		}
	}

	return ""
}

func newUser(s ssh.Session) *user {
	term := terminal.NewTerminal(s, "> ")
	_ = term.SetSize(10000, 10000) // disable any formatting done by term
	pty, winChan, _ := s.Pty()
	w := pty.Window
	host, _, _ := net.SplitHostPort(s.RemoteAddr().String()) // definitely should not give an err

	toHash := ""

	pubkey := s.PublicKey()
	if pubkey != nil {
		toHash = string(pubkey.Marshal())
	} else { // If we can't get the public key fall back to the IP.
		toHash = host
	}

	u := &user{
		name:          "",
		pronouns:      []string{"unset"},
		session:       s,
		term:          term,
		bell:          true,
		colorBG:       "bg-off", // the FG will be set randomly
		id:            shasum(toHash),
		addr:          host,
		win:           w,
		lastTimestamp: time.Now(),
		joinTime:      time.Now(),
		room:          mainRoom}

	go func() {
		for u.win = range winChan {
		}
	}()

	l.Println("Connected " + u.name + " [" + u.id + "]")

	if bansContains(bans, u.addr, u.id) {
		l.Println("Rejected " + u.name + " [" + host + "]")
		u.writeln(devbot, "**You are banned**. If you feel this was a mistake, please reach out at github.com/quackduck/devzat/issues or email igoel.mail@gmail.com. Please include the following information: [ID "+u.id+"]")
		u.closeQuietly()
		return nil
	}
	idsInMinToTimes[u.id]++
	time.AfterFunc(60*time.Second, func() {
		idsInMinToTimes[u.id]--
	})
	if idsInMinToTimes[u.id] > 6 {
		bans = append(bans, ban{u.addr, u.id})
		mainRoom.broadcast(devbot, "`"+s.User()+"` has been banned automatically. ID: "+u.id)
		return nil
	}

	clearCMD("", u) // always clear the screen on connect

	if len(backlog) > 0 {
		lastStamp := backlog[0].timestamp
		u.rWriteln(printPrettyDuration(u.joinTime.Sub(lastStamp)) + " earlier")
		for i := range backlog {
			if backlog[i].timestamp.Sub(lastStamp) > time.Minute {
				lastStamp = backlog[i].timestamp
				u.rWriteln(printPrettyDuration(u.joinTime.Sub(lastStamp)) + " earlier")
			}
			u.writeln(backlog[i].senderName, backlog[i].text)
		}
	}

	if err := u.pickUsernameQuietly(s.User()); err != nil { // user exited or had some error
		l.Println(err)
		s.Close()
		return nil
	}

	mainRoom.usersMutex.Lock()
	mainRoom.users = append(mainRoom.users, u)
	mainRoom.usersMutex.Unlock()

	u.term.SetBracketedPasteMode(true) // experimental paste bracketing support
	term.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		return autocompleteCallback(u, line, pos, key)
	}

	switch len(mainRoom.users) - 1 {
	case 0:
		u.writeln("", blue.Paint("Welcome to the chat. There are no more users"))
	case 1:
		u.writeln("", yellow.Paint("Welcome to the chat. There is one more user"))
	default:
		u.writeln("", green.Paint("Welcome to the chat. There are", strconv.Itoa(len(mainRoom.users)-1), "more users"))
	}
	mainRoom.broadcast(devbot, u.name+" has joined the chat")
	return u
}

// accepts a ':' separated list of emoji
func fetchEmoji(names []string) string {
	result := ""
	for _, name := range names {
		result += fetchEmojiSingle(name)
	}
	return result
}

func fetchEmojiSingle(name string) string {
	r, err := http.Get("https://e.benjaminsmith.dev/" + name)
	if err != nil {
		return ""
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return ""
	}
	return "![" + name + "](https://e.benjaminsmith.dev/" + name + ")"
}

// may contain a bug ("may" because it could be the terminal's fault)
func calculateLinesTaken(u *user, s string, width int) {
	s = stripansi.Strip(s)
	//fmt.Println("`"+s+"`", "width", width)
	pos := 0
	//lines := 1
	u.term.Write([]byte("\033[A\033[2K"))
	currLine := ""
	for _, c := range s {
		pos++
		currLine += string(c)
		if c == '\t' {
			pos += 8
		}
		if c == '\n' || pos > width {
			pos = 1
			//lines++
			u.term.Write([]byte("\033[A\033[2K"))
		}
		//fmt.Println(string(c), "`"+currLine+"`", "pos", pos, "lines", lines)
	}
	//return lines
}

// bansContains reports if the addr or id is found in the bans list
func bansContains(b []ban, addr string, id string) bool {
	for i := 0; i < len(b); i++ {
		if b[i].Addr == addr || b[i].ID == id {
			return true
		}
	}
	return false
}
