package main

import (
	"errors"
	"io"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/acarl005/stripansi"
	"github.com/gliderlabs/ssh"
	terminal "github.com/quackduck/term"
)

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

// TODO: give more accurate name
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

// TODO: use mutex?
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
