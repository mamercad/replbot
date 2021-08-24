package bot

import (
	"context"
	"errors"
	"heckel.io/replbot/config"
	"heckel.io/replbot/util"
	"log"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const maxMessageWaitTime = 5 * time.Second

var (
	memUserMentionRegex = regexp.MustCompile(`@(\S+)`)
)

// memConn is an implementation of conn specifically used for testing
type memConn struct {
	config    *config.Config
	eventChan chan event
	messages  map[string]*messageEvent
	currentID int
	mu        sync.RWMutex
}

func newMemConn(conf *config.Config) *memConn {
	return &memConn{
		config:    conf,
		eventChan: make(chan event),
		messages:  make(map[string]*messageEvent),
		currentID: 0,
	}
}

func (c *memConn) Connect(ctx context.Context) (<-chan event, error) {
	return c.eventChan, nil
}

func (c *memConn) Send(target *chatID, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentID++
	c.messages[strconv.Itoa(c.currentID)] = &messageEvent{
		ID:      strconv.Itoa(c.currentID),
		Channel: target.Channel,
		Thread:  target.Thread,
		Message: message,
	}
	return nil
}

func (c *memConn) SendWithID(target *chatID, message string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentID++
	c.messages[strconv.Itoa(c.currentID)] = &messageEvent{
		ID:      strconv.Itoa(c.currentID),
		Channel: target.Channel,
		Thread:  target.Thread,
		Message: message,
	}
	return strconv.Itoa(c.currentID), nil
}

func (c *memConn) Update(target *chatID, id string, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages[id] = &messageEvent{
		ID:      id,
		Channel: target.Channel,
		Thread:  target.Thread,
		Message: message,
	}
	return nil
}

func (c *memConn) Archive(target *chatID) error {
	return nil
}

func (c *memConn) Close() error {
	return nil
}

func (c *memConn) MentionBot() string {
	return "@replbot"
}

func (c *memConn) Mention(user string) string {
	return "@" + user
}

func (c *memConn) ParseMention(user string) (string, error) {
	if matches := memUserMentionRegex.FindStringSubmatch(user); len(matches) > 0 {
		return matches[1], nil
	}
	return "", errors.New("invalid user")
}

func (c *memConn) Unescape(s string) string {
	return s
}

func (c *memConn) Event(ev event) {
	c.eventChan <- ev
}

func (c *memConn) Message(id string) *messageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.messages[id]
	if !ok {
		return nil
	}
	return &messageEvent{ // copy!
		ID:          m.ID,
		Channel:     m.Channel,
		ChannelType: m.ChannelType,
		Thread:      m.Thread,
		User:        m.User,
		Message:     m.Message,
	}
}

func (c *memConn) MessageContainsWait(id string, needle string) (contains bool) {
	haystackFn := func() string {
		c.mu.Lock()
		defer c.mu.Unlock()
		m, ok := c.messages[id]
		if !ok {
			return ""
		}
		return m.Message
	}
	if !util.StringContainsWait(haystackFn, needle, maxMessageWaitTime) {
		c.LogMessages()
		return false
	}
	return true
}

func (c *memConn) LogMessages() {
	c.mu.Lock()
	defer c.mu.Unlock()
	log.Printf("Messages:")
	for id, m := range c.messages {
		log.Printf("\nBEGIN MESSAGE %s: id=%s, channel=%s, thread=%s\n---\n%s\n---\nEND MESSAGE %s", id, m.ID, m.Channel, m.Thread, m.Message, m.ID)
	}
}
