package main

import (
	"context"
	"fmt"
	"github.com/creack/pty"
	"github.com/slack-go/slack"
	"io"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	shellEscapeRegex = regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)
	controlCharTable = map[string]byte {
		"r": 0x10,
		"ret": 0x10,
		"ctrl-c": 0x03,
		"ctrl-d": 0x04,
	}
	availableREPLs = map[string]string {
		"bash": "docker run -it ubuntu",
		"python": "docker run -it python",
		"nodejs": "docker run -it node",
	}
	welcomeMessage = "REPLbot welcomes you!\n\nYou may start a new session by choosing any one of the " +
		"available REPLs: %s. Type `!help` for help and `!exit` to exit this session."
	sessionExitedMessage = "REPL session ended.\n\nYou may start a new session by choosing any one of the " +
		"available REPLs: %s. Type `!help` for help and `!exit` to exit this session."
	byeMessage = "REPLbot says bye bye!"
	availableCommandsMessage = "Available commands:\n" +
		"  `!ret`, `!r` - Send empty return\n" +
		"  `!ctrl-c`, `!ctrl-d`, ... - Send command sequence\n" +
		"  `!exit` - Exit this session"
)

const (
	maxMessageLength = 512
)

type Session struct {
	rtm *slack.RTM
	started time.Time
	lastAction time.Time
	channel string
	threadTS string
	inputChan chan string
	closed bool
	mu sync.Mutex
}

func NewSession(rtm *slack.RTM, channel string, threadTS string) *Session {
	session := &Session{
		rtm: rtm,
		started: time.Now(),
		lastAction: time.Now(),
		channel: channel,
		threadTS: threadTS,
		inputChan: make(chan string, 10), // buffered!
		closed: false,
	}
	go session.inputLoop()
	return session
}

func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Session) inputLoop() {
	s.sayHello()

	for input := range s.inputChan {
		if input == "!exit" {
			s.close(byeMessage)
			return
		} else if input == "!help" {
			s.sendMarkdown(availableCommandsMessage)
			continue
		}
		command, ok := availableREPLs[input]
		if !ok {
			s.sendMarkdown("Invalid command")
			continue
		}
		s.replSession(command)
		s.sayExited()
	}
}

func (s *Session) sayHello() error {
	_, err := s.sendMarkdown(fmt.Sprintf(welcomeMessage, strings.Join(s.replList(), ", ")))
	return err
}

func (s *Session) sayExited() error {
	_, err := s.sendMarkdown(fmt.Sprintf(sessionExitedMessage, strings.Join(s.replList(), ", ")))
	return err
}

func (s *Session) replList() []string {
	repls := make([]string, 0)
	for name, _ := range availableREPLs {
		repls = append(repls, fmt.Sprintf("`%s`", name))
	}
	return repls
}

func (s *Session) replSession(command string) error {
	c := exec.Command("sh", "-c", command)
	ptmx, err := pty.Start(c)
	if err != nil {
		return fmt.Errorf("cannot start REPL session: %s", err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		ptmx.Close()
		c.Process.Kill()
		log.Printf("Closed REPL session")
	}()

	var message string
	readChan := make(chan *result, 10)

	go func() {
		for {
			buf := make([]byte, 4096) // FIXME alloc in a loop!
			n, err := ptmx.Read(buf)
			select {
			case <-ctx.Done():
				log.Printf("Exiting read loop")
				return
			default:
				readChan <- &result{buf[:n], err}
			}
		}
	}()
	go func() {
		for {
			log.Printf("read chan loop")
			select {
			case result := <-readChan:
				if result.err != nil && result.err != io.EOF {
					log.Printf("Error reading from REPL: %s", result.err.Error())
					cancel()
					return
				}
				if len(result.bytes) > 0 {
					message += shellEscapeRegex.ReplaceAllString(string(result.bytes), "")
				}
				if len(message) > maxMessageLength {
					s.sendCode(message)
					message = ""
				}
				if result.err == io.EOF {
					if len(message) > 0 {
						s.sendCode(message)
					}
					s.close("REPL exited. Terminating session")
					return
				}
			case <-time.After(300 * time.Millisecond):
				if len(message) > 0 {
					s.sendCode(message)
					message = ""
				}
			case <-ctx.Done():
				log.Printf("Exiting main output loop")
				return
			}
		}
	}()

	s.sendMarkdown("Started a new REPL session")
	for input := range s.inputChan {
		if strings.HasPrefix(input, "!") {
			if input == "!help" {
				s.sendMarkdown(availableCommandsMessage)
				continue
			} else if input == "!exit" {
				return nil
			} else {
				controlChar, ok := controlCharTable[input[1:]]
				if ok {
					ptmx.Write([]byte{controlChar})
					continue
				}
			}
			// Fallthrough to underlying REPL
		}
		if _, err := io.WriteString(ptmx, fmt.Sprintf("%s\n", input)); err != nil {
			return err
		}
	}

	return nil
}


func (s *Session) sendText(message string) (string, error) {
	return s.send(slack.MsgOptionText(message, false))
}

func (s *Session) sendCode(message string) (string, error) {
	markdown := fmt.Sprintf("```%s```", strings.ReplaceAll(message, "```", "` ` `")) // Hack ...
	return s.sendMarkdown(markdown)
}

func (s *Session) sendMarkdown(markdown string) (string, error) {
	textBlock := slack.NewTextBlockObject("mrkdwn", markdown, false, true)
	sectionBlock := slack.NewSectionBlock(textBlock, nil, nil)
	return s.send(slack.MsgOptionBlocks(sectionBlock))
}

func (s *Session) send(options ...slack.MsgOption) (string, error) {
	options = append(options, slack.MsgOptionTS(s.threadTS))
	_, responseTS, err := s.rtm.PostMessage(s.channel, options...)
	if err != nil {
		log.Printf("Cannot send message: %s", err.Error())
		return "", err
	}
	return responseTS, nil
}

func (s *Session) close(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	log.Printf(message)
	s.sendText(message)
	s.closed = true
}

type result struct {
	bytes []byte
	err error
}


