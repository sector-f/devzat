package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
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
	port        = 22
	scrollback  = 16
	profilePort = 5555

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

type ban struct {
	Addr string
	ID   string
}

type room struct {
	name       string
	users      []*user
	usersMutex sync.Mutex
}

type user struct {
	name     string
	pronouns []string
	session  ssh.Session
	term     *terminal.Terminal

	room      *room
	messaging *user // currently messaging this user in a DM

	bell          bool
	pingEverytime bool
	formatTime24  bool

	color   string
	colorBG string
	id      string
	addr    string

	win           ssh.Window
	closeOnce     sync.Once
	lastTimestamp time.Time
	joinTime      time.Time
	timezone      *time.Location
}

type backlogMessage struct {
	timestamp  time.Time
	senderName string
	text       string
}

// TODO: have a web dashboard that shows logs
func main() {
	go func() {
		err := http.ListenAndServe(fmt.Sprintf(":%d", profilePort), nil)
		if err != nil {
			l.Println(err)
		}
	}()
	devbot = green.Paint("devbot")
	rand.Seed(time.Now().Unix())
	readBans()
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-c
		fmt.Println("Shutting down...")
		saveBans()
		logfile.Close()
		time.AfterFunc(time.Second, func() {
			l.Println("Broadcast taking too long, exiting server early.")
			os.Exit(4)
		})
		universeBroadcast(devbot, "Server going down! This is probably because it is being updated. Try joining back immediately.  \n"+
			"If you still can't join, try joining back in 2 minutes. If you _still_ can't join, make an issue at github.com/quackduck/devzat/issues")
		os.Exit(0)
	}()
	ssh.Handle(func(s ssh.Session) {
		u := newUser(s)
		if u == nil {
			s.Close()
			return
		}
		defer func() { // crash protection
			if i := recover(); i != nil {
				mainRoom.broadcast(devbot, "Slap the developers in the face for me, the server almost crashed, also tell them this: "+fmt.Sprint(i)+", stack: "+string(debug.Stack()))
			}
		}()
		u.repl()
	})
	var err error
	if os.Getenv("PORT") != "" {
		port, err = strconv.Atoi(os.Getenv("PORT"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	fmt.Printf("Starting chat server on port %d and profiling on port %d\n", port, profilePort)
	go func() {
		if port == 22 {
			fmt.Println("Also starting chat server on port 443")
			err = ssh.ListenAndServe(":443", nil, ssh.HostKeyFile(os.Getenv("HOME")+"/.ssh/id_rsa"))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}()
	err = ssh.ListenAndServe(
		fmt.Sprintf(":%d", port),
		nil,
		ssh.HostKeyFile(os.Getenv("HOME")+"/.ssh/id_rsa"),
		ssh.PublicKeyAuth(
			func(ctx ssh.Context, key ssh.PublicKey) bool {
				return true // allow all keys, this lets us hash pubkeys later
			},
		),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func universeBroadcast(senderName, msg string) {
	for _, r := range rooms {
		r.broadcast(senderName, msg)
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
	// trying to refer to a room?
	if len(words) > 0 && words[len(words)-1][0] == '#' {
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
	valentines(u)

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

func valentines(u *user) {
	if time.Now().Month() == time.February && (time.Now().Day() == 14 || time.Now().Day() == 15 || time.Now().Day() == 13) {
		// TODO: add a few more random images
		u.writeln("", "![❤️](https://emojipedia-us.s3.dualstack.us-west-1.amazonaws.com/thumbs/160/apple/81/heavy-black-heart_2764.png)")
		//u.term.Write([]byte("\u001B[A\u001B[2K\u001B[A\u001B[2K")) // delete last line of rendered markdown
		time.Sleep(time.Second)
		// clear screen
		clearCMD("", u)
	}
}

// cleanupRoom deletes a room if it's empty and isn't the main room
func cleanupRoom(r *room) {
	if r != mainRoom && len(r.users) == 0 {
		delete(rooms, r.name)
	}
}

// Removes a user and chat message
func (u *user) close(msg string) {
	u.closeOnce.Do(func() {
		u.closeQuietly()
		if time.Since(u.joinTime) > time.Minute/2 {
			msg += ". They were online for " + printPrettyDuration(time.Since(u.joinTime))
		}
		u.room.broadcast(devbot, msg)
		u.room.users = remove(u.room.users, u)
		cleanupRoom(u.room)
	})
}

// Removes a user silently, used to close banned users
func (u *user) closeQuietly() {
	u.room.usersMutex.Lock()
	u.room.users = remove(u.room.users, u)
	u.room.usersMutex.Unlock()
	u.session.Close()
}

func (u *user) writeln(senderName string, msg string) {
	if strings.Contains(msg, u.name) { // is a ping
		msg += "\a"
	}
	msg = strings.ReplaceAll(msg, `\n`, "\n")
	msg = strings.ReplaceAll(msg, `\`+"\n", `\n`) // let people escape newlines
	if senderName != "" {
		if strings.HasSuffix(senderName, " <- ") || strings.HasSuffix(senderName, " -> ") { // TODO: kinda hacky DM detection
			msg = strings.TrimSpace(mdRender(msg, lenString(senderName), u.win.Width))
			msg = senderName + msg + "\a"
		} else {
			msg = strings.TrimSpace(mdRender(msg, lenString(senderName)+2, u.win.Width))
			msg = senderName + ": " + msg
		}
	} else {
		msg = strings.TrimSpace(mdRender(msg, 0, u.win.Width)) // No sender
	}
	if time.Since(u.lastTimestamp) > time.Minute {
		if u.timezone == nil {
			u.rWriteln(printPrettyDuration(time.Since(u.joinTime)) + " in")
		} else {
			if u.formatTime24 {
				u.rWriteln(time.Now().In(u.timezone).Format("15:04"))
			} else {
				u.rWriteln(time.Now().In(u.timezone).Format("3:04 pm"))
			}
		}
		u.lastTimestamp = time.Now()
	}
	if u.pingEverytime && senderName != u.name {
		msg += "\a"
	}
	if !u.bell {
		msg = strings.ReplaceAll(msg, "\a", "")
	}
	_, err := u.term.Write([]byte(msg + "\n"))
	if err != nil {
		u.close(u.name + "has left the chat because of an error writing to their terminal: " + err.Error())
	}
}

// Write to the right of the user's window
func (u *user) rWriteln(msg string) {
	if u.win.Width-lenString(msg) > 0 {
		u.term.Write([]byte(strings.Repeat(" ", u.win.Width-lenString(msg)) + msg + "\n"))
	} else {
		u.term.Write([]byte(msg + "\n"))
	}
}

// pickUsernameQuietly changes the user's username, broadcasting a name change notification if needed.
// An error is returned if the username entered had a bad word or reading input failed.
func (u *user) pickUsername(possibleName string) error {
	oldName := u.name
	err := u.pickUsernameQuietly(possibleName)
	if err != nil {
		return err
	}
	if stripansi.Strip(u.name) != stripansi.Strip(oldName) && stripansi.Strip(u.name) != possibleName { // did the name change, and is it not what the user entered?
		u.room.broadcast(devbot, oldName+" is now called "+u.name)
	}
	return nil
}

// pickUsernameQuietly is like pickUsername but does not
func (u *user) pickUsernameQuietly(possibleName string) error {
	possibleName = cleanName(possibleName)
	var err error
	for {
		if possibleName == "" {
		} else if strings.HasPrefix(possibleName, "#") || possibleName == "devbot" {
			u.writeln("", "Your username is invalid. Pick a different one:")
		} else if otherUser, dup := userDuplicate(u.room, possibleName); dup {
			if otherUser == u {
				break // allow selecting the same name as before
			}
			u.writeln("", "Your username is already in use. Pick a different one:")
		} else {
			possibleName = cleanName(possibleName)
			break
		}

		u.term.SetPrompt("> ")
		possibleName, err = u.term.ReadLine()
		if err != nil {
			return err
		}
		possibleName = cleanName(possibleName)
	}

	if detectBadWords(possibleName) { // sadly this is necessary
		banUser("devbot [grow up]", u)
		return errors.New(u.name + "'s username contained a bad word")
	}

	u.name = possibleName

	if rand.Float64() <= 0.1 { // 10% chance of a random bg color
		// changeColor also sets prompt
		defer u.changeColor("bg-random") //nolint:errcheck // we know "bg-random" is a valid color
	}
	if rand.Float64() <= 0.4 { // 40% chance of being a random color
		u.changeColor("random") //nolint:errcheck // we know "random" is a valid color
		return nil
	}
	u.changeColor(styles[rand.Intn(len(styles))].name) //nolint:errcheck // we know this is a valid color
	return nil
}

func (u *user) displayPronouns() string {
	result := ""
	for i := 0; i < len(u.pronouns); i++ {
		str, _ := applyColorToData(u.pronouns[i], u.color, u.colorBG)
		result += "/" + str
	}
	if result == "" {
		return result
	}
	return result[1:]
}

func (u *user) changeRoom(r *room) {
	if u.room == r {
		return
	}
	u.room.users = remove(u.room.users, u)
	u.room.broadcast("", u.name+" is joining "+blue.Paint(r.name)) // tell the old room
	cleanupRoom(u.room)
	u.room = r
	if _, dup := userDuplicate(u.room, u.name); dup {
		u.pickUsername("") //nolint:errcheck // if reading input failed the next repl will err out
	}
	u.room.users = append(u.room.users, u)
	u.room.broadcast(devbot, u.name+" has joined "+blue.Paint(u.room.name))
}

func (u *user) repl() {
	for {
		line, err := u.term.ReadLine()
		if err == io.EOF {
			u.close(u.name + " has left the chat")
			return
		}
		line += "\n"
		hasNewlines := false
		//oldPrompt := u.name + ": "
		for err == terminal.ErrPasteIndicator {
			hasNewlines = true
			//u.term.SetPrompt(strings.Repeat(" ", lenString(u.name)+2))
			u.term.SetPrompt("")
			additionalLine := ""
			additionalLine, err = u.term.ReadLine()
			additionalLine = strings.ReplaceAll(additionalLine, `\n`, `\\n`)
			//additionalLine = strings.ReplaceAll(additionalLine, "\t", strings.Repeat(" ", 8))
			line += additionalLine + "\n"
		}
		if err != nil {
			l.Println(u.name, err)
			u.close(u.name + " has left the chat due to an error: " + err.Error())
			return
		}
		if len(line) > maxMsgLen { // limit msg len as early as possible.
			line = line[0:maxMsgLen]
		}
		line = strings.TrimSpace(line)

		u.term.SetPrompt(u.name + ": ")

		//fmt.Println("window", u.win)
		if hasNewlines {
			calculateLinesTaken(u, u.name+": "+line, u.win.Width)
		} else {
			u.term.Write([]byte(strings.Repeat("\033[A\033[2K", int(math.Ceil(float64(lenString(u.name+line)+2)/(float64(u.win.Width))))))) // basically, ceil(length of line divided by term width)
		}
		//u.term.Write([]byte(strings.Repeat("\033[A\033[2K", calculateLinesTaken(u.name+": "+line, u.win.Width))))

		if line == "" {
			continue
		}

		antispamMessages[u.id]++
		time.AfterFunc(5*time.Second, func() {
			antispamMessages[u.id]--
		})
		if antispamMessages[u.id] >= 30 {
			u.room.broadcast(devbot, u.name+", stop spamming or you could get banned.")
		}
		if antispamMessages[u.id] >= 50 {
			if !bansContains(bans, u.addr, u.id) {
				bans = append(bans, ban{u.addr, u.id})
				saveBans()
			}
			u.writeln(devbot, "anti-spam triggered")
			u.close(red.Paint(u.name + " has been banned for spamming"))
			return
		}

		runCommands(line, u)
	}
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
