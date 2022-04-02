package main

import (
	"encoding/json"
	"os"
	"sync"
)

type ban struct {
	Addr string
	ID   string
}

type banlist struct {
	filename string
	bans     []ban
	mu       sync.Mutex
}

func banlistFromFile(filename string) (*banlist, error) {
	b := banlist{filename: filename}
	err := b.reload()
	return &b, err
}

func (b *banlist) reload() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	banfile, err := os.Open(b.filename)
	if err != nil {
		return err
	}
	defer banfile.Close()

	newBans := []ban{}

	err = json.NewDecoder(banfile).Decode(&newBans)
	if err != nil {
		return err
	}

	b.bans = newBans
	return nil
}

func (b *banlist) save() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	banfile, err := os.Create(b.filename)
	if err != nil {
		return err
	}
	defer banfile.Close()

	j := json.NewEncoder(banfile)
	j.SetIndent("", "   ")
	return j.Encode(bans)
}

// contains returns true if the addr or id is found in the bans list
func (b *banlist) contains(addr string, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ban := range b.bans {
		if ban.Addr == addr || ban.ID == id {
			return true
		}
	}

	return false
}
